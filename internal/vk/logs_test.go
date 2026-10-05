package vk

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// TestParseLogOptions checks the kubelet-compatible parsing the handler uses by default.
func TestParseLogOptions(t *testing.T) {
	since := testNow.Add(-90 * time.Second)
	sinceTime := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		query string
		want  workerlogs.Options
		bad   bool
	}{
		{query: "", want: workerlogs.Options{}},
		{query: "follow=true&timestamps=1&previous=false", want: workerlogs.Options{Follow: true, Timestamps: true}},
		// absent tailLines (nil) and tailLines=0 differ
		{query: "tailLines=0", want: workerlogs.Options{TailLines: ptr.To[int64](0)}},
		{query: "tailLines=50", want: workerlogs.Options{TailLines: ptr.To[int64](50)}},
		{query: "limitBytes=1024", want: workerlogs.Options{LimitBytes: ptr.To[int64](1024)}},
		{query: "sinceSeconds=90", want: workerlogs.Options{SinceTime: &since}},
		{query: "sinceTime=2026-10-04T11:00:00Z", want: workerlogs.Options{SinceTime: &sinceTime}},
		{query: "previous=true", want: workerlogs.Options{Previous: true}},
		// like the kubelet (apimachinery Convert_Slice_string_To_bool), anything but "0"/"false" is true
		{query: "follow=maybe", want: workerlogs.Options{Follow: true}},
		// parameters the kubelet ignores here
		{query: "container=worker&stream=All&insecureSkipTLSVerifyBackend=true", want: workerlogs.Options{}},
		{query: "tailLines=-1", bad: true},
		{query: "tailLines=x", bad: true},
		{query: "limitBytes=0", bad: true},
		{query: "sinceSeconds=0", bad: true},
		{query: "sinceSeconds=10&sinceTime=2026-10-04T11:00:00Z", bad: true},
		{query: "sinceTime=yesterday", bad: true},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := workerlogs.ParseOptions(q, testNow)
		if tc.bad {
			if err == nil { // the handler turns any parse error into a 400 (TestLogsHandler*)
				t.Errorf("%q: err = nil, want an error", tc.query)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.query, err)
			continue
		}
		if fmt.Sprint(describe(got)) != fmt.Sprint(describe(tc.want)) {
			t.Errorf("%q: got %v, want %v", tc.query, describe(got), describe(tc.want))
		}
	}
}

func describe(o workerlogs.Options) string {
	s := fmt.Sprintf("follow=%v ts=%v prev=%v", o.Follow, o.Timestamps, o.Previous)
	if o.TailLines != nil {
		s += fmt.Sprintf(" tail=%d", *o.TailLines)
	}
	if o.LimitBytes != nil {
		s += fmt.Sprintf(" limit=%d", *o.LimitBytes)
	}
	if o.SinceTime != nil {
		s += " since=" + o.SinceTime.UTC().Format(time.RFC3339)
	}
	return s
}

// fakeResolver answers Resolve from a table.
type fakeResolver struct {
	targets map[string]workerlogs.Target // "ns/pod/container"
	err     error
}

func (f fakeResolver) Resolve(_ context.Context, ns, name, container string) (workerlogs.Target, error) {
	if f.err != nil {
		return workerlogs.Target{}, f.err
	}
	t, ok := f.targets[ns+"/"+name+"/"+container]
	if !ok {
		return workerlogs.Target{}, ErrPodNotFound
	}
	return t, nil
}

// fakeStreamer records the last call and streams lines; with Follow it then streams from live
// until ctx ends or the reader is closed.
type fakeStreamer struct {
	mu     sync.Mutex
	target workerlogs.Target
	opts   workerlogs.Options
	lines  []string
	live   chan string
	closed chan struct{}
	err    error
}

func (f *fakeStreamer) Logs(ctx context.Context, t workerlogs.Target, o workerlogs.Options) (io.ReadCloser, error) {
	f.mu.Lock()
	f.target, f.opts = t, o
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer func() { _ = pw.Close() }()
		for _, l := range f.lines {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
		}
		if !o.Follow {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case l := <-f.live:
				if _, err := io.WriteString(pw, l+"\n"); err != nil {
					return
				}
			}
		}
	}()
	return &closeNotifier{ReadCloser: pr, once: &sync.Once{}, done: done, notify: f.closed}, nil
}

func (f *fakeStreamer) last() (workerlogs.Target, workerlogs.Options) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, f.opts
}

type closeNotifier struct {
	io.ReadCloser
	once   *sync.Once
	done   chan struct{}
	notify chan struct{}
}

func (c *closeNotifier) Close() error {
	c.once.Do(func() {
		close(c.done)
		if c.notify != nil {
			close(c.notify)
		}
	})
	return c.ReadCloser.Close()
}

var apiTarget = workerlogs.Target{AccountID: "acct", Script: "api", WorkerScript: types.NamespacedName{Namespace: "ns", Name: "api"}}

func newTestHandler(r PodResolver, s workerlogs.Streamer) http.Handler {
	return NewHandler(HandlerConfig{NodeName: "cf-workers", Resolver: r, Streamer: s, Now: func() time.Time { return testNow }})
}

func TestLogsHandlerMapsQueryAndStreams(t *testing.T) {
	fs := &fakeStreamer{lines: []string{"[log] one", "[log] two"}}
	srv := httptest.NewServer(newTestHandler(fakeResolver{targets: map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget}}, fs))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/containerLogs/ns/api-worker/worker?tailLines=0&timestamps=true&sinceSeconds=600&limitBytes=7")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if string(body) != "[log] o" {
		t.Errorf("body %q, want the first 7 bytes", body)
	}
	target, opts := fs.last()
	if target != apiTarget {
		t.Errorf("target %+v", target)
	}
	since := testNow.Add(-10 * time.Minute)
	want := workerlogs.Options{TailLines: ptr.To[int64](0), Timestamps: true, SinceTime: &since, LimitBytes: ptr.To[int64](7)}
	if describe(opts) != describe(want) {
		t.Errorf("options %v, want %v", describe(opts), describe(want))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q", ct)
	}
}

// A follow delivers each line as it arrives (flushes), and the caller going away closes the
// stream (ending the upstream tail).
func TestLogsHandlerFollowFlushesAndCloses(t *testing.T) {
	fs := &fakeStreamer{lines: []string{"history"}, live: make(chan string), closed: make(chan struct{})}
	srv := httptest.NewServer(newTestHandler(fakeResolver{targets: map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget}}, fs))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/containerLogs/ns/api-worker/worker?follow=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	readLine := func() string {
		t.Helper()
		got := make(chan string, 1)
		go func() {
			l, _ := r.ReadString('\n')
			got <- l
		}()
		select {
		case l := <-got:
			return l
		case <-time.After(5 * time.Second):
			t.Fatal("no line within 5s: the handler does not flush")
			return ""
		}
	}
	if l := readLine(); l != "history\n" {
		t.Fatalf("first line %q", l)
	}
	fs.live <- "live 1"
	if l := readLine(); l != "live 1\n" {
		t.Fatalf("live line %q", l)
	}
	if _, o := fs.last(); !o.Follow {
		t.Error("Follow not passed to the streamer")
	}
	cancel()
	select {
	case <-fs.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the log stream was not closed after the caller went away")
	}
}

func TestLogsHandlerErrors(t *testing.T) {
	targets := map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget}
	for _, tc := range []struct {
		name     string
		path     string
		method   string
		resolver PodResolver
		streamer *fakeStreamer
		status   int
		body     string
	}{
		{name: "unknown pod", path: "/containerLogs/ns/nope/worker", status: 404, body: ErrPodNotFound.Error()},
		{name: "not a stand-in", path: "/containerLogs/ns/api-worker/worker", resolver: fakeResolver{err: ErrNotStandIn}, status: 404},
		{name: "other container", path: "/containerLogs/ns/api-worker/sidecar", resolver: fakeResolver{err: ErrContainerNotFound}, status: 404, body: `one container, "worker"`},
		{name: "previous", path: "/containerLogs/ns/api-worker/worker?previous=true", status: 400, body: workerlogs.ErrPreviousUnsupported.Error()},
		{name: "bad query", path: "/containerLogs/ns/api-worker/worker?tailLines=-3", status: 400},
		{name: "account not ready", path: "/containerLogs/ns/api-worker/worker",
			resolver: fakeResolver{err: &reconcile.AccountError{Reason: "AccountNotReady", Message: "CloudflareAccount ns/a is not Ready"}}, status: 503, body: "account is not ready"},
		{name: "too many followers", path: "/containerLogs/ns/api-worker/worker?follow=true",
			streamer: &fakeStreamer{err: workerlogs.ErrTooManyFollowers}, status: 429},
		{name: "token lacks permission", path: "/containerLogs/ns/api-worker/worker",
			streamer: &fakeStreamer{err: &cfclient.APIError{Status: 403, Errors: []cfclient.ErrorDetail{{Code: 10000, Message: "Authentication error"}}}},
			status:   403, body: "Cloudflare API"},
		{name: "insecure tail", path: "/containerLogs/ns/api-worker/worker?follow=true", streamer: &fakeStreamer{err: workerlogs.ErrInsecureTailURL}, status: 502},
		{name: "exec", path: "/exec/ns/api-worker/worker?command=sh&stdin=true", method: http.MethodPost, status: 501, body: "only `kubectl logs` is available"},
		{name: "attach", path: "/attach/ns/api-worker/worker", method: http.MethodPost, status: 501},
		{name: "port-forward", path: "/portForward/ns/api-worker", method: http.MethodPost, status: 501},
		{name: "run", path: "/run/ns/api-worker/worker", method: http.MethodPost, status: 501},
		{name: "unknown path", path: "/configz", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.resolver
			if r == nil {
				r = fakeResolver{targets: targets}
			}
			s := tc.streamer
			if s == nil {
				s = &fakeStreamer{}
			}
			srv := httptest.NewServer(newTestHandler(r, s))
			defer srv.Close()
			m := tc.method
			if m == "" {
				m = http.MethodGet
			}
			req, _ := http.NewRequest(m, srv.URL+tc.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.body) {
				t.Errorf("got %d %q, want %d containing %q", resp.StatusCode, body, tc.status, tc.body)
			}
		})
	}
}

func TestEmptyNodeEndpoints(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(fakeResolver{}, &fakeStreamer{}))
	defer srv.Close()
	for path, want := range map[string]string{
		"/pods":             `"kind":"PodList"`,
		"/stats/summary":    `"nodeName":"cf-workers"`,
		"/metrics/resource": "",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("%s: %d %q, want 200 containing %q", path, resp.StatusCode, body, want)
		}
	}
}
