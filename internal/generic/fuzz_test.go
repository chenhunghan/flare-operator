package generic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Fuzz targets for the JSON diff/merge helpers of the reconciler. Seeds run with every
// `go test`; a real run is `go test -fuzz=FuzzCovers ./internal/generic/` (not part of CI).

func decodeJSONValue(s string) (any, bool) {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil, false
	}
	return v, true
}

// withExtra returns got with an extra field added to every object (the API adds fields and
// defaults the desired object does not set).
func withExtra(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{"__api_added": "x"}
		for k, e := range x {
			out[k] = withExtra(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = withExtra(e)
		}
		return out
	}
	return v
}

func deepCopyJSON(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// FuzzCovers: any value covers itself and itself with API-added fields; Covers never panics; a
// changed scalar is detected.
func FuzzCovers(f *testing.F) {
	for _, s := range []string{
		`{"title":"a"}`, `{"settings":{"delivery_delay":5,"retention":null},"name":"q"}`,
		`[1,2,{"a":[true,null]}]`, `null`, `"x"`, `3.5`, `{"a":{"b":{"c":[]}}}`, `{"a":[{"x":1},{"y":2}]}`,
	} {
		f.Add(s, `{"title":"b"}`)
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		want, ok1 := decodeJSONValue(a)
		got, ok2 := decodeJSONValue(b)
		if ok1 && ok2 {
			_ = Covers(want, got) // must not panic
		}
		if !ok1 {
			return
		}
		if !Covers(want, deepCopyJSON(want)) {
			t.Fatalf("%s does not cover itself", a)
		}
		if !Covers(want, withExtra(deepCopyJSON(want))) {
			t.Fatalf("%s does not cover itself with API-added fields", a)
		}
		if m, ok := want.(map[string]any); ok {
			for k, v := range m {
				if s, isStr := v.(string); isStr {
					changed := deepCopyJSON(want).(map[string]any)
					changed[k] = s + "-changed"
					if Covers(want, changed) {
						t.Fatalf("a changed field %q is not detected", k)
					}
				}
			}
		}
	})
}

// FuzzWriteOnlyHashAndWithout: WriteOnlyHash is deterministic and parses back with one entry
// per set path; without never mutates its input and removes exactly the given paths; pick keeps
// only set, named fields.
func FuzzWriteOnlyHashAndWithout(f *testing.F) {
	f.Add(`{"password":"p","origin":{"password":"s","host":"h"},"name":"n"}`, "password,origin.password")
	f.Add(`{"settings":{"delivery_paused":true}}`, "settings.delivery_paused,settings")
	f.Add(`{"a":null}`, "a,b.c")
	f.Add(`{}`, "")
	f.Fuzz(func(t *testing.T, doc, paths string) {
		v, ok := decodeJSONValue(doc)
		m, isObj := v.(map[string]any)
		if !ok || !isObj {
			return
		}
		var ps []string
		for _, p := range strings.Split(paths, ",") {
			if p != "" && !strings.ContainsAny(p, ";=") {
				ps = append(ps, p)
			}
		}
		h1, h2 := WriteOnlyHash(m, ps), WriteOnlyHash(deepCopyJSON(m).(map[string]any), ps)
		if h1 != h2 {
			t.Fatalf("WriteOnlyHash not deterministic: %q vs %q", h1, h2)
		}
		fields, known := parseWriteOnlyHash(h1)
		if !known {
			t.Fatalf("WriteOnlyHash %q does not parse", h1)
		}
		for _, p := range ps {
			_, set := pathValue(m, p)
			if _, in := fields[p]; in != set {
				t.Fatalf("path %q: set=%v but in hash=%v (%q)", p, set, in, h1)
			}
		}
		before := deepCopyJSON(m)
		out := without(m, ps)
		if !reflect.DeepEqual(before, deepCopyJSON(m)) {
			t.Fatalf("without mutated its input")
		}
		if om, ok := out.(map[string]any); ok {
			for _, p := range ps {
				if _, still := pathValue(om, p); still {
					t.Fatalf("without left %q in place", p)
				}
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		for k, val := range pick(m, keys[:len(keys)/2]) {
			if val == nil || !reflect.DeepEqual(val, m[k]) {
				t.Fatalf("pick returned %q=%v", k, val)
			}
		}
	})
}
