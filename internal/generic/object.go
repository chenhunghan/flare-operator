package generic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Generated kinds (cmd/flaregen) share one shape: Spec.ForProvider holds the API fields with
// the API's JSON names and Status.AtProvider the last observed API object. The reconciler
// reaches both by reflection and works on their JSON form, so one implementation serves every
// Descriptor.

func structField(obj any, names ...string) (reflect.Value, error) {
	v := reflect.ValueOf(obj)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, fmt.Errorf("generic: nil %T", obj)
		}
		v = v.Elem()
	}
	for _, n := range names {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("generic: %T: %s is not a struct", obj, n)
		}
		v = v.FieldByName(n)
		if !v.IsValid() {
			return reflect.Value{}, fmt.Errorf("generic: %T has no field %s", obj, strings.Join(names, "."))
		}
	}
	return v, nil
}

// ForProvider returns spec.forProvider of a generated object as JSON values (unset optional
// fields are absent).
func ForProvider(obj any) (map[string]any, error) {
	v, err := structField(obj, "Spec", "ForProvider")
	if err != nil {
		return nil, err
	}
	return toMap(v.Interface())
}

// SetAtProvider replaces status.atProvider with the API result raw. Unknown fields are dropped;
// a type mismatch is an error.
func SetAtProvider(obj any, raw json.RawMessage) error {
	v, err := structField(obj, "Status", "AtProvider")
	if err != nil {
		return err
	}
	v.Set(reflect.Zero(v.Type()))
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v.Addr().Interface()); err != nil {
		v.Set(reflect.Zero(v.Type()))
		return fmt.Errorf("decode API result into status.atProvider: %w", err)
	}
	return nil
}

// ClearAtProvider empties status.atProvider.
func ClearAtProvider(obj any) {
	if v, err := structField(obj, "Status", "AtProvider"); err == nil {
		v.Set(reflect.Zero(v.Type()))
	}
}

// statusOf returns the Status field (for change detection).
func statusOf(obj any) any {
	v, err := structField(obj, "Status")
	if err != nil {
		return nil
	}
	return v.Interface()
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeObject decodes an API result into JSON values; a non-object result is an error.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	m := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("API result is not an object: %w", err)
	}
	return m, nil
}

// pick keeps the named top-level fields of m that are set.
func pick(m map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := m[f]; ok && v != nil {
			out[f] = v
		}
	}
	return out
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Covers reports whether the observed value got agrees with the desired value want: objects
// are compared key by key for the keys want sets (the API may add fields and defaults), arrays
// element-wise with equal length, scalars by JSON value. Both sides are JSON values
// (encoding/json: float64, string, bool, nil, []any, map[string]any).
func Covers(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if v == nil {
				continue // unset in the desired object
			}
			if !Covers(v, g[k]) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !Covers(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

// Write-only fields are never returned by reads, so drift is detected through
// status.writeOnlyHash: "v1" followed by ";<field>=<hash>" for every write-only field that was
// set when last applied (sorted). "" means unknown (e.g. an adopted resource).

const writeOnlyHashVersion = "v1"

func valueHash(v any) string {
	b, _ := json.Marshal(v) // map keys are sorted: canonical enough for JSON values
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// WriteOnlyHash renders the hash of the write-only fields set in desired.
func WriteOnlyHash(desired map[string]any, writeOnly []string) string {
	parts := []string{writeOnlyHashVersion}
	fields := append([]string(nil), writeOnly...)
	sort.Strings(fields)
	for _, f := range fields {
		if v, ok := desired[f]; ok && v != nil {
			parts = append(parts, f+"="+valueHash(v))
		}
	}
	return strings.Join(parts, ";")
}

// parseWriteOnlyHash returns the per-field hashes; known=false when the hash is absent or of an
// unknown version.
func parseWriteOnlyHash(s string) (fields map[string]string, known bool) {
	parts := strings.Split(s, ";")
	if s == "" || parts[0] != writeOnlyHashVersion {
		return nil, false
	}
	fields = map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			fields[k] = v
		}
	}
	return fields, true
}
