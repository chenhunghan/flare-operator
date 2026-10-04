package artifact

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tlsServer serves h over TLS and returns it with a loader that trusts it and may connect to
// loopback (the allowlist the manager's --artifact-allowed-cidr gives).
func tlsServer(t *testing.T, h http.Handler, mut ...func(*Options)) (*httptest.Server, *Loader) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	o := Options{RootCAs: pool, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, CacheBytes: 1 << 20, UserAgent: "artifact-test"}
	for _, m := range mut {
		m(&o)
	}
	l, err := NewLoader(o)
	if err != nil {
		t.Fatal(err)
	}
	return srv, l
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func urlSource(u, sha, p string) sharedv1alpha1.ArtifactSource {
	return sharedv1alpha1.ArtifactSource{URL: &sharedv1alpha1.URLArtifactSource{URL: u, SHA256: sha, Path: p}}
}

func TestURLSource(t *testing.T) {
	archive := gz(t, tarOf(t, file("dist/index.html", "<h1>ok</h1>"), file("dist/app.js", "x()"), file("src/main.ts", "secret")))
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/site.tgz", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") != "artifact-test" {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/site.tgz", http.StatusFound) })
	mux.HandleFunc("/missing", http.NotFound)
	srv, l := tlsServer(t, mux)

	tr, err := l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/site.tgz", sum(archive), "dist"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.Paths(), ",") != "app.js,index.html" || tr.ResolvedDigest != "" {
		t.Errorf("paths %v resolved %q", tr.Paths(), tr.ResolvedDigest)
	}
	f, _ := tr.File("app.js")
	if f.ContentType != "text/javascript; charset=utf-8" {
		t.Errorf("app.js content type %q", f.ContentType)
	}

	// Cached by URL, SHA-256 and path: no second download.
	if _, err := l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/site.tgz", sum(archive), "dist")); err != nil || hits.Load() != 1 {
		t.Errorf("second load: %v, %d downloads", err, hits.Load())
	}
	// A redirect to https is followed.
	if tr2, err := l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/redirect", sum(archive), "dist")); err != nil || tr2.Digest != tr.Digest {
		t.Errorf("redirect: %v", err)
	}

	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/site.tgz", strings.Repeat("0", 64), ""))
	wantKind(t, err, KindRejected, "SHA-256")
	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/missing", sum(archive), ""))
	wantKind(t, err, KindFetch, "HTTP 404")
}

func TestURLSourceInvalid(t *testing.T) {
	l, err := NewLoader(Options{})
	if err != nil {
		t.Fatal(err)
	}
	good := strings.Repeat("a", 64)
	for name, src := range map[string]sharedv1alpha1.ArtifactSource{
		"http":           urlSource("http://example.com/a.tgz", good, ""),
		"no host":        urlSource("https:///a.tgz", good, ""),
		"credentials":    urlSource("https://user:pw@example.com/a.tgz", good, ""),
		"sha256 case":    urlSource("https://example.com/a.tgz", strings.ToUpper(good), ""),
		"sha256 short":   urlSource("https://example.com/a.tgz", "abc", ""),
		"path traversal": urlSource("https://example.com/a.tgz", good, "../x"),
		"none":           {},
		"two": {URL: &sharedv1alpha1.URLArtifactSource{URL: "https://example.com", SHA256: good},
			OCIRef: &sharedv1alpha1.OCIArtifactSource{Image: "example.com/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := l.Load(ctx(t), nil, "ns", src)
			wantKind(t, err, KindInvalid, "")
			if strings.Contains(err.Error(), "pw") {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestURLSourceNetworkPolicy(t *testing.T) {
	archive := tarOf(t, file("a", "b"))
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(plain.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/a.tar", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/a.tar", http.StatusFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(DefaultMaxArchiveBytes+1))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 3; i++ {
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	})
	srv, l := tlsServer(t, mux, func(o *Options) { o.Limits.MaxArchiveBytes = 2 << 20 })

	// Without the allowlist, loopback is refused at connect time.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	strict, err := NewLoader(Options{RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	_, err = strict.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/a.tar", sum(archive), ""))
	wantKind(t, err, KindFetch, "not a public address")
	// The same through a host name that resolves to loopback (as a rebinding DNS answer would).
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")
	_, err = strict.Load(ctx(t), nil, "ns", urlSource("https://localhost:"+port+"/a.tar", sum(archive), ""))
	wantKind(t, err, KindFetch, "not a public address")

	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/to-http", sum(archive), ""))
	wantKind(t, err, KindFetch, "only https")
	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/to-metadata", sum(archive), ""))
	wantKind(t, err, KindFetch, "not a public address")
	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/loop", sum(archive), ""))
	wantKind(t, err, KindFetch, "redirects")
	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/big", sum(archive), ""))
	wantKind(t, err, KindRejected, "--artifact-max-archive-bytes")
	_, err = l.Load(ctx(t), nil, "ns", urlSource(srv.URL+"/big-chunked", sum(archive), ""))
	wantKind(t, err, KindRejected, "--artifact-max-archive-bytes")
}

func TestAddrPolicy(t *testing.T) {
	strict := addrPolicy{}
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200",
		"0.0.0.0", "255.255.255.255", "224.0.0.1", "::1", "::", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "fd00:ec2::254",
		"fe80::1", "64:ff9b::a00:1", "2002:a00:1::1", "2001::1", "ff02::1", "fe80::1%eth0"} {
		if err := strict.check(netip.MustParseAddr(a)); err == nil {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"104.16.0.1", "8.8.8.8", "2606:4700::6810:1", "::ffff:1.1.1.1"} {
		if err := strict.check(netip.MustParseAddr(a)); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	allow := addrPolicy{allowed: []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}}
	if err := allow.check(netip.MustParseAddr("10.96.0.10")); err != nil {
		t.Errorf("allowlisted address refused: %v", err)
	}
	if err := allow.check(netip.MustParseAddr("::ffff:10.96.0.10")); err != nil {
		t.Errorf("IPv4-mapped allowlisted address refused: %v", err)
	}
	if err := allow.check(netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("address outside the allowlist allowed")
	}
}
