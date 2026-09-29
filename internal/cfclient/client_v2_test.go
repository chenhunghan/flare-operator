package cfclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRequestHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.UserAgent = "ua-owned" })
	h := http.Header{}
	h.Set("If-Match", `"v1:abc"`)
	h.Add("X-Multi", "1")
	h.Add("X-Multi", "2")
	h["x-lower"] = []string{"raw-key"} // not canonicalised by the caller
	h.Set("Authorization", "Bearer stolen")
	h["user-agent"] = []string{"evil"}
	h.Set("Content-Type", "text/plain")
	h.Set("Accept", "application/octet-stream")
	if _, err := c.Do(context.Background(), Request{Method: "PUT", Path: "/x", Body: map[string]int{"a": 1}, Header: h}); err != nil {
		t.Fatal(err)
	}
	if got.Get("If-Match") != `"v1:abc"` || strings.Join(got.Values("X-Multi"), ",") != "1,2" ||
		got.Get("X-Lower") != "raw-key" || got.Get("Accept") != "application/octet-stream" {
		t.Errorf("caller headers not sent: %v", got)
	}
	if got.Get("Authorization") != "Bearer tok-"+t.Name() || got.Get("User-Agent") != "ua-owned" || got.Get("Content-Type") != "application/json" {
		t.Errorf("owned headers overridden: %v", got)
	}
	if len(got.Values("User-Agent")) != 1 || len(got.Values("Authorization")) != 1 {
		t.Errorf("owned headers duplicated: %v", got)
	}
	if h.Get("Authorization") != "Bearer stolen" {
		t.Error("caller's header map was modified")
	}
}

func TestEscapedPathSegments(t *testing.T) {
	var rawURI, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawURI, path = r.RequestURI, r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "v")
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	ctx := context.Background()
	for _, tc := range []struct{ in, wantURI, wantPath string }{
		// A KV key containing '/', escaped by the caller with url.PathEscape.
		{"/accounts/a/storage/kv/namespaces/n/values/" + url.PathEscape("dir/file name"),
			"/client/v4/accounts/a/storage/kv/namespaces/n/values/dir%2Ffile%20name",
			"/client/v4/accounts/a/storage/kv/namespaces/n/values/dir/file name"},
		// Unescaped input is still encoded once.
		{"/values/a b", "/client/v4/values/a%20b", "/client/v4/values/a b"},
		// A bare '%' (not a valid escape) is taken literally.
		{"/values/100%", "/client/v4/values/100%25", "/client/v4/values/100%"},
		// A pre-escaped '%' stays single-encoded.
		{"/values/" + url.PathEscape("100%"), "/client/v4/values/100%25", "/client/v4/values/100%"},
		{"plain/path", "/client/v4/plain/path", "/client/v4/plain/path"},
		// Mixed: a pre-escaped '/' plus characters that still need escaping; the escaped '/'
		// is kept, the rest is encoded once.
		{"/values/dir%2Ffile name", "/client/v4/values/dir%2Ffile%20name", "/client/v4/values/dir/file name"},
		{"/values/a%2Fb%", "/client/v4/values/a%2Fb%25", "/client/v4/values/a/b%"},
		{"/values/x%2fy?z#w", "/client/v4/values/x%2fy%3Fz%23w", "/client/v4/values/x/y?z#w"},
		{"/values/é%2F\"q\"", "/client/v4/values/%C3%A9%2F%22q%22", "/client/v4/values/é/\"q\""},
		{"/values/100%/k%2Fv", "/client/v4/values/100%25/k%2Fv", "/client/v4/values/100%/k/v"},
		// Sub-delims, ':' and '@' are sent literally.
		{"/values/a:b@c!$&'()*+,;=~", "/client/v4/values/a:b@c!$&'()*+,;=~", "/client/v4/values/a:b@c!$&'()*+,;=~"},
		// Any string escaped with url.PathEscape round-trips.
		{"/values/" + url.PathEscape("k/ey %2F?#é"), "/client/v4/values/k%2Fey%20%252F%3F%23%C3%A9", "/client/v4/values/k/ey %2F?#é"},
	} {
		if _, err := c.Do(ctx, Request{Path: tc.in}); err != nil {
			t.Fatal(err)
		}
		if rawURI != tc.wantURI || path != tc.wantPath {
			t.Errorf("%q: sent %q (path %q), want %q (path %q)", tc.in, rawURI, path, tc.wantURI, tc.wantPath)
		}
	}
}

func TestNonEnvelopeJSONIsResult(t *testing.T) {
	for _, body := range []string{`{"a":1,"nested":{"success":true}}`, `{"result":"x"}`, `{}`, `[1,2]`, `"s"`, `plain text`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeEnv(w, 200, body) }))
		resp, err := newTestClient(t, srv.URL, nil).Do(context.Background(), Request{Path: "/accounts/a/storage/kv/namespaces/n/values/k"})
		srv.Close()
		if err != nil || string(resp.Result) != body {
			t.Errorf("body %s: result %q err %v", body, resp.Result, err)
		}
	}
	// A real envelope still unwraps.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, 200, `{"success":true,"errors":[],"result":{"id":"x"}}`)
	}))
	defer srv.Close()
	resp, err := newTestClient(t, srv.URL, nil).Do(context.Background(), Request{Path: "/x"})
	if err != nil || string(resp.Result) != `{"id":"x"}` {
		t.Errorf("envelope: %q %v", resp.Result, err)
	}
}

// A raw payload that is a JSON array (e.g. a KV value) is not a list result: it must not be
// served from the list cache.
func TestNonEnvelopeArrayNotCached(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/client/v4/list" {
			writeEnv(w, 200, `{"success":true,"errors":[],"result":[{"id":"a"}]}`)
			return
		}
		writeEnv(w, 200, `[1,2]`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.ListTTL = time.Minute })
	for range 3 {
		if _, err := c.Do(context.Background(), Request{Path: "/accounts/a/storage/kv/namespaces/n/values/k"}); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 3 {
		t.Errorf("raw JSON array: %d server hits for 3 GETs, want 3 (not cached)", hits)
	}
	hits = 0
	for range 3 {
		if _, err := c.Do(context.Background(), Request{Path: "/list"}); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Errorf("envelope list: %d server hits for 3 GETs, want 1 (cached)", hits)
	}
}

func TestErrorTextSanitized(t *testing.T) {
	long := strings.Repeat("é", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/html" {
			writeEnv(w, 400, "<html>\n\x1b[31mmetadata: secret\r\n"+long+"</html>")
			return
		}
		errs := make([]string, 0, 15)
		for i := range 15 {
			errs = append(errs, fmt.Sprintf(`{"code":%d,"message":"line1\nline2\u0000 %s"}`, i, long))
		}
		writeEnv(w, 400, `{"success":false,"errors":[`+strings.Join(errs, ",")+`]}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	_, err := c.Do(context.Background(), Request{Path: "/html"})
	ae, _ := AsAPIError(err)
	if ae == nil || len(ae.Errors) != 1 {
		t.Fatalf("%v", err)
	}
	m := ae.Errors[0].Message
	if strings.ContainsAny(m, "\n\r\x1b") || len(m) > len("non-JSON response: ")+maxBodySnippet+len(truncationSuffix) || !utf8.ValidString(m) {
		t.Errorf("html body not sanitized: %q (%d bytes)", m, len(m))
	}
	_, err = c.Do(context.Background(), Request{Path: "/json"})
	ae, _ = AsAPIError(err)
	if ae == nil || len(ae.Errors) != maxErrorsKept+1 || !ae.HasCode(9) || ae.HasCode(10) {
		t.Fatalf("%v", err)
	}
	for _, d := range ae.Errors {
		if strings.ContainsAny(d.Message, "\n\x00") || len(d.Message) > maxErrorMessage+len(truncationSuffix) || !utf8.ValidString(d.Message) {
			t.Errorf("message not sanitized: %q", d.Message)
		}
	}
	if got := Sanitize("  a \t\n b  ", 100); got != "a b" {
		t.Errorf("Sanitize: %q", got)
	}
	if got := Sanitize("abcdef", 3); got != "abc"+truncationSuffix {
		t.Errorf("Sanitize truncation: %q", got)
	}
}
