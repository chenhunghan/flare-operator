package fake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Behaviors not covered by recordings (or found in peer review). Anything asserted here that is
// not in a recording is marked UNVERIFIED in the profile code.

const acctID = "0123456789abcdef0123456789abcdef"

func TestConcurrentRequests(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c2 := *c
			c2.tok = fmt.Sprintf("tok-%d", i%3)
			c2.do("POST", acct+"/queues", map[string]string{"queue_name": fmt.Sprintf("q-%d", i)})
			c2.do("GET", acct+"/queues", nil)
			s.EnqueueIDs(hex32())
		}(i)
	}
	wg.Wait()
	s.Reset()
}

func TestPaginationEdges(t *testing.T) {
	c := newClient(t, New(Options{}))
	for i := 0; i < 3; i++ {
		c.do("POST", acct+"/queues", map[string]string{"queue_name": fmt.Sprintf("q%d", i)})
	}
	for _, q := range []string{
		"page=3&per_page=9223372036854775807", "page=9223372036854775807&per_page=100",
		"page=-1&per_page=-5", "per_page=0", "page=99",
	} {
		st, env, _ := c.do("GET", acct+"/queues?"+q, nil)
		if st != 200 {
			t.Fatalf("%s: status %d", q, st)
		}
		info := env.ResultInfo.(map[string]any)
		if pp := info["per_page"].(float64); pp < 1 || pp > maxPerPage {
			t.Errorf("%s: per_page echoed as %v", q, pp)
		}
	}
	// The server must still answer after the edge cases above.
	if st, env, _ := c.do("GET", acct+"/queues", nil); st != 200 || len(env.Result.([]any)) != 3 {
		t.Fatalf("server wedged or lost state: %d", st)
	}
}

func TestKVListOrdering(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	s.EnqueueIDs("bbbb0000000000000000000000000000", "aaaa0000000000000000000000000000", "cccc0000000000000000000000000000")
	for _, title := range []string{"z", "y", "x"} {
		c.do("POST", acct+"/storage/kv/namespaces", map[string]string{"title": title})
	}
	titles := func(q string) string {
		_, env, _ := c.do("GET", acct+"/storage/kv/namespaces"+q, nil)
		var out []string
		for _, n := range env.Result.([]any) {
			out = append(out, n.(map[string]any)["title"].(string))
		}
		return strings.Join(out, "")
	}
	if got := titles(""); got != "yzx" { // id ascending: aaaa(y) bbbb(z) cccc(x)
		t.Errorf("default order %q", got)
	}
	if got := titles("?order=title&direction=asc"); got != "xyz" {
		t.Errorf("title asc %q", got)
	}
	if got := titles("?order=title&direction=desc"); got != "zyx" {
		t.Errorf("title desc %q", got)
	}
}

func TestD1NameSearchIsSubstring(t *testing.T) {
	c := newClient(t, New(Options{}))
	for _, n := range []string{"orders", "orders-staging", "users"} {
		c.do("POST", acct+"/d1/database", map[string]string{"name": n})
	}
	_, env, _ := c.do("GET", acct+"/d1/database?name=orders", nil)
	if n := len(env.Result.([]any)); n != 2 {
		t.Fatalf("name=orders matched %d databases, want 2 (clients must exact-match themselves)", n)
	}
	if st, env, _ := c.do("POST", acct+"/d1/database", map[string]string{"name": ""}); st != 400 || env.Errors[0].Code != 7400 {
		t.Fatalf("empty name: %d %v", st, env.Errors)
	}
}

func TestD1StatementCounting(t *testing.T) {
	for sql, want := range map[string]int{
		"SELECT 1":                     1,
		"SELECT 1; SELECT 2;":          2,
		"INSERT INTO t VALUES ('a;b')": 1,
		"SELECT \"x;y\" FROM t; -- ;":  1,
		"/* ; */ SELECT 1":             1,
		" ; ; ":                        0,
	} {
		if got := countSQLStatements(sql); got != want {
			t.Errorf("%q: %d statements, want %d", sql, got, want)
		}
	}
}

func TestQueueRenameAndSettings(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	s.Clock.Set(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	_, env, _ := c.do("POST", acct+"/queues", map[string]string{"queue_name": "a"})
	id := env.Result.(map[string]any)["queue_id"].(string)
	c.do("POST", acct+"/queues", map[string]string{"queue_name": "b"})
	if st, env, _ := c.do("PATCH", acct+"/queues/"+id, map[string]string{"queue_name": "b"}); st != 409 || env.Errors[0].Code != 11009 {
		t.Fatalf("rename onto existing: %d %v", st, env.Errors)
	}
	s.Clock.Advance(time.Minute)
	_, env, _ = c.do("PATCH", acct+"/queues/"+id, map[string]any{"settings": map[string]int{"delivery_delay": 5}})
	q := env.Result.(map[string]any)
	if q["modified_on"] == q["created_on"] {
		t.Error("PATCH must bump modified_on")
	}
	if st, _, _ := c.do("PATCH", acct+"/queues/"+id, map[string]any{"settings": map[string]int{"delivery_delay": -5}}); st != 400 {
		t.Errorf("negative delivery_delay accepted: %d", st)
	}
	if st, _, _ := c.do("POST", acct+"/queues", map[string]any{"queue_name": "c", "settings": map[string]int{"message_retention_period": 1}}); st != 400 {
		t.Errorf("retention below 60s accepted: %d", st)
	}
}

func TestVPCPutResetsCreatedAt(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	s.Clock.Set(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t", "config_src": "cloudflare"})
	tid := env.Result.(map[string]any)["id"].(string)
	body := map[string]any{"name": "s", "type": "http", "http_port": 80, "host": map[string]any{"ipv4": "10.0.0.1", "network": map[string]any{"tunnel_id": tid}}}
	_, env, _ = c.do("POST", acct+"/connectivity/directory/services", body)
	sid := env.Result.(map[string]any)["service_id"].(string)
	created := env.Result.(map[string]any)["created_at"]
	s.Clock.Advance(time.Hour)
	_, env, _ = c.do("PUT", acct+"/connectivity/directory/services/"+sid, body)
	if env.Result.(map[string]any)["created_at"] == created {
		t.Fatal("VPC PUT must reset created_at (recording 0066)")
	}
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+tid, nil)
	if tun := env.Result.(map[string]any); tun["conns_inactive_at"] != tun["created_at"] {
		t.Errorf("new tunnel: conns_inactive_at should equal created_at (0040): %v vs %v", tun["conns_inactive_at"], tun["created_at"])
	}
}

func TestTunnelListFilters(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	ids := map[string]string{}
	for _, n := range []string{"prod-a", "prod-b", "dev-a"} {
		_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": n, "config_src": "cloudflare"})
		ids[n] = env.Result.(map[string]any)["id"].(string)
	}
	s.ConnectTunnel(acctID, ids["prod-b"], 1, 4)
	count := func(q string) int {
		st, env, _ := c.do("GET", acct+"/cfd_tunnel?"+q, nil)
		if st != 200 {
			t.Fatalf("%s: %d %v", q, st, env.Errors)
		}
		return len(env.Result.([]any))
	}
	for q, want := range map[string]int{
		"include_prefix=prod": 2, "exclude_prefix=prod": 1, "status=healthy": 1, "status=inactive": 2,
		"uuid=" + ids["dev-a"]: 1, "name=prod-a": 1,
	} {
		if got := count(q); got != want {
			t.Errorf("%s: %d tunnels, want %d", q, got, want)
		}
	}
	if st, _, _ := c.do("GET", acct+"/cfd_tunnel?existed_at=2026-01-01T00:00:00Z", nil); st != 400 {
		t.Errorf("unsupported filter must be rejected, got %d", st)
	}
	if st, _, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "x", "config_src": "bogus"}); st != 400 {
		t.Errorf("invalid config_src accepted: %d", st)
	}
}

func TestTunnelControlEndpointsAndCleanup(t *testing.T) {
	s := New(Options{})
	hs := httptest.NewServer(s)
	defer hs.Close()
	c := &client{t: t, base: hs.URL, tok: "x"}
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t", "config_src": "cloudflare"})
	id := env.Result.(map[string]any)["id"].(string)
	resp, _ := http.Post(hs.URL+"/_fake/accounts/"+acctID+"/tunnels/"+id+"/connect", "application/json", strings.NewReader(`{"replicas":2,"connections":4}`))
	resp.Body.Close()
	_, env, _ = c.do("GET", acct+"/cfd_tunnel/"+id+"/connections", nil)
	if len(env.Result.([]any)) != 2 {
		t.Fatalf("want 2 connectors, got %v", env.Result)
	}
	if st, _, _ := c.do("DELETE", acct+"/cfd_tunnel/"+id+"/connections", nil); st != 200 {
		t.Fatalf("cleanup connections: %d", st)
	}
	if st, _, _ := c.do("DELETE", acct+"/cfd_tunnel/"+id, nil); st != 200 {
		t.Fatalf("delete after cleanup: %d", st)
	}
	resp, _ = http.Post(hs.URL+"/_fake/accounts/"+acctID+"/tunnels/nope/connect", "application/json", nil)
	if resp.StatusCode != 404 {
		t.Errorf("connect unknown tunnel: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVnetNonDefaultDelete(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("POST", acct+"/teamnet/virtual_networks", map[string]any{"name": "extra"})
	id := env.Result.(map[string]any)["id"].(string)
	if st, _, _ := c.do("DELETE", acct+"/teamnet/virtual_networks/"+id, nil); st != 200 {
		t.Fatalf("delete non-default vnet: %d", st)
	}
	_, env, _ = c.do("GET", acct+"/teamnet/virtual_networks?is_deleted=false", nil)
	if len(env.Result.([]any)) != 0 {
		t.Fatalf("deleted vnet still listed: %v", env.Result)
	}
}

func TestFaultDefaultsAndReset(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	if err := s.InjectFault(Fault{PathRegex: "/queues"}); err != nil { // no status → 500
		t.Fatal(err)
	}
	if st, _, _ := c.do("GET", acct+"/queues", nil); st != 500 {
		t.Fatalf("fault with no status: %d", st)
	}
	if err := s.InjectFault(Fault{PathRegex: "/x", Status: 42}); err == nil {
		t.Fatal("status 42 must be rejected")
	}
	c.do("POST", acct+"/storage/kv/namespaces", map[string]string{"title": "t"})
	s.Reset()
	if st, env, _ := c.do("GET", acct+"/storage/kv/namespaces", nil); st != 200 || len(env.Result.([]any)) != 0 {
		t.Fatalf("Reset must clear state and faults: %d %v", st, env.Result)
	}
}

func TestInputHygiene(t *testing.T) {
	c := newClient(t, New(Options{}))
	req, _ := http.NewRequest("POST", c.base+acct+"/queues", strings.NewReader(`{"queue_name":"a"} trailing`))
	req.Header.Set("Authorization", "Bearer x")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("trailing garbage accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if st, _, _ := c.do("GET", "/client/v4/accounts//queues", nil); st != 404 {
		t.Errorf("empty account id: %d", st)
	}
	if st, _, _ := c.do("HEAD", acct+"/queues", nil); st != 405 {
		t.Errorf("HEAD on known path: %d", st)
	}
	_, env, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]string{"title": "t"})
	id := env.Result.(map[string]any)["id"].(string)
	if st, _, _ := c.do("PUT", acct+"/storage/kv/namespaces/"+id, map[string]string{"title": ""}); st != 400 {
		t.Errorf("rename to empty title accepted: %d", st)
	}
}
