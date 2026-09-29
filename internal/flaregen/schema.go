package flaregen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Kind of a Type in the generator's schema model.
type Kind int

const (
	KString Kind = iota
	KInteger
	KNumber
	KBool
	KObject // fixed set of properties
	KArray
	KMap // additionalProperties with a schema
	// KAny is a value of unknown shape: emitted as x-kubernetes-preserve-unknown-fields
	// (Go: apiextensionsv1.JSON). Last resort, see flattening rules in doc.go.
	KAny
)

func (k Kind) String() string {
	return [...]string{"string", "integer", "number", "boolean", "object", "array", "map", "any"}[k]
}

func (k Kind) scalar() bool { return k <= KBool }

// Type is the generator's structural view of an OpenAPI schema, after
// flattening allOf/oneOf/anyOf.
type Type struct {
	Kind Kind
	// AnyObject marks a KAny known to be a JSON object (type: object, preserve unknown fields).
	AnyObject bool
	Fields    []*Field // KObject, sorted by JSON name
	Elem      *Type    // KArray items, KMap values

	Description string

	// Validations. Kept only in forProvider; atProvider never validates what the API returns.
	Enum             []any
	Pattern          string
	MinLength        *int64
	MaxLength        *int64
	Minimum, Maximum *float64
	ExclusiveMinimum bool
	ExclusiveMaximum bool
	MinItems         *int64
	MaxItems         *int64
}

// Field is one property of a KObject.
type Field struct {
	JSONName    string
	Type        *Type
	Required    bool
	ReadOnly    bool // OpenAPI readOnly: never sent (excluded from forProvider)
	WriteOnly   bool // OpenAPI writeOnly: never returned (excluded from atProvider)
	Deprecated  bool
	Description string
}

// Field returns the property with the JSON name, or nil.
func (t *Type) Field(name string) *Field {
	if t == nil {
		return nil
	}
	for _, f := range t.Fields {
		if f.JSONName == name {
			return f
		}
	}
	return nil
}

func anyType() *Type { return &Type{Kind: KAny} }

// maxDepth caps nesting; deeper schemas become KAny.
const maxDepth = 12

type converter struct {
	stack    map[*openapi3.Schema]bool
	warnings []string
	path     []string
}

func (c *converter) warn(format string, args ...any) {
	c.warnings = append(c.warnings, strings.Join(c.path, ".")+": "+fmt.Sprintf(format, args...))
}

// Convert flattens an OpenAPI schema into a Type. The returned warnings name
// every place that fell back to KAny.
func Convert(ref *openapi3.SchemaRef) (*Type, []string) {
	c := &converter{stack: map[*openapi3.Schema]bool{}}
	t := c.convert(ref, 0)
	return t, c.warnings
}

func (c *converter) convert(ref *openapi3.SchemaRef, depth int) *Type {
	if ref == nil || ref.Value == nil {
		return anyType()
	}
	s := ref.Value
	if s.Always != nil {
		return anyType()
	}
	if c.stack[s] {
		c.warn("recursive schema %s → preserve-unknown-fields", ref.Ref)
		return anyType()
	}
	if depth > maxDepth {
		c.warn("nesting deeper than %d → preserve-unknown-fields", maxDepth)
		return anyType()
	}
	c.stack[s] = true
	defer delete(c.stack, s)

	t := c.base(s, depth)
	for _, sub := range s.AllOf {
		t = c.intersect(t, c.convert(sub, depth))
	}
	if variants := append(append(openapi3.SchemaRefs{}, s.OneOf...), s.AnyOf...); len(variants) > 0 {
		var u *Type
		for _, v := range variants {
			if isNullOnly(v) {
				continue
			}
			vt := c.convert(v, depth)
			if u == nil {
				u = vt
			} else {
				u = c.union(u, vt)
			}
		}
		if u != nil {
			t = c.intersect(t, u)
		}
	}
	if t == nil {
		return anyType()
	}
	if t.Description == "" {
		t.Description = describe(s)
	}
	return t
}

func isNullOnly(ref *openapi3.SchemaRef) bool {
	if ref == nil || ref.Value == nil {
		return false
	}
	s := ref.Value
	if s.Type != nil && len(s.Type.Slice()) == 1 && s.Type.Is("null") {
		return true
	}
	return (s.Type == nil || s.Type.IsEmpty()) && s.Nullable && len(s.Properties) == 0 && len(s.AllOf)+len(s.OneOf)+len(s.AnyOf) == 0 && s.Items == nil
}

func describe(s *openapi3.Schema) string {
	d := strings.TrimSpace(s.Description)
	if d == "" {
		d = strings.TrimSpace(s.Title)
	}
	return d
}

// base converts the schema's own keywords (type, properties, items,
// additionalProperties), ignoring allOf/oneOf/anyOf. It returns nil when the
// schema says nothing about the shape (the identity for intersect).
func (c *converter) base(s *openapi3.Schema, depth int) *Type {
	var types []string
	if s.Type != nil {
		for _, t := range s.Type.Slice() {
			if t != "null" {
				types = append(types, t)
			}
		}
	}
	if len(types) == 2 && contains(types, "integer") && contains(types, "number") {
		types = []string{"number"}
	}
	if len(types) > 1 {
		c.warn("multiple types %v → preserve-unknown-fields", types)
		return anyType()
	}
	typ := ""
	if len(types) == 1 {
		typ = types[0]
	}
	if typ == "" {
		switch {
		case len(s.Properties) > 0:
			typ = "object"
		case s.Items != nil:
			typ = "array"
		case s.AdditionalProperties.Schema != nil:
			typ = "object"
		case len(s.Enum) > 0 || s.Const != nil:
			typ = scalarTypeOf(firstNonNil(append(append([]any{}, s.Enum...), s.Const)))
		}
	}
	var t *Type
	switch typ {
	case "":
		return nil
	case "string", "integer", "number", "boolean":
		t = &Type{Kind: map[string]Kind{"string": KString, "integer": KInteger, "number": KNumber, "boolean": KBool}[typ]}
		for _, e := range s.Enum {
			if e != nil {
				t.Enum = append(t.Enum, e)
			}
		}
		if s.Const != nil {
			t.Enum = []any{s.Const}
		}
		if s.Pattern != "" {
			t.Pattern = s.Pattern
		}
		if s.MinLength > 0 {
			t.MinLength = i64(int64(s.MinLength))
		}
		if s.MaxLength != nil {
			t.MaxLength = i64(int64(*s.MaxLength))
		}
		t.Minimum, t.Maximum = s.Min, s.Max
		t.ExclusiveMinimum = s.ExclusiveMin.IsTrue()
		t.ExclusiveMaximum = s.ExclusiveMax.IsTrue()
		if v := s.ExclusiveMin.Value; v != nil {
			t.Minimum, t.ExclusiveMinimum = v, true
		}
		if v := s.ExclusiveMax.Value; v != nil {
			t.Maximum, t.ExclusiveMaximum = v, true
		}
	case "array":
		t = &Type{Kind: KArray}
		c.path = append(c.path, "[]")
		t.Elem = c.convert(s.Items, depth+1)
		c.path = c.path[:len(c.path)-1]
		if s.MinItems > 0 {
			t.MinItems = i64(int64(s.MinItems))
		}
		if s.MaxItems != nil {
			t.MaxItems = i64(int64(*s.MaxItems))
		}
	case "object":
		switch {
		case len(s.Properties) > 0:
			t = &Type{Kind: KObject}
			req := map[string]bool{}
			for _, r := range s.Required {
				req[r] = true
			}
			for _, name := range sortedKeys(s.Properties) {
				pref := s.Properties[name]
				c.path = append(c.path, name)
				ft := c.convert(pref, depth+1)
				c.path = c.path[:len(c.path)-1]
				f := &Field{JSONName: name, Type: ft, Required: req[name]}
				if v := pref.Value; v != nil {
					f.ReadOnly, f.WriteOnly, f.Deprecated = v.ReadOnly, v.WriteOnly, v.Deprecated
					f.Description = describe(v)
				}
				t.Fields = append(t.Fields, f)
			}
		case s.AdditionalProperties.Schema != nil:
			t = &Type{Kind: KMap}
			c.path = append(c.path, "{}")
			t.Elem = c.convert(s.AdditionalProperties.Schema, depth+1)
			c.path = c.path[:len(c.path)-1]
		default:
			// Free-form object: identity for intersect, but remembers it is an object.
			t = &Type{Kind: KAny, AnyObject: true}
		}
	default:
		c.warn("unknown type %q → preserve-unknown-fields", typ)
		return anyType()
	}
	return t
}

// isFreeObject reports a free-form object (type: object and nothing else).
func isFreeObject(t *Type) bool { return t != nil && t.Kind == KAny && t.AnyObject }

// intersect merges two constraints that both apply (allOf, or a base schema
// combined with its oneOf/anyOf union). nil and free-form objects are identities.
func (c *converter) intersect(a, b *Type) *Type {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case isFreeObject(a) && (b.Kind == KObject || b.Kind == KMap || isFreeObject(b)):
		return keepDesc(b, a)
	case isFreeObject(b) && (a.Kind == KObject || a.Kind == KMap):
		return a
	case a.Kind == KAny && !a.AnyObject:
		return keepDesc(b, a)
	case b.Kind == KAny && !b.AnyObject:
		return a
	}
	if a.Kind != b.Kind {
		if numeric(a.Kind) && numeric(b.Kind) {
			out := *a
			out.Kind = KInteger
			return &out
		}
		c.warn("allOf combines %s and %s → preserve-unknown-fields", a.Kind, b.Kind)
		return anyType()
	}
	out := *a
	switch a.Kind {
	case KObject:
		out.Fields = mergeFields(a.Fields, b.Fields, func(x, y *Type) *Type { return c.intersect(x, y) }, true)
	case KArray, KMap:
		out.Elem = c.intersect(a.Elem, b.Elem)
	default:
		if len(out.Enum) == 0 {
			out.Enum = b.Enum
		}
		if out.Pattern == "" {
			out.Pattern = b.Pattern
		}
		out.MinLength, out.MaxLength = firstI(a.MinLength, b.MinLength), firstI(a.MaxLength, b.MaxLength)
		out.Minimum, out.Maximum = firstF(a.Minimum, b.Minimum), firstF(a.Maximum, b.Maximum)
	}
	if out.Description == "" {
		out.Description = b.Description
	}
	return &out
}

func keepDesc(t, from *Type) *Type {
	if t.Description == "" && from.Description != "" {
		cp := *t
		cp.Description = from.Description
		return &cp
	}
	return t
}

// union merges alternatives (oneOf/anyOf): a value may match either.
func (c *converter) union(a, b *Type) *Type {
	if a.Kind == KAny || b.Kind == KAny {
		if isFreeObject(a) && isFreeObject(b) || isFreeObject(a) && (b.Kind == KObject || b.Kind == KMap) || isFreeObject(b) && (a.Kind == KObject || a.Kind == KMap) {
			return &Type{Kind: KAny, AnyObject: true, Description: a.Description}
		}
		return &Type{Kind: KAny, Description: a.Description}
	}
	if a.Kind != b.Kind {
		if numeric(a.Kind) && numeric(b.Kind) {
			return &Type{Kind: KNumber, Description: a.Description}
		}
		if (a.Kind == KObject && b.Kind == KMap) || (a.Kind == KMap && b.Kind == KObject) {
			c.warn("oneOf mixes object and map → preserve-unknown-fields (object)")
			return &Type{Kind: KAny, AnyObject: true, Description: a.Description}
		}
		c.warn("oneOf/anyOf mixes %s and %s → preserve-unknown-fields", a.Kind, b.Kind)
		return &Type{Kind: KAny, Description: a.Description}
	}
	out := &Type{Kind: a.Kind, Description: a.Description}
	if out.Description == "" {
		out.Description = b.Description
	}
	switch a.Kind {
	case KObject:
		out.Fields = mergeFields(a.Fields, b.Fields, c.union, false)
	case KArray, KMap:
		out.Elem = c.union(a.Elem, b.Elem)
	default:
		if len(a.Enum) > 0 && len(b.Enum) > 0 {
			out.Enum = unionEnum(a.Enum, b.Enum)
		}
		if a.Pattern == b.Pattern {
			out.Pattern = a.Pattern
		}
		if eqI(a.MinLength, b.MinLength) {
			out.MinLength = a.MinLength
		}
		if eqI(a.MaxLength, b.MaxLength) {
			out.MaxLength = a.MaxLength
		}
		if eqF(a.Minimum, b.Minimum) && a.ExclusiveMinimum == b.ExclusiveMinimum {
			out.Minimum, out.ExclusiveMinimum = a.Minimum, a.ExclusiveMinimum
		}
		if eqF(a.Maximum, b.Maximum) && a.ExclusiveMaximum == b.ExclusiveMaximum {
			out.Maximum, out.ExclusiveMaximum = a.Maximum, a.ExclusiveMaximum
		}
	}
	return out
}

// mergeFields merges two property lists. With requiredAll (intersect) a field
// is required if either side requires it; otherwise (union) only if both do.
func mergeFields(a, b []*Field, merge func(x, y *Type) *Type, requiredAny bool) []*Field {
	byName := map[string]*Field{}
	for _, f := range a {
		cp := *f
		if !requiredAny && !hasField(b, f.JSONName) {
			cp.Required = false
		}
		byName[f.JSONName] = &cp
	}
	for _, f := range b {
		ex, ok := byName[f.JSONName]
		if !ok {
			cp := *f
			if !requiredAny {
				cp.Required = false
			}
			byName[f.JSONName] = &cp
			continue
		}
		ex.Type = merge(ex.Type, f.Type)
		if requiredAny {
			ex.Required = ex.Required || f.Required
			ex.ReadOnly = ex.ReadOnly || f.ReadOnly
			ex.WriteOnly = ex.WriteOnly || f.WriteOnly
		} else {
			ex.Required = ex.Required && f.Required
			ex.ReadOnly = ex.ReadOnly && f.ReadOnly
			ex.WriteOnly = ex.WriteOnly && f.WriteOnly
		}
		ex.Deprecated = ex.Deprecated && f.Deprecated
		if ex.Description == "" {
			ex.Description = f.Description
		}
	}
	out := make([]*Field, 0, len(byName))
	for _, k := range sortedKeys(byName) {
		out = append(out, byName[k])
	}
	return out
}

func hasField(fs []*Field, name string) bool {
	for _, f := range fs {
		if f.JSONName == name {
			return true
		}
	}
	return false
}

func unionEnum(a, b []any) []any {
	seen := map[string]bool{}
	var out []any
	for _, v := range append(append([]any{}, a...), b...) {
		k := enumKey(v)
		if !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out
}

func enumKey(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// View selects which half of a schema a CRD field shows.
type View int

const (
	// ViewParameters is spec.forProvider: readOnly fields dropped, validations kept.
	ViewParameters View = iota
	// ViewObservation is status.atProvider: writeOnly fields dropped, validations and
	// required dropped (the API's answer is never rejected).
	ViewObservation
)

// Project returns a copy of t for the view.
func Project(t *Type, v View) *Type {
	if t == nil {
		return nil
	}
	out := *t
	if v == ViewObservation {
		out.Enum, out.Pattern = nil, ""
		out.MinLength, out.MaxLength, out.Minimum, out.Maximum = nil, nil, nil, nil
		out.ExclusiveMinimum, out.ExclusiveMaximum = false, false
		out.MinItems, out.MaxItems = nil, nil
	} else {
		out.Enum = sanitizeEnum(t)
		if out.Pattern != "" && !goRegexpOK(out.Pattern) {
			out.Pattern = "" // ECMA-only regex; the API still validates it
		}
		if out.Kind != KString {
			out.Pattern, out.MinLength, out.MaxLength = "", nil, nil
		}
		if out.Kind != KInteger && out.Kind != KNumber {
			out.Minimum, out.Maximum = nil, nil
		}
	}
	out.Elem = Project(t.Elem, v)
	out.Fields = nil
	for _, f := range t.Fields {
		if (v == ViewParameters && f.ReadOnly) || (v == ViewObservation && f.WriteOnly) {
			continue
		}
		cp := *f
		cp.Type = Project(f.Type, v)
		if v == ViewObservation {
			cp.Required = false
		}
		out.Fields = append(out.Fields, &cp)
	}
	if out.Kind == KObject && len(out.Fields) == 0 {
		return &Type{Kind: KAny, AnyObject: true, Description: out.Description}
	}
	return &out
}

// sanitizeEnum keeps only enum values of the field's own JSON type.
func sanitizeEnum(t *Type) []any {
	var out []any
	for _, e := range t.Enum {
		switch e.(type) {
		case string:
			if t.Kind == KString {
				out = append(out, e)
			}
		case float64, int, int64:
			if t.Kind == KInteger || t.Kind == KNumber {
				out = append(out, e)
			}
		case bool:
			if t.Kind == KBool {
				out = append(out, e)
			}
		}
	}
	return out
}

func goRegexpOK(p string) bool {
	_, err := regexp.Compile(p)
	return err == nil
}

func numeric(k Kind) bool { return k == KInteger || k == KNumber }

func scalarTypeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	}
	return ""
}

func firstNonNil(vs []any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func i64(v int64) *int64 { return &v }

func firstI(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}

func firstF(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}

func eqI(a, b *int64) bool   { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func eqF(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
