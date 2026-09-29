package fake

import (
	"testing"
)

// TestTagsList covers GET /accounts/{id}/tags/resources (UNVERIFIED model, see tags.go).
func TestTagsList(t *testing.T) {
	var spec *Spec
	if !testing.Short() {
		var err error
		if spec, err = LoadDefaultSpec(); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Options{Spec: spec, RejectSchemaViolations: spec != nil})
	c := newClient(t, s)
	put := func(typ, id string, tags map[string]string) {
		t.Helper()
		if st, env, _ := c.do("PUT", acct+"/tags", map[string]any{"resource_type": typ, "resource_id": id, "tags": tags}); st != 200 {
			t.Fatalf("PUT %s %s: %d %+v", typ, id, st, env.Errors)
		}
	}
	put("kv_namespace", "n1", map[string]string{"env": "prod", "team": "a"})
	put("kv_namespace", "n2", map[string]string{"env": "Staging"})
	put("queue", "q1", map[string]string{"team": "a"})
	put("queue", "q2", map[string]string{"x": "1"})
	if st, _, _ := c.do("DELETE", acct+"/tags", map[string]any{"resource_type": "queue", "resource_id": "q2"}); st != 204 {
		t.Fatal(st)
	}

	ids := func(q string) []string {
		t.Helper()
		st, env, _ := c.do("GET", acct+"/tags/resources"+q, nil)
		if st != 200 {
			t.Fatalf("%s: %d %+v", q, st, env.Errors)
		}
		var out []string
		for _, r := range env.Result.([]any) {
			m := r.(map[string]any)
			if m["etag"] == "" || m["tags"] == nil {
				t.Errorf("%s: entry %+v", q, m)
			}
			out = append(out, m["id"].(string))
		}
		return out
	}
	for q, want := range map[string]string{
		"":                                       "n1,n2,q1", // q2 has no tags left
		"?type=queue":                            "q1",
		"?type=queue&type=kv_namespace":          "n1,n2,q1",
		"?id=n2&id=q1":                           "n2,q1",
		"?tag=team":                              "n1,q1",
		"?tag=!team":                             "n2",
		"?tag=env=prod,staging":                  "n1",
		"?tag=env=staging&case_insensitive=true": "n2",
		"?tag=env!=prod":                         "n2,q1",
		"?tag=team=a&tag=env":                    "n1",
		"?type=queue&id=n1":                      "",
		"?type=zone&type=dns_record":             "", // spec-valid zone-level types
	} {
		got := ""
		for i, id := range ids(q) {
			if i > 0 {
				got += ","
			}
			got += id
		}
		if got != want {
			t.Errorf("%q: got %q want %q", q, got, want)
		}
	}
	if st, _, _ := c.do("GET", acct+"/tags/resources?type=bogus", nil); st != 400 {
		t.Errorf("bad type: %d", st)
	}
	for _, e := range s.Journal() {
		if e.SchemaViolation != "" && e.Status < 400 {
			t.Errorf("schema violation for %s %s?%s: %s", e.Method, e.Path, e.Query, e.SchemaViolation)
		}
	}
}
