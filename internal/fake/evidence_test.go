package fake

import (
	"fmt"
	"net/http"
	"testing"
)

// Tests for behaviors taken from official Cloudflare clients and docs (SOURCED / DOCS markers
// in the profiles; see docs/emulator-fidelity.md).

func TestQueueListNameFilterAndItemShape(t *testing.T) {
	c := newClient(t, New(Options{}))
	for _, n := range []string{"a", "b"} {
		if st, _, _ := c.do("POST", acct+"/queues", map[string]any{"queue_name": n}); st != 200 {
			t.Fatal(st)
		}
	}
	_, env, _ := c.do("GET", acct+"/queues?name=b", nil)
	items := env.Result.([]any)
	if len(items) != 1 || items[0].(map[string]any)["queue_name"] != "b" {
		t.Fatalf("name filter: %v", items)
	}
	// wrangler's `queues list` reads producers_total_count on every item.
	if _, has := items[0].(map[string]any)["producers_total_count"]; !has {
		t.Errorf("list item lacks producers_total_count: %v", items[0])
	}
	_, env, _ = c.do("GET", acct+"/queues?name=a&name=b", nil)
	if n := len(env.Result.([]any)); n != 2 {
		t.Errorf("repeated name filter: %d items", n)
	}
}

func TestQueueSettingsLimits(t *testing.T) {
	c := newClient(t, New(Options{}))
	if st, env, _ := c.do("POST", acct+"/queues", map[string]any{"queue_name": "d", "settings": map[string]int{"delivery_delay": 86400}}); st != 200 {
		t.Fatalf("24 h delay rejected: %d %v", st, env.Errors)
	}
	st, env, _ := c.do("POST", acct+"/queues", map[string]any{"queue_name": "e", "settings": map[string]int{"delivery_delay": 86401}})
	if st != 400 || env.Errors[0].Code != 100128 {
		t.Fatalf("delay above 24 h: %d %v", st, env.Errors)
	}
}

func TestD1GetByName(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("POST", acct+"/d1/database", map[string]any{"name": "flare-spike-db"})
	uuid := env.Result.(map[string]any)["uuid"]
	st, env, _ := c.do("GET", acct+"/d1/database/flare-spike-db?fields=uuid,name", nil)
	if st != 200 || env.Result.(map[string]any)["uuid"] != uuid {
		t.Fatalf("get by name: %d %v", st, env.Result)
	}
	if st, _, _ := c.do("GET", acct+"/d1/database/flare-spike", nil); st != 404 {
		t.Errorf("a name prefix must not match: %d", st)
	}
}

func TestWorkerSubdomainPreviewsFollowEnabled(t *testing.T) {
	c := newClient(t, New(Options{}))
	if st, _, _ := c.upload("w", map[string]any{"main_module": "index.js"}); st != 200 {
		t.Fatal(st)
	}
	_, env, _ := c.do("POST", acct+"/workers/scripts/w/subdomain", map[string]any{"enabled": true})
	if m := env.Result.(map[string]any); m["previews_enabled"] != true {
		t.Fatalf("previews_enabled should follow enabled: %v", m)
	}
	_, env, _ = c.do("POST", acct+"/workers/scripts/w/subdomain", map[string]any{"enabled": true, "previews_enabled": false})
	if m := env.Result.(map[string]any); m["enabled"] != true || m["previews_enabled"] != false {
		t.Fatalf("explicit previews_enabled: %v", m)
	}
}

func TestWorkerVersionsDeployableIgnoresPagination(t *testing.T) {
	c := newClient(t, New(Options{}))
	for i := 0; i < 12; i++ {
		if st, _, _ := c.upload("w", map[string]any{"main_module": "index.js"}); st != 200 {
			t.Fatal(st)
		}
	}
	_, env, _ := c.do("GET", acct+"/workers/scripts/w/versions", nil)
	if n := len(env.Result.(map[string]any)["items"].([]any)); n != 10 {
		t.Errorf("default page: %d items, want 10", n)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts/w/versions?deployable=true", nil)
	if n := len(env.Result.(map[string]any)["items"].([]any)); n != 12 {
		t.Errorf("deployable=true: %d items, want all 12", n)
	}
}

func TestTunnelDuplicateNameConflicts(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t"})
	id := env.Result.(map[string]any)["id"].(string)
	if st, _, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t"}); st != http.StatusConflict {
		t.Fatalf("duplicate live name: %d, want 409", st)
	}
	if st, _, _ := c.do("DELETE", acct+"/cfd_tunnel/"+id, nil); st != 200 {
		t.Fatal(st)
	}
	if st, _, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t"}); st != 200 {
		t.Fatalf("name of a deleted tunnel: %d", st)
	}
}

func TestVnetCreateIsDefaultNetwork(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("POST", acct+"/teamnet/virtual_networks", map[string]any{"name": "v", "is_default_network": true})
	if env.Result.(map[string]any)["is_default_network"] != true {
		t.Fatalf("is_default_network ignored: %v", env.Result)
	}
}

func TestVPCServiceHostShapes(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]string{"name": "t"})
	tid := env.Result.(map[string]any)["id"].(string)
	for i, host := range []map[string]any{
		{"ipv4": "10.0.0.1"},
		{"hostname": "a.example", "network": map[string]any{"tunnel_id": tid}},
		{"ipv4": "10.0.0.1", "hostname": "a.example", "network": map[string]any{"tunnel_id": tid}},
	} {
		st, env, _ := c.do("POST", acct+"/connectivity/directory/services", map[string]any{"name": fmt.Sprint("s", i), "type": "http", "http_port": 80, "host": host})
		if st != 400 || env.Errors[0].Code != 5101 {
			t.Errorf("host %v: %d %v, want 400/5101", host, st, env.Errors)
		}
	}
}

func TestTokenVerifyMessage(t *testing.T) {
	c := newClient(t, New(Options{}))
	_, env, _ := c.do("GET", "/client/v4/user/tokens/verify", nil)
	if len(env.Messages) != 1 || env.Messages[0].(map[string]any)["code"] != float64(10000) {
		t.Fatalf("verify messages: %v", env.Messages)
	}
}

func TestTagsListCursorPagination(t *testing.T) {
	c := newClient(t, New(Options{}))
	for i := 0; i < 150; i++ {
		if st, _, _ := c.do("PUT", acct+"/tags", map[string]any{"resource_type": "queue", "resource_id": fmt.Sprintf("q%03d", i), "tags": map[string]string{"k": "v"}}); st != 200 {
			t.Fatal(st)
		}
	}
	_, env, _ := c.do("GET", acct+"/tags/resources", nil)
	info := env.ResultInfo.(map[string]any)
	cur, _ := info["cursor"].(string)
	if len(env.Result.([]any)) != 100 || cur == "" {
		t.Fatalf("first page: %d items, cursor %v", len(env.Result.([]any)), info["cursor"])
	}
	_, env, _ = c.do("GET", acct+"/tags/resources?cursor="+cur, nil)
	info = env.ResultInfo.(map[string]any)
	if len(env.Result.([]any)) != 50 || info["cursor"] != nil {
		t.Fatalf("last page: %d items, cursor %v (want null)", len(env.Result.([]any)), info["cursor"])
	}
	q := ""
	for i := 0; i < 21; i++ {
		q += fmt.Sprintf("&tag=k%d", i)
	}
	if st, env, _ := c.do("GET", acct+"/tags/resources?"+q[1:], nil); st != 400 || env.Errors[0].Code != 1010 {
		t.Fatalf("21 tag filters: %d %v", st, env.Errors)
	}
}
