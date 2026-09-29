package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
	tok  string
}

func newClient(t *testing.T, s *Server) *client {
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return &client{t: t, base: hs.URL, tok: "test-token"}
}

// testEnv decodes the v4 envelope in tests.
type testEnv struct {
	Result     any        `json:"result"`
	Success    bool       `json:"success"`
	Errors     []APIError `json:"errors"`
	Messages   []any      `json:"messages"`
	ResultInfo any        `json:"result_info"`
}

func (c *client) do(method, path string, body any) (int, testEnv, http.Header) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env testEnv
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env, resp.Header
}

const acct = "/client/v4/accounts/0123456789abcdef0123456789abcdef"

func TestRateLimitEnforcedWith429(t *testing.T) {
	s := New(Options{RateLimit: 3, RateWindow: time.Minute})
	s.Clock.Set(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	c := newClient(t, s)
	for i := 0; i < 3; i++ {
		if st, _, _ := c.do("GET", acct+"/queues", nil); st != 200 {
			t.Fatalf("request %d: status %d", i, st)
		}
	}
	st, env, h := c.do("GET", acct+"/queues", nil)
	if st != http.StatusTooManyRequests || env.Errors[0].Code != rateLimitedCode || h.Get("Retry-After") == "" {
		t.Fatalf("want 429/1015 with Retry-After, got %d %v %q", st, env.Errors, h.Get("Retry-After"))
	}
	// Budget is per token.
	other := *c
	other.tok = "other-token"
	if st, _, _ := other.do("GET", acct+"/queues", nil); st != 200 {
		t.Fatalf("other token throttled: %d", st)
	}
	s.Clock.Advance(61 * time.Second)
	if st, _, _ := c.do("GET", acct+"/queues", nil); st != 200 {
		t.Fatalf("after window: %d", st)
	}
}

func TestRateLimitHeadersMimicRealAPI(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, _, h := c.do("GET", acct+"/queues", nil)
	if got := h.Get("Ratelimit"); got != `"default";r=1199;t=1` {
		t.Errorf("Ratelimit = %q", got)
	}
	if got := h.Get("Ratelimit-Policy"); got != `"default";q=1200;w=300` {
		t.Errorf("Ratelimit-Policy = %q", got)
	}
	if _, _, h := c.do("GET", acct+"/storage/kv/namespaces", nil); h.Get("Ratelimit") != "" {
		t.Errorf("KV must not send Ratelimit headers, got %q", h.Get("Ratelimit"))
	}
}

func TestMissingAuth(t *testing.T) {
	c := newClient(t, New(Options{}))
	c.tok = ""
	st, env, _ := c.do("GET", acct+"/queues", nil)
	if st != 400 || env.Errors[0].Code != 9106 {
		t.Fatalf("got %d %v", st, env.Errors)
	}
}

func TestFaultInjection(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	if err := s.InjectFault(Fault{Method: "POST", PathRegex: `/queues$`, Status: 503, Code: 10000, Message: "boom", Times: 1}); err != nil {
		t.Fatal(err)
	}
	st, env, _ := c.do("POST", acct+"/queues", map[string]string{"queue_name": "a"})
	if st != 503 || env.Errors[0].Message != "boom" {
		t.Fatalf("fault not applied: %d %v", st, env.Errors)
	}
	if st, _, _ := c.do("POST", acct+"/queues", map[string]string{"queue_name": "a"}); st != 200 {
		t.Fatalf("fault should be consumed after 1 use, got %d", st)
	}
	j := s.Journal()
	if len(j) != 2 || !j[0].Fault || j[1].Fault {
		t.Fatalf("journal = %+v", j)
	}
}

func TestKVLegacyPathSunset(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	s.Clock.Set(time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC))
	if st, _, _ := c.do("GET", acct+"/workers/namespaces", nil); st != 200 {
		t.Fatalf("before sunset: %d", st)
	}
	s.Clock.Advance(2 * time.Hour)
	if st, _, _ := c.do("GET", acct+"/workers/namespaces", nil); st != 404 {
		t.Fatalf("after sunset: %d", st)
	}
	if st, _, _ := c.do("GET", acct+"/storage/kv/namespaces", nil); st != 200 {
		t.Fatalf("current path must keep working: %d", st)
	}
}

func TestTunnelConnectionsFieldRemovedAfterSunset(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	s.Clock.Set(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t", "config_src": "cloudflare"})
	id := env.Result.(map[string]any)["id"].(string)
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+id, nil)
	if _, has := env.Result.(map[string]any)["connections"]; !has {
		t.Fatal("connections field should exist before 2026-10-05")
	}
	s.Clock.Set(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+id, nil)
	if _, has := env.Result.(map[string]any)["connections"]; has {
		t.Fatal("connections field must be gone from 2026-10-05")
	}
	if st, _, _ := c.do("GET", acct+"/cfd_tunnel/"+id+"/connections", nil); st != 200 {
		t.Fatalf("/connections must keep working: %d", st)
	}
}

func TestTunnelLifecycleAndDependencies(t *testing.T) {
	s := New(Options{})
	// Pinned before tunnelConnectionsFieldSunset: the checks below read the "connections" field,
	// which the emulator stops returning on that date (TestTunnelConnectionsFieldRemovedAfterSunset).
	s.Clock.Set(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	c := newClient(t, s)
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t", "config_src": "cloudflare"})
	tun := env.Result.(map[string]any)
	id := tun["id"].(string)
	if tun["token"] == nil || tun["credentials_file"] == nil {
		t.Fatal("create response must carry token and credentials_file")
	}
	st, env, _ := c.do("POST", acct+"/connectivity/directory/services", map[string]any{
		"name": "svc", "type": "http", "http_port": 80,
		"host": map[string]any{"hostname": "Marker.NS.svc.cluster.local", "resolver_network": map[string]any{"tunnel_id": id, "resolver_ips": []string{"10.96.0.10"}}},
	})
	if st != 200 || env.Result.(map[string]any)["host"].(map[string]any)["hostname"] != "marker.ns.svc.cluster.local" {
		t.Fatalf("service create: %d %v", st, env.Result)
	}
	s.ConnectTunnel("0123456789abcdef0123456789abcdef", id, 2, 4)
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+id, nil)
	if conns, _ := env.Result.(map[string]any)["connections"].([]any); env.Result.(map[string]any)["status"] != "healthy" || len(conns) != 8 {
		t.Fatalf("connected tunnel: %v", env.Result)
	}
	if st, env, _ := c.do("DELETE", acct+"/cfd_tunnel/"+id, nil); st != 400 || env.Errors[0].Code != 1022 {
		t.Fatalf("delete while connected: %d %v", st, env.Errors)
	}
	s.DisconnectTunnel("0123456789abcdef0123456789abcdef", id)
	// Deleting a tunnel still referenced by a VPC service is allowed (recording 0099).
	if st, _, _ := c.do("DELETE", acct+"/cfd_tunnel/"+id, nil); st != 200 {
		t.Fatalf("delete disconnected: %d", st)
	}
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+id, nil)
	if env.Result.(map[string]any)["deleted_at"] == nil {
		t.Fatal("tunnel delete must be a soft delete")
	}
	st, env, _ = c.do("POST", acct+"/connectivity/directory/services", map[string]any{
		"name": "svc2", "type": "http", "http_port": 80, "host": map[string]any{"ipv4": "10.0.0.1", "network": map[string]any{"tunnel_id": id}},
	})
	if st != 400 || env.Errors[0].Code != 5101 {
		t.Fatalf("create against deleted tunnel: %d %v", st, env.Errors)
	}
	_, env, _ = c.do("GET", acct+"/teamnet/virtual_networks?is_deleted=false", nil)
	vnets := env.Result.([]any)
	if len(vnets) != 1 {
		t.Fatalf("first tunnel should auto-create one default vnet, got %v", vnets)
	}
	vid := vnets[0].(map[string]any)["id"].(string)
	if st, env, _ := c.do("DELETE", acct+"/teamnet/virtual_networks/"+vid, nil); st != 400 || env.Errors[0].Code != 1049 {
		t.Fatalf("default vnet delete: %d %v", st, env.Errors)
	}
}

func TestSchemaViolationsAreJournaled(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the 26 MB spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Spec: spec})
	c := newClient(t, s)
	st, _, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": 123})
	j := s.Journal()
	if len(j) != 1 || !strings.Contains(j[0].SchemaViolation, "title") {
		t.Fatalf("expected a journaled schema violation on /title, got %+v", j)
	}
	if st == 200 {
		t.Log("note: emulated response unaffected by schema violation (by design)")
	}
	s2 := New(Options{Spec: spec, RejectSchemaViolations: true})
	c2 := newClient(t, s2)
	if st, env, _ := c2.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": 123}); st != 400 || env.Errors[0].Code != 10001 {
		t.Fatalf("reject mode: %d %v", st, env.Errors)
	}
	if st, _, _ := c2.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": "ok"}); st != 200 {
		t.Fatalf("valid request rejected: %d", st)
	}
}

func TestControlAPI(t *testing.T) {
	s := New(Options{})
	hs := httptest.NewServer(s)
	defer hs.Close()
	post := func(path, body string) int {
		resp, err := http.Post(hs.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("/_fake/clock", `{"set":"2030-01-01T00:00:00Z"}`) != 200 || !s.Clock.Now().Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("clock set")
	}
	if post("/_fake/ids", `{"ids":["00000000000000000000000000000abc"]}`) != 200 {
		t.Fatal("ids")
	}
	c := &client{t: t, base: hs.URL, tok: "x"}
	_, env, _ := c.do("POST", acct+"/queues", map[string]string{"queue_name": "q"})
	if env.Result.(map[string]any)["queue_id"] != "00000000000000000000000000000abc" {
		t.Fatalf("queued id not used: %v", env.Result)
	}
	if post("/_fake/faults", `{"method":"GET","path_regex":"/queues","status":500,"code":1,"message":"x","times":1}`) != 200 {
		t.Fatal("faults")
	}
	if st, _, _ := c.do("GET", acct+"/queues", nil); st != 500 {
		t.Fatalf("fault via HTTP control not applied: %d", st)
	}
	if post("/_fake/reset", ``) != 200 || len(s.Journal()) != 0 {
		t.Fatal("reset")
	}
}

func TestUnknownRoute(t *testing.T) {
	c := newClient(t, New(Options{}))
	if st, env, _ := c.do("GET", acct+"/no/such/thing", nil); st != 404 || env.Errors[0].Code != 7000 {
		t.Fatalf("got %d %v", st, env.Errors)
	}
	if st, _, _ := c.do("PATCH", acct+"/storage/kv/namespaces", nil); st != 405 {
		t.Fatalf("wrong method on known path: %d", st)
	}
}
