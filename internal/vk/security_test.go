package vk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// countingAuth accepts the token "good" and client certificates; it counts authentications.
type countingAuth struct {
	authorizer.AuthorizerFunc
	calls   atomic.Int32
	block   chan struct{} // when set, AuthenticateRequest waits on it
	started chan struct{}
}

func (a *countingAuth) AuthenticateRequest(r *http.Request) (*authenticator.Response, bool, error) {
	a.calls.Add(1)
	if a.started != nil {
		a.started <- struct{}{}
	}
	if a.block != nil {
		<-a.block
	}
	if bearerToken(r) == "good" || (r.TLS != nil && len(r.TLS.PeerCertificates) > 0) {
		return &authenticator.Response{User: &user.DefaultInfo{Name: "u"}}, true, nil
	}
	return nil, false, nil
}

func (a *countingAuth) GetRequestAttributes(u user.Info, r *http.Request) authorizer.Attributes {
	return authorizer.AttributesRecord{User: u}
}

func tokenRequest(ip, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/pods", nil)
	r.RemoteAddr = ip + ":40000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// Random bad tokens from one source cost at most AuthFailureBurst TokenReviews; a repeated bad
// token costs one; client certificates and other sources are not affected.
func TestGuardAuthBoundsTokenReviews(t *testing.T) {
	inner := &countingAuth{}
	g := GuardAuth(inner)
	for i := range 100 {
		if _, ok, _ := g.AuthenticateRequest(tokenRequest("10.0.0.1", "bad-"+string(rune('a'+i%26))+strings.Repeat("x", i))); ok {
			t.Fatal("bad token accepted")
		}
	}
	if n := inner.calls.Load(); n > AuthFailureBurst+1 {
		t.Errorf("%d TokenReviews for 100 bad tokens from one IP, want at most %d", n, AuthFailureBurst+1)
	}
	if _, _, err := g.AuthenticateRequest(tokenRequest("10.0.0.1", "good")); !errors.Is(err, errTooManyAuthFailures) {
		t.Errorf("token from a blocked IP: %v", err)
	}
	cert := tokenRequest("10.0.0.1", "")
	cert.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	if _, ok, _ := g.AuthenticateRequest(cert); !ok {
		t.Error("client certificate refused from a blocked IP")
	}
	if _, ok, _ := g.AuthenticateRequest(tokenRequest("10.0.0.2", "good")); !ok {
		t.Error("good token from another IP refused")
	}

	inner2 := &countingAuth{}
	g2 := GuardAuth(inner2)
	for range 5 {
		_, _, _ = g2.AuthenticateRequest(tokenRequest("10.0.0.3", "same-bad"))
	}
	if n := inner2.calls.Load(); n != 1 {
		t.Errorf("a repeated bad token cost %d TokenReviews, want 1", n)
	}
}

func TestGuardAuthCapsConcurrentReviews(t *testing.T) {
	inner := &countingAuth{block: make(chan struct{}), started: make(chan struct{}, MaxConcurrentTokenReviews)}
	g := GuardAuth(inner)
	var wg sync.WaitGroup
	for i := range MaxConcurrentTokenReviews {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = g.AuthenticateRequest(tokenRequest("10.1.0."+string(rune('0'+i%10)), "good"))
		}()
	}
	for range MaxConcurrentTokenReviews {
		<-inner.started
	}
	if _, _, err := g.AuthenticateRequest(tokenRequest("10.2.0.1", "good")); !errors.Is(err, errAuthUnavailable) {
		t.Errorf("review beyond the cap: %v", err)
	}
	close(inner.block)
	wg.Wait()
}

// floodStreamer streams data until closed (follow) and reports when it was closed.
type floodStreamer struct {
	block  chan struct{} // non-follow requests wait on it
	closed chan struct{}
	once   sync.Once
}

func (f *floodStreamer) Logs(ctx context.Context, _ workerlogs.Target, o workerlogs.Options) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		if !o.Follow && f.block != nil {
			<-f.block
		}
		line := []byte(strings.Repeat("x", 1023) + "\n")
		for ctx.Err() == nil {
			if _, err := pw.Write(line); err != nil {
				return
			}
			if !o.Follow && f.block != nil {
				break
			}
		}
		_ = pw.Close()
	}()
	return closeFunc{ReadCloser: pr, close: func() {
		f.once.Do(func() {
			if f.closed != nil {
				close(f.closed)
			}
		})
		_ = pw.Close()
	}}, nil
}

type closeFunc struct {
	io.ReadCloser
	close func()
}

func (c closeFunc) Close() error { c.close(); return c.ReadCloser.Close() }

func limitedServer(t *testing.T, s workerlogs.Streamer, l ServerLimits, targets map[string]workerlogs.Target) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(HandlerConfig{NodeName: "cf-workers", Resolver: fakeResolver{targets: targets}, Streamer: s, Limits: l}))
	t.Cleanup(srv.Close)
	return srv
}

// A reader that stops reading cannot hold a follow (and its slot): the stream ends after the
// write timeout.
func TestStalledReaderEndsStream(t *testing.T) {
	fs := &floodStreamer{closed: make(chan struct{})}
	srv := limitedServer(t, fs, ServerLimits{WriteTimeout: 200 * time.Millisecond}, map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget})
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	_, _ = io.WriteString(conn, "GET /containerLogs/ns/api-worker/worker?follow=true HTTP/1.1\r\nHost: x\r\n\r\n")
	// Never read.
	select {
	case <-fs.closed:
	case <-time.After(20 * time.Second):
		t.Fatal("a stalled reader kept the stream open")
	}
}

func TestMaxStreamDuration(t *testing.T) {
	fs := &fakeStreamer{lines: []string{"x"}, live: make(chan string), closed: make(chan struct{})}
	srv := limitedServer(t, fs, ServerLimits{MaxStreamDuration: 300 * time.Millisecond}, map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget})
	resp, err := http.Get(srv.URL + "/containerLogs/ns/api-worker/worker?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow outlived the maximum stream duration")
	}
}

func TestFollowAndRequestCaps(t *testing.T) {
	targets := map[string]workerlogs.Target{}
	for _, p := range []string{"a", "b", "c"} {
		targets["ns/"+p+"/worker"] = workerlogs.Target{AccountID: "acct", Script: p}
		targets["other/"+p+"/worker"] = workerlogs.Target{AccountID: "acct", Script: "o" + p}
	}
	block := make(chan struct{})
	defer close(block)
	srv := limitedServer(t, &floodStreamer{block: block},
		ServerLimits{MaxFollowersPerScript: 2, MaxFollowersPerNamespace: 3, MaxConcurrentRequests: 1}, targets)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := func(path string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
		}
		return resp.StatusCode
	}
	for i, tc := range []struct {
		path string
		want int
	}{
		{"/containerLogs/ns/a/worker?follow=true", 200},
		{"/containerLogs/ns/a/worker?follow=true", 200},
		{"/containerLogs/ns/a/worker?follow=true", 429}, // third follower of script a
		{"/containerLogs/ns/b/worker?follow=true", 200},
		{"/containerLogs/ns/c/worker?follow=true", 429},    // fourth in namespace ns
		{"/containerLogs/other/a/worker?follow=true", 200}, // another namespace
	} {
		if got := open(tc.path); got != tc.want {
			t.Errorf("%d %s: %d, want %d", i, tc.path, got, tc.want)
		}
	}
	// Non-follow requests: one at a time (the first blocks in the streamer).
	first := make(chan int, 1)
	go func() { first <- open("/containerLogs/other/b/worker") }()
	time.Sleep(200 * time.Millisecond)
	if got := open("/containerLogs/other/c/worker"); got != http.StatusTooManyRequests {
		t.Errorf("second concurrent request: %d, want 429", got)
	}
}

// Errors reach the caller as fixed messages; account names and internal errors stay in the log.
func TestPublicErrorsDoNotLeak(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		leak   string
	}{
		{&reconcile.AccountError{Reason: "AccountNotReady", Message: "CloudflareAccount secret-ns/prod-acct is not Ready (TokenInvalid)"}, 503, "prod-acct"},
		{errors.New("dial tcp 10.96.0.1:443: connection refused"), 500, "10.96.0.1"},
		{errors.Join(ErrNotStandIn, errors.New("WorkerScript ns/hidden: gone")), 404, "hidden"},
	} {
		s, msg := publicError(tc.err)
		if s != tc.status || strings.Contains(msg, tc.leak) {
			t.Errorf("%v: %d %q", tc.err, s, msg)
		}
	}
}

func resolverScheme(t *testing.T) *runtime.Scheme {
	s := testScheme(t)
	if err := cloudflarev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The resolver serves only the stand-in Pod name of a WorkerScript, and only in namespaces the
// namespace selector matches.
func TestResolverNameAndNamespace(t *testing.T) {
	ctx := context.Background()
	ws := workerScript("team", "api", "uid-api", true)
	good := standInPod(ws, "cf-workers")
	forged := standInPod(ws, "cf-workers")
	forged.Name = "api-logs"
	c := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(ws, good, forged,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", Labels: map[string]string{"logs": "on"}}},
	).Build()
	accounts := reconcile.NewAccounts(ReadOnly(c))
	r := NewResolver(c, accounts, "cf-workers")
	if _, err := r.Resolve(ctx, "team", "api-logs", "worker"); !errors.Is(err, ErrNotStandIn) {
		t.Errorf("forged pod name: %v, want ErrNotStandIn", err)
	}
	// The real stand-in gets as far as the account (none exists here).
	if _, err := r.Resolve(ctx, "team", good.Name, "worker"); !reconcile.IsAccountNotReady(err) {
		t.Errorf("stand-in pod: %v, want an account error", err)
	}
	r.WithNamespaceSelector(labels.SelectorFromSet(labels.Set{"logs": "off"}))
	if _, err := r.Resolve(ctx, "team", good.Name, "worker"); !errors.Is(err, ErrPodNotFound) {
		t.Errorf("namespace outside the selector: %v, want ErrPodNotFound", err)
	}
	r2 := NewResolver(c, accounts, "cf-workers").WithNamespaceSelector(labels.SelectorFromSet(labels.Set{"logs": "on"}))
	if _, err := r2.Resolve(ctx, "team", good.Name, "worker"); !reconcile.IsAccountNotReady(err) {
		t.Errorf("namespace inside the selector: %v", err)
	}
}
