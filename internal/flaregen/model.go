package flaregen

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"flare.dev/operator/internal/generic"
)

// Operation is one spec operation annotated with fern SDK names.
type Operation struct {
	FernGroup   string
	FernMethod  string
	Method      string // HTTP method, upper-case
	Path        string // as in the spec
	OperationID string
	Op          *openapi3.Operation
}

// Resource is a group of operations that together manage one Cloudflare object:
// either CRUD (create on a collection, get/update/delete on an item) or a
// singleton (get + update/edit on one fixed path, no create).
type Resource struct {
	FernGroup  string
	Product    string
	Scope      string // "account" | "zone"
	ScopeParam string // the spec's name for the account/zone path parameter
	Singleton  bool

	CollectionPath string // CRUD only
	ItemPath       string
	ItemParam      string // CRUD only, e.g. "namespace_id"

	Create, Get, List, Update, Edit, Delete *Operation

	// Unsupported is why the resource cannot be generated yet ("" if it can).
	Unsupported string
}

// Key identifies the resource for generator.yaml (fern group + path).
func (r *Resource) Key() string {
	if r.Singleton {
		return r.FernGroup + " " + r.ItemPath
	}
	return r.FernGroup + " " + r.CollectionPath
}

// Operations returns all spec operations with a fern group name.
func Operations(doc *openapi3.T) []*Operation {
	var out []*Operation
	for _, p := range doc.Paths.InMatchingOrder() {
		item := doc.Paths.Find(p)
		if item == nil {
			continue
		}
		for method, op := range item.Operations() {
			g, _ := op.Extensions["x-fern-sdk-group-name"].(string)
			m, _ := op.Extensions["x-fern-sdk-method-name"].(string)
			if g == "" || m == "" {
				continue
			}
			out = append(out, &Operation{FernGroup: g, FernMethod: m, Method: strings.ToUpper(method), Path: p, OperationID: op.OperationID, Op: op})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

func pathSegments(p string) []string { return strings.Split(strings.Trim(p, "/"), "/") }

func isParam(seg string) bool { return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") }

func paramName(seg string) string { return strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}") }

// scopeOf classifies a path by its leading /accounts/{..} or /zones/{..}.
func scopeOf(p string) (scope, param string) {
	segs := pathSegments(p)
	if len(segs) >= 2 && isParam(segs[1]) {
		switch segs[0] {
		case "accounts":
			return "account", paramName(segs[1])
		case "zones":
			return "zone", paramName(segs[1])
		}
	}
	return "", ""
}

// Discover builds the resource model of a spec from its fern annotations.
func Discover(doc *openapi3.T) []*Resource {
	byGroup := map[string][]*Operation{}
	for _, op := range Operations(doc) {
		byGroup[op.FernGroup] = append(byGroup[op.FernGroup], op)
	}
	var out []*Resource
	for _, g := range sortedKeys(byGroup) {
		out = append(out, discoverGroup(g, byGroup[g])...)
	}
	return out
}

func find(ops []*Operation, path, method string, fernMethods ...string) *Operation {
	for _, op := range ops {
		if op.Path == path && (method == "" || op.Method == method) && contains(fernMethods, op.FernMethod) {
			return op
		}
	}
	return nil
}

func discoverGroup(group string, ops []*Operation) []*Resource {
	var out []*Resource
	claimed := map[string]bool{}
	for _, c := range ops {
		if c.FernMethod != "create" || c.Method != http.MethodPost {
			continue
		}
		r := &Resource{FernGroup: group, Product: productOf(group), CollectionPath: c.Path, Create: c}
		r.List = find(ops, c.Path, http.MethodGet, "list")
		// Item path: the collection path plus exactly one parameter segment; prefer one with get.
		base := len(pathSegments(c.Path))
		var items []string
		seen := map[string]bool{}
		for _, op := range ops {
			segs := pathSegments(op.Path)
			if strings.HasPrefix(op.Path, c.Path+"/") && len(segs) == base+1 && isParam(segs[base]) && !seen[op.Path] {
				seen[op.Path] = true
				items = append(items, op.Path)
			}
		}
		sort.Strings(items)
		for _, ip := range items {
			if r.ItemPath == "" || (find(ops, ip, http.MethodGet, "get") != nil && r.Get == nil) {
				r.ItemPath = ip
				r.Get = find(ops, ip, http.MethodGet, "get")
			}
		}
		if r.ItemPath != "" {
			segs := pathSegments(r.ItemPath)
			r.ItemParam = paramName(segs[len(segs)-1])
			r.Update = find(ops, r.ItemPath, "", "update")
			r.Edit = find(ops, r.ItemPath, "", "edit")
			r.Delete = find(ops, r.ItemPath, http.MethodDelete, "delete")
			claimed[r.ItemPath] = true
		}
		claimed[c.Path] = true
		r.Scope, r.ScopeParam = scopeOf(c.Path)
		r.Unsupported = crudUnsupported(r)
		out = append(out, r)
	}
	// Singletons: get + update/edit on a path no CRUD resource claimed.
	paths := map[string]bool{}
	for _, op := range ops {
		paths[op.Path] = true
	}
	for _, p := range sortedKeys(paths) {
		if claimed[p] {
			continue
		}
		get := find(ops, p, http.MethodGet, "get")
		upd, edit := find(ops, p, "", "update"), find(ops, p, "", "edit")
		if get == nil || (upd == nil && edit == nil) {
			continue
		}
		if upd != nil && upd.Method == http.MethodPost {
			upd = nil
		}
		if edit != nil && edit.Method == http.MethodPost {
			edit = nil
		}
		if upd == nil && edit == nil {
			continue
		}
		r := &Resource{FernGroup: group, Product: productOf(group), Singleton: true, ItemPath: p, Get: get, Update: upd, Edit: edit}
		r.Scope, r.ScopeParam = scopeOf(p)
		r.Unsupported = singletonUnsupported(r)
		out = append(out, r)
	}
	return out
}

func extraParams(p, allowed1, allowed2 string) []string {
	var extra []string
	for _, s := range pathSegments(p) {
		if isParam(s) && paramName(s) != allowed1 && paramName(s) != allowed2 {
			extra = append(extra, s)
		}
	}
	return extra
}

func crudUnsupported(r *Resource) string {
	switch {
	case r.Scope == "":
		return "path is neither account- nor zone-scoped"
	case r.ItemPath == "":
		return "no item path under the create path"
	case r.Get == nil:
		return "no get operation on the item path"
	case r.Delete == nil:
		return "no delete operation on the item path"
	}
	if extra := extraParams(r.ItemPath, r.ScopeParam, r.ItemParam); len(extra) > 0 {
		return "parent path parameters " + strings.Join(extra, ",") + " (nested resources not supported yet)"
	}
	if why := bodyUnsupported(r.Create); why != "" {
		return why
	}
	for _, op := range []*Operation{r.Update, r.Edit} {
		if why := bodyUnsupported(op); why != "" && op != nil {
			return why
		}
	}
	return ""
}

func singletonUnsupported(r *Resource) string {
	if r.Scope == "" {
		return "path is neither account- nor zone-scoped"
	}
	if segs := pathSegments(r.ItemPath); isParam(segs[len(segs)-1]) && len(segs) > 2 {
		return "item path ends in a parameter but has no create (upsert-style resource)"
	}
	if extra := extraParams(r.ItemPath, r.ScopeParam, ""); len(extra) > 0 {
		return "parent path parameters " + strings.Join(extra, ",") + " (nested resources not supported yet)"
	}
	for _, op := range []*Operation{r.Update, r.Edit} {
		if why := bodyUnsupported(op); why != "" && op != nil {
			return why
		}
	}
	return ""
}

// bodyUnsupported rejects request bodies that are not JSON (multipart uploads).
func bodyUnsupported(op *Operation) string {
	if op == nil || op.Op.RequestBody == nil || op.Op.RequestBody.Value == nil {
		return ""
	}
	c := op.Op.RequestBody.Value.Content
	if len(c) == 0 || c.Get("application/json") != nil {
		return ""
	}
	return fmt.Sprintf("%s %s has a non-JSON request body", op.Method, op.Path)
}

func requestSchema(op *Operation) *openapi3.SchemaRef {
	if op == nil || op.Op.RequestBody == nil || op.Op.RequestBody.Value == nil {
		return nil
	}
	if mt := op.Op.RequestBody.Value.Content.Get("application/json"); mt != nil {
		return mt.Schema
	}
	return nil
}

func responseSchema(op *Operation) *openapi3.SchemaRef {
	if op == nil || op.Op.Responses == nil {
		return nil
	}
	for _, code := range []int{200, 201, 202} {
		if rr := op.Op.Responses.Status(code); rr != nil && rr.Value != nil {
			if mt := rr.Value.Content.Get("application/json"); mt != nil {
				return mt.Schema
			}
		}
	}
	return nil
}

// resultType returns the "result" member of a v4 envelope schema (or the whole
// schema for unwrapped responses).
func resultType(t *Type) *Type {
	if f := t.Field("result"); f != nil && t.Field("success") != nil {
		return f.Type
	}
	if f := t.Field("result"); f != nil && len(t.Fields) <= 4 {
		return f.Type
	}
	return t
}

var (
	kindRe  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	labelRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

// KindModel is everything the emitters need for one kind.
type KindModel struct {
	Resource    *Resource
	Kind        string
	Plural      string
	Product     string
	Group       string
	Version     string
	Params      *Type // spec.forProvider (ViewParameters)
	Observation *Type // status.atProvider (ViewObservation)

	// Descriptor carries CreateFields/UpdateFields: the top-level forProvider
	// fields the create and update bodies accept.
	Descriptor generic.Descriptor
	// TagResourceType is the Resource Tagging resource_type (generator.yaml).
	TagResourceType string
	// CreateRequired are the top-level forProvider fields the create body
	// requires; required (by a CEL rule) only when the object may create.
	CreateRequired []string

	Warnings []string
}

func objectOrEmpty(t *Type) *Type {
	if t == nil || t.Kind != KObject {
		return &Type{Kind: KObject}
	}
	return t
}

func topNames(t *Type) []string {
	var out []string
	if t == nil {
		return out
	}
	for _, f := range t.Fields {
		out = append(out, f.JSONName)
	}
	return out
}

// BuildKind turns a Resource plus its generator.yaml entry into a KindModel.
func BuildKind(r *Resource, kc KindConfig, groupSuffix, version string) (*KindModel, error) {
	if r.Unsupported != "" {
		return nil, fmt.Errorf("%s: unsupported: %s", r.Key(), r.Unsupported)
	}
	m := &KindModel{Resource: r, Version: version}
	m.Kind = kc.Kind
	if m.Kind == "" {
		m.Kind = defaultKind(r.FernGroup)
	}
	m.Product = kc.Group
	if m.Product == "" {
		m.Product = r.Product
	}
	m.Plural = kc.Plural
	if m.Plural == "" {
		m.Plural = plural(m.Kind)
	}
	m.Group = m.Product + "." + groupSuffix
	switch {
	case !kindRe.MatchString(m.Kind):
		return nil, fmt.Errorf("%s: kind %q is not an exported Go identifier; set kind", r.Key(), m.Kind)
	case !labelRe.MatchString(m.Product):
		return nil, fmt.Errorf("%s: group %q must match %s; set group", r.Key(), m.Product, labelRe)
	case !labelRe.MatchString(m.Plural):
		return nil, fmt.Errorf("%s: plural %q must match %s; set plural", r.Key(), m.Plural, labelRe)
	}

	// Update method: PATCH (edit) is preferred because it never resets fields
	// the object does not mention; PUT is used when it is the only one.
	var upd *Operation
	method := kc.UpdateMethod
	if method == "" {
		switch {
		case r.Edit != nil && r.Edit.Method == http.MethodPatch:
			method = http.MethodPatch
		case r.Update != nil:
			method = r.Update.Method
		case r.Edit != nil:
			method = r.Edit.Method
		}
	}
	if method == "-" {
		method = ""
	}
	if method != "" {
		for _, op := range []*Operation{r.Edit, r.Update} {
			if op != nil && op.Method == method {
				upd = op
				break
			}
		}
		if upd == nil {
			return nil, fmt.Errorf("%s: updateMethod %s: no update/edit operation uses it", r.Key(), method)
		}
	}

	var warns []string
	conv := func(ref *openapi3.SchemaRef, what string) *Type {
		if ref == nil {
			return nil
		}
		t, w := Convert(ref)
		for _, x := range w {
			warns = append(warns, what+": "+x)
		}
		return t
	}
	createT := conv(requestSchema(r.Create), "create body")
	updT := conv(requestSchema(upd), "update body")
	var getT *Type
	if r.Get != nil {
		if t := conv(responseSchema(r.Get), "get response"); t != nil {
			getT = resultType(t)
		}
	}
	if getT == nil && r.Create != nil {
		if t := conv(responseSchema(r.Create), "create response"); t != nil {
			getT = resultType(t)
		}
	}
	createT, updT, getT = objectOrEmpty(createT), objectOrEmpty(updT), objectOrEmpty(getT)

	// forProvider = create body ∪ update body; top-level required only as the
	// create body says (a singleton has no create: nothing is required).
	c := &converter{stack: map[*openapi3.Schema]bool{}}
	params := c.intersect(createT, updT)
	params = objectOrEmpty(params)
	// Top-level fields the create body requires are not required by the
	// schema: an object that cannot create (managementPolicies without Create,
	// e.g. ["Observe"]) never sends them. BuildCRD enforces them with a CEL rule
	// on spec instead (CreateRequired).
	var createRequired []string
	for _, f := range params.Fields {
		cf := createT.Field(f.JSONName)
		if !r.Singleton && cf != nil && cf.Required && !cf.ReadOnly {
			createRequired = append(createRequired, f.JSONName)
		}
		f.Required = false
	}
	obs := getT
	if err := applyFieldOverrides(kc, params, obs); err != nil {
		return nil, fmt.Errorf("%s: %w", r.Key(), err)
	}
	m.Params = objectOrEmpty(Project(params, ViewParameters))
	for _, n := range createRequired {
		if m.Params.Field(n) != nil {
			m.CreateRequired = append(m.CreateRequired, n)
		}
	}
	m.Observation = objectOrEmpty(Project(obs, ViewObservation))
	m.Warnings = warns

	var createFields, updateFields []string
	for _, f := range Project(createT, ViewParameters).Fields {
		createFields = append(createFields, f.JSONName)
	}
	if upd != nil {
		for _, f := range objectOrEmpty(Project(updT, ViewParameters)).Fields {
			updateFields = append(updateFields, f.JSONName)
		}
	}
	if r.Singleton {
		createFields = nil
	}

	d := generic.Descriptor{
		Group: m.Group, Version: version, Kind: m.Kind, Scope: r.Scope,
		UpdateMethod: method, Singleton: r.Singleton, ListOrder: kc.ListOrder,
	}
	rewrite := func(p string) string {
		scopeName := map[string]string{"account": "account_id", "zone": "zone_id"}[r.Scope]
		p = strings.ReplaceAll(p, "{"+r.ScopeParam+"}", "{"+scopeName+"}")
		if r.ItemParam != "" {
			p = strings.ReplaceAll(p, "{"+r.ItemParam+"}", "{id}")
		}
		return p
	}
	d.ItemPath = rewrite(r.ItemPath)
	if !r.Singleton {
		d.CreatePath = rewrite(r.CollectionPath)
		if r.List != nil {
			d.ListPath = rewrite(r.List.Path)
		}
	}

	// ID field.
	if !r.Singleton {
		d.IDField = kc.IDField
		if d.IDField == "" {
			sing := singular(lastSegment(r.FernGroup))
			for _, cand := range []string{r.ItemParam, "id", "uuid", sing + "_id", "tag"} {
				if m.Observation.Field(cand) != nil {
					d.IDField = cand
					break
				}
			}
		}
		if d.IDField == "" {
			return nil, fmt.Errorf("%s: cannot derive the ID field from the get response; set idField", r.Key())
		}
	}
	// Name field (adoption by name).
	switch kc.NameField {
	case "-":
	case "":
		if !r.Singleton {
			sing := singular(lastSegment(r.FernGroup))
			for _, cand := range []string{"name", sing + "_name", "title"} {
				if m.Params.Field(cand) != nil {
					d.NameField = cand
					break
				}
			}
		}
	default:
		if m.Params.Field(kc.NameField) == nil {
			return nil, fmt.Errorf("%s: nameField %q is not a forProvider field", r.Key(), kc.NameField)
		}
		d.NameField = kc.NameField
	}

	// Immutable: create-only fields, plus overrides.
	imm := map[string]bool{}
	if !r.Singleton {
		for _, n := range createFields {
			if upd == nil || !contains(updateFields, n) {
				imm[n] = true
			}
		}
	}
	for _, n := range kc.Immutable {
		if m.Params.Field(n) == nil {
			return nil, fmt.Errorf("%s: immutable %q is not a forProvider field", r.Key(), n)
		}
		imm[n] = true
	}
	d.Immutable = sortedKeys(imm)
	var updFields []string
	for _, n := range updateFields {
		if !imm[n] {
			updFields = append(updFields, n)
		}
	}
	d.CreateFields, d.UpdateFields = createFields, updFields

	// WriteOnly: forProvider fields the get response never returns, plus overrides.
	wo := map[string]bool{}
	for _, f := range m.Params.Fields {
		if m.Observation.Field(f.JSONName) == nil {
			wo[f.JSONName] = true
		}
	}
	for _, n := range kc.WriteOnly {
		if m.Params.Field(n) == nil {
			return nil, fmt.Errorf("%s: writeOnly %q is not a forProvider field", r.Key(), n)
		}
		wo[n] = true
	}
	for _, n := range kc.NotWriteOnly {
		if !wo[n] {
			return nil, fmt.Errorf("%s: notWriteOnly %q is not a derived write-only field", r.Key(), n)
		}
		delete(wo, n)
	}
	d.WriteOnly = sortedKeys(wo)

	d.DefaultDeletionPolicy = kc.DefaultDeletionPolicy
	if d.DefaultDeletionPolicy == "" {
		d.DefaultDeletionPolicy = "Delete"
		if r.Singleton {
			d.DefaultDeletionPolicy = "Orphan"
		}
	}
	m.Descriptor = d
	m.TagResourceType = kc.TagResourceType
	return m, nil
}

func lastSegment(fernGroup string) string {
	segs := strings.Split(fernGroup, ".")
	return segs[len(segs)-1]
}

// applyFieldOverrides applies generator.yaml field overrides to every tree
// in which the path exists; a path found nowhere is an error (typo guard).
func applyFieldOverrides(kc KindConfig, trees ...*Type) error {
	for _, p := range sortedKeys(kc.Fields) {
		o := kc.Fields[p]
		found := false
		for _, t := range trees {
			if ft := lookupPath(t, strings.Split(p, ".")); ft != nil {
				found = true
				if o.Type != "" {
					ft.Kind = map[string]Kind{"string": KString, "integer": KInteger, "number": KNumber, "boolean": KBool}[o.Type]
					ft.AnyObject, ft.Fields, ft.Elem = false, nil, nil
				}
				if o.DropEnum {
					ft.Enum = nil
				}
				if len(o.Enum) > 0 {
					ft.Enum = append([]any(nil), o.Enum...)
				}
			}
		}
		if !found {
			return fmt.Errorf("fields.%s: no such field", p)
		}
	}
	return nil
}

// lookupPath walks object fields, stepping through array items and map values.
func lookupPath(t *Type, path []string) *Type {
	for t != nil && (t.Kind == KArray || t.Kind == KMap) {
		t = t.Elem
	}
	if t == nil {
		return nil
	}
	if len(path) == 0 {
		return t
	}
	f := t.Field(path[0])
	if f == nil {
		return nil
	}
	return lookupPath(f.Type, path[1:])
}
