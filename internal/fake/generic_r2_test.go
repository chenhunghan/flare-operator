package fake

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGenericR2Bucket drives R2Bucket through the generic profile's per-kind extensions
// (generator.yaml requestHeaders, observedAs, subResources) and the wrapped list result, with
// strict response validation. UNVERIFIED throughout except the 404 code (SOURCED, quirk).
func TestGenericR2Bucket(t *testing.T) {
	k := genericKind(t, "R2Bucket")
	s := genericServer(t, k)
	if sk := s.GenericSkipped(); len(sk) > 0 {
		t.Fatalf("not served: %v", sk)
	}
	plain := newClient(t, s)
	eu := newClient(t, s)
	eu.hdr = http.Header{"Cf-R2-Jurisdiction": {"eu"}}
	buckets := gpath(k.ListPath, "")
	item := gpath(k.ItemPath, "flare-spike-r2")
	cors := item + "/cors"
	expect := func(what string, st int, env testEnv, want int) map[string]any {
		t.Helper()
		if st != want {
			t.Fatalf("%s: %d %v, want %d", what, st, env.Errors, want)
		}
		res, _ := env.Result.(map[string]any)
		return res
	}

	// Create in the EU jurisdiction: the header is stored as jurisdiction, storageClass as
	// storage_class.
	st, env, _ := eu.do(http.MethodPost, buckets, map[string]any{"name": "flare-spike-r2", "storageClass": "InfrequentAccess", "locationHint": "weur"})
	res := expect("create (eu)", st, env, 200)
	if res["name"] != "flare-spike-r2" || res["jurisdiction"] != "eu" || res["storage_class"] != "InfrequentAccess" {
		t.Errorf("create result %v", res)
	}
	if ts, _ := res["creation_date"].(string); !strings.HasPrefix(ts, "2026-09-29T07:00:00") {
		t.Errorf("creation_date %v, want the fake clock's time", res["creation_date"])
	}
	if _, has := res["storageClass"]; has {
		t.Errorf("create result keeps the request name storageClass: %v", res)
	}
	// Without the header the bucket is invisible (another jurisdiction): 404/10006.
	st, env, _ = plain.do(http.MethodGet, item, nil)
	expect("get without the jurisdiction header", st, env, 404)
	if env.Errors[0].Code != 10006 {
		t.Errorf("404 code %d, want 10006", env.Errors[0].Code)
	}
	st, env, _ = eu.do(http.MethodGet, item, nil)
	expect("get (eu)", st, env, 200)
	// The same name is free in the default jurisdiction, taken in the EU one.
	st, env, _ = plain.do(http.MethodPost, buckets, map[string]any{"name": "flare-spike-r2"})
	if res := expect("create (default)", st, env, 200); res["jurisdiction"] != "default" || res["storage_class"] != "Standard" {
		t.Errorf("default create result %v", res)
	}
	st, env, _ = eu.do(http.MethodPost, buckets, map[string]any{"name": "flare-spike-r2"})
	expect("duplicate create (eu)", st, env, 409)

	// The list result wraps the items: {"buckets": [...]}, with cursor paging.
	for _, n := range []string{"flare-spike-r2-b", "flare-spike-r2-c"} {
		st, env, _ = eu.do(http.MethodPost, buckets, map[string]any{"name": n})
		expect("create "+n, st, env, 200)
	}
	var names []string
	cursor := ""
	for page := 0; page < 5; page++ {
		q := buckets + "?per_page=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		st, env, _ = eu.do(http.MethodGet, q, nil)
		res := expect("list (eu)", st, env, 200)
		items, ok := res["buckets"].([]any)
		if !ok {
			t.Fatalf("list result %v: no buckets array", env.Result)
		}
		for _, it := range items {
			names = append(names, it.(map[string]any)["name"].(string))
		}
		info, _ := env.ResultInfo.(map[string]any)
		cursor, _ = info["cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(names) != 3 || names[0] != "flare-spike-r2" {
		t.Errorf("eu buckets %v, want 3 in creation order", names)
	}
	st, env, _ = plain.do(http.MethodGet, buckets, nil)
	if res := expect("list (default)", st, env, 200); len(res["buckets"].([]any)) != 1 {
		t.Errorf("default jurisdiction lists %v, want only its own bucket", res)
	}

	// PATCH carries the storage class in cf-r2-storage-class and has no body.
	patch := newClient(t, s)
	patch.hdr = http.Header{"Cf-R2-Jurisdiction": {"eu"}, "Cf-R2-Storage-Class": {"Standard"}}
	st, env, _ = patch.do(http.MethodPatch, item, nil)
	if res := expect("patch (eu)", st, env, 200); res["storage_class"] != "Standard" || res["jurisdiction"] != "eu" {
		t.Errorf("patch result %v", res)
	}

	// CORS: a document stored with the bucket.
	st, env, _ = eu.do(http.MethodGet, cors, nil)
	expect("cors before any PUT", st, env, 404)
	rules := map[string]any{"rules": []any{map[string]any{"allowed": map[string]any{"origins": []any{"https://example.com"}, "methods": []any{"GET"}}, "maxAgeSeconds": 3600}}}
	st, env, _ = eu.do(http.MethodPut, cors, rules)
	expect("cors put", st, env, 200)
	st, env, _ = eu.do(http.MethodGet, cors, nil)
	if res := expect("cors get", st, env, 200); mustJSON(t, res) != mustJSON(t, rules) {
		t.Errorf("cors %s, want %s", mustJSON(t, res), mustJSON(t, rules))
	}
	st, env, _ = plain.do(http.MethodGet, cors, nil)
	expect("cors of the default-jurisdiction bucket", st, env, 404)
	st, env, _ = eu.do(http.MethodDelete, cors, nil)
	expect("cors delete", st, env, 200)
	st, env, _ = eu.do(http.MethodGet, cors, nil)
	expect("cors after delete", st, env, 404)
	st, env, _ = eu.do(http.MethodPut, gpath(k.ItemPath, "flare-spike-missing")+"/cors", rules)
	expect("cors of a missing bucket", st, env, 404)

	// Delete: the bucket goes, with its CORS policy.
	st, env, _ = eu.do(http.MethodPut, cors, rules)
	expect("cors put again", st, env, 200)
	st, env, _ = eu.do(http.MethodDelete, item, nil)
	expect("delete (eu)", st, env, 200)
	st, env, _ = eu.do(http.MethodGet, item, nil)
	expect("get after delete", st, env, 404)
	st, env, _ = eu.do(http.MethodGet, cors, nil)
	expect("cors after the bucket's delete", st, env, 404)
	st, env, _ = plain.do(http.MethodGet, item, nil)
	expect("default bucket still there", st, env, 200)

	for _, v := range s.ResponseViolations() {
		if v.Status < 400 || v.Allowed == "" {
			t.Errorf("response violates the pinned spec: %s", v)
		}
	}
	for _, j := range s.Journal() {
		if j.SchemaViolation != "" {
			t.Errorf("request violates the pinned spec: %s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}

// wrangler's `r2 bucket create` sends its JSON body as text/plain (SOURCED, spec.go
// plainTextBodies): the generic profile validates it as JSON, like the hand-written profiles.
func TestGenericR2BucketPlainTextCreate(t *testing.T) {
	s := genericServer(t, genericKind(t, "R2Bucket"))
	hs := httptest.NewServer(s)
	defer hs.Close()
	for body, want := range map[string]int{`{"name":"flare-spike-r2"}`: 200, `{"name":"A"}`: 400} {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+gpath("/accounts/{account_id}/r2/buckets", ""), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("text/plain create %s: %d, want %d", body, resp.StatusCode, want)
		}
	}
}
