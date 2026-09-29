// Package sdkdiff drives cloudflare-go (pinned in this module's go.mod) against an in-process
// flarefake. It is a separate module so the operator's go.mod stays free of the SDK; see
// docs/differential-testing.md.
package sdkdiff

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// FieldReport says how cloudflare-go's decoder saw one response. cloudflare-go decodes
// leniently: a missing or mistyped field does not return an error, it only leaves the Go field
// zero and marks it in the struct's JSON metadata (apijson.Field: IsMissing, IsNull,
// IsInvalid; cloudflare-go@v7.11.0:internal/apijson/field.go#L22-L24). FieldReport walks that
// metadata so the tests can see what "decodes without error" hides.
type FieldReport struct {
	// Invalid: present but not decodable into the SDK's Go type (always a discrepancy).
	Invalid []string
	// RequiredMissing: fields tagged api:"required" that were absent or null.
	RequiredMissing []string
	// OptionalMissing: other fields the SDK exposes that the response left out.
	OptionalMissing []string
	// Extra: response fields the SDK does not know (renamed, or newer than the SDK).
	Extra []string
}

// Problems returns the entries that make a response unusable for the SDK's callers.
func (r FieldReport) Problems() []string {
	var out []string
	for _, f := range r.Invalid {
		out = append(out, "invalid: "+f)
	}
	for _, f := range r.RequiredMissing {
		out = append(out, "required but missing/null: "+f)
	}
	return out
}

func (r FieldReport) String() string {
	return fmt.Sprintf("invalid=%v required-missing=%v optional-missing=%v extra=%v",
		r.Invalid, r.RequiredMissing, r.OptionalMissing, r.Extra)
}

// Inspect builds the FieldReport of v, a decoded cloudflare-go response (pointer, struct or
// slice of them).
func Inspect(v any) FieldReport {
	var r FieldReport
	inspect(reflect.ValueOf(v), "", &r)
	sort.Strings(r.Invalid)
	sort.Strings(r.RequiredMissing)
	sort.Strings(r.OptionalMissing)
	sort.Strings(r.Extra)
	return r
}

func inspect(v reflect.Value, prefix string, r *FieldReport) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			inspect(v.Index(i), fmt.Sprintf("%s[%d]", prefix, i), r)
		}
		return
	case reflect.Struct:
	default:
		return
	}
	meta := v.FieldByName("JSON")
	if !meta.IsValid() || meta.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.Name == "JSON" || !sf.IsExported() {
			continue
		}
		name := jsonName(sf)
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		mf := meta.FieldByName(sf.Name)
		if !mf.IsValid() {
			continue
		}
		missing, null, invalid := call(mf, "IsMissing"), call(mf, "IsNull"), call(mf, "IsInvalid")
		required := strings.Contains(sf.Tag.Get("api"), "required")
		switch {
		case invalid:
			r.Invalid = append(r.Invalid, path+" (raw "+rawOf(mf)+")")
		case missing || null:
			if required {
				r.RequiredMissing = append(r.RequiredMissing, path)
			} else if missing {
				r.OptionalMissing = append(r.OptionalMissing, path)
			}
		default:
			inspect(v.Field(i), path, r)
		}
	}
	if extra := meta.FieldByName("ExtraFields"); extra.IsValid() && extra.Kind() == reflect.Map {
		for _, k := range extra.MapKeys() {
			p := k.String()
			if prefix != "" {
				p = prefix + "." + p
			}
			r.Extra = append(r.Extra, p)
		}
	}
}

func jsonName(sf reflect.StructField) string {
	tag := sf.Tag.Get("json")
	if tag == "" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

func call(v reflect.Value, method string) bool {
	m := v.MethodByName(method)
	if !m.IsValid() {
		return false
	}
	return m.Call(nil)[0].Bool()
}

func rawOf(v reflect.Value) string {
	m := v.MethodByName("Raw")
	if !m.IsValid() {
		return "?"
	}
	s := m.Call(nil)[0].String()
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
