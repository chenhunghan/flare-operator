package chart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// valuesRow is one row of the values table in charts/flare-operator/README.md.
type valuesRow struct {
	key, typ, def string
	line          int
}

// readmeValuesTable returns the rows between the values-table markers of the chart README.
func readmeValuesTable(t *testing.T) []valuesRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chartDir(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = "<!-- values-table:begin -->", "<!-- values-table:end -->"
	var rows []valuesRow
	in := false
	for i, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.TrimSpace(line) == begin:
			in = true
			continue
		case strings.TrimSpace(line) == end:
			if !in {
				t.Fatalf("README.md:%d: %s before %s", i+1, end, begin)
			}
			return rows
		}
		if !in || !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 6 {
			t.Fatalf("README.md:%d: want 4 cells (Key | Type | Default | Description) and no '|' inside a cell, got %d: %s", i+1, len(cells)-2, line)
		}
		key, ok := codeSpan(cells[1])
		if !ok {
			t.Fatalf("README.md:%d: the Key cell must be one code span: %q", i+1, cells[1])
		}
		rows = append(rows, valuesRow{key: key, typ: strings.TrimSpace(cells[2]), def: strings.TrimSpace(cells[3]), line: i + 1})
	}
	t.Fatalf("README.md: no %s ... %s block", begin, end)
	return nil
}

// codeSpan returns the content of s when s (trimmed) is exactly one `code span`.
func codeSpan(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '`' || s[len(s)-1] != '`' || strings.Count(s, "`") != 2 {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// schemaNode resolves local "#/definitions/<name>" references.
func schemaNode(t *testing.T, root, n map[string]any) map[string]any {
	t.Helper()
	for {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		name, found := strings.CutPrefix(ref, "#/definitions/")
		defs, _ := root["definitions"].(map[string]any)
		next, ok := defs[name].(map[string]any)
		if !found || !ok {
			t.Fatalf("values.schema.json: unresolvable $ref %q", ref)
		}
		n = next
	}
}

// schemaType renders the JSON-schema type of n: "string", "array", "integer or string", ...
func schemaType(t *testing.T, root, n map[string]any) string {
	t.Helper()
	n = schemaNode(t, root, n)
	switch v := n["type"].(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			parts = append(parts, p.(string))
		}
		return strings.Join(parts, " or ")
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if alts, ok := n[k].([]any); ok {
			var parts []string
			for _, a := range alts {
				parts = append(parts, schemaType(t, root, a.(map[string]any)))
			}
			return strings.Join(parts, " or ")
		}
	}
	return "(untyped)"
}

// TestValuesTableMatchesChart keeps the values table of charts/flare-operator/README.md in step
// with values.yaml and values.schema.json (the chart has no helm-docs generator): every
// values.yaml key has a row (or lies under a row for an object), every row names a key of both
// files, the Type column is the schema's type, and a Default given as a code span equals the
// values.yaml default.
func TestValuesTableMatchesChart(t *testing.T) {
	rows := readmeValuesTable(t)
	if len(rows) == 0 {
		t.Fatal("README.md: the values table is empty")
	}

	vb, err := os.ReadFile(filepath.Join(chartDir(t), "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(vb, &values); err != nil {
		t.Fatalf("values.yaml: %v", err)
	}
	sb, err := os.ReadFile(filepath.Join(chartDir(t), "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(sb, &schema); err != nil {
		t.Fatalf("values.schema.json: %v", err)
	}

	byKey := map[string]valuesRow{}
	for _, r := range rows {
		if prev, dup := byKey[r.key]; dup {
			t.Errorf("README.md:%d: `%s` already has a row (line %d)", r.line, r.key, prev.line)
		}
		byKey[r.key] = r

		// The key exists in values.yaml and in the schema.
		var v any = values
		node := schema
		for _, seg := range strings.Split(r.key, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				t.Errorf("README.md:%d: `%s`: values.yaml has no key %q", r.line, r.key, seg)
				v = nil
				break
			}
			if v, ok = m[seg]; !ok {
				t.Errorf("README.md:%d: `%s`: values.yaml has no key %q", r.line, r.key, seg)
				break
			}
			props, _ := schemaNode(t, schema, node)["properties"].(map[string]any)
			next, ok := props[seg].(map[string]any)
			if !ok {
				t.Errorf("README.md:%d: `%s`: values.schema.json has no property %q", r.line, r.key, seg)
				node = nil
				break
			}
			node = next
		}
		if node != nil {
			if want := schemaType(t, schema, node); r.typ != want {
				t.Errorf("README.md:%d: `%s`: Type %q, values.schema.json says %q", r.line, r.key, r.typ, want)
			}
		}
		if def, ok := codeSpan(r.def); ok && v != nil {
			var got any
			if err := yaml.Unmarshal([]byte(def), &got); err != nil {
				t.Errorf("README.md:%d: `%s`: Default %s does not parse: %v", r.line, r.key, r.def, err)
			} else if !reflect.DeepEqual(got, v) {
				want, _ := json.Marshal(v)
				t.Errorf("README.md:%d: `%s`: Default %s, values.yaml has %s", r.line, r.key, r.def, want)
			}
		}
	}

	// Every values.yaml key is documented, by its own row or by a row for an enclosing object.
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := prefix + k
			if _, ok := byKey[p]; ok {
				continue
			}
			if sub, ok := m[k].(map[string]any); ok && len(sub) > 0 {
				walk(p+".", sub)
				continue
			}
			t.Errorf("values.yaml key %s has no row in the README values table", p)
		}
	}
	walk("", values)
}
