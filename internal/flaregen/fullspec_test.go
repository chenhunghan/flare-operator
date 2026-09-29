package flaregen

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func repoRoot(t testing.TB) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

var (
	pinnedOnce sync.Once
	pinnedDoc  *openapi3.T
	pinnedErr  error
)

// pinnedSpec loads spec/openapi.json.gz once per test binary.
func pinnedSpec(t testing.TB) *openapi3.T {
	t.Helper()
	if testing.Short() {
		t.Skip("loads the 26 MB pinned spec")
	}
	pinnedOnce.Do(func() { pinnedDoc, pinnedErr = LoadSpec(filepath.Join(repoRoot(t), "spec", "openapi.json.gz")) })
	if pinnedErr != nil {
		t.Fatal(pinnedErr)
	}
	return pinnedDoc
}

// TestWholeSpecGenerates builds every supported resource of the pinned spec
// with default settings and checks that the emitted CRD is structural and the
// Go code formats: the flattening rules must hold for all of Cloudflare, not
// only the configured kinds.
func TestWholeSpecGenerates(t *testing.T) {
	doc := pinnedSpec(t)
	var ok, skipped, noID, preserve int
	for _, r := range Discover(doc) {
		if r.Unsupported != "" {
			skipped++
			continue
		}
		m, err := BuildKind(r, KindConfig{FernGroup: r.FernGroup}, "cloudflare.flare.dev", "v1alpha1")
		if err != nil {
			// Deriving an ID field can legitimately fail (it needs an idField override);
			// everything else is a bug.
			if !strings.Contains(err.Error(), "cannot derive the ID field") {
				t.Errorf("%s: %v", r.Key(), err)
			}
			noID++
			continue
		}
		preserve += len(m.Warnings)
		if err := ValidateCRD(BuildCRD(m)); err != nil {
			t.Errorf("%s: %v", r.Key(), err)
			continue
		}
		pkg := newGoPkg()
		k := &kindGo{m: m}
		k.params = pkg.addStruct(m.Params, m.Kind, "Parameters", "")
		k.obs = pkg.addStruct(m.Observation, m.Kind, "Observation", "")
		if _, err := emitTypesFile("example.com/m", k, pkg.structs); err != nil {
			t.Errorf("%s: types: %v", r.Key(), err)
		}
		if _, err := emitDeepCopy("v1alpha1", pkg.structs, []string{m.Kind}); err != nil {
			t.Errorf("%s: deepcopy: %v", r.Key(), err)
		}
		ok++
	}
	t.Logf("generated %d resources (%d preserve-unknown-fields fallbacks); %d need an idField override; %d unsupported", ok, preserve, noID, skipped)
	if ok < 150 {
		t.Errorf("only %d resources generated; the model lost coverage", ok)
	}
}
