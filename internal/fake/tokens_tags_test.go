package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenVerifyOpenMode(t *testing.T) {
	c := newClient(t, New(Options{}))
	st, env, _ := c.do("GET", acct+"/tokens/verify", nil)
	if st != 200 || env.Result.(map[string]any)["status"] != "active" || env.Result.(map[string]any)["id"] == "" {
		t.Fatalf("%d %+v", st, env)
	}
	if st, _, _ := c.do("GET", "/client/v4/user/tokens/verify", nil); st != 200 {
		t.Fatalf("user verify %d", st)
	}
	if st, env, _ := c.do("GET", acct, nil); st != 200 || env.Result.(map[string]any)["id"] != acctID {
		t.Fatalf("account get %d %+v", st, env)
	}
}

func TestTokenVerifyStrictMode(t *testing.T) {
	s := New(Options{})
	exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.AddToken(Token{Value: "acct-tok", AccountID: acctID, ExpiresOn: &exp})
	s.AddToken(Token{Value: "user-tok", Kind: "user"})
	s.AddToken(Token{Value: "user-other", Kind: "user", AccountID: "ffffffffffffffffffffffffffffffff"})
	s.AddToken(Token{Value: "disabled", AccountID: acctID, Status: "disabled"})
	s.Clock.Set(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	c := newClient(t, s)
	check := func(tok, path string, wantStatus, wantCode int, wantTokStatus string) {
		t.Helper()
		c.tok = tok
		st, env, _ := c.do("GET", path, nil)
		if st != wantStatus {
			t.Fatalf("%s %s: status %d want %d (%+v)", tok, path, st, wantStatus, env.Errors)
		}
		if wantCode != 0 && (len(env.Errors) == 0 || env.Errors[0].Code != wantCode) {
			t.Fatalf("%s %s: errors %+v want code %d", tok, path, env.Errors, wantCode)
		}
		if wantTokStatus != "" && env.Result.(map[string]any)["status"] != wantTokStatus {
			t.Fatalf("%s %s: token status %v", tok, path, env.Result)
		}
	}
	other := "/client/v4/accounts/ffffffffffffffffffffffffffffffff"
	check("acct-tok", acct+"/tokens/verify", 200, 0, "active")
	check("acct-tok", other+"/tokens/verify", 403, 9109, "")
	check("acct-tok", "/client/v4/user/tokens/verify", 401, 1000, "")
	check("nope", acct+"/tokens/verify", 401, 1000, "")
	check("nope", acct, 401, 1000, "")
	check("user-tok", acct+"/tokens/verify", 401, 1000, "")
	check("user-tok", "/client/v4/user/tokens/verify", 200, 0, "active")
	check("user-tok", acct, 200, 0, "")
	check("user-other", acct, 403, 9109, "")
	check("disabled", acct+"/tokens/verify", 200, 0, "disabled")
	s.Clock.Advance(72 * time.Hour)
	check("acct-tok", acct+"/tokens/verify", 200, 0, "expired")

	// Control API: register and clear.
	hs := httptest.NewServer(s)
	defer hs.Close()
	b, _ := json.Marshal(Token{Value: "ctl", AccountID: acctID})
	resp, err := http.Post(hs.URL+"/_fake/tokens", "application/json", bytes.NewReader(b))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("control add: %v %v", resp, err)
	}
	c.tok = "ctl"
	c.base = hs.URL
	check("ctl", acct+"/tokens/verify", 200, 0, "active")
	req, _ := http.NewRequest("DELETE", hs.URL+"/_fake/tokens", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("control clear: %v %v", resp, err)
	}
	check("anything", acct+"/tokens/verify", 200, 0, "active") // back to open mode
	s.AddToken(Token{Value: "x", AccountID: acctID})
	s.Reset()
	check("anything", acct+"/tokens/verify", 200, 0, "active") // Reset clears tokens
}

func (c *client) doRaw(method, path string, body any, hdr map[string]string) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestTags(t *testing.T) {
	var spec *Spec
	if !testing.Short() {
		var err error
		if spec, err = LoadDefaultSpec(); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Options{Spec: spec, RejectSchemaViolations: spec != nil})
	c := newClient(t, s)
	st, env, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": "tagged"})
	if st != 200 {
		t.Fatal(st)
	}
	nsID := env.Result.(map[string]any)["id"].(string)
	q := "?resource_type=kv_namespace&resource_id=" + nsID

	// Never tagged → 500.
	if st, _, _ := c.do("GET", acct+"/tags"+q, nil); st != 500 {
		t.Fatalf("never-tagged GET: %d", st)
	}
	// PUT replaces all.
	st, env, _ = c.do("PUT", acct+"/tags", map[string]any{"resource_type": "kv_namespace", "resource_id": nsID, "tags": map[string]string{"a": "1", "b": "2"}})
	if st != 200 {
		t.Fatalf("PUT %d %+v", st, env.Errors)
	}
	r := env.Result.(map[string]any)
	if r["name"] != "tagged" || r["type"] != "kv_namespace" || len(r["tags"].(map[string]any)) != 2 {
		t.Fatalf("PUT result %+v", r)
	}
	etag := r["etag"].(string)
	st, env, _ = c.do("PUT", acct+"/tags", map[string]any{"resource_type": "kv_namespace", "resource_id": nsID, "tags": map[string]string{"c": "3"}})
	if st != 200 {
		t.Fatal(st)
	}
	st, env, _ = c.do("GET", acct+"/tags"+q, nil)
	tags := env.Result.(map[string]any)["tags"].(map[string]any)
	if st != 200 || len(tags) != 1 || tags["c"] != "3" {
		t.Fatalf("replace-all: %d %+v", st, tags)
	}
	// Stale If-Match → 412.
	if st, _ := c.doRaw("PUT", acct+"/tags", map[string]any{"resource_type": "kv_namespace", "resource_id": nsID, "tags": map[string]string{}}, map[string]string{"If-Match": etag}); st != 412 {
		t.Fatalf("stale If-Match: %d", st)
	}
	cur := env.Result.(map[string]any)["etag"].(string)
	if st, _ := c.doRaw("PUT", acct+"/tags", map[string]any{"resource_type": "kv_namespace", "resource_id": nsID, "tags": map[string]string{"c": "3", "d": ""}}, map[string]string{"If-Match": cur}); st != 200 {
		t.Fatalf("current If-Match: %d", st)
	}
	// DELETE → 204, no body; then GET is an empty map.
	st, raw := c.doRaw("DELETE", acct+"/tags", map[string]any{"resource_type": "kv_namespace", "resource_id": nsID}, nil)
	if st != 204 || len(raw) != 0 {
		t.Fatalf("DELETE %d %q", st, raw)
	}
	st, env, _ = c.do("GET", acct+"/tags"+q, nil)
	if st != 200 || len(env.Result.(map[string]any)["tags"].(map[string]any)) != 0 {
		t.Fatalf("after delete: %d %+v", st, env.Result)
	}
	// Validation.
	if st, _, _ := c.do("PUT", acct+"/tags", map[string]any{"resource_type": "bogus", "resource_id": "x", "tags": map[string]string{}}); st != 400 {
		t.Fatalf("bad type: %d", st)
	}
	for _, e := range s.Journal() {
		if e.SchemaViolation != "" && e.Status < 400 {
			t.Errorf("schema violation for %s %s: %s", e.Method, e.Path, e.SchemaViolation)
		}
	}
}
