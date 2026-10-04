package fake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func genericServer(t *testing.T, kinds ...GenericKind) *Server {
	t.Helper()
	if testing.Short() {
		t.Skip("the generic profile loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	all := kinds == nil
	if all {
		kinds = GeneratedGenericKinds()
	}
	s := New(Options{Spec: spec, Generic: kinds})
	if sk := s.GenericSkipped(); all && len(sk) > 0 {
		t.Fatalf("generated generic kinds not served: %v", sk)
	}
	s.Clock.Set(time.Date(2026, 9, 29, 7, 0, 0, 123456000, time.UTC))
	return s
}

func genericKind(t *testing.T, kind string) GenericKind {
	t.Helper()
	for _, k := range GeneratedGenericKinds() {
		if k.Kind == kind {
			return k
		}
	}
	t.Fatalf("no generated generic kind %s", kind)
	return GenericKind{}
}

func gpath(p, id string) string {
	return strings.NewReplacer("/accounts/{account_id}", acct, "{id}", id).Replace(p)
}

// The profile is opt-in: without Options.Generic no generic route exists, so the conformance
// rule about recordings that hit emulated routes is unaffected.
func TestGenericOptIn(t *testing.T) {
	s := New(Options{})
	for _, k := range GeneratedGenericKinds() {
		for _, p := range []string{k.CreatePath, k.ItemPath} {
			if h, _, _ := s.match(http.MethodGet, strings.ReplaceAll(strings.ReplaceAll(p, "{account_id}", "a"), "{id}", "x")); h != nil {
				t.Errorf("%s: %s is served without Options.Generic", k.Kind, p)
			}
		}
	}
}

// Hand-written profiles take precedence: a generic KV kind is skipped and KV keeps its
// recorded behavior (0011: 404/10013).
func TestGenericHandWrittenPrecedence(t *testing.T) {
	kv := GenericKind{Group: "flare.dev", Kind: "KVNamespace", Scope: "account",
		CreatePath: "/accounts/{account_id}/storage/kv/namespaces", ItemPath: "/accounts/{account_id}/storage/kv/namespaces/{id}",
		ListPath: "/accounts/{account_id}/storage/kv/namespaces", IDField: "id", NameField: "title", UpdateMethod: "PUT"}
	s := genericServer(t, kv)
	if sk := s.GenericSkipped(); len(sk) != 1 || !strings.Contains(sk[0], "hand-written") {
		t.Fatalf("skipped = %v", sk)
	}
	c := newClient(t, s)
	st, env, _ := c.do(http.MethodGet, acct+"/storage/kv/namespaces/0123456789abcdef0123456789abcdef", nil)
	if st != 404 || env.Errors[0].Code != 10013 {
		t.Fatalf("KV GET missing: %d %v", st, env.Errors)
	}
}

func TestGenericVectorizeIndex(t *testing.T) {
	s := genericServer(t)
	c := newClient(t, s)
	k := genericKind(t, "VectorizeIndex")

	// Spec validation: config is required.
	if st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), map[string]any{"name": "flare-spike-v"}); st != 400 || env.Errors[0].Code != genericViolationCode {
		t.Fatalf("invalid create: %d %v", st, env.Errors)
	}
	body := map[string]any{"name": "flare-spike-v", "description": "d", "config": map[string]any{"dimensions": 3, "metric": "cosine"}}
	st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), body)
	if st != 200 {
		t.Fatalf("create: %d %v", st, env.Errors)
	}
	res := env.Result.(map[string]any)
	// The ID is the client-chosen name; timestamps come from the fake clock with the spec
	// example's precision (microseconds).
	if res["name"] != "flare-spike-v" || res["created_on"] != "2026-09-29T07:00:00.123456Z" || res["modified_on"] != res["created_on"] {
		t.Fatalf("create result %v", res)
	}
	if cfg := res["config"].(map[string]any); cfg["dimensions"] != float64(3) || cfg["metric"] != "cosine" {
		t.Fatalf("config %v", cfg)
	}
	if st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), body); st != 409 {
		t.Fatalf("duplicate create: %d %v", st, env.Errors)
	}
	st, env, _ = c.do(http.MethodGet, gpath(k.ItemPath, "flare-spike-v"), nil)
	if st != 200 || env.Result.(map[string]any)["description"] != "d" {
		t.Fatalf("get: %d %v", st, env.Result)
	}
	// The v2 API has no update: PATCH is not a route (405).
	if st, _, _ := c.do(http.MethodPatch, gpath(k.ItemPath, "flare-spike-v"), map[string]any{}); st != 405 {
		t.Fatalf("PATCH: %d, want 405", st)
	}
	// List without result_info (spec; 0154).
	st, env, _ = c.do(http.MethodGet, gpath(k.ListPath, ""), nil)
	if st != 200 || len(env.Result.([]any)) != 1 || env.ResultInfo != nil {
		t.Fatalf("list: %d %v %v", st, env.Result, env.ResultInfo)
	}
	if st, _, _ := c.do(http.MethodDelete, gpath(k.ItemPath, "flare-spike-v"), nil); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	st, env, _ = c.do(http.MethodGet, gpath(k.ItemPath, "flare-spike-v"), nil)
	if st != 404 || env.Errors[0].Code != genericNotFoundCode {
		t.Fatalf("get after delete: %d %v", st, env.Errors)
	}
	if st, _, _ := c.do(http.MethodDelete, gpath(k.ItemPath, "flare-spike-v"), nil); st != 404 {
		t.Fatalf("delete again: %d", st)
	}
}

func TestGenericSecretsStorePagination(t *testing.T) {
	s := genericServer(t)
	c := newClient(t, s)
	k := genericKind(t, "SecretsStore")
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), map[string]any{"name": "flare-spike-" + n})
		if st != 200 {
			t.Fatalf("create: %d %v", st, env.Errors)
		}
		r := env.Result.(map[string]any)
		id, _ := r["id"].(string)
		// Spec: id is 32 hex characters (example); account_id is the path's account; created
		// and modified are server timestamps.
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || r["account_id"] != "0123456789abcdef0123456789abcdef" ||
			r["created"] == nil || r["modified"] == nil {
			t.Fatalf("create result %v", r)
		}
		ids = append(ids, id)
	}
	if st, _, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), map[string]any{"name": "flare-spike-a"}); st != 409 {
		t.Fatalf("duplicate name: %d", st)
	}
	st, env, _ := c.do(http.MethodGet, gpath(k.ListPath, "")+"?per_page=2&page=2", nil)
	if st != 200 {
		t.Fatalf("list: %d %v", st, env.Errors)
	}
	items := env.Result.([]any)
	info := env.ResultInfo.(map[string]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != ids[2] {
		t.Fatalf("page 2: %v", items)
	}
	// 0155: no total_pages (quirk), although the spec declares it.
	want := map[string]any{"page": float64(2), "per_page": float64(2), "count": float64(1), "total_count": float64(3)}
	if len(info) != len(want) {
		t.Fatalf("result_info %v, want %v", info, want)
	}
	for kk, v := range want {
		if info[kk] != v {
			t.Fatalf("result_info %v, want %v", info, want)
		}
	}
}

// PUT replaces the fields its body carries (omitted ones reset to defaults), keeps the ID and
// creation time, and advances modified_at.
func TestGenericAIGatewayPut(t *testing.T) {
	s := genericServer(t)
	c := newClient(t, s)
	k := genericKind(t, "AIGateway")
	create := map[string]any{"id": "flare-spike-gw", "cache_invalidate_on_update": false, "cache_ttl": 60, "collect_logs": true,
		"rate_limiting_interval": 0, "rate_limiting_limit": 0, "logpush": true}
	st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), create)
	if st != 200 {
		t.Fatalf("create: %d %v", st, env.Errors)
	}
	r := env.Result.(map[string]any)
	if r["id"] != "flare-spike-gw" || r["logpush"] != true || r["created_at"] == nil || r["workers_ai_billing_mode"] != "postpaid" {
		t.Fatalf("create result %v", r)
	}
	created := r["created_at"]
	s.Clock.Advance(time.Minute)
	put := map[string]any{"cache_invalidate_on_update": true, "cache_ttl": 120, "collect_logs": true,
		"rate_limiting_interval": 0, "rate_limiting_limit": 0}
	st, env, _ = c.do(http.MethodPut, gpath(k.ItemPath, "flare-spike-gw"), put)
	if st != 200 {
		t.Fatalf("put: %d %v", st, env.Errors)
	}
	r = env.Result.(map[string]any)
	if r["id"] != "flare-spike-gw" || r["cache_ttl"] != float64(120) || r["created_at"] != created || r["modified_at"] == created {
		t.Fatalf("put result %v", r)
	}
	if _, set := r["logpush"]; set {
		t.Fatalf("PUT without logpush kept it: %v", r)
	}
	if st, _, _ := c.do(http.MethodPut, gpath(k.ItemPath, "nope"), put); st != 404 {
		t.Fatalf("put missing: %d", st)
	}
}

// PATCH merges (RFC 7386); the spec's writeOnly password is never returned.
func TestGenericPatchMergeAndWriteOnly(t *testing.T) {
	hd := GenericKind{Group: "flare.dev", Kind: "HyperdriveConfig", Scope: "account",
		CreatePath: "/accounts/{account_id}/hyperdrive/configs", ItemPath: "/accounts/{account_id}/hyperdrive/configs/{id}",
		ListPath: "/accounts/{account_id}/hyperdrive/configs", IDField: "id", NameField: "name", UpdateMethod: "PATCH"}
	s := genericServer(t, hd)
	c := newClient(t, s)
	st, env, _ := c.do(http.MethodPost, gpath(hd.CreatePath, ""), map[string]any{"name": "flare-spike-hd",
		"origin": map[string]any{"scheme": "postgres", "database": "db", "user": "u", "password": "secret", "host": "db.example.com", "port": 5432}})
	if st != 200 {
		t.Fatalf("create: %d %v", st, env.Errors)
	}
	r := env.Result.(map[string]any)
	id := r["id"].(string)
	origin := r["origin"].(map[string]any)
	if _, leaked := origin["password"]; leaked || origin["host"] != "db.example.com" {
		t.Fatalf("origin %v", origin)
	}
	st, env, _ = c.do(http.MethodPatch, gpath(hd.ItemPath, id), map[string]any{"origin_connection_limit": 30})
	if st != 200 {
		t.Fatalf("patch: %d %v", st, env.Errors)
	}
	r = env.Result.(map[string]any)
	if r["name"] != "flare-spike-hd" || r["origin_connection_limit"] != float64(30) || r["origin"].(map[string]any)["user"] != "u" {
		t.Fatalf("patch result %v", r)
	}
}

// A singleton serves a default object shaped from the spec until PATCH changes it.
func TestGenericSingleton(t *testing.T) {
	ws := GenericKind{Group: "flare.dev", Kind: "WorkflowsSettings", Scope: "account",
		ItemPath: "/accounts/{account_id}/workflows/settings", UpdateMethod: "PATCH", Singleton: true}
	s := genericServer(t, ws)
	c := newClient(t, s)
	st, env, _ := c.do(http.MethodGet, gpath(ws.ItemPath, ""), nil)
	if st != 200 {
		t.Fatalf("get: %d %v", st, env.Errors)
	}
	st, env, _ = c.do(http.MethodPatch, gpath(ws.ItemPath, ""), map[string]any{"default_retention": map[string]any{"error_retention": 7}})
	if st != 200 {
		t.Fatalf("patch: %d %v", st, env.Errors)
	}
	st, env, _ = c.do(http.MethodGet, gpath(ws.ItemPath, ""), nil)
	if dr, _ := env.Result.(map[string]any)["default_retention"].(map[string]any); st != 200 || dr["error_retention"] != float64(7) {
		t.Fatalf("get after patch: %d %v", st, env.Result)
	}
	s.Reset()
	st, env, _ = c.do(http.MethodGet, gpath(ws.ItemPath, ""), nil)
	if dr, _ := env.Result.(map[string]any)["default_retention"].(map[string]any); st != 200 || dr["error_retention"] == float64(7) {
		t.Fatalf("Reset kept the singleton: %v", env.Result)
	}
}

func TestGenericMergePatch(t *testing.T) {
	dst := map[string]any{"a": 1, "o": map[string]any{"x": 1, "y": 2}, "gone": true}
	mergePatch(dst, map[string]any{"o": map[string]any{"y": 3, "z": 4}, "gone": nil, "b": 2})
	want := `map[a:1 b:2 o:map[x:1 y:3 z:4]]`
	if got := fmt.Sprint(dst); got != want { // fmt prints map keys sorted
		t.Fatalf("merge = %s, want %s", got, want)
	}
}

func TestGenericCursor(t *testing.T) {
	for _, n := range []int{0, 1, 17} {
		if got := decodeCursor(encodeCursor(n)); got != n {
			t.Errorf("cursor %d round-trips to %d", n, got)
		}
	}
	if decodeCursor("!!") != 0 {
		t.Error("a bad cursor must restart at 0")
	}
}

// TestGenericConformance replays the recordings of generically emulated kinds (0154, 0155).
// It is separate from TestConformance, whose probe server (New(Options{})) has no generic
// routes: these recordings stay "not emulated" there.
func TestGenericConformance(t *testing.T) {
	s := genericServer(t)
	hs := httptest.NewServer(s)
	defer hs.Close()
	n := 0
	for _, rec := range loadRecordings(t) {
		if oneOf(label(rec.file), "final-vectorize", "final-secrets-stores") {
			replay(t, s, hs.URL, rec)
			n++
		}
	}
	if n != 2 {
		t.Fatalf("replayed %d recordings, want 2", n)
	}
}
