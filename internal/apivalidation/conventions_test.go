package apivalidation

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"flare.dev/operator/internal/flaregen"
	"flare.dev/operator/internal/testenv"
)

// TestCRDConventions checks the API conventions every CRD in config/crd/bases follows,
// whether flaregen or controller-gen wrote it (docs/api-reference.md describes them).
func TestCRDConventions(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testenv.RepoRoot(), "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) < 10 {
		t.Fatalf("CRDs: %v %v", files, err)
	}
	short := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		kind := crd.Spec.Names.Kind
		managedKind := kind != "CloudflareAccount"
		t.Run(kind, func(t *testing.T) {
			n := crd.Spec.Names
			if !slices.Contains(n.Categories, flaregen.CategoryCloudflare) || slices.Contains(n.Categories, flaregen.CategoryManaged) != managedKind {
				t.Errorf("categories %v", n.Categories)
			}
			if len(n.ShortNames) == 0 {
				t.Error("no short names")
			}
			for _, sn := range n.ShortNames {
				if !strings.HasPrefix(sn, "cf") {
					t.Errorf("short name %q lacks the cf prefix", sn)
				}
				if o, dup := short[sn]; dup {
					t.Errorf("short name %q is also used by %s", sn, o)
				}
				short[sn] = kind
			}
			if len(crd.Spec.Versions) != 1 {
				t.Fatalf("%d versions", len(crd.Spec.Versions))
			}
			v := crd.Spec.Versions[0]
			var cols []string
			for _, c := range v.AdditionalPrinterColumns {
				cols = append(cols, c.Name+"="+c.JSONPath)
			}
			want := []string{
				"READY=.status.conditions[?(@.type=='Ready')].status",
				"SYNCED=.status.conditions[?(@.type=='Synced')].status",
				"EXTERNAL-ID=.status.id",
			}
			if len(cols) < 5 || !slices.Equal(cols[:3], want) || cols[len(cols)-1] != "AGE=.metadata.creationTimestamp" {
				t.Errorf("columns %v: want %v, kind-specific columns, then AGE", cols, want)
			}
			if v.Subresources == nil || v.Subresources.Status == nil {
				t.Error("no status subresource")
			}
			root := v.Schema.OpenAPIV3Schema
			status := root.Properties["status"]
			for _, f := range []string{"id", "conditions", "observedGeneration", "atProvider"} {
				if _, ok := status.Properties[f]; !ok {
					t.Errorf("status.%s missing", f)
				}
			}
			if c := status.Properties["conditions"]; c.XListType == nil || *c.XListType != "map" || !slices.Equal(c.XListMapKeys, []string{"type"}) {
				t.Errorf("status.conditions is not a map list keyed by type")
			}
			if !managedKind {
				return
			}
			spec := root.Properties["spec"]
			if !slices.Contains(spec.Required, "accountRef") {
				t.Error("spec.accountRef is not required")
			}
			if !slices.ContainsFunc(spec.XValidations, func(r apiextensionsv1.ValidationRule) bool { return r.Rule == flaregen.AccountRefRule }) {
				t.Errorf("spec lacks the accountRef rule %q", flaregen.AccountRefRule)
			}
			if !slices.ContainsFunc(root.XValidations, func(r apiextensionsv1.ValidationRule) bool {
				return r.Rule == flaregen.AccountRefImmutableRule && r.Message == flaregen.AccountRefImmutableMessage
			}) {
				t.Errorf("the object root lacks the accountRef transition rule %q", flaregen.AccountRefImmutableRule)
			}
			if dp := spec.Properties["deletionPolicy"]; len(dp.Enum) != 2 {
				t.Errorf("deletionPolicy enum %v", dp.Enum)
			}
			if mp := spec.Properties["managementPolicies"]; mp.Items == nil || len(mp.Items.Schema.Enum) != 6 {
				t.Errorf("managementPolicies items enum missing")
			}
			if _, ok := spec.Properties["forProvider"]; !ok {
				t.Error("spec.forProvider missing")
			}
		})
	}
}
