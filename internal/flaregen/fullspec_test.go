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
	var built []*KindModel
	for _, r := range Discover(doc) {
		if r.Unsupported != "" {
			skipped++
			continue
		}
		m, err := BuildKind(r, KindConfig{FernGroup: r.FernGroup}, "flare.dev", "v1alpha1")
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
		built = append(built, m)
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
	checkWholeSpecNames(t, built)
}

// wholeSpecCollisions is the number of names CheckNames reports for the whole pinned spec with
// default names plus the hand-written kinds. A change means the spec, the naming rules or the
// hand-written kinds changed: review the logged collisions and update it.
const wholeSpecCollisions = 80

// checkWholeSpecNames runs the one-group name check (CheckNames) over every resource of the
// spec with default names, plus the hand-written kinds. All of Cloudflare in one API group has
// real collisions (many products have a "Rule", a "Setting", ...), so generating every
// resource with default names must fail: the check must find them all (their number is
// pinned). Each must also be fixable as the error says, with an explicit kind and plural:
// the names <FernPrefix><Kind> (the fern group without its last segment, e.g.
// zero-trust.dex.rules → ZeroTrustDexRule) resolve every collision between fern groups. The
// ones left are between resources of one fern group, which generator.yaml can only select with
// an explicit path, so such an entry needs an explicit kind as well.
func checkWholeSpecNames(t *testing.T, built []*KindModel) {
	t.Helper()
	handWritten, err := LoadHandWrittenNames(filepath.Join(repoRoot(t), CRDDir), DefaultGroup)
	if err != nil {
		t.Fatal(err)
	}
	if len(handWritten) == 0 {
		t.Fatal("no hand-written CRDs found in config/crd/bases")
	}
	names := append([]KindNames(nil), handWritten...)
	prefixed := append([]KindNames(nil), handWritten...)
	fern := map[string]string{} // prefixed Source → fern group
	for _, m := range built {
		names = append(names, NamesOf(m))
		segs := strings.Split(m.Resource.FernGroup, ".")
		kind := m.Kind
		if len(segs) > 1 {
			kind = GoName(strings.Join(segs[:len(segs)-1], ".")) + m.Kind
		}
		src := "fern-prefixed " + m.Resource.Key()
		fern[src] = m.Resource.FernGroup
		prefixed = append(prefixed, KindNames{Kind: kind, Plural: plural(kind), Singular: strings.ToLower(kind), Source: src, Product: m.Product})
	}

	collisions := CheckNames(names)
	involved := map[string]bool{}
	for _, c := range collisions {
		for _, k := range c.Kinds {
			involved[k.Source] = true
		}
	}
	t.Logf("one API group, default names: %d kinds (%d hand-written), %d colliding names involving %d kinds",
		len(names), len(handWritten), len(collisions), len(involved))
	for _, c := range collisions {
		t.Logf("  %s", c)
	}
	if len(collisions) != wholeSpecCollisions {
		t.Errorf("CheckNames reports %d collisions across the whole spec, want %d (see the log)", len(collisions), wholeSpecCollisions)
	}
	if msg := (&CollisionError{Group: DefaultGroup, Collisions: collisions}).Error(); !strings.Contains(msg, "kind: and plural:") {
		t.Errorf("the collision error does not name the fix: %s", msg)
	}

	left := CheckNames(prefixed)
	for _, c := range left {
		groups, products := map[string]bool{}, map[string]bool{}
		for _, k := range c.Kinds {
			if g, generated := fern[k.Source]; generated {
				groups[g] = true
			}
			products[k.Product] = true
		}
		// Left: kinds of one fern group, or a hand-written kind and the spec resource of the
		// same product that it implements (Tunnel and tunnels' cfd_tunnel), which would never
		// be generated as well.
		if len(groups) > 1 || len(products) > 1 {
			t.Errorf("not resolved by <FernPrefix><Kind> names: %s", c)
		}
	}
	t.Logf("<FernPrefix><Kind> names: %d colliding names left, each within one fern group or a hand-written kind's own resource", len(left))
}

// checkModule is the module path of the scratch module compileGenerated builds.
const checkModule = "github.com/chenhunghan/flare-operator"

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
