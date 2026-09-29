package cfclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"flare.dev/operator/internal/fake"
)

func TestMain(m *testing.M) {
	backoffBase = 5 * time.Millisecond
	backoffCap = 50 * time.Millisecond
	os.Exit(m.Run())
}

func newTestClient(t *testing.T, srvURL string, mod func(*Options)) Client {
	t.Helper()
	o := Options{Token: "tok-" + t.Name(), BaseURL: srvURL + "/client/v4", RPS: 1000, Burst: 1000}
	if mod != nil {
		mod(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeEnv(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty token accepted")
	}
	if _, err := New(Options{Token: "a\nb"}); err == nil {
		t.Error("token with newline accepted")
	}
	if _, err := New(Options{Token: "x", BaseURL: "ftp://x"}); err == nil {
		t.Error("ftp base URL accepted")
	}
	if _, err := New(Options{Token: "x"}); err != nil {
		t.Errorf("default base URL: %v", err)
	}
}

func TestDoDecodesEnvelopeAndSendsHeaders(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{"id":"x"},"result_info":{"page":2,"per_page":5,"count":1,"total_count":6,"total_pages":2}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.UserAgent = "ua-test" })
	resp, err := c.Do(context.Background(), Request{Method: "post", Path: "accounts/a/things", Query: url.Values{"x": {"1 2"}}, Body: map[string]string{"name": "n"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/client/v4/accounts/a/things" || got.URL.Query().Get("x") != "1 2" {
		t.Errorf("request %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer tok-"+t.Name() || got.Header.Get("User-Agent") != "ua-test" || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", got.Header)
	}
	if gotBody != `{"name":"n"}` {
		t.Errorf("body %q", gotBody)
	}
	if string(resp.Result) != `{"id":"x"}` || resp.Status != 200 || resp.ResultInfo == nil || resp.ResultInfo.TotalPages != 2 || resp.ResultInfo.Page != 2 {
		t.Errorf("resp %+v %+v", resp, resp.ResultInfo)
	}
}

func TestDoRawBodyAndBothBodies(t *testing.T) {
	var ct, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":null}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	if _, err := c.Do(context.Background(), Request{Method: "PUT", Path: "/x", RawBody: []byte("--b--"), ContentType: "multipart/form-data; boundary=b"}); err != nil {
		t.Fatal(err)
	}
	if ct != "multipart/form-data; boundary=b" || body != "--b--" {
		t.Errorf("ct=%q body=%q", ct, body)
	}
	if _, err := c.Do(context.Background(), Request{Method: "PUT", Path: "/x", RawBody: []byte("a"), Body: 1}); err == nil {
		t.Error("both bodies accepted")
	}
}

func TestDoErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(t *testing.T, err error)
	}{
		{"success-false-200", 200, `{"success":false,"errors":[{"code":10014,"message":"dup"}],"messages":[],"result":null}`, func(t *testing.T, err error) {
			ae, ok := AsAPIError(err)
			if !ok || ae.Status != 200 || !ae.HasCode(10014) || !HasCode(err, 10014) {
				t.Errorf("%#v", err)
			}
		}},
		{"404", 404, `{"success":false,"errors":[{"code":10007,"message":"not found"}],"messages":[],"result":null}`, func(t *testing.T, err error) {
			if !IsNotFound(err) || !HasCode(err, 10007) || HasCode(err, 1) {
				t.Errorf("%#v", err)
			}
			if !strings.Contains(err.Error(), "10007: not found") {
				t.Errorf("message %q", err.Error())
			}
		}},
		{"d1-bare", 400, `{"success":false,"errors":[{"code":7500,"message":"bad"}]}`, func(t *testing.T, err error) {
			if !HasCode(err, 7500) {
				t.Errorf("%#v", err)
			}
		}},
		{"html-502", 502, `<html>bad gateway</html>`, func(t *testing.T, err error) {
			ae, ok := AsAPIError(err)
			if !ok || ae.Status != 502 || len(ae.Errors) != 1 || !strings.Contains(ae.Errors[0].Message, "non-JSON") {
				t.Errorf("%#v", err)
			}
		}},
		{"empty-403", 403, ``, func(t *testing.T, err error) {
			ae, ok := AsAPIError(err)
			if !ok || ae.Status != 403 {
				t.Errorf("%#v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeEnv(w, tc.status, tc.body) }))
			defer srv.Close()
			// POST so 5xx is not retried.
			_, err := newTestClient(t, srv.URL, nil).Do(context.Background(), Request{Method: "POST", Path: "/x"})
			if err == nil {
				t.Fatal("no error")
			}
			tc.check(t, err)
		})
	}
}

func TestDo204NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	resp, err := newTestClient(t, srv.URL, nil).Do(context.Background(), Request{Method: "DELETE", Path: "/accounts/a/tags", Body: map[string]string{"resource_id": "x"}})
	if err != nil || resp.Status != 204 || resp.Result != nil {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestRatelimitHeaderIgnored(t *testing.T) {
	// The server claims the budget is exhausted on every response; the client must not slow down.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Ratelimit", `"default";r=0;t=300`)
		w.Header().Set("Ratelimit-Policy", `"default";q=1200;w=300`)
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.RPS, o.Burst = 0, 0 }) // defaults: burst 20
	start := time.Now()
	for range 10 {
		if _, err := c.Do(context.Background(), Request{Path: "/x"}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("10 requests took %v; Ratelimit header must be ignored", d)
	}
}

func TestLimiterSharedPerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	}))
	defer srv.Close()
	mk := func(token string) Client {
		c, err := New(Options{Token: token, BaseURL: srv.URL, RPS: 20, Burst: 1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := mk("shared-"+t.Name()), mk("shared-"+t.Name())
	start := time.Now()
	for i := range 6 {
		c := a
		if i%2 == 1 {
			c = b
		}
		if _, err := c.Do(context.Background(), Request{Path: "/x"}); err != nil {
			t.Fatal(err)
		}
	}
	// 6 requests at 20 rps with burst 1 need ≥ 5 intervals of 50 ms.
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("shared limiter not applied across clients: %v", d)
	}
	// A different token has its own limiter.
	other := mk("other-" + t.Name())
	start = time.Now()
	if _, err := other.Do(context.Background(), Request{Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 40*time.Millisecond {
		t.Errorf("independent token waited %v", d)
	}
}

func TestRetryAfterOn429(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			writeEnv(w, 429, `{"success":false,"errors":[{"code":971,"message":"slow down"}],"messages":[],"result":null}`)
			return
		}
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{"ok":true}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	start := time.Now()
	// POST: a 429 means "not processed", so even non-idempotent calls are retried.
	resp, err := c.Do(context.Background(), Request{Method: "POST", Path: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Errorf("Retry-After not honoured: %v", d)
	}
	if hits.Load() != 2 || string(resp.Result) != `{"ok":true}` {
		t.Errorf("hits=%d result=%s", hits.Load(), resp.Result)
	}
}

func TestLongRetryAfterSurfacesAndBlocksToken(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "120")
		writeEnv(w, 429, `{"success":false,"errors":[{"code":971,"message":"slow down"}],"messages":[],"result":null}`)
	}))
	defer srv.Close()
	a := newTestClient(t, srv.URL, nil)
	b := newTestClient(t, srv.URL, nil) // same token → same back-off state
	_, err := a.Do(context.Background(), Request{Path: "/x"})
	ae, ok := AsAPIError(err)
	if !ok || ae.Status != 429 || ae.RetryAfter != 120*time.Second || !ae.HasCode(971) {
		t.Fatalf("%#v", err)
	}
	_, err = b.Do(context.Background(), Request{Path: "/y"})
	ae, ok = AsAPIError(err)
	if !ok || ae.Status != 429 || ae.RetryAfter < 100*time.Second {
		t.Fatalf("second client not blocked: %#v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("blocked token still hit the server: %d", hits.Load())
	}
}

func Test5xxRetries(t *testing.T) {
	type tc struct {
		name      string
		method    string
		failFirst int32
		ctx       func(context.Context) context.Context
		maxRetry  int
		wantHits  int32
		wantErr   bool
	}
	for _, c := range []tc{
		{name: "get-recovers", method: "GET", failFirst: 2, wantHits: 3},
		{name: "put-recovers", method: "PUT", failFirst: 1, wantHits: 2},
		{name: "post-not-retried", method: "POST", failFirst: 1, wantHits: 1, wantErr: true},
		{name: "patch-not-retried", method: "PATCH", failFirst: 1, wantHits: 1, wantErr: true},
		{name: "bounded", method: "GET", failFirst: 100, maxRetry: 2, wantHits: 3, wantErr: true},
		{name: "ctx-no-retry", method: "GET", failFirst: 100, ctx: func(c context.Context) context.Context { return WithMaxRetries(c, 0) }, wantHits: 1, wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) <= c.failFirst {
					writeEnv(w, 503, `{"success":false,"errors":[{"code":10000,"message":"oops"}],"messages":[],"result":null}`)
					return
				}
				writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{}}`)
			}))
			defer srv.Close()
			cl := newTestClient(t, srv.URL, func(o *Options) { o.MaxRetries = c.maxRetry })
			ctx := context.Background()
			if c.ctx != nil {
				ctx = c.ctx(ctx)
			}
			_, err := cl.Do(ctx, Request{Method: c.method, Path: "/x"})
			if (err != nil) != c.wantErr {
				t.Errorf("err=%v", err)
			}
			if hits.Load() != c.wantHits {
				t.Errorf("hits=%d want %d", hits.Load(), c.wantHits)
			}
		})
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, 500, `{"success":false,"errors":[],"messages":[],"result":null}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cl := newTestClient(t, srv.URL, func(o *Options) { o.MaxRetries = 1000 })
	_, err := cl.Do(ctx, Request{Path: "/x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/ns") && r.Method == "GET" {
			writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":[{"id":"1"}]}`)
			return
		}
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{"id":"1"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.ListTTL = time.Hour })
	ctx := context.Background()
	get := func(p string) {
		t.Helper()
		if _, err := c.Do(ctx, Request{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(n int32) {
		t.Helper()
		if h := hits.Load(); h != n {
			t.Fatalf("hits=%d want %d", h, n)
		}
	}
	get("/a/ns")
	get("/a/ns")
	expect(1) // list cached
	get("/a/ns/1")
	get("/a/ns/1")
	expect(3) // item GETs (object results) are not cached
	if _, err := c.Do(WithoutCache(ctx), Request{Path: "/a/ns"}); err != nil {
		t.Fatal(err)
	}
	expect(4)
	// A write under the collection invalidates it.
	if _, err := c.Do(ctx, Request{Method: "PUT", Path: "/a/ns/1", Body: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	expect(5)
	get("/a/ns")
	expect(6)
	get("/a/ns")
	expect(6)
	// A write to an unrelated path does not.
	if _, err := c.Do(ctx, Request{Method: "POST", Path: "/a/other"}); err != nil {
		t.Fatal(err)
	}
	get("/a/ns")
	expect(7)
	// Query strings are part of the key.
	if _, err := c.Do(ctx, Request{Path: "/a/ns", Query: url.Values{"page": {"2"}}}); err != nil {
		t.Fatal(err)
	}
	expect(8)
	// TTL expiry.
	cc := c.(*client).cache
	cc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	get("/a/ns")
	expect(9)
}

func TestRelated(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/a/ns", "/a/ns", true},
		{"/a/ns/1", "/a/ns", true},
		{"/a/ns", "/a/ns/1", true},
		{"/a/ns2", "/a/ns", false},
		{"/a/ns", "/a/other", false},
	} {
		if got := related(tc.a, tc.b); got != tc.want {
			t.Errorf("related(%q,%q)=%v", tc.a, tc.b, got)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	if d := parseRetryAfter("7", now); d != 7*time.Second {
		t.Error(d)
	}
	if d := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); d != 90*time.Second {
		t.Error(d)
	}
	if d := parseRetryAfter("junk", now); d != 0 {
		t.Error(d)
	}
}

// ---- pagination ------------------------------------------------------------------------------

func TestListAllPageBased(t *testing.T) {
	items := make([]int, 23)
	for i := range items {
		items[i] = i
	}
	for _, withTotalPages := range []bool{true, false} {
		t.Run(fmt.Sprint("totalPages=", withTotalPages), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page, per := 1, 10
				fmt.Sscan(r.URL.Query().Get("page"), &page)
				fmt.Sscan(r.URL.Query().Get("per_page"), &per)
				start, end := min((page-1)*per, len(items)), min(page*per, len(items))
				res, _ := json.Marshal(items[start:end])
				info := map[string]int{"page": page, "per_page": per, "count": end - start, "total_count": len(items)}
				if withTotalPages {
					info["total_pages"] = (len(items) + per - 1) / per
				}
				ib, _ := json.Marshal(info)
				writeEnv(w, 200, fmt.Sprintf(`{"success":true,"errors":[],"messages":[],"result":%s,"result_info":%s}`, res, ib))
			}))
			defer srv.Close()
			q := url.Values{"per_page": {"10"}}
			got, err := ListAllInto[int](context.Background(), newTestClient(t, srv.URL, nil), Request{Path: "/x", Query: q})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 23 || got[22] != 22 {
				t.Errorf("got %v", got)
			}
			if q.Get("page") != "" {
				t.Error("caller's query mutated")
			}
		})
	}
}

func TestListAllCursorBased(t *testing.T) {
	pages := map[string]string{
		"":   `{"success":true,"errors":[],"messages":[],"result":[1,2],"result_info":{"count":2,"cursor":"c1"}}`,
		"c1": `{"success":true,"errors":[],"messages":[],"result":[3,4],"result_info":{"count":2,"cursor":"c2"}}`,
		"c2": `{"success":true,"errors":[],"messages":[],"result":[5],"result_info":{"count":1,"cursor":""}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, 200, pages[r.URL.Query().Get("cursor")])
	}))
	defer srv.Close()
	got, err := ListAllInto[int](context.Background(), newTestClient(t, srv.URL, nil), Request{Path: "/keys"})
	if err != nil || len(got) != 5 || got[4] != 5 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestListAllNoResultInfoAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{"not":"array"}}`)
			return
		}
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":[1]}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	got, err := ListAll(context.Background(), c, Request{Path: "/one"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ListAll(context.Background(), c, Request{Path: "/bad"}); err == nil {
		t.Error("object result accepted as a list")
	}
}

// Against flarefake: create 7 KV namespaces and list them with per_page=3 (page-based with
// total_pages, like recordings 0007/0008).
func TestAgainstFlarefake(t *testing.T) {
	fs := fake.New(fake.Options{})
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	ctx := context.Background()
	acct := "0123456789abcdef0123456789abcdef"
	for i := range 7 {
		if _, err := c.Do(ctx, Request{Method: "POST", Path: "/accounts/" + acct + "/storage/kv/namespaces", Body: map[string]string{"title": fmt.Sprintf("ns-%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Do(ctx, Request{Method: "POST", Path: "/accounts/" + acct + "/storage/kv/namespaces", Body: map[string]string{"title": "ns-0"}})
	if !HasCode(err, 10014) {
		t.Errorf("duplicate: %v", err)
	}
	type ns struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	all, err := ListAllInto[ns](ctx, c, Request{Path: "/accounts/" + acct + "/storage/kv/namespaces", Query: url.Values{"per_page": {"3"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 {
		t.Errorf("listed %d namespaces", len(all))
	}
	_, err = c.Do(ctx, Request{Method: "GET", Path: "/accounts/" + acct + "/storage/kv/namespaces/nope"})
	if !IsNotFound(err) {
		t.Errorf("missing: %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Error("not an APIError")
	}
}
