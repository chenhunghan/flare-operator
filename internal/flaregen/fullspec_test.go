package flaregen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// Go code compiles (go vet in a scratch module that imports this repository):
// the flattening rules must hold for all of Cloudflare, not only the
// configured kinds.
func TestWholeSpecGenerates(t *testing.T) {
	doc := pinnedSpec(t)
	var ok, skipped, noID, preserve int
	files := map[string][]byte{} // module-relative path → content
	pkgs := map[string]string{}  // package dir → resource key
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
		pkg.used[m.Kind], pkg.used[m.Kind+"List"], pkg.used[m.Kind+"Spec"], pkg.used[m.Kind+"Status"] = true, true, true, true
		k := &kindGo{m: m}
		k.params = pkg.addStruct(m.Params, m.Kind, "Parameters", "")
		k.obs = pkg.addStruct(m.Observation, m.Kind, "Observation", "")
		types, err := emitTypesFile(checkModule, k, pkg.structs)
		if err != nil {
			t.Errorf("%s: types: %v", r.Key(), err)
		}
		dc, err := emitDeepCopy("v1alpha1", pkg.structs, []string{m.Kind})
		if err != nil {
			t.Errorf("%s: deepcopy: %v", r.Key(), err)
		}
		gv, err := emitGroupVersion(m.Product, m.Group, "v1alpha1", []string{r.FernGroup}, []string{m.Kind})
		if err != nil {
			t.Errorf("%s: groupversion: %v", r.Key(), err)
		}
		dir := fmt.Sprintf("p%03d/v1alpha1", ok)
		pkgs[dir] = r.Key()
		files[dir+"/types.go"], files[dir+"/zz_generated.deepcopy.go"], files[dir+"/groupversion_info.go"] = types, dc, gv
		ok++
	}
	if !t.Failed() {
		compileGenerated(t, files, pkgs)
	}
	t.Logf("generated %d resources (%d preserve-unknown-fields fallbacks); %d need an idField override; %d unsupported", ok, preserve, noID, skipped)
	if ok < 150 {
		t.Errorf("only %d resources generated; the model lost coverage", ok)
	}
}

// checkModule is the module path of the scratch module compileGenerated builds.
const checkModule = "flare.dev/operator"

// compileGenerated writes the generated packages into a scratch copy of this
// module (go.mod, go.sum and api/common only, so the imports resolve) and runs
// go vet over them offline. Errors are mapped back to the resource that
// produced the package.
func compileGenerated(t *testing.T, files map[string][]byte, pkgs map[string]string) {
	t.Helper()
	root, err := filepath.Abs(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, "gen", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	common := filepath.Join(root, "api", "common", "v1alpha1")
	entries, err := os.ReadDir(common)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			b, err := os.ReadFile(filepath.Join(common, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "api", "common", "v1alpha1"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "api", "common", "v1alpha1", e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "vet", "./gen/...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	// Name the resources whose packages failed.
	re := regexp.MustCompile(`gen/(p\d{3}/v1alpha1)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			t.Errorf("%s: generated package does not compile", pkgs[m[1]])
		}
	}
	msg := string(out)
	if len(msg) > 8000 {
		msg = msg[:8000] + "\n…"
	}
	t.Fatalf("go vet of generated packages: %v\n%s", err, msg)
}
