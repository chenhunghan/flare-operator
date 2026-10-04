package flaregen

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chenhunghan/flare-operator/internal/generic"
)

// r2Config is generator.yaml's R2Bucket entry, reduced to what the extensions need.
func r2Config() KindConfig {
	return KindConfig{
		FernGroup: "r2.buckets", Kind: "R2Bucket", IDField: "name", NameField: "-",
		RequestHeaders: []RequestHeader{
			{Header: "cf-r2-jurisdiction", Field: "jurisdiction"},
			{Header: "cf-r2-storage-class", Field: "storageClass", SentOn: "update"},
		},
		ObservedAs:   map[string]string{"storageClass": "storage_class"},
		SubResources: []SubResourceConfig{{Field: "cors", Path: "/cors", ServerSet: []string{"rules.id"}}},
	}
}

func buildR2(t *testing.T, kc KindConfig) (*KindModel, error) {
	t.Helper()
	r, err := SelectResource(Discover(pinnedSpec(t)), kc)
	if err != nil {
		t.Fatal(err)
	}
	return BuildKind(r, kc, "flare.dev", "v1alpha1")
}

// TestExtensionsR2: requestHeaders, observedAs and subResources resolve against the spec and
// shape the descriptor and the forProvider/atProvider types.
func TestExtensionsR2(t *testing.T) {
	m, err := buildR2(t, r2Config())
	if err != nil {
		t.Fatal(err)
	}
	want := generic.Extension{
		Headers: []generic.HeaderField{
			{Header: "cf-r2-jurisdiction", Field: "jurisdiction", Default: "default"},
			{Header: "cf-r2-storage-class", Field: "storageClass", Update: true},
		},
		ObservedAs:   map[string]string{"storageClass": "storage_class"},
		SubResources: []generic.SubResource{{Field: "cors", Path: "/cors", Delete: true, ServerSet: []string{"rules.id"}}},
	}
	if !reflect.DeepEqual(m.Extension, want) {
		t.Errorf("Extension %+v\nwant %+v", m.Extension, want)
	}
	d := m.Descriptor
	// jurisdiction: a header on every request (immutable, not a body field); storageClass: the
	// PATCH's header (an UpdateField, read back as storage_class, so not write-only); cors: a
	// sub-resource (neither a create nor an update body field).
	if got, exp := d.Immutable, []string{"jurisdiction", "locationHint", "name"}; !reflect.DeepEqual(got, exp) {
		t.Errorf("Immutable %v, want %v", got, exp)
	}
	if got, exp := d.UpdateFields, []string{"storageClass"}; !reflect.DeepEqual(got, exp) {
		t.Errorf("UpdateFields %v, want %v", got, exp)
	}
	if got, exp := d.CreateFields, []string{"locationHint", "name", "storageClass"}; !reflect.DeepEqual(got, exp) {
		t.Errorf("CreateFields %v, want %v", got, exp)
	}
	if got, exp := d.WriteOnly, []string{"locationHint"}; !reflect.DeepEqual(got, exp) {
		t.Errorf("WriteOnly %v, want %v", got, exp)
	}
	j := m.Params.Field("jurisdiction")
	if j == nil || j.Type.Kind != KString || len(j.Type.Enum) != 5 || !strings.Contains(j.Description, "cf-r2-jurisdiction") {
		t.Errorf("forProvider.jurisdiction %+v", j)
	}
	for _, typ := range []*Type{m.Params, m.Observation} {
		c := typ.Field("cors")
		if c == nil || c.Type.Kind != KObject || c.Type.Field("rules") == nil {
			t.Errorf("cors field %+v", c)
		}
	}
	// The CRD's rule for the header field compares effective values (unset = "default").
	var rule string
	for _, r := range immutableRules(m) {
		if strings.Contains(r.Rule, "jurisdiction") {
			rule = r.Rule
		}
	}
	if !strings.Contains(rule, `self.spec.forProvider.jurisdiction : "default") == (has(oldSelf.spec.forProvider)`) {
		t.Errorf("jurisdiction rule %q", rule)
	}
}

func TestExtensionErrors(t *testing.T) {
	for name, mutate := range map[string]func(*KindConfig){
		"undeclared header": func(kc *KindConfig) {
			kc.RequestHeaders = append(kc.RequestHeaders, RequestHeader{Header: "cf-nope", Field: "nope"})
		},
		"update header on create": func(kc *KindConfig) {
			kc.RequestHeaders[1].SentOn = "" // the create operation declares no cf-r2-storage-class
		},
		"header over a body field": func(kc *KindConfig) {
			kc.RequestHeaders[0].Field = "name"
		},
		"observedAs unknown forProvider field": func(kc *KindConfig) {
			kc.ObservedAs = map[string]string{"nope": "storage_class"}
		},
		"observedAs unknown atProvider field": func(kc *KindConfig) {
			kc.ObservedAs = map[string]string{"storageClass": "nope"}
		},
		"subResource without spec path": func(kc *KindConfig) {
			kc.SubResources = []SubResourceConfig{{Field: "nope", Path: "/nope"}}
		},
		"subResource over an existing field": func(kc *KindConfig) {
			kc.SubResources = []SubResourceConfig{{Field: "name", Path: "/cors"}}
		},
		"serverSet not in the GET result": func(kc *KindConfig) {
			kc.SubResources = []SubResourceConfig{{Field: "cors", Path: "/cors", ServerSet: []string{"rules.nope"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			kc := r2Config()
			kc.RequestHeaders = append([]RequestHeader(nil), kc.RequestHeaders...)
			mutate(&kc)
			if _, err := buildR2(t, kc); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestExtensionConfigParse(t *testing.T) {
	for _, bad := range []string{
		"kinds:\n- fernGroup: r2.buckets\n  requestHeaders: [{header: h, field: f, sentOn: sometimes}]\n",
		"kinds:\n- fernGroup: r2.buckets\n  requestHeaders: [{header: h}]\n",
		"kinds:\n- fernGroup: r2.buckets\n  subResources: [{field: cors, path: cors}]\n",
		"kinds:\n- fernGroup: r2.buckets\n  subResources: [{field: cors, path: /{x}}]\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
	good := "kinds:\n- fernGroup: r2.buckets\n  requestHeaders: [{header: h, field: f, sentOn: update}]\n  subResources: [{field: cors, path: /cors}]\n  observedAs: {a: b}\n"
	if _, err := ParseConfig([]byte(good)); err != nil {
		t.Errorf("rejected: %v", err)
	}
}
