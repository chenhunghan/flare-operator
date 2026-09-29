package fake

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// genericSample is the request bodies a generic-profile kind is driven with: one create body per
// variant (e.g. one per oneOf branch of the create body), and the update body (PUT or PATCH,
// whichever routes the spec defines; nil: no update).
type genericSample struct {
	creates map[string]map[string]any
	update  map[string]any
	// check inspects a variant's create result (optional).
	check func(t *testing.T, variant string, res map[string]any)
}

// Kinds that tests emulate with the generic profile although generator.yaml does not mark them
// `emulate: generic` (yet): their spec schemas exercise oneOf/anyOf (Hyperdrive) and singletons.
var (
	hyperdriveGenericKind = GenericKind{Group: "hyperdrive.cloudflare.flare.dev", Kind: "HyperdriveConfig", Scope: "account",
		CreatePath: "/accounts/{account_id}/hyperdrive/configs", ItemPath: "/accounts/{account_id}/hyperdrive/configs/{id}",
		ListPath: "/accounts/{account_id}/hyperdrive/configs", IDField: "id", NameField: "name", UpdateMethod: "PATCH"}
	workflowsSettingsGenericKind = GenericKind{Group: "workflows.cloudflare.flare.dev", Kind: "WorkflowsSettings", Scope: "account",
		ItemPath: "/accounts/{account_id}/workflows/settings", UpdateMethod: "PATCH", Singleton: true}
)

var genericSamples = map[string]genericSample{
	"AIGateway": {
		creates: map[string]map[string]any{"minimal": {"id": "flare-spike-gw", "cache_invalidate_on_update": false, "cache_ttl": 60,
			"collect_logs": true, "rate_limiting_interval": 0, "rate_limiting_limit": 0}},
		update: map[string]any{"cache_invalidate_on_update": true, "cache_ttl": 120, "collect_logs": false,
			"rate_limiting_interval": 60, "rate_limiting_limit": 10},
	},
	"SecretsStore": {creates: map[string]map[string]any{"minimal": {"name": "flare-spike-store"}}},
	"VectorizeIndex": {
		creates: map[string]map[string]any{
			"dimensions": {"name": "flare-spike-v", "config": map[string]any{"dimensions": 3, "metric": "cosine"}},
			"preset":     {"name": "flare-spike-v", "description": "d", "config": map[string]any{"preset": "@cf/baai/bge-small-en-v1.5"}},
		},
	},
	"HyperdriveConfig": {
		// One create per branch of hyperdrive_hyperdrive-origin-full (a oneOf).
		creates: map[string]map[string]any{
			"public": {"name": "flare-spike-hd", "origin": map[string]any{"scheme": "postgres", "database": "db", "user": "u",
				"password": "secret", "host": "db.example.com", "port": 5432}},
			"access": {"name": "flare-spike-hd", "origin": map[string]any{"scheme": "postgres", "database": "db", "user": "u",
				"password": "secret", "host": "db.example.com", "access_client_id": "0123456789abcdef0123456789abcdef.access",
				"access_client_secret": "shh"}},
			// As in 0195 (the real create response keeps service_id and drops the password).
			"vpc": {"name": "flare-spike-hd", "origin": map[string]any{"scheme": "postgresql", "database": "spike", "user": "spike",
				"password": "secret", "service_id": "01a0ec40-eee6-7b11-ae54-57bd3f9e6609"}},
		},
		update: map[string]any{"origin_connection_limit": 30, "origin": map[string]any{"user": "u2"}},
		check: func(t *testing.T, variant string, res map[string]any) {
			origin, _ := res["origin"].(map[string]any)
			want := map[string][]string{
				"public": {"host", "port"},
				"access": {"host", "access_client_id"},
				"vpc":    {"service_id"},
			}[variant]
			for _, k := range want {
				if _, has := origin[k]; !has {
					t.Errorf("%s: origin lost %s: %v", variant, k, origin)
				}
			}
			for _, k := range []string{"password", "access_client_secret"} {
				if _, has := origin[k]; has {
					t.Errorf("%s: origin returns the writeOnly %s: %v", variant, k, origin)
				}
			}
			if _, has := origin["host"]; variant == "vpc" && has {
				t.Errorf("vpc: origin got a zero-filled host of another branch: %v", origin)
			}
		},
	},
	"WorkflowsSettings": {update: map[string]any{"default_retention": map[string]any{"error_retention": 7}}},
}

// TestGenericCRUDStrictResponses drives every generic-profile kind (every generator.yaml kind
// marked `emulate: generic`, plus the extra test kinds) through its whole lifecycle and requires
// every emulated response to satisfy the pinned spec's response schema, with no allowlist
// entry: the generic profile's results are synthesized from the spec, so no real-API defect
// can excuse a violation.
func TestGenericCRUDStrictResponses(t *testing.T) {
	kinds := append(GeneratedGenericKinds(), hyperdriveGenericKind, workflowsSettingsGenericKind)
	for _, k := range kinds {
		sample, found := genericSamples[k.Kind]
		if !found {
			t.Errorf("%s: no genericSamples entry (add request bodies for the new generic kind)", k.Kind)
			continue
		}
		variants := make([]string, 0, len(sample.creates))
		for v := range sample.creates {
			variants = append(variants, v)
		}
		slices.Sort(variants)
		if k.Singleton {
			variants = []string{"singleton"}
		}
		for _, variant := range variants {
			t.Run(k.Kind+"/"+variant, func(t *testing.T) {
				s := genericServer(t, k)
				if sk := s.GenericSkipped(); len(sk) > 0 {
					t.Fatalf("not served: %v", sk)
				}
				if s.responseSpec() == nil {
					t.Fatal("responses are not validated")
				}
				c := newClient(t, s)
				m := s.generic.models[0]
				expect := func(what string, st int, env testEnv, want int) map[string]any {
					t.Helper()
					if st != want {
						t.Fatalf("%s: %d %v, want %d", what, st, env.Errors, want)
					}
					res, _ := env.Result.(map[string]any)
					return res
				}
				if k.Singleton {
					st, env, _ := c.do(http.MethodGet, gpath(k.ItemPath, ""), nil)
					expect("get", st, env, 200)
					for _, method := range m.updateMethods() {
						st, env, _ = c.do(method, gpath(k.ItemPath, ""), sample.update)
						expect(method, st, env, 200)
					}
					st, env, _ = c.do(http.MethodGet, gpath(k.ItemPath, ""), nil)
					expect("get after update", st, env, 200)
				} else {
					st, env, _ := c.do(http.MethodPost, gpath(k.CreatePath, ""), sample.creates[variant])
					res := expect("create", st, env, 200)
					if sample.check != nil {
						sample.check(t, variant, res)
					}
					id, _ := res[k.IDField].(string)
					if id == "" {
						t.Fatalf("create result without %s: %v", k.IDField, res)
					}
					if m.get != nil {
						st, env, _ = c.do(http.MethodGet, gpath(k.ItemPath, id), nil)
						expect("get", st, env, 200)
					}
					if m.list != nil {
						st, env, _ = c.do(http.MethodGet, gpath(k.ListPath, ""), nil)
						if st != 200 || !strings.Contains(mustJSON(t, env.Result), id) {
							t.Fatalf("list: %d %v", st, env.Result)
						}
					}
					if sample.update != nil {
						methods := m.updateMethods()
						if len(methods) == 0 {
							t.Fatal("sample has an update but the spec defines no PUT or PATCH")
						}
						for _, method := range methods {
							body := sample.update
							if method == http.MethodPut && k.UpdateMethod != http.MethodPut {
								body = sample.creates[variant] // a full replacement, not the PATCH body
							}
							st, env, _ = c.do(method, gpath(k.ItemPath, id), body)
							res := expect(method, st, env, 200)
							if sample.check != nil {
								sample.check(t, variant, res)
							}
						}
					}
					if m.del != nil {
						st, env, _ = c.do(http.MethodDelete, gpath(k.ItemPath, id), nil)
						expect("delete", st, env, 200)
						st, env, _ = c.do(http.MethodDelete, gpath(k.ItemPath, id), nil)
						expect("delete again", st, env, 404)
					}
					if m.get != nil {
						st, env, _ = c.do(http.MethodGet, gpath(k.ItemPath, id), nil)
						expect("get after delete", st, env, 404)
					}
				}
				for _, v := range s.ResponseViolations() {
					if v.Status < 400 || v.Allowed == "" {
						t.Errorf("response violates the pinned spec: %s", v)
					}
				}
				for _, j := range s.Journal() {
					if j.SchemaViolation != "" {
						t.Errorf("sample request violates the pinned spec: %s %s: %s", j.Method, j.Path, j.SchemaViolation)
					}
				}
			})
		}
	}
}

// updateMethods lists the update routes the generic profile serves for the model.
func (m *genericModel) updateMethods() []string {
	var out []string
	if m.put != nil {
		out = append(out, http.MethodPut)
	}
	if m.patch != nil {
		out = append(out, http.MethodPatch)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pickBranch prefers the branch that keeps the value over an earlier branch that validates only
// after dropping keys and zero-filling its own required fields; an anyOf value using several
// branches keeps all of them; a null the spec rejects becomes the first accepted zero value.
func TestGenericPickBranch(t *testing.T) {
	load := func(src string) *gSchema {
		t.Helper()
		var s openapi3.Schema
		if err := json.Unmarshal([]byte(src), &s); err != nil {
			t.Fatal(err)
		}
		return flattenSchema(openapi3.NewSchemaRef("", &s))
	}
	ctx := shapeCtx{fill: true}
	oneOf := load(`{"type":"object","properties":{"db":{"type":"string"}},"required":["db"],"oneOf":[
		{"type":"object","properties":{"host":{"type":"string"},"port":{"type":"integer","minimum":1}},"required":["host","port"]},
		{"type":"object","properties":{"service_id":{"type":"string"}},"required":["service_id"]}]}`)
	if len(oneOf.alts) != 2 {
		t.Fatalf("alts %d", len(oneOf.alts))
	}
	got := shape(oneOf, map[string]any{"db": "d", "service_id": "s"}, ctx)
	if want := `{"db":"d","service_id":"s"}`; mustJSON(t, got) != want {
		t.Errorf("oneOf: %s, want %s", mustJSON(t, got), want)
	}
	got = shape(oneOf, map[string]any{"db": "d", "host": "h", "port": 5432.0}, ctx)
	if want := `{"db":"d","host":"h","port":5432}`; mustJSON(t, got) != want {
		t.Errorf("oneOf: %s, want %s", mustJSON(t, got), want)
	}
	anyOf := load(`{"type":"object","anyOf":[
		{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},
		{"type":"object","properties":{"b":{"type":"string"}},"required":["b"]}]}`)
	got = shape(anyOf, map[string]any{"a": "1", "b": "2"}, ctx)
	if want := `{"a":"1","b":"2"}`; mustJSON(t, got) != want {
		t.Errorf("anyOf: %s, want %s", mustJSON(t, got), want)
	}
	noNull := load(`{"anyOf":[{"type":"object"},{"type":"array","items":{}},{"type":"string"}]}`)
	if noNull.accepts(nil) {
		t.Fatal("accepts null")
	}
	if got := shape(noNull, nil, ctx); mustJSON(t, got) != `{}` {
		t.Errorf("null → %s, want {}", mustJSON(t, got))
	}
}
