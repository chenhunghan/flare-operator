package fake

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

// Schema side of the generic profile (generic.go): a flattened view of the pinned spec's
// schemas, enough to shape emulated results. Every behavior derived here is UNVERIFIED: it
// follows what the spec says, not what a recording shows (see generic.go).

// gSchema is an OpenAPI schema with allOf merged and oneOf/anyOf unioned (the same flattening
// idea as internal/flaregen, simplified: only what shaping a result needs).
type gSchema struct {
	typ      string // object | array | string | integer | number | boolean | "" (unknown)
	props    map[string]*gSchema
	required map[string]bool
	items    *gSchema
	format   string
	example  any
	def      any
	hasDef   bool
	enum     []any
	min      *float64

	readOnly, writeOnly, nullable bool
}

const gMaxDepth = 12

// flattenSchema converts a spec schema. Recursive schemas stop at the point of recursion
// (an unknown-typed node), as do schemas nested deeper than gMaxDepth.
func flattenSchema(ref *openapi3.SchemaRef) *gSchema {
	return flattenRec(ref, map[*openapi3.Schema]bool{}, 0)
}

func flattenRec(ref *openapi3.SchemaRef, stack map[*openapi3.Schema]bool, depth int) *gSchema {
	if ref == nil || ref.Value == nil || depth > gMaxDepth || stack[ref.Value] {
		return &gSchema{}
	}
	s := ref.Value
	stack[s] = true
	defer delete(stack, s)

	out := &gSchema{
		format: s.Format, example: s.Example, enum: s.Enum, min: s.Min,
		readOnly: s.ReadOnly, writeOnly: s.WriteOnly, nullable: s.Nullable,
	}
	if s.Default != nil {
		out.def, out.hasDef = s.Default, true
	}
	if out.example == nil && len(s.Examples) > 0 {
		out.example = s.Examples[0]
	}
	if s.Type != nil {
		for _, t := range s.Type.Slice() {
			if t == "null" {
				out.nullable = true
				continue
			}
			if out.typ == "" {
				out.typ = t
			}
		}
	}
	if len(s.Properties) > 0 {
		out.typ = "object"
		out.props = map[string]*gSchema{}
		for name, p := range s.Properties {
			out.props[name] = flattenRec(p, stack, depth+1)
		}
		out.required = map[string]bool{}
		for _, r := range s.Required {
			out.required[r] = true
		}
	} else if len(s.Required) > 0 {
		out.required = map[string]bool{}
		for _, r := range s.Required {
			out.required[r] = true
		}
	}
	if s.Items != nil {
		out.items = flattenRec(s.Items, stack, depth+1)
		if out.typ == "" {
			out.typ = "array"
		}
	}
	for _, m := range s.AllOf {
		out = mergeAll(out, flattenRec(m, stack, depth+1))
	}
	var union []*gSchema
	for _, m := range append(append(openapi3.SchemaRefs(nil), s.OneOf...), s.AnyOf...) {
		f := flattenRec(m, stack, depth+1)
		if f.typ == "" && len(f.props) == 0 && f.nullable {
			continue // a null-only member
		}
		union = append(union, f)
	}
	if len(union) > 0 {
		out = mergeAll(out, unionOf(union))
	}
	return out
}

// mergeAll intersects two schemas that both apply (allOf): properties are merged, a property
// is required if either requires it, scalar facts come from the first that has them.
func mergeAll(a, b *gSchema) *gSchema {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := *a
	if out.typ == "" {
		out.typ = b.typ
	}
	if out.format == "" {
		out.format = b.format
	}
	if out.example == nil {
		out.example = b.example
	}
	if !out.hasDef && b.hasDef {
		out.def, out.hasDef = b.def, true
	}
	if out.enum == nil {
		out.enum = b.enum
	}
	if out.min == nil {
		out.min = b.min
	}
	out.readOnly = out.readOnly || b.readOnly
	out.writeOnly = out.writeOnly || b.writeOnly
	out.nullable = out.nullable || b.nullable
	if out.items == nil {
		out.items = b.items
	} else if b.items != nil {
		out.items = mergeAll(out.items, b.items)
	}
	if len(b.props) > 0 {
		props := map[string]*gSchema{}
		for k, v := range a.props {
			props[k] = v
		}
		for k, v := range b.props {
			props[k] = mergeAll(props[k], v)
		}
		out.props = props
		out.typ = "object"
	}
	if len(b.required) > 0 {
		req := map[string]bool{}
		for k := range a.required {
			req[k] = true
		}
		for k := range b.required {
			req[k] = true
		}
		out.required = req
	}
	return &out
}

// unionOf merges alternatives (oneOf/anyOf): every member's properties, required only when
// every member requires it; enums are unioned.
func unionOf(ms []*gSchema) *gSchema {
	out := &gSchema{}
	var reqCount map[string]int
	sameType := true
	for i, m := range ms {
		if i == 0 {
			out.typ = m.typ
		} else if m.typ != out.typ {
			sameType = false
		}
		if out.format == "" {
			out.format = m.format
		}
		if out.example == nil {
			out.example = m.example
		}
		out.nullable = out.nullable || m.nullable
		out.enum = append(out.enum, m.enum...)
		if m.items != nil {
			out.items = mergeAll(out.items, m.items)
		}
		if len(m.props) > 0 {
			if out.props == nil {
				out.props = map[string]*gSchema{}
			}
			for k, v := range m.props {
				out.props[k] = mergeAll(out.props[k], v)
			}
		}
		if reqCount == nil {
			reqCount = map[string]int{}
		}
		for k := range m.required {
			reqCount[k]++
		}
	}
	if !sameType {
		out.typ = ""
		if len(out.props) > 0 {
			out.typ = "object"
		}
	}
	for k, n := range reqCount {
		if n == len(ms) {
			if out.required == nil {
				out.required = map[string]bool{}
			}
			out.required[k] = true
		}
	}
	return out
}

// resultOf returns the "result" member of a v4 envelope schema (or the schema itself for
// unwrapped responses), and the result_info member when there is one.
func resultOf(s *gSchema) (result, info *gSchema) {
	if s == nil {
		return nil, nil
	}
	if r, ok := s.props["result"]; ok && (s.props["success"] != nil || len(s.props) <= 4) {
		return r, s.props["result_info"]
	}
	return s, nil
}

func requestBodySchema(op *openapi3.Operation) *gSchema {
	if op == nil || op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	if mt := op.RequestBody.Value.Content.Get("application/json"); mt != nil {
		return flattenSchema(mt.Schema)
	}
	return nil
}

func responseResult(op *openapi3.Operation) (result, info *gSchema) {
	if op == nil || op.Responses == nil {
		return nil, nil
	}
	for _, code := range []int{200, 201, 202} {
		if rr := op.Responses.Status(code); rr != nil && rr.Value != nil {
			if mt := rr.Value.Content.Get("application/json"); mt != nil {
				return resultOf(flattenSchema(mt.Schema))
			}
		}
	}
	return nil, nil
}

// queryDefault returns the default of an operation's integer query parameter (0 = none).
func queryDefault(op *openapi3.Operation, name string) int {
	if op == nil {
		return 0
	}
	for _, p := range op.Parameters {
		if p == nil || p.Value == nil || p.Value.In != "query" || p.Value.Name != name || p.Value.Schema == nil || p.Value.Schema.Value == nil {
			continue
		}
		if d, ok := p.Value.Schema.Value.Default.(float64); ok && d > 0 {
			return int(d)
		}
	}
	return 0
}

func hasQueryParam(op *openapi3.Operation, name string) bool {
	if op == nil {
		return false
	}
	for _, p := range op.Parameters {
		if p != nil && p.Value != nil && p.Value.In == "query" && p.Value.Name == name {
			return true
		}
	}
	return false
}

var paramRe = regexp.MustCompile(`\{[^}]*\}`)

// specPathItem finds the spec path whose template equals a descriptor path up to parameter
// names ({account_id}/{id} in descriptors, {account_id}/{index_name} in the spec).
func specPathItem(doc *openapi3.T, descPath string) *openapi3.PathItem {
	if doc == nil || doc.Paths == nil || descPath == "" {
		return nil
	}
	want := paramRe.ReplaceAllString(strings.TrimSuffix(descPath, "/"), "{}")
	keys := make([]string, 0, doc.Paths.Len())
	for k := range doc.Paths.Map() {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic if two templates normalize alike
	for _, k := range keys {
		if paramRe.ReplaceAllString(strings.TrimSuffix(k, "/"), "{}") == want {
			return doc.Paths.Value(k)
		}
	}
	return nil
}

// validate checks a request against the spec like Spec.ValidateRequest, optionally without
// the body: the Descriptor contract says DELETE never sends one, even where the spec
// declares a required DELETE body (a known spec defect, e.g. KV 0013).
func (s *Spec) validate(r *http.Request, body []byte, excludeBody bool) error {
	clone := r.Clone(context.Background())
	clone.Body = io.NopCloser(strings.NewReader(string(body)))
	route, params, err := s.router.FindRoute(clone)
	if err != nil {
		return fmt.Errorf("no such operation in pinned spec: %w", err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request: clone, PathParams: params, Route: route,
		Options: &openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			MultiError:         true,
			ExcludeRequestBody: excludeBody,
		},
	}
	return openapi3filter.ValidateRequest(context.Background(), in)
}

// ---- shaping results -------------------------------------------------------------------------

// tsRole says whether a property is a server-set timestamp.
type tsRole int

const (
	tsNone tsRole = iota
	tsCreated
	tsModified
)

// timestampRole classifies date-time properties by name (created_on, created_at, created,
// modified_on, modified_at, modified, updated_at, last_modified, …). UNVERIFIED heuristic.
func timestampRole(name string, s *gSchema) tsRole {
	if s == nil || (s.typ != "" && s.typ != "string") {
		return tsNone
	}
	n := strings.ToLower(name)
	named := strings.HasPrefix(n, "created") || n == "create_time" ||
		strings.HasPrefix(n, "modified") || strings.HasPrefix(n, "updated") ||
		strings.HasPrefix(n, "last_modified") || strings.HasPrefix(n, "last_updated")
	if !named || (s.format != "" && s.format != "date-time") {
		return tsNone
	}
	if strings.HasPrefix(n, "created") || n == "create_time" {
		return tsCreated
	}
	return tsModified
}

var fracRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.(\d+))?Z$`)

// formatTimestamp renders t with the sub-second precision of the spec's example (microseconds
// when the example has none to go by; UNVERIFIED).
func formatTimestamp(t time.Time, s *gSchema) string {
	digits := 6
	if ex, ok := s.example.(string); ok {
		if m := fracRe.FindStringSubmatch(ex); m != nil {
			digits = len(m[2])
		}
	}
	layout := "2006-01-02T15:04:05"
	if digits > 0 {
		layout += "." + strings.Repeat("0", digits)
	}
	return t.UTC().Format(layout + "Z")
}

// shapeCtx carries the server-side values shaping fills in.
type shapeCtx struct {
	now       time.Time
	created   time.Time
	accountID string
	// fill: add defaults, server-set timestamps and required zero values for absent
	// properties (a new or replaced object); false only projects existing values.
	fill bool
}

// shape projects a value onto a response schema: properties the schema has are kept (writeOnly
// ones dropped), others are dropped, unless the schema declares no properties (free-form
// objects keep everything). With ctx.fill, absent properties get their spec default, server
// timestamps, or (when required) a type-appropriate zero value.
func shape(s *gSchema, in any, ctx shapeCtx) any {
	if s == nil {
		return deepCopyJSON(in)
	}
	switch x := in.(type) {
	case map[string]any:
		if len(s.props) == 0 {
			return deepCopyJSON(x)
		}
		return shapeObject(s, x, ctx)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = shape(s.items, el, ctx)
		}
		return out
	case nil:
		if s.typ == "object" && len(s.props) > 0 && ctx.fill && !s.nullable {
			return shapeObject(s, map[string]any{}, ctx)
		}
		return nil
	default:
		return x
	}
}

func shapeObject(s *gSchema, in map[string]any, ctx shapeCtx) map[string]any {
	out := map[string]any{}
	for k, ps := range s.props {
		if ps.writeOnly {
			continue
		}
		if role := timestampRole(k, ps); role != tsNone && ctx.fill {
			t := ctx.now
			if role == tsCreated && !ctx.created.IsZero() {
				t = ctx.created
			}
			out[k] = formatTimestamp(t, ps)
			continue
		}
		if v, ok := in[k]; ok {
			out[k] = shape(ps, v, ctx)
			continue
		}
		if !ctx.fill {
			continue
		}
		switch {
		case k == "account_id" && ps.typ == "string" && ctx.accountID != "":
			out[k] = ctx.accountID
		case ps.hasDef:
			out[k] = deepCopyJSON(ps.def)
		case s.required[k]:
			out[k] = zeroValue(ps, ctx)
		}
	}
	return out
}

// zeroValue is what an absent required property reads back as (UNVERIFIED).
func zeroValue(s *gSchema, ctx shapeCtx) any {
	switch {
	case s.hasDef:
		return deepCopyJSON(s.def)
	case s.nullable:
		return nil
	case len(s.enum) > 0:
		return s.enum[0]
	}
	switch s.typ {
	case "object":
		return shapeObject(s, map[string]any{}, ctx)
	case "array":
		return []any{}
	case "string":
		return ""
	case "integer", "number":
		if s.min != nil && *s.min > 0 {
			return *s.min
		}
		return 0
	case "boolean":
		return false
	}
	return nil
}

func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopyJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyJSON(e)
		}
		return out
	}
	return v
}

// mergePatch applies a JSON merge patch (RFC 7386: objects merge recursively, null deletes).
// PATCH semantics of the generic profile; UNVERIFIED per product (Queues' settings merge is
// recorded in 0032).
func mergePatch(dst map[string]any, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		pm, isObj := v.(map[string]any)
		if dm, ok := dst[k].(map[string]any); ok && isObj {
			mergePatch(dm, pm)
			continue
		}
		dst[k] = deepCopyJSON(v)
	}
}

// deletePath removes a dotted path (settings.delivery_paused) from an object.
func deletePath(m map[string]any, p string) {
	segs := strings.Split(p, ".")
	for _, s := range segs[:len(segs)-1] {
		next, ok := m[s].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, segs[len(segs)-1])
}

// ---- IDs -------------------------------------------------------------------------------------

var (
	uuidExRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-([0-9a-fA-F])[0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexExRe  = regexp.MustCompile(`^[0-9a-f]+$`)
)

// idGenerator derives a server-assigned ID's shape from the ID property's schema: format uuid
// or a UUID example → UUID (v7 when the example is one); a lower-case hex example → hex of
// that length (32 for KV/Queues-style IDs); another example → a random string with the
// example's character classes; nothing to go by → 32 hex characters. UNVERIFIED per kind.
func idGenerator(s *gSchema) func(time.Time) string {
	ex, _ := s.example.(string)
	switch {
	case s.format == "uuid":
		return func(time.Time) string { return uuidV4() }
	case uuidExRe.MatchString(ex):
		if m := uuidExRe.FindStringSubmatch(ex); m[1] == "7" {
			return uuidV7
		}
		return func(time.Time) string { return uuidV4() }
	case ex != "" && hexExRe.MatchString(ex) && len(ex)%2 == 0:
		n := len(ex) / 2
		return func(time.Time) string { return hex.EncodeToString(randBytes(n)) }
	case ex != "":
		return func(time.Time) string { return likeExample(ex) }
	}
	return func(time.Time) string { return hex32() }
}

// likeExample returns a random string with the character classes of ex (digits, lower- and
// upper-case letters replaced, everything else kept).
func likeExample(ex string) string {
	const lower, upper, digits = "abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "0123456789"
	rb := randBytes(len(ex))
	out := []byte(ex)
	for i := range out {
		switch c := out[i]; {
		case c >= 'a' && c <= 'z':
			out[i] = lower[int(rb[i])%len(lower)]
		case c >= 'A' && c <= 'Z':
			out[i] = upper[int(rb[i])%len(upper)]
		case c >= '0' && c <= '9':
			out[i] = digits[int(rb[i])%len(digits)]
		}
	}
	return string(out)
}
