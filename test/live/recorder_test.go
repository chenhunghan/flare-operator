package live

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecorderWritesRecordingFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get(LabelHeader) != "" {
			t.Errorf("label header forwarded")
		}
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/client/v4/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("plain"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cf-Ray", "abc-XYZ")
			w.Header().Set("Ratelimit", `"default";r=1199;t=1`)
			w.Header().Set("Set-Cookie", "secret=1")
			_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":{"echo":` + string(orNull(b)) + `}}`))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	rec := &Recorder{Base: http.DefaultTransport, Dir: dir, PathPrefix: "/client/v4"}
	hc := &http.Client{Transport: rec}

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/client/v4/accounts/0123456789abcdef0123456789abcdef/tags?x=1",
		strings.NewReader(`{"tags":{"a":"b"}}`))
	req.Header.Set(LabelHeader, "probe")
	req.Header.Set("Authorization", "Bearer SECRET-TOKEN")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "v1:etag")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"echo":{"tags":{"a":"b"}}`) {
		t.Fatalf("request body did not reach the server / response body not passed through: %s", body)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/client/v4/accounts/0123456789abcdef0123456789abcdef/cfd_tunnel/5456b9ca-8b73-4b44-93df-67ad7acc8e2c", nil)
	if _, err := hc.Do(req); err != nil {
		t.Fatal(err)
	}
	rec.SetPhase("op")
	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/client/v4/text", strings.NewReader("--b\r\nx\r\n--b--\r\n"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	if _, err := hc.Do(req); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	want := []string{"0001-probe.json", "0002-delete-cfd-tunnel.json", "0003-op-patch-text.json"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files = %v, want %v", names, want)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, want[0]))
	if strings.Contains(string(raw), "SECRET-TOKEN") || strings.Contains(string(raw), "secret=1") {
		t.Fatalf("cassette leaks request auth or unlisted headers: %s", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "method", "path", "request_body", "status", "elapsed_ms", "response_headers", "response_body"} {
		if _, ok := m[k]; !ok {
			t.Errorf("cassette lacks key %q: %s", k, raw)
		}
	}
	var c Cassette
	_ = json.Unmarshal(raw, &c)
	if c.Path != "/accounts/0123456789abcdef0123456789abcdef/tags?x=1" || c.Status != 200 ||
		c.RequestHeaders["If-Match"] != "v1:etag" || c.RequestHeaders["Content-Type"] != "" ||
		c.ResponseHeaders["CF-Ray"] != "abc-XYZ" || c.ResponseHeaders["Ratelimit"] == "" || len(c.ResponseHeaders) != 3 {
		t.Errorf("cassette = %+v", c)
	}
	if _, ok := c.RequestBody.(map[string]any); !ok {
		t.Errorf("JSON request body should be stored decoded, got %T", c.RequestBody)
	}

	raw, _ = os.ReadFile(filepath.Join(dir, want[1]))
	_ = json.Unmarshal(raw, &m)
	if string(m["response_body"]) != "null" || string(m["request_body"]) != "null" {
		t.Errorf("204 cassette bodies = %s / %s, want null", m["request_body"], m["response_body"])
	}
	raw, _ = os.ReadFile(filepath.Join(dir, want[2]))
	c = Cassette{}
	_ = json.Unmarshal(raw, &c)
	if c.RequestBody != "--b\r\nx\r\n--b--\r\n" || c.ResponseBody != "plain" || !strings.HasPrefix(c.RequestHeaders["Content-Type"], "multipart/form-data") {
		t.Errorf("non-JSON cassette = %+v", c)
	}

	if w := Writes(rec.Since(0)); !reflect.DeepEqual(w, []string{
		"PUT /accounts/0123456789abcdef0123456789abcdef/tags?x=1",
		"DELETE /accounts/0123456789abcdef0123456789abcdef/cfd_tunnel/5456b9ca-8b73-4b44-93df-67ad7acc8e2c",
		"PATCH /text",
	}) {
		t.Errorf("Writes = %v", w)
	}
	if n := len(rec.Since(2)); n != 1 {
		t.Errorf("Since(2) = %d entries, want 1", n)
	}
	if errs := rec.Errors(); len(errs) != 0 {
		t.Errorf("write errors: %v", errs)
	}
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

func TestLabel(t *testing.T) {
	for _, tc := range []struct{ phase, method, path, want string }{
		{"", "GET", "/accounts/0123456789abcdef0123456789abcdef/tokens/verify", "get-tokens-verify"},
		{"tags-never-tagged", "GET", "/accounts/0123456789abcdef0123456789abcdef/tags?resource_type=kv_namespace&resource_id=x", "tags-never-tagged-get-tags"},
		{"op", "PATCH", "/accounts/0123456789abcdef0123456789abcdef/workers/scripts/flare-spike-w/settings", "op-patch-workers-scripts-flare-spike-w-settings"},
		{"", "GET", "/user/tokens/verify", "get-user-tokens-verify"},
	} {
		if got := Label(tc.phase, tc.method, tc.path); got != tc.want {
			t.Errorf("Label(%q,%q,%q) = %q, want %q", tc.phase, tc.method, tc.path, got, tc.want)
		}
	}
}
