package flaregen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/randfill"
	"sigs.k8s.io/yaml"

	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/flaregen"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/generic/descriptors"
)

const crdDir = "../../config/crd/bases"

// generatedCRDs loads every flaregen-owned CRD, keyed by group/kind.
func generatedCRDs(t *testing.T) map[string]*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*apiextensionsv1.CustomResourceDefinition{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(flaregen.GeneratedHeader)) {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(b, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[crd.Spec.Group+"/"+crd.Spec.Names.Kind] = &crd
	}
	return out
}

// TestCRDsStructural validates every committed flaregen CRD with the API
// server's structural-schema rules.
func TestCRDsStructural(t *testing.T) {
	crds := generatedCRDs(t)
	if len(crds) != len(descriptors.All()) {
		t.Errorf("%d generated CRDs for %d descriptors", len(crds), len(descriptors.All()))
	}
	for name, crd := range crds {
		if err := flaregen.ValidateCRD(crd); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func structural(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) *structuralschema.Structural {
	t.Helper()
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// stripValidations removes value constraints so random data can be checked for
// types and required fields only.
func stripValidations(s *apiextensionsv1.JSONSchemaProps) {
	s.Enum, s.Pattern, s.Format = nil, "", ""
	s.MinLength, s.MaxLength, s.Minimum, s.Maximum, s.MinItems, s.MaxItems = nil, nil, nil, nil, nil, nil
	s.ExclusiveMinimum, s.ExclusiveMaximum = false, false
	for k, p := range s.Properties {
		stripValidations(&p)
		s.Properties[k] = p
	}
	if s.Items != nil && s.Items.Schema != nil {
		stripValidations(s.Items.Schema)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
		stripValidations(s.AdditionalProperties.Schema)
	}
}

func kubeSchema(t *testing.T, p *apiextensionsv1.JSONSchemaProps) *spec.Schema {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var s spec.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func filler(seed int64) *randfill.Filler {
	return randfill.NewWithSeed(seed).NilChance(0).NumElements(1, 2).Funcs(
		func(j *apiextensionsv1.JSON, c randfill.Continue) { j.Raw = []byte(`{"any":["thing",1]}`) },
		func(f *metav1.FieldsV1, c randfill.Continue) { f.SetRawBytes([]byte(`{}`)) },
	)
}

func toMap(t *testing.T, obj any) map[string]any {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := utiljson.Unmarshal(b, &m); err != nil { // int64 stays int64, as in the API server
		t.Fatal(err)
	}
	return m
}

// TestGoTypesMatchCRDs fills every generated kind with random data and checks
// that (1) the CRD schema prunes nothing, i.e. every Go field (including the
// hand-written commonv1alpha1 types the generator mirrors) is in the schema, and
// (2) the JSON validates against the schema's types and required fields.
func TestGoTypesMatchCRDs(t *testing.T) {
	crds := generatedCRDs(t)
	for _, e := range descriptors.Entries() {
		crd := crds[e.Group+"/"+e.Kind]
		if crd == nil {
			t.Errorf("%s/%s: no CRD", e.Group, e.Kind)
			continue
		}
		s := structural(t, crd)
		lax := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.DeepCopy()
		stripValidations(lax)
		validator := validate.NewSchemaValidator(kubeSchema(t, lax), nil, "", strfmt.Default)
		for seed := int64(0); seed < 20; seed++ {
			obj := e.New()
			filler(seed).Fill(obj)
			m := toMap(t, obj)
			before := toMap(t, obj)
			if pruned := pruning.PruneWithOptions(m, s, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}); len(pruned) > 0 {
				t.Fatalf("%s: schema prunes Go fields %v", e.Kind, pruned)
			}
			if !reflect.DeepEqual(m, before) {
				t.Fatalf("%s: pruning changed the object", e.Kind)
			}
			delete(m, "metadata") // random ObjectMeta is not a valid ObjectMeta; the schema only says object
			if r := validator.Validate(m); !r.IsValid() {
				t.Fatalf("%s: random object does not validate: %v", e.Kind, r.AsError())
			}
		}
	}
}

// TestSampleObjects validates hand-written objects against the full schemas,
// including enums and patterns taken from the spec and generator.yaml.
func TestSampleObjects(t *testing.T) {
	crds := generatedCRDs(t)
	cases := []struct {
		gk    string
		yaml  string
		valid bool
	}{
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {accountRef: {name: a}, forProvider: {title: flare-spike-kv}}}`, true},
		// title is required by a CEL rule (checked in envtest), not by the OpenAPI schema.
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {accountRef: {name: a}, forProvider: {}}}`, true},
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {accountRef: {name: a}, managementPolicies: [Observe]}}`, true},
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {accountRef: {name: a}, forProvider: {title: x, jurisdiction: mars}}}`, false},
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {forProvider: {title: x}}}`, false}, // accountRef required
		{"kv.cloudflare.flare.dev/KVNamespace", `{spec: {accountRef: {name: a}, deletionPolicy: Keep, forProvider: {title: x}}}`, false},
		{"queues.cloudflare.flare.dev/Queue", `{spec: {accountRef: {name: a}, forProvider: {queue_name: q, settings: {delivery_delay: 5, message_retention_period: 86400}}}}`, true},
		{"queues.cloudflare.flare.dev/Queue", `{spec: {accountRef: {name: a}, forProvider: {queue_name: q, settings: {delivery_delay: 1.5}}}}`, false}, // integer override
		{"d1.cloudflare.flare.dev/D1Database", `{spec: {accountRef: {name: a}, forProvider: {name: db1, primary_location_hint: WEUR, read_replication: {mode: auto}}}}`, true},
		{"d1.cloudflare.flare.dev/D1Database", `{spec: {accountRef: {name: a}, forProvider: {name: db1, primary_location_hint: weur}}}`, false}, // 0019
		{"d1.cloudflare.flare.dev/D1Database", `{spec: {accountRef: {name: a}, forProvider: {name: "-bad"}}}`, false},                           // pattern
		{"d1.cloudflare.flare.dev/D1Database", `{spec: {accountRef: {name: a}, forProvider: {name: db, read_replication: {}}}}`, false},         // mode required
		{"d1.cloudflare.flare.dev/D1Database", `{spec: {accountRef: {name: a}, managementPolicies: [Observe], forProvider: {name: db}}, status: {atProvider: {uuid: u, jurisdiction: null, file_size: 8192}}}`, true},
	}
	for i, c := range cases {
		crd := crds[c.gk]
		if crd == nil {
			t.Fatalf("no CRD %s", c.gk)
		}
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(c.yaml), &obj); err != nil {
			t.Fatal(err)
		}
		// atProvider nulls: the reconciler decodes API JSON into Go types, where null → omitted.
		if st, ok := obj["status"].(map[string]any); ok {
			if ap, ok := st["atProvider"].(map[string]any); ok {
				for k, v := range ap {
					if v == nil {
						delete(ap, k)
					}
				}
			}
		}
		v := validate.NewSchemaValidator(kubeSchema(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema), nil, "", strfmt.Default)
		if r := v.Validate(obj); r.IsValid() != c.valid {
			t.Errorf("case %d %s %s: valid=%v, want %v (%v)", i, c.gk, c.yaml, r.IsValid(), c.valid, r.AsError())
		}
	}
}

// TestDeepCopy checks that DeepCopy is equal and shares no memory.
func TestDeepCopy(t *testing.T) {
	for _, e := range descriptors.Entries() {
		for seed := int64(0); seed < 20; seed++ {
			obj := e.New()
			filler(seed).Fill(obj)
			snapshot := toMap(t, obj)
			cp := obj.DeepCopyObject()
			if !reflect.DeepEqual(obj, cp) {
				t.Fatalf("%s: DeepCopy differs", e.Kind)
			}
			filler(seed + 1000).Fill(cp) // overwrite every field of the copy
			if !reflect.DeepEqual(toMap(t, obj), snapshot) {
				t.Fatalf("%s: DeepCopy shares memory with the original", e.Kind)
			}
		}
		list := e.NewList()
		filler(1).Fill(list)
		if !reflect.DeepEqual(list, list.DeepCopyObject()) {
			t.Fatalf("%sList: DeepCopy differs", e.Kind)
		}
	}
}

func TestRegistry(t *testing.T) {
	s := runtime.NewScheme()
	if err := descriptors.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	all := descriptors.All()
	if len(all) != 7 {
		t.Errorf("%d descriptors", len(all))
	}
	for _, d := range all {
		e, ok := descriptors.Lookup(d.Group, d.Kind)
		if !ok || !reflect.DeepEqual(e.Descriptor, d) {
			t.Fatalf("Lookup(%s, %s)", d.Group, d.Kind)
		}
		gvk := schema.GroupVersionKind{Group: d.Group, Version: d.Version, Kind: d.Kind}
		obj, err := s.New(gvk)
		if err != nil {
			t.Fatalf("%v: %v", gvk, err)
		}
		if reflect.TypeOf(obj) != reflect.TypeOf(e.New()) {
			t.Errorf("%v: scheme type %T, New() %T", gvk, obj, e.New())
		}
		if _, err := s.New(gvk.GroupVersion().WithKind(d.Kind + "List")); err != nil {
			t.Errorf("%vList: %v", gvk, err)
		}
		// Managed accessors point into the object.
		m := e.New()
		m.GetResourceSpec().AccountRef.Name = "acct"
		m.GetResourceStatus().ID = "x"
		j := toMap(t, m)
		if j["spec"].(map[string]any)["accountRef"].(map[string]any)["name"] != "acct" || j["status"].(map[string]any)["id"] != "x" {
			t.Errorf("%s: Managed accessors do not alias the object: %v", d.Kind, j)
		}
		if d.Version != "v1alpha1" || !strings.HasSuffix(d.Group, ".cloudflare.flare.dev") {
			t.Errorf("%s: group/version %s/%s", d.Kind, d.Group, d.Version)
		}
	}
	if _, ok := descriptors.Lookup("kv.cloudflare.flare.dev", "Nope"); ok {
		t.Error("Lookup found a missing kind")
	}
}

// TestDescriptorsMatchEmulator cross-checks the generated descriptors with the
// flarefake routes and the recordings they cite (internal/fake/kv.go, queues.go, d1.go), and
// pins the spec-derived descriptors of the generic-profile kinds.
func TestDescriptorsMatchEmulator(t *testing.T) {
	want := map[string]struct {
		create, item, idField, nameField, update string
		immutable, writeOnly                     []string
		deletion                                 string
	}{
		// 0003 create → id; 0009 rename is PUT {title}; 0013 delete.
		"KVNamespace": {"/accounts/{account_id}/storage/kv/namespaces", "/accounts/{account_id}/storage/kv/namespaces/{id}", "id", "title", "PUT",
			[]string{"jurisdiction"}, nil, "Orphan"},
		// 0028 create → queue_id; 0032 PATCH merges settings; 0031 GET omits settings.delivery_paused.
		"Queue": {"/accounts/{account_id}/queues", "/accounts/{account_id}/queues/{id}", "queue_id", "queue_name", "PATCH",
			[]string{"jurisdiction"}, []string{"settings.delivery_paused"}, "Orphan"},
		// 0017 create → uuid; 0023 PATCH read_replication; flarefake has no PUT for D1.
		"D1Database": {"/accounts/{account_id}/d1/database", "/accounts/{account_id}/d1/database/{id}", "uuid", "name", "PATCH",
			[]string{"jurisdiction", "name", "primary_location_hint"}, []string{"primary_location_hint"}, "Orphan"},
		// Generic-profile kinds (emulate: generic): spec-derived, UNVERIFIED until recorded.
		// Vectorize v2 has no update; the index name is the item path parameter. The GET
		// result's config has dimensions and metric only, so a preset is never read back.
		"VectorizeIndex": {"/accounts/{account_id}/vectorize/v2/indexes", "/accounts/{account_id}/vectorize/v2/indexes/{id}", "name", "name", "",
			[]string{"config", "description", "name"}, []string{"config.preset"}, "Delete"},
		"SecretsStore": {"/accounts/{account_id}/secrets_store/stores", "/accounts/{account_id}/secrets_store/stores/{id}", "id", "name", "",
			[]string{"name"}, nil, "Delete"},
		// The client chooses the gateway id; PUT is the only update.
		"AIGateway": {"/accounts/{account_id}/ai-gateway/gateways", "/accounts/{account_id}/ai-gateway/gateways/{id}", "id", "", "PUT",
			[]string{"id"}, nil, "Delete"},
		// The bucket name is the ID (client-chosen; no adoption by listing: the list wraps its
		// items). jurisdiction travels in a header on every request, the storage class in the
		// PATCH header (Extension, pinned below); the location hint is never read back.
		"R2Bucket": {"/accounts/{account_id}/r2/buckets", "/accounts/{account_id}/r2/buckets/{id}", "name", "", "PATCH",
			[]string{"jurisdiction", "locationHint", "name"}, []string{"locationHint"}, "Orphan"},
	}
	for _, d := range descriptors.All() {
		w, ok := want[d.Kind]
		if !ok {
			t.Errorf("unexpected kind %s", d.Kind)
			continue
		}
		got := []any{d.CreatePath, d.ItemPath, d.ListPath, d.IDField, d.NameField, d.UpdateMethod, d.Immutable, d.WriteOnly, d.DefaultDeletionPolicy, d.Scope, d.Singleton}
		exp := []any{w.create, w.item, w.create, w.idField, w.nameField, w.update, w.immutable, w.writeOnly, w.deletion, "account", false}
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s:\n got %v\nwant %v", d.Kind, got, exp)
		}
	}

	// Per-kind extensions (generator.yaml requestHeaders, observedAs, subResources): pinned for
	// R2Bucket, absent elsewhere, and the emulator's copy (zz_generated_generic.go) agrees.
	wantExt := map[string]generic.Extension{
		"R2Bucket": {
			Headers: []generic.HeaderField{
				{Header: "cf-r2-jurisdiction", Field: "jurisdiction", Default: "default"},
				{Header: "cf-r2-storage-class", Field: "storageClass", Update: true},
			},
			ObservedAs:   map[string]string{"storageClass": "storage_class"},
			SubResources: []generic.SubResource{{Field: "cors", Path: "/cors"}},
		},
	}
	for _, e := range descriptors.Entries() {
		if !reflect.DeepEqual(e.Extension, wantExt[e.Kind]) {
			t.Errorf("%s: Extension %+v, want %+v", e.Kind, e.Extension, wantExt[e.Kind])
		}
		for _, k := range fake.GeneratedGenericKinds() {
			if k.Kind != e.Kind {
				continue
			}
			var hs []generic.HeaderField
			for _, h := range k.Headers {
				hs = append(hs, generic.HeaderField{Header: h.Header, Field: h.Field, Update: h.Update, Default: h.Default})
			}
			var ss []generic.SubResource
			for _, s := range k.SubResources {
				ss = append(ss, generic.SubResource{Field: s.Field, Path: s.Path})
			}
			if got := (generic.Extension{Headers: hs, ObservedAs: k.ObservedAs, SubResources: ss}); !reflect.DeepEqual(got, e.Extension) {
				t.Errorf("%s: emulator extension %+v, descriptor %+v", e.Kind, got, e.Extension)
			}
		}
	}
}
