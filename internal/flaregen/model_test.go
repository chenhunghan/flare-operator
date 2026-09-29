package flaregen

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func loadFragment(t *testing.T) *openapi3.T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fragment.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func resourceByKey(t *testing.T, rs []*Resource, key string) *Resource {
	t.Helper()
	for _, r := range rs {
		if r.Key() == key {
			return r
		}
	}
	var keys []string
	for _, r := range rs {
		keys = append(keys, r.Key())
	}
	t.Fatalf("no resource %q in %v", key, keys)
	return nil
}

func TestDiscover(t *testing.T) {
	rs := Discover(loadFragment(t))
	cases := []struct {
		key         string
		singleton   bool
		scope       string
		unsupported string // substring; "" means supported
	}{
		{"widgets.gadgets /accounts/{account_id}/widgets", false, "account", ""},
		{"widget_tools.settings /zones/{zone_identifier}/widget_settings", true, "zone", ""},
		{"widgets.parts /accounts/{account_id}/widgets/{widget_id}/parts", false, "account", "parent path parameters {widget_id}"},
		{"widgets.uploads /accounts/{account_id}/uploads", false, "account", "non-JSON request body"},
		{"widgets.gizmos /user/gizmos", false, "", "neither account- nor zone-scoped"},
	}
	if len(rs) != len(cases) {
		t.Errorf("Discover found %d resources, want %d", len(rs), len(cases))
	}
	for _, c := range cases {
		r := resourceByKey(t, rs, c.key)
		if r.Singleton != c.singleton || r.Scope != c.scope {
			t.Errorf("%s: singleton=%v scope=%q, want %v %q", c.key, r.Singleton, r.Scope, c.singleton, c.scope)
		}
		if c.unsupported == "" && r.Unsupported != "" || c.unsupported != "" && !strings.Contains(r.Unsupported, c.unsupported) {
			t.Errorf("%s: Unsupported=%q, want %q", c.key, r.Unsupported, c.unsupported)
		}
	}
	w := resourceByKey(t, rs, "widgets.gadgets /accounts/{account_id}/widgets")
	if w.ItemParam != "widget_id" || w.Get == nil || w.List == nil || w.Edit == nil || w.Update == nil || w.Delete == nil {
		t.Errorf("widget operations not all found: %+v", w)
	}
	if w.Product != "widgets" {
		t.Errorf("product = %q", w.Product)
	}
	if s := resourceByKey(t, rs, "widget_tools.settings /zones/{zone_identifier}/widget_settings"); s.Product != "widgettools" {
		t.Errorf("product of widget_tools = %q, want widgettools", s.Product)
	}
}

func buildWidget(t *testing.T, kc KindConfig) *KindModel {
	t.Helper()
	r := resourceByKey(t, Discover(loadFragment(t)), "widgets.gadgets /accounts/{account_id}/widgets")
	kc.FernGroup = r.FernGroup
	m, err := BuildKind(r, kc, "cloudflare.flare.dev", "v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildKindDescriptor(t *testing.T) {
	m := buildWidget(t, KindConfig{})
	d := m.Descriptor
	want := map[string]any{
		"Group": "widgets.cloudflare.flare.dev", "Kind": "Gadget", "Scope": "account",
		"CreatePath": "/accounts/{account_id}/widgets", "ItemPath": "/accounts/{account_id}/widgets/{id}",
		"ListPath": "/accounts/{account_id}/widgets", "IDField": "id", "NameField": "name",
		"UpdateMethod": "PATCH", "DefaultDeletionPolicy": "Delete", "Singleton": false,
		// region is create-only; the PUT body would accept it, but PATCH is preferred.
		"Immutable": []string{"region"},
		// region is never read back; secret is writeOnly in the spec.
		"WriteOnly": []string{"region", "secret"},
	}
	got := reflect.ValueOf(d)
	for f, w := range want {
		if g := got.FieldByName(f).Interface(); !reflect.DeepEqual(g, w) {
			t.Errorf("Descriptor.%s = %#v, want %#v", f, g, w)
		}
	}
	if contains(d.UpdateFields, "region") || !contains(d.CreateFields, "region") {
		t.Errorf("CreateFields=%v UpdateFields=%v", d.CreateFields, d.UpdateFields)
	}
}

func TestBuildKindOverrides(t *testing.T) {
	m := buildWidget(t, KindConfig{
		Kind: "Widget", Group: "gadgets", UpdateMethod: "PUT", IDField: "name", NameField: "-",
		Immutable: []string{"kind"}, WriteOnly: []string{"meta", "origin.timeout"}, NotWriteOnly: []string{"secret"},
		DefaultDeletionPolicy: "Orphan",
		Fields:                map[string]FieldOverride{"ttl": {Type: "integer"}, "kind": {Enum: []any{"A", "B"}}, "origin.timeout": {Type: "string"}},
	})
	d := m.Descriptor
	if d.Kind != "Widget" || d.Group != "gadgets.cloudflare.flare.dev" || d.UpdateMethod != "PUT" || d.IDField != "name" || d.NameField != "" || d.DefaultDeletionPolicy != "Orphan" {
		t.Errorf("overrides not applied: %+v", d)
	}
	// With PUT (full widget_create body) nothing is create-only; kind is immutable by override.
	if !reflect.DeepEqual(d.Immutable, []string{"kind"}) {
		t.Errorf("Immutable = %v", d.Immutable)
	}
	// A dotted path names a nested field (reached through objects only).
	if !reflect.DeepEqual(d.WriteOnly, []string{"meta", "origin.timeout", "region"}) {
		t.Errorf("WriteOnly = %v", d.WriteOnly)
	}
	if k := m.Params.Field("ttl").Type.Kind; k != KInteger {
		t.Errorf("ttl kind = %v", k)
	}
	if k := m.Observation.Field("ttl").Type.Kind; k != KInteger {
		t.Errorf("atProvider ttl kind = %v (overrides apply to both views)", k)
	}
	if e := m.Params.Field("kind").Type.Enum; !reflect.DeepEqual(e, []any{"A", "B"}) {
		t.Errorf("kind enum = %v", e)
	}
	if k := m.Params.Field("origin").Type.Field("timeout").Type.Kind; k != KString {
		t.Errorf("origin.timeout kind = %v", k)
	}
}

func TestBuildKindOverrideErrors(t *testing.T) {
	r := resourceByKey(t, Discover(loadFragment(t)), "widgets.gadgets /accounts/{account_id}/widgets")
	for name, kc := range map[string]KindConfig{
		"unknown field override":   {Fields: map[string]FieldOverride{"nope.x": {Type: "string"}}},
		"unknown immutable":        {Immutable: []string{"nope"}},
		"unknown writeOnly":        {WriteOnly: []string{"nope"}},
		"unknown nested writeOnly": {WriteOnly: []string{"origin.nope"}},
		"writeOnly below a scalar": {WriteOnly: []string{"ttl.x"}},
		"notWriteOnly not derived": {NotWriteOnly: []string{"name"}},
		"unknown nameField":        {NameField: "nope"},
		"no such update method":    {UpdateMethod: "POST"},
		"kind not a Go name":       {Kind: "my-widget"},
		"group not a DNS label":    {Group: "Widgets_2"},
		"plural not a DNS label":   {Plural: "Widgets"},
	} {
		kc.FernGroup = r.FernGroup
		if _, err := BuildKind(r, kc, "cloudflare.flare.dev", "v1alpha1"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	unsupported := resourceByKey(t, Discover(loadFragment(t)), "widgets.parts /accounts/{account_id}/widgets/{widget_id}/parts")
	if _, err := BuildKind(unsupported, KindConfig{FernGroup: "widgets.parts"}, "x", "v1"); err == nil {
		t.Error("nested resource built without error")
	}
}

func TestBuildKindSingleton(t *testing.T) {
	r := resourceByKey(t, Discover(loadFragment(t)), "widget_tools.settings /zones/{zone_identifier}/widget_settings")
	m, err := BuildKind(r, KindConfig{FernGroup: r.FernGroup}, "cloudflare.flare.dev", "v1alpha1")
	if err != nil {
		t.Fatal(err)
	}
	d := m.Descriptor
	if !d.Singleton || d.Scope != "zone" || d.ItemPath != "/zones/{zone_id}/widget_settings" || d.CreatePath != "" || d.ListPath != "" ||
		d.IDField != "" || d.NameField != "" || d.UpdateMethod != "PATCH" || d.DefaultDeletionPolicy != "Orphan" || len(d.Immutable) != 0 {
		t.Errorf("singleton descriptor: %+v", d)
	}
	if m.Kind != "Setting" || m.Group != "widgettools.cloudflare.flare.dev" {
		t.Errorf("kind %s group %s", m.Kind, m.Group)
	}
	if got := topNames(m.Params); !reflect.DeepEqual(got, []string{"enabled", "level"}) {
		t.Errorf("forProvider = %v (updated_at is readOnly)", got)
	}
	if got := topNames(m.Observation); !reflect.DeepEqual(got, []string{"enabled", "level", "updated_at"}) {
		t.Errorf("atProvider = %v", got)
	}
	for _, f := range m.Params.Fields {
		if f.Required {
			t.Errorf("singleton field %s is required", f.JSONName)
		}
	}
	if CRD := BuildCRD(m); !contains(CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Required, "zoneRef") {
		t.Error("zone-scoped CRD does not require zoneRef")
	}
}

// TestFlattening checks each rule of doc.go on the widget fragment.
func TestFlattening(t *testing.T) {
	m := buildWidget(t, KindConfig{})
	p, o := m.Params, m.Observation

	field := func(t0 *Type, path string) *Type {
		t.Helper()
		ft := lookupPath(t0, strings.Split(path, "."))
		if ft == nil {
			t.Fatalf("no field %s", path)
		}
		return ft
	}

	// readOnly/writeOnly split the views.
	if p.Field("id") != nil || p.Field("created_on") != nil {
		t.Error("readOnly fields in forProvider")
	}
	if o.Field("id") == nil || o.Field("secret") != nil {
		t.Error("atProvider must have id and not the writeOnly secret")
	}
	// Top-level required from the create body only, and only via CreateRequired
	// (a CEL rule): the schema itself requires nothing at the top level.
	for _, f := range p.Fields {
		if f.Required {
			t.Errorf("forProvider.%s required=true; top-level fields are required by CEL only", f.JSONName)
		}
	}
	if !reflect.DeepEqual(m.CreateRequired, []string{"kind", "name"}) && !reflect.DeepEqual(m.CreateRequired, []string{"name", "kind"}) {
		t.Errorf("CreateRequired = %v, want kind and name", m.CreateRequired)
	}
	// oneOf of objects → one object; discriminator enum unioned; required intersected.
	origin := field(p, "origin")
	if origin.Kind != KObject || !reflect.DeepEqual(topNames(origin), []string{"bucket", "timeout", "type", "url"}) {
		t.Errorf("origin = %v %v", origin.Kind, topNames(origin))
	}
	if e := origin.Field("type").Type.Enum; !reflect.DeepEqual(e, []any{"http", "s3"}) {
		t.Errorf("origin.type enum = %v", e)
	}
	if !origin.Field("type").Required || origin.Field("url").Required || origin.Field("bucket").Required {
		t.Error("origin required should be exactly [type]")
	}
	if tm := origin.Field("timeout").Type; tm.Minimum == nil || *tm.Minimum != 1 || tm.Maximum == nil || *tm.Maximum != 60 {
		t.Error("agreeing constraints across oneOf members must be kept")
	}
	// anyOf integer|number → number, constraint dropped (members disagree).
	if ttl := field(p, "ttl"); ttl.Kind != KNumber || ttl.Minimum != nil {
		t.Errorf("ttl = %v min=%v", ttl.Kind, ttl.Minimum)
	}
	// oneOf of enums → enum union.
	if mode := field(p, "mode"); mode.Kind != KString || !reflect.DeepEqual(mode.Enum, []any{"x", "y"}) {
		t.Errorf("mode = %v %v", mode.Kind, mode.Enum)
	}
	// string | object → preserve-unknown-fields.
	if tg := field(p, "target"); tg.Kind != KAny || tg.AnyObject {
		t.Errorf("target = %v anyObject=%v", tg.Kind, tg.AnyObject)
	}
	// additionalProperties → map; type: object alone → free-form object.
	if l := p.Field("labels").Type; l.Kind != KMap || l.Elem.Kind != KString {
		t.Errorf("labels = %v", l.Kind)
	}
	if mt := field(p, "meta"); mt.Kind != KAny || !mt.AnyObject {
		t.Errorf("meta = %v anyObject=%v", mt.Kind, mt.AnyObject)
	}
	// Recursion stops at the first self-reference.
	tree := field(p, "tree")
	if tree.Kind != KObject || tree.Field("children").Type.Kind != KArray || tree.Field("children").Type.Elem.Kind != KAny {
		t.Errorf("tree not cut at recursion: %+v", tree)
	}
	// oneOf [string, null] → string.
	if n := field(p, "note"); n.Kind != KString {
		t.Errorf("note = %v", n.Kind)
	}
	// allOf merges properties and required.
	combo := field(p, "combo")
	if !reflect.DeepEqual(topNames(combo), []string{"extra", "name", "weight"}) || !combo.Field("name").Required {
		t.Errorf("combo = %v", topNames(combo))
	}
	// Validations: kept in forProvider, dropped in atProvider; RE2-incompatible patterns dropped.
	if n := field(p, "name"); n.Pattern == "" || n.MaxLength == nil || *n.MaxLength != 64 {
		t.Error("forProvider.name lost its validations")
	}
	if n := field(o, "name"); n.Pattern != "" || n.MaxLength != nil || len(field(o, "kind").Enum) != 0 {
		t.Error("atProvider must not validate")
	}
	if la := field(p, "lookahead"); la.Pattern != "" {
		t.Errorf("lookahead pattern kept: %q", la.Pattern)
	}
	if tags := field(p, "tags"); tags.Kind != KString || tags.MaxLength == nil { // lookupPath steps into items
		t.Errorf("tags items = %v", tags.Kind)
	}
	if tags := p.Field("tags").Type; tags.MaxItems == nil || *tags.MaxItems != 5 {
		t.Error("tags maxItems lost")
	}
	// Every fallback is reported.
	var joined = strings.Join(m.Warnings, "\n")
	for _, w := range []string{"target", "recursive"} {
		if !strings.Contains(joined, w) {
			t.Errorf("no warning mentioning %q in:\n%s", w, joined)
		}
	}
}

func TestConvertTypeLists(t *testing.T) {
	doc, err := ParseSpec([]byte(`{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{},
	"components":{"schemas":{
	  "a":{"type":["string","null"]},
	  "b":{"type":["string","integer"]},
	  "c":{"type":["integer","number"]},
	  "d":{"const":"only"},
	  "e":{"oneOf":[{"type":"object","properties":{"x":{"type":"string"}}},{"type":"object","additionalProperties":{"type":"string"}}]},
	  "f":{"type":"object","properties":{"ro":{"type":"string","readOnly":true}}}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	get := func(n string) *Type {
		ty, _ := Convert(doc.Components.Schemas[n])
		return ty
	}
	if k := get("a").Kind; k != KString {
		t.Errorf("[string,null] → %v", k)
	}
	if k := get("b").Kind; k != KAny {
		t.Errorf("[string,integer] → %v", k)
	}
	if k := get("c").Kind; k != KNumber {
		t.Errorf("[integer,number] → %v", k)
	}
	if d := get("d"); d.Kind != KString || !reflect.DeepEqual(d.Enum, []any{"only"}) {
		t.Errorf("const → %v %v", d.Kind, d.Enum)
	}
	if e := get("e"); e.Kind != KAny || !e.AnyObject {
		t.Errorf("object|map → %v", e.Kind)
	}
	if f := Project(get("f"), ViewParameters); f.Kind != KAny || !f.AnyObject {
		t.Errorf("object with only readOnly props in forProvider → %v", f.Kind)
	}
}

func TestNaming(t *testing.T) {
	for in, want := range map[string]string{
		"queue_id": "QueueID", "title": "Title", "read_replication": "ReadReplication", "max_wait_time_ms": "MaxWaitTimeMs",
		"supports_url_encoding": "SupportsURLEncoding", "ipv4_cidrs": "IPv4Cidrs", "2fa": "X2fa", "x-foo.bar": "XFooBar",
		"camelCase": "CamelCase", "HTTPServer": "HTTPServer", "uuid": "UUID", "": "Field",
	} {
		if got := GoName(in); got != want {
			t.Errorf("GoName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"namespaces": "namespace", "queues": "queue", "policies": "policy", "addresses": "address", "status": "status", "d1": "d1"} {
		if got := singular(in); got != want {
			t.Errorf("singular(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"KVNamespace": "kvnamespaces", "Queue": "queues", "D1Database": "d1databases", "Policy": "policies", "Gateway": "gateways", "Address": "addresses", "WidgetSettings": "widgetsettings"} {
		if got := plural(in); got != want {
			t.Errorf("plural(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte("kinds:\n- fernGroup: kv.namespaces\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "v1alpha1" || c.GroupSuffix != "cloudflare.flare.dev" {
		t.Errorf("defaults: %+v", c)
	}
	for _, bad := range []string{
		"kinds:\n- kind: X\n",
		"kinds:\n- fernGroup: a\n  defaultDeletionPolicy: Keep\n",
		"kinds:\n- fernGroup: a\n  updateMethod: POST\n",
		"kinds:\n- fernGroup: a\n  fields: {x: {type: object}}\n",
		"kinds:\n- fernGroup: a\n  typo: 1\n",
		"kinds:\n- fernGroup: a\n  emulate: handwritten\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCELField(t *testing.T) {
	for in, want := range map[string]string{
		"title": "title", "queue_name": "queue_name", "namespace": "__namespace__", "in": "__in__",
		"a-b": "a__dash__b", "a.b": "a__dot__b", "a/b": "a__slash__b", "a__b": "a__underscores__b",
	} {
		if got := CELField(in); got != want {
			t.Errorf("CELField(%q) = %q, want %q", in, got, want)
		}
	}
	for name, ok := range map[string]bool{"title": true, "x-y": true, "1abc": false, "a b": false, "@type": false} {
		if CELAccessible(name) != ok {
			t.Errorf("CELAccessible(%q) = %v", name, !ok)
		}
	}
	// A create-required field CEL cannot name falls back to a plain schema requirement.
	m := &KindModel{Kind: "X", Plural: "xs", Group: "g.cloudflare.flare.dev", Product: "g", Version: "v1alpha1",
		Resource: &Resource{FernGroup: "g"}, Params: &Type{Kind: KObject, Fields: []*Field{
			{JSONName: "ok", Type: &Type{Kind: KString}}, {JSONName: "1bad", Type: &Type{Kind: KString}}}},
		Observation: &Type{Kind: KObject}, CreateRequired: []string{"ok", "1bad"}}
	crd := BuildCRD(m)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if len(spec.XValidations) != 1 || !strings.Contains(spec.XValidations[0].Rule, "has(self.forProvider.ok)") {
		t.Errorf("rules %+v", spec.XValidations)
	}
	if fp := spec.Properties["forProvider"]; !reflect.DeepEqual(fp.Required, []string{"1bad"}) {
		t.Errorf("forProvider.required = %v", fp.Required)
	}
}
