package fake

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for the surfaces added for real clients (test/differential): KV values and keys, D1
// constant SELECTs, tunnel IP routes, the Workers versions API and the routes wrangler deploy
// reads, text/plain bodies, VPC list paging and handler detection.

func (c *client) text(method, path, contentType, body string) (int, []byte, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestKVValuesAndKeys(t *testing.T) {
	s := New(Options{})
	s.Clock.Set(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	c := newClient(t, s)
	_, env, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": "ns"})
	ns := acct + "/storage/kv/namespaces/" + resultMap(t, env)["id"].(string)

	if st, _, _ := c.text("PUT", ns+"/values/k1", "text/plain;charset=UTF-8", "v1"); st != 200 {
		t.Fatalf("put k1: %d", st)
	}
	// A key with "/" arrives percent-encoded (wrangler encodeURIComponent).
	if st, _, _ := c.text("PUT", ns+"/values/dir%2Fk2?expiration_ttl=60", "application/octet-stream", "v2"); st != 200 {
		t.Fatalf("put dir/k2: %d", st)
	}
	// With metadata, wrangler sends plain form fields "value" and "metadata" (JSON text).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("value", "v3")
	_ = mw.WriteField("metadata", `{"m":1}`)
	_ = mw.Close()
	if st, env, _ := c.raw("PUT", ns+"/values/k3", mw.FormDataContentType(), buf.Bytes()); st != 200 {
		t.Fatalf("put k3 (multipart): %d %v", st, env.Errors)
	}
	for key, want := range map[string]string{"k1": "v1", "dir%2Fk2": "v2", "k3": "v3"} {
		st, got, h := c.text("GET", ns+"/values/"+key, "", "")
		if st != 200 || string(got) != want || h.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("get %s: %d %q %q, want 200 %q octet-stream", key, st, got, h.Get("Content-Type"), want)
		}
	}
	if _, env, _ := c.do("GET", ns+"/metadata/k3", nil); fmt.Sprint(env.Result) != "map[m:1]" {
		t.Errorf("metadata k3: %v", env.Result)
	}

	// Keys come sorted, with metadata and expiration, and page by cursor.
	for i := 0; i < 12; i++ {
		c.text("PUT", fmt.Sprintf("%s/values/p%02d", ns, i), "application/octet-stream", "x")
	}
	st, env, _ := c.do("GET", ns+"/keys?prefix=p&limit=10", nil)
	items, _ := env.Result.([]any)
	info, _ := env.ResultInfo.(map[string]any)
	if st != 200 || len(items) != 10 || info["count"] != float64(10) || info["cursor"] == "" {
		t.Fatalf("keys page 1: %d %d items, info %v", st, len(items), info)
	}
	_, env, _ = c.do("GET", ns+"/keys?prefix=p&limit=10&cursor="+info["cursor"].(string), nil)
	items, _ = env.Result.([]any)
	info, _ = env.ResultInfo.(map[string]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "p10" || info["cursor"] != "" {
		t.Fatalf("keys page 2: %v, info %v", items, info)
	}
	_, env, _ = c.do("GET", ns+"/keys", nil)
	all, _ := env.Result.([]any)
	var names []string
	for _, it := range all {
		m := it.(map[string]any)
		names = append(names, m["name"].(string))
		if m["name"] == "dir/k2" && m["expiration"] != float64(s.Clock.Now().Unix()+60) {
			t.Errorf("dir/k2 expiration %v", m["expiration"])
		}
		if m["name"] == "k3" && fmt.Sprint(m["metadata"]) != "map[m:1]" {
			t.Errorf("k3 metadata %v", m["metadata"])
		}
	}
	if !slices.IsSorted(names) || len(names) != 15 {
		t.Errorf("keys %v, want 15 sorted", names)
	}

	// Expiry, delete, and a missing key.
	s.Clock.Advance(61 * time.Second)
	if st, _, _ := c.text("GET", ns+"/values/dir%2Fk2", "", ""); st != 404 {
		t.Errorf("expired key: %d, want 404", st)
	}
	if st, _, _ := c.do("DELETE", ns+"/values/k1", nil); st != 200 {
		t.Errorf("delete k1: %d", st)
	}
	if st, env, _ := c.do("GET", ns+"/values/k1", nil); st != 404 || env.Errors[0].Code != 10009 {
		t.Errorf("get deleted k1: %d %v", st, env.Errors)
	}
	if st, env, _ := c.do("GET", acct+"/storage/kv/namespaces/nope/values/k1", nil); st != 404 || env.Errors[0].Code != 10013 {
		t.Errorf("unknown namespace: %d %v", st, env.Errors)
	}
}

// text/plain is validated as the declared media type only where a client is shown to send it,
// so strict mode still rejects a bad body there and text/plain everywhere else.
func TestPlainTextBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the 26 MB spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Spec: spec, RejectSchemaViolations: true})
	c := newClient(t, s)
	if st, b, _ := c.text("POST", acct+"/queues", "text/plain;charset=UTF-8", `{"queue_name":"q1"}`); st != 200 {
		t.Fatalf("queue create as text/plain: %d %s", st, b)
	}
	if st, _, _ := c.text("POST", acct+"/queues", "text/plain;charset=UTF-8", `{"queue_name":7}`); st != 400 {
		t.Errorf("schema-violating text/plain queue create: %d, want 400", st)
	}
	if st, _, _ := c.text("POST", acct+"/storage/kv/namespaces", "text/plain;charset=UTF-8", `{"title":"t"}`); st != 400 {
		t.Errorf("text/plain KV namespace create: %d, want 400 (no evidence a client sends it)", st)
	}
	_, env, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": "t"})
	ns := acct + "/storage/kv/namespaces/" + resultMap(t, env)["id"].(string)
	if st, b, _ := c.text("PUT", ns+"/values/k", "text/plain;charset=UTF-8", "v"); st != 200 {
		t.Errorf("text/plain KV value put: %d %s", st, b)
	}
}

func TestD1ConstantSelect(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	_, env, _ := c.do("POST", acct+"/d1/database", map[string]any{"name": "db"})
	q := acct + "/d1/database/" + resultMap(t, env)["uuid"].(string) + "/query"

	if st, env, _ := c.do("POST", q, map[string]any{"sql": "select 1 as one; -- c\nSELECT ?1 AS p, ?, true", "params": []any{"x"}}); st != 400 || env.Errors[0].Code != 7400 {
		t.Fatalf("params with two statements: %d %v, want 400/7400 (0022)", st, env.Errors)
	}
	st, env, _ := c.do("POST", q, map[string]any{"sql": "select 1 as one, 'it''s' AS \"s\", NULL, -2.5 x; /* c */ SELECT 2;"})
	res, _ := env.Result.([]any)
	if st != 200 || len(res) != 2 {
		t.Fatalf("query: %d %v %v", st, env.Errors, env.Result)
	}
	row := res[0].(map[string]any)["results"].([]any)[0].(map[string]any)
	if fmt.Sprint(row) != "map[NULL:<nil> one:1 s:it's x:-2.5]" {
		t.Errorf("row %v", row)
	}
	if row2 := res[1].(map[string]any)["results"].([]any)[0].(map[string]any); fmt.Sprint(row2) != "map[2:2]" {
		t.Errorf("row 2 %v", row2)
	}
	st, env, _ = c.do("POST", q, map[string]any{"sql": "SELECT ?1 AS a, ? AS b, ?5 AS c", "params": []any{"x", "y"}})
	row = env.Result.([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)
	if st != 200 || fmt.Sprint(row) != "map[a:x b:y c:<nil>]" {
		t.Errorf("params: %d %v", st, row)
	}
	st, env, _ = c.do("POST", q, map[string]any{"batch": []any{map[string]any{"sql": "select 1 a"}, map[string]any{"sql": "select 'b' b"}}})
	if res, _ := env.Result.([]any); st != 200 || len(res) != 2 {
		t.Errorf("batch: %d %v", st, env.Result)
	}
	for _, sql := range []string{"CREATE TABLE t(id)", "select * from t", "select 1+1", "select abs(1)"} {
		if st, env, _ := c.do("POST", q, map[string]any{"sql": sql}); st != 400 || env.Errors[0].Code != 99999 {
			t.Errorf("%q: %d %v, want 400/99999 (not emulated)", sql, st, env.Errors)
		}
	}
}

func TestTeamnetRoutes(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	_, env, _ := c.do("POST", acct+"/cfd_tunnel", map[string]any{"name": "t", "config_src": "local"})
	tid := resultMap(t, env)["id"].(string)
	_, env, _ = c.do("POST", acct+"/teamnet/virtual_networks", map[string]any{"name": "v2"})
	vnet := resultMap(t, env)["id"].(string)

	st, env, _ := c.do("POST", acct+"/teamnet/routes", map[string]any{"network": "10.1.2.3/16", "tunnel_id": tid, "comment": "a"})
	r1 := resultMap(t, env)
	if st != 200 || r1["network"] != "10.1.0.0/16" || r1["virtual_network_id"] == "" || r1["deleted_at"] != nil {
		t.Fatalf("create: %d %v", st, env.Result)
	}
	c.do("POST", acct+"/teamnet/routes", map[string]any{"network": "10.1.5.0/24", "tunnel_id": tid, "virtual_network_id": vnet})
	c.do("POST", acct+"/teamnet/routes", map[string]any{"network": "10.1.0.0/24", "tunnel_id": tid})
	if st, _, _ := c.do("POST", acct+"/teamnet/routes", map[string]any{"network": "10.1.0.0/16", "tunnel_id": tid}); st != 409 {
		t.Errorf("duplicate network in the default vnet: %d, want 409", st)
	}
	if st, _, _ := c.do("POST", acct+"/teamnet/routes", map[string]any{"network": "10.9.0.0/16", "tunnel_id": "00000000-0000-4000-8000-000000000000"}); st != 400 {
		t.Errorf("unknown tunnel: %d, want 400", st)
	}
	list := func(q string) []map[string]any {
		t.Helper()
		st, env, _ := c.do("GET", acct+"/teamnet/routes"+q, nil)
		if st != 200 {
			t.Fatalf("list %s: %d %v", q, st, env.Errors)
		}
		var out []map[string]any
		for _, it := range env.Result.([]any) {
			out = append(out, it.(map[string]any))
		}
		return out
	}
	if l := list("?is_deleted=false&tun_types=cfd_tunnel"); len(l) != 3 || l[0]["tunnel_name"] != "t" || l[0]["virtual_network_name"] != "default" {
		t.Errorf("list %v", l)
	}
	if l := list("?network_subset=10.1.0.0/16&network_superset=10.1.0.0/16"); len(l) != 1 || l[0]["id"] != r1["id"] {
		t.Errorf("exact network lookup %v", l)
	}
	if l := list("?network_subset=10.1.0.0/16"); len(l) != 3 {
		t.Errorf("routes inside 10.1.0.0/16: %d, want 3", len(l))
	}
	if l := list("?virtual_network_id=" + vnet); len(l) != 1 || l[0]["network"] != "10.1.5.0/24" {
		t.Errorf("vnet filter %v", l)
	}
	if _, env, _ := c.do("GET", acct+"/teamnet/routes/ip/10.1.0.7", nil); resultMap(t, env)["network"] != "10.1.0.0/24" {
		t.Errorf("by ip (longest prefix in the default vnet): %v", env.Result)
	}
	if _, env, _ := c.do("GET", acct+"/teamnet/routes/ip/192.168.0.1", nil); len(resultMap(t, env)) != 0 {
		t.Errorf("by ip, no match: %v", env.Result)
	}
	if st, env, _ := c.do("DELETE", acct+"/teamnet/routes/"+r1["id"].(string), nil); st != 200 || resultMap(t, env)["deleted_at"] == nil {
		t.Errorf("delete: %d %v", st, env.Result)
	}
	if l := list("?is_deleted=true"); len(l) != 1 {
		t.Errorf("deleted routes %v", l)
	}
	if l := list("?is_deleted=false"); len(l) != 2 {
		t.Errorf("live routes %v", l)
	}
	if st, _, _ := c.do("DELETE", acct+"/teamnet/routes/"+r1["id"].(string), nil); st != 404 {
		t.Errorf("second delete: %d, want 404", st)
	}
}

func TestVPCListPaging(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	for i := 0; i < 3; i++ {
		c.createVPCService(fmt.Sprintf("svc%d", i))
	}
	count := func(q string) int {
		_, env, _ := c.do("GET", acct+"/connectivity/directory/services"+q, nil)
		items, _ := env.Result.([]any)
		return len(items)
	}
	if n1, n2, n3 := count("?per_page=2"), count("?per_page=2&page=2"), count("?per_page=2&page=3"); n1 != 2 || n2 != 1 || n3 != 0 {
		t.Errorf("pages %d, %d, %d; want 2, 1, 0", n1, n2, n3)
	}
	if n := count("?page=2"); n != 0 {
		t.Errorf("page 2 at the default per_page: %d, want 0", n)
	}
	if n := count("?type=tcp"); n != 0 {
		t.Errorf("type=tcp: %d, want 0", n)
	}
}

func TestWorkerHandlerDetection(t *testing.T) {
	for src, want := range map[string]string{
		"export default { async fetch(req, env) { return new Response('hi'); } };":                "[fetch]",
		"var src_default = {\n  async fetch(request, env) {\n    await env.KV.fetch(x);\n  }\n};": "[fetch]",
		"export default {fetch: async (r) => new Response(''), scheduled: function() {}}":         "[fetch scheduled]",
		"export default { fetch(r) {}, async queue(b) {} }":                                       "[fetch queue]",
		"const r = await env.SVC.fetch(url); export default {}":                                   "[]",
	} {
		var got []string
		for _, m := range workerHandlerRe.FindAllStringSubmatch(src, -1) {
			if !slices.Contains(got, m[1]) {
				got = append(got, m[1])
			}
		}
		if fmt.Sprint(emptyIfNil(got)) != want {
			t.Errorf("%q: handlers %v, want %s", src, got, want)
		}
	}
}

// The routes wrangler deploy and delete use, and the versions API.
func TestWorkerVersionsAPIAndWranglerRoutes(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	w := acct + "/workers/scripts/w"
	for _, p := range []string{acct + "/workers/services/w", w + "/secrets", acct + "/workers/workers/w", w + "/script-settings"} {
		if st, env, _ := c.do("GET", p, nil); st != 404 || env.Errors[0].Code != 10007 {
			t.Errorf("GET %s before upload: %d %v, want 404/10007", p, st, env.Errors)
		}
	}
	_, kvenv, _ := c.do("POST", acct+"/storage/kv/namespaces", map[string]any{"title": "ns"})
	kvID := resultMap(t, kvenv)["id"].(string)
	meta := map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01", "bindings": []any{
		map[string]any{"type": "kv_namespace", "name": "KV", "namespace_id": kvID},
		map[string]any{"type": "plain_text", "name": "MODE", "text": "a"},
		map[string]any{"type": "secret_text", "name": "TOKEN", "text": "s3cret"},
	}}
	if st, env, _ := c.upload("w", meta); st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	_, env, _ := c.do("GET", acct+"/workers/services/w", nil)
	script := resultMap(t, env)["default_environment"].(map[string]any)["script"].(map[string]any)
	if script["tag"] == "" || script["last_deployed_from"] != "api" {
		t.Errorf("service script %v", script)
	}
	if _, env, _ := c.do("GET", w+"/secrets", nil); fmt.Sprint(env.Result) != "[map[name:TOKEN type:secret_text]]" {
		t.Errorf("secrets %v", env.Result)
	}
	_, env, _ = c.do("GET", acct+"/workers/workers/w", nil)
	if m := resultMap(t, env); m["name"] != "w" || m["id"] != script["tag"] || fmt.Sprint(m["subdomain"]) != "map[enabled:false previews_enabled:false]" {
		t.Errorf("worker %v", m)
	}
	if _, env2, _ := c.do("GET", acct+"/workers/workers/"+script["tag"].(string), nil); resultMap(t, env2)["name"] != "w" {
		t.Errorf("worker by tag %v", env2.Result)
	}

	// A version upload inherits MODE and TOKEN, keeps plain_text by type, and is not deployed.
	vmeta := map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-02",
		"annotations": map[string]any{"workers/message": "m1"},
		"bindings": []any{
			map[string]any{"type": "inherit", "name": "KV2", "old_name": "KV"},
			map[string]any{"type": "inherit", "name": "TOKEN"},
		},
		"keep_bindings": []any{"plain_text"}}
	body, ct := multipartBody(t, "metadata", vmeta, map[string]string{"index.js": "export default { async fetch() {} }"})
	st, env, _ := c.raw("POST", w+"/versions?bindings_inherit=strict", ct, body)
	v2 := resultMap(t, env)
	if st != 200 || v2["number"] != float64(2) || v2["annotations"].(map[string]any)["workers/message"] != "m1" {
		t.Fatalf("version upload: %d %v %v", st, env.Errors, env.Result)
	}
	res := v2["resources"].(map[string]any)
	var bnames []string
	for _, b := range res["bindings"].([]any) {
		bm := b.(map[string]any)
		bnames = append(bnames, bm["name"].(string)+":"+bm["type"].(string))
		if bm["name"] == "TOKEN" && bm["text"] != nil {
			t.Errorf("secret value returned: %v", bm)
		}
	}
	if fmt.Sprint(bnames) != "[KV2:kv_namespace TOKEN:secret_text MODE:plain_text]" {
		t.Errorf("version bindings %v", bnames)
	}
	if res["script_runtime"].(map[string]any)["compatibility_date"] != "2026-09-02" || fmt.Sprint(res["script"].(map[string]any)["handlers"]) != "[fetch]" {
		t.Errorf("version resources %v", res)
	}
	_, env, _ = c.do("GET", w+"/settings", nil)
	if m := resultMap(t, env); m["compatibility_date"] != "2026-09-01" {
		t.Errorf("an undeployed version changed the settings: %v", m["compatibility_date"])
	}
	if st, env, _ := c.do("GET", w+"/versions/"+v2["id"].(string), nil); st != 200 || resultMap(t, env)["id"] != v2["id"] {
		t.Errorf("version get: %d %v", st, env.Result)
	}
	if st, _, _ := c.do("GET", w+"/versions/00000000-0000-4000-8000-000000000000", nil); st != 404 {
		t.Errorf("unknown version: %d, want 404", st)
	}

	// An unresolvable inherit binding fails a strict upload with 10057.
	bad := map[string]any{"main_module": "index.js", "bindings": []any{map[string]any{"type": "inherit", "name": "NOPE"}}}
	body, ct = multipartBody(t, "metadata", bad, map[string]string{"index.js": moduleSrc})
	if st, env, _ := c.raw("POST", w+"/versions?bindings_inherit=strict", ct, body); st != 400 || env.Errors[0].Code != invalidInheritCode ||
		!strings.HasPrefix(env.Errors[0].Message, "inherit binding 'NOPE' is invalid") {
		t.Errorf("strict inherit of a missing binding: %d %v", st, env.Errors)
	}
	if st, _, _ := c.raw("POST", w+"/versions", ct, body); st != 200 {
		t.Errorf("non-strict inherit of a missing binding: %d, want 200 (dropped)", st)
	}

	// Deploy v2 at 100%: it becomes the script's code and bindings.
	if st, _, _ := c.do("POST", w+"/deployments", map[string]any{"strategy": "percentage",
		"versions": []any{map[string]any{"version_id": v2["id"], "percentage": 50}}}); st != 400 {
		t.Errorf("deployment at 50%%: %d, want 400", st)
	}
	st, env, _ = c.do("POST", w+"/deployments", map[string]any{"strategy": "percentage",
		"versions": []any{map[string]any{"version_id": v2["id"], "percentage": 100}}, "annotations": map[string]any{"workers/message": "go"}})
	d := resultMap(t, env)
	if st != 200 || d["annotations"].(map[string]any)["workers/message"] != "go" {
		t.Fatalf("deployment: %d %v %v", st, env.Errors, env.Result)
	}
	_, env, _ = c.do("GET", w+"/settings", nil)
	if m := resultMap(t, env); m["compatibility_date"] != "2026-09-02" || len(m["bindings"].([]any)) != 3 {
		t.Errorf("settings after deploying v2: %v", m)
	}
	_, env, _ = c.do("GET", w+"/deployments", nil)
	if ds := resultMap(t, env)["deployments"].([]any); len(ds) != 2 || ds[0].(map[string]any)["id"] != d["id"] {
		t.Errorf("deployments %v", ds)
	}

	// Non-versioned settings.
	st, env, _ = c.do("PATCH", w+"/script-settings", map[string]any{"logpush": true, "observability": map[string]any{"enabled": false}, "tags": []any{"a"}})
	if m := resultMap(t, env); st != 200 || m["logpush"] != true || fmt.Sprint(m["tags"]) != "[a]" || m["observability"].(map[string]any)["enabled"] != false {
		t.Errorf("script-settings patch: %d %v", st, env.Result)
	}
	// Versions: the upload, v2 and the non-strict upload; deployments and settings add none.
	if _, env, _ := c.do("GET", w+"/versions", nil); len(resultMap(t, env)["items"].([]any)) != 3 {
		t.Errorf("versions after script-settings: %v", env.Result)
	}

	// wrangler's uploads report "wrangler" as their source.
	body, ct = multipartBody(t, "metadata", map[string]any{"main_module": "index.js"}, map[string]string{"index.js": moduleSrc})
	req, _ := http.NewRequest("PUT", c.base+acct+"/workers/scripts/w2", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", "wrangler/4.143.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("wrangler upload: %v %v", err, resp)
	}
	resp.Body.Close()
	_, env, _ = c.do("GET", acct+"/workers/services/w2", nil)
	if src := resultMap(t, env)["default_environment"].(map[string]any)["script"].(map[string]any)["last_deployed_from"]; src != "wrangler" {
		t.Errorf("last_deployed_from after a wrangler upload: %v", src)
	}

	// The services DELETE removes the script.
	if st, _, _ := c.do("DELETE", acct+"/workers/services/w?force=true", nil); st != 200 {
		t.Errorf("service delete: %d", st)
	}
	if st, _, _ := c.do("GET", w+"/settings", nil); st != 404 {
		t.Errorf("script after service delete: %d, want 404", st)
	}
}
