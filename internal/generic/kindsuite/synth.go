package kindsuite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/chenhunghan/flare-operator/internal/generic/descriptors"
)

// Schema is a kind's forProvider schema from its generated CRD, plus the top-level fields the
// CRD requires when the object may create (its FieldValueRequired CEL rules).
type Schema struct {
	ForProvider apiextensionsv1.JSONSchemaProps
	Required    []string
}

// LoadSchema reads the kind's CRD from <root>/config/crd/bases.
func LoadSchema(root string, e descriptors.Entry) (*Schema, error) {
	files, err := filepath.Glob(filepath.Join(root, "config", "crd", "bases", e.Group+"_*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if crd.Spec.Names.Kind != e.Kind {
			continue
		}
		for _, v := range crd.Spec.Versions {
			if v.Name != e.Version || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			spec := v.Schema.OpenAPIV3Schema.Properties["spec"]
			s := &Schema{ForProvider: spec.Properties["forProvider"]}
			for _, r := range spec.XValidations {
				if r.Reason != nil && *r.Reason == apiextensionsv1.FieldValueRequired {
					if f, ok := strings.CutPrefix(r.FieldPath, ".forProvider."); ok {
						s.Required = append(s.Required, f)
					}
				}
			}
			sort.Strings(s.Required)
			return s, nil
		}
	}
	return nil, fmt.Errorf("no CRD for %s/%s %s under %s", e.Group, e.Kind, e.Version, filepath.Join(root, "config", "crd", "bases"))
}

// Synthesize builds a minimal forProvider: every required top-level field, recursively with
// the required properties of objects, valued from the schema (first enum value, a name built
// from uniq that satisfies pattern and length limits, the smallest allowed positive number,
// false). Kinds whose schema cannot express a valid body (flattened unions such as Vectorize's
// config) need a fixture (testdata/<Kind>.json).
func (s *Schema) Synthesize(uniq string) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range s.Required {
		p, ok := s.ForProvider.Properties[f]
		if !ok {
			return nil, fmt.Errorf("required field %s is not in the schema", f)
		}
		v, err := synth(p, uniq, f)
		if err != nil {
			return nil, err
		}
		out[f] = v
	}
	return out, nil
}

func synth(p apiextensionsv1.JSONSchemaProps, uniq, where string) (any, error) {
	switch p.Type {
	case "string":
		if len(p.Enum) > 0 {
			var v any
			if err := json.Unmarshal(p.Enum[0].Raw, &v); err != nil {
				return nil, err
			}
			return v, nil
		}
		return nameFor(p, uniq, where)
	case "integer", "number":
		return number(p, 1), nil
	case "boolean":
		return false, nil
	case "array":
		n := 1
		if p.MinItems != nil && *p.MinItems > 1 {
			n = int(*p.MinItems)
		}
		if p.Items == nil || p.Items.Schema == nil {
			return []any{}, nil
		}
		out := make([]any, n)
		for i := range out {
			v, err := synth(*p.Items.Schema, fmt.Sprintf("%s%d", uniq, i), where+"[]")
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case "object", "":
		out := map[string]any{}
		for _, r := range p.Required {
			v, err := synth(p.Properties[r], uniq, where+"."+r)
			if err != nil {
				return nil, err
			}
			out[r] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: cannot synthesize type %q", where, p.Type)
}

func number(p apiextensionsv1.JSONSchemaProps, want float64) any {
	if p.Minimum != nil && *p.Minimum > want {
		want = *p.Minimum
	}
	if p.Maximum != nil && *p.Maximum < want {
		want = *p.Maximum
	}
	if p.Type == "integer" {
		return int64(want)
	}
	return want
}

// nameFor returns a variant of uniq that the string schema accepts.
func nameFor(p apiextensionsv1.JSONSchemaProps, uniq, where string) (string, error) {
	cands := []string{uniq, strings.ReplaceAll(uniq, "-", ""), strings.ReplaceAll(uniq, "-", "_"), "a" + strings.ReplaceAll(uniq, "-", "")}
	for _, c := range cands {
		if ok, _ := accepts(p, c); ok {
			return c, nil
		}
	}
	if p.MinLength != nil && int64(len(cands[1])) < *p.MinLength {
		c := cands[1] + strings.Repeat("a", int(*p.MinLength)-len(cands[1]))
		if ok, _ := accepts(p, c); ok {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s: no generated value matches pattern %q (lengths %v-%v): add a fixture", where, p.Pattern, p.MinLength, p.MaxLength)
}

// accepts reports whether a string schema accepts v (pattern and length; an ECMA pattern Go
// cannot compile is not checked).
func accepts(p apiextensionsv1.JSONSchemaProps, v string) (bool, error) {
	if p.MaxLength != nil && int64(len(v)) > *p.MaxLength {
		return false, nil
	}
	if p.MinLength != nil && int64(len(v)) < *p.MinLength {
		return false, nil
	}
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			var s string
			if json.Unmarshal(e.Raw, &s) == nil && s == v {
				return true, nil
			}
		}
		return false, nil
	}
	if p.Pattern != "" {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return true, err
		}
		return re.MatchString(v), nil
	}
	return true, nil
}

// Mutate returns a copy of fp with one field changed, and the dotted path it changed. It
// tries fields in order (those set in fp first, sorted): strings get a suffix (or the next
// enum value), numbers move by one within their bounds, booleans flip, objects recurse. ok is
// false when no field can be changed.
func (s *Schema) Mutate(fp map[string]any, fields []string, skip func(string) bool) (map[string]any, string, bool) {
	sorted := append([]string(nil), fields...)
	sort.SliceStable(sorted, func(i, j int) bool {
		_, si := fp[sorted[i]]
		_, sj := fp[sorted[j]]
		if si != sj {
			return si
		}
		return sorted[i] < sorted[j]
	})
	for _, f := range sorted {
		if skip != nil && skip(f) {
			continue
		}
		p, ok := s.ForProvider.Properties[f]
		if !ok {
			continue
		}
		if v, sub, ok := mutate(p, fp[f]); ok {
			out := deepCopy(fp).(map[string]any)
			out[f] = v
			return out, strings.TrimSuffix(f+"."+sub, "."), true
		}
	}
	return nil, "", false
}

func mutate(p apiextensionsv1.JSONSchemaProps, cur any) (any, string, bool) {
	switch p.Type {
	case "string":
		if len(p.Enum) > 0 {
			var vals []any
			for _, e := range p.Enum {
				var v any
				if json.Unmarshal(e.Raw, &v) == nil {
					vals = append(vals, v)
				}
			}
			for _, v := range vals {
				if v != cur {
					return v, "", true
				}
			}
			return nil, "", false
		}
		base, _ := cur.(string)
		if base == "" {
			base = "flare-spike"
		}
		for _, c := range []string{base + "-u", base + "u", base + "_u"} {
			if ok, _ := accepts(p, c); ok {
				return c, "", true
			}
		}
		if len(base) > 1 {
			c := base[:len(base)-1] + "z"
			if base[len(base)-1] == 'z' {
				c = base[:len(base)-1] + "y"
			}
			if ok, _ := accepts(p, c); ok {
				return c, "", true
			}
		}
		return nil, "", false
	case "integer", "number":
		f, _ := toFloat(cur)
		for _, d := range []float64{1, -1} {
			n := f + d
			if (p.Minimum == nil || n >= *p.Minimum) && (p.Maximum == nil || n <= *p.Maximum) {
				return number(p, n), "", true
			}
		}
		return nil, "", false
	case "boolean":
		b, _ := cur.(bool)
		return !b, "", true
	case "object":
		m, _ := cur.(map[string]any)
		if m == nil {
			base, err := synth(p, "flare-spike-m", "")
			if err != nil {
				return nil, "", false
			}
			m = base.(map[string]any)
			if len(m) > 0 {
				return m, "", true // absent → a minimal object is the change
			}
		}
		keys := make([]string, 0, len(p.Properties))
		for k := range p.Properties {
			keys = append(keys, k)
		}
		sort.SliceStable(keys, func(i, j int) bool {
			_, si := m[keys[i]]
			_, sj := m[keys[j]]
			if si != sj {
				return si
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			if v, sub, ok := mutate(p.Properties[k], m[k]); ok {
				out := deepCopy(m).(map[string]any)
				out[k] = v
				return out, strings.TrimSuffix(k+"."+sub, "."), true
			}
		}
	}
	return nil, "", false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func deepCopy(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
