package descriptors_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic/descriptors"
)

const accountID = "0123456789abcdef0123456789abcdef"

type apiClient struct {
	t    *testing.T
	base string
}

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *apiClient) do(method, path string, body any) (int, envelope) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+"/client/v4"+path, rd)
	req.Header.Set("Authorization", "Bearer test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env envelope
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &env); err != nil {
		c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
	}
	return resp.StatusCode, env
}

func path(p, id string) string {
	return strings.NewReplacer("{account_id}", accountID, "{id}", id).Replace(p)
}

// pick keeps the named top-level fields of a forProvider object.
func pick(m map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	return out
}

// subset reports whether every value in want is present and equal in got
// (objects recursively): server-side defaults may add fields.
func subset(want, got any) bool {
	wm, ok := want.(map[string]any)
	if !ok {
		return reflect.DeepEqual(want, got)
	}
	gm, ok := got.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range wm {
		if !subset(v, gm[k]) {
			return false
		}
	}
	return true
}

// forProvider round-trips a sample through the generated Go types, so only
// fields the kind really has survive, with the Go types' JSON encoding.
func forProvider(t *testing.T, e descriptors.Entry, sample string) map[string]any {
	t.Helper()
	obj := e.New()
	if err := json.Unmarshal([]byte(`{"spec":{"accountRef":{"name":"a"},"forProvider":`+sample+`}}`), obj); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Spec struct {
			ForProvider map[string]any `json:"forProvider"`
		} `json:"spec"`
	}
	b, _ := json.Marshal(obj)
	_ = json.Unmarshal(b, &m)
	return m.Spec.ForProvider
}

// observe decodes an API result into the kind's atProvider and returns it as
// JSON: this is what the generic reconciler will store in status.
func observe(t *testing.T, e descriptors.Entry, result json.RawMessage) map[string]any {
	t.Helper()
	obj := e.New()
	if err := json.Unmarshal([]byte(`{"status":{"atProvider":`+string(result)+`}}`), obj); err != nil {
		t.Fatalf("%s: API result does not decode into atProvider: %v\n%s", e.Kind, err, result)
	}
	var m struct {
		Status struct {
			AtProvider map[string]any `json:"atProvider"`
		} `json:"status"`
	}
	b, _ := json.Marshal(obj)
	_ = json.Unmarshal(b, &m)
	return m.Status.AtProvider
}

// TestDescriptorsAgainstFlarefake drives each generated Descriptor through its
// whole lifecycle on flarefake, the way the generic reconciler will: create
// with CreateFields, read the ID from IDField, observe with GET, list, update
// with UpdateFields via UpdateMethod, delete, and see 404. Requests are checked
// against the pinned spec.
func TestDescriptorsAgainstFlarefake(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec for request validation")
	}
	spec, err := fake.LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	samples := map[string]struct{ create, update string }{
		"KVNamespace": {`{"title":"flare-spike-kv-1"}`, `{"title":"flare-spike-kv-2"}`},
		// jurisdiction reads back (flarefake UNVERIFIED): no permanent drift for set jurisdictions.
		"Queue": {`{"queue_name":"flare-spike-q-1","jurisdiction":"eu","settings":{"delivery_delay":5}}`,
			`{"queue_name":"flare-spike-q-1","jurisdiction":"eu","settings":{"delivery_delay":10,"message_retention_period":3600}}`},
		"D1Database": {`{"name":"flare-spike-d1-1","primary_location_hint":"WEUR","read_replication":{"mode":"disabled"}}`,
			`{"name":"flare-spike-d1-1","primary_location_hint":"WEUR","read_replication":{"mode":"auto"}}`},
		// Generic-profile kinds (emulate: generic; UNVERIFIED emulation). "" = no update operation.
		"VectorizeIndex": {`{"name":"flare-spike-vec-1","description":"d","config":{"dimensions":3,"metric":"cosine"}}`, ""},
		"SecretsStore":   {`{"name":"flare-spike-store-1"}`, ""},
		"AIGateway": {`{"id":"flare-spike-gw-1","cache_invalidate_on_update":false,"cache_ttl":60,"collect_logs":true,"rate_limiting_interval":0,"rate_limiting_limit":0}`,
			`{"id":"flare-spike-gw-1","cache_invalidate_on_update":true,"cache_ttl":120,"collect_logs":false,"rate_limiting_interval":60,"rate_limiting_limit":10}`},
	}
	// Known spec defects: requests the real API accepts but the pinned spec rejects.
	type violation struct{ kind, method, contains, why string }
	known := []violation{
		{"D1Database", http.MethodPost, "primary_location_hint", "spec enum is lower-case; the API wants upper-case (0019)"},
		{"D1Database", "", `parameter "database_id" in path has an error: input matches more than one oneOf schemas`,
			"spec's database_id is a oneOf that a UUID matches twice; GET by uuid works (0020)"},
		{"KVNamespace", http.MethodDelete, "request body has an error: value is required but missing",
			"spec requires a DELETE body; the API deletes without one (0013)"},
	}
	isKnown := func(kind string, j fake.JournalEntry) bool {
		for _, k := range known {
			if k.kind == kind && (k.method == "" || k.method == j.Method) && strings.Contains(j.SchemaViolation, k.contains) {
				return true
			}
		}
		return false
	}

	for _, e := range descriptors.Entries() {
		t.Run(e.Kind, func(t *testing.T) {
			s := fake.New(fake.Options{Spec: spec, Generic: fake.GeneratedGenericKinds()})
			hs := httptest.NewServer(s)
			defer hs.Close()
			c := &apiClient{t: t, base: hs.URL}
			sm, ok := samples[e.Kind]
			if !ok {
				t.Fatalf("no sample for %s", e.Kind)
			}
			d := e.Descriptor
			desired := forProvider(t, e, sm.create)

			st, env := c.do(http.MethodPost, path(d.CreatePath, ""), pick(desired, d.CreateFields))
			if st != http.StatusOK || !env.Success {
				t.Fatalf("create: %d %+v", st, env.Errors)
			}
			var created map[string]any
			_ = json.Unmarshal(env.Result, &created)
			id, _ := created[d.IDField].(string)
			if id == "" {
				t.Fatalf("create result has no %s: %s", d.IDField, env.Result)
			}
			observe(t, e, env.Result)

			// Fields sent only on update (e.g. queue settings) need an update after create.
			if extra := pick(desired, d.UpdateFields); len(extra) > 0 && d.UpdateMethod != "" {
				if st, env := c.do(d.UpdateMethod, path(d.ItemPath, id), extra); st != http.StatusOK {
					t.Fatalf("post-create update: %d %+v", st, env.Errors)
				}
			}
			checkObserved(t, c, e, id, desired)

			// Adoption by name: the list contains the object with NameField.
			st, env = c.do(http.MethodGet, path(d.ListPath, ""), nil)
			if st != http.StatusOK || !strings.Contains(string(env.Result), id) {
				t.Fatalf("list: %d, id %s missing in %s", st, id, env.Result)
			}
			if name, _ := desired[d.NameField].(string); d.NameField != "" && !strings.Contains(string(env.Result), name) {
				t.Errorf("list does not show %s", d.NameField)
			}

			// Update.
			if (sm.update == "") != (d.UpdateMethod == "") {
				t.Fatalf("sample update %q for UpdateMethod %q", sm.update, d.UpdateMethod)
			}
			if d.UpdateMethod != "" {
				desired = forProvider(t, e, sm.update)
				if st, env := c.do(d.UpdateMethod, path(d.ItemPath, id), pick(desired, d.UpdateFields)); st != http.StatusOK {
					t.Fatalf("update: %d %+v", st, env.Errors)
				}
				checkObserved(t, c, e, id, desired)
			}

			// Delete, then 404.
			if st, env := c.do(http.MethodDelete, path(d.ItemPath, id), nil); st != http.StatusOK {
				t.Fatalf("delete: %d %+v", st, env.Errors)
			}
			if st, _ := c.do(http.MethodGet, path(d.ItemPath, id), nil); st != http.StatusNotFound {
				t.Fatalf("get after delete: %d, want 404", st)
			}

			for _, j := range s.Journal() {
				if j.SchemaViolation == "" {
					continue
				}
				if isKnown(e.Kind, j) {
					continue
				}
				t.Errorf("request violates the pinned spec: %s %s: %s", j.Method, j.Path, j.SchemaViolation)
			}
		})
	}
}

// checkObserved GETs the object and checks that every desired field that is
// not write-only reads back equal (drift detection is possible), and that the
// write-only ones are indeed absent.
func checkObserved(t *testing.T, c *apiClient, e descriptors.Entry, id string, desired map[string]any) {
	t.Helper()
	d := e.Descriptor
	st, env := c.do(http.MethodGet, path(d.ItemPath, id), nil)
	if st != http.StatusOK {
		t.Fatalf("get: %d %+v", st, env.Errors)
	}
	obs := observe(t, e, env.Result)
	if obs[d.IDField] != id {
		t.Errorf("atProvider.%s = %v, want %s", d.IDField, obs[d.IDField], id)
	}
	for k, v := range desired {
		if contains(d.WriteOnly, k) {
			if _, ok := obs[k]; ok {
				t.Errorf("write-only %s is returned by GET", k)
			}
			continue
		}
		if !subset(v, obs[k]) {
			t.Errorf("forProvider.%s = %v, atProvider has %v", k, v, obs[k])
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
