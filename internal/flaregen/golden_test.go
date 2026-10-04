package flaregen

import (
	"flag"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden from the current generator")

// TestGolden generates the fragment spec and compares every file with
// testdata/golden. Run `go test ./internal/flaregen -run TestGolden -update`
// after an intended change and review the diff.
func TestGolden(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("testdata", "generator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(loadFragment(t), cfg, Options{Module: "github.com/chenhunghan/flare-operator"})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "golden")
	if *update {
		if err := out.Write(golden); err != nil {
			t.Fatal(err)
		}
	}
	diffs, err := out.Diff(golden)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("golden files differ (rerun with -update and review):\n%s", strings.Join(diffs, "\n"))
	}
	for _, want := range []string{
		"api/widgets/v1alpha1/widget_types.go", "api/widgets/v1alpha1/groupversion_info.go", "api/widgets/v1alpha1/zz_generated.deepcopy.go",
		"api/widgettools/v1alpha1/widgetsettings_types.go",
		"config/crd/bases/widgets.cloudflare.flare.dev_widgets.yaml", "config/crd/bases/widgettools.cloudflare.flare.dev_widgetsettings.yaml",
		DescriptorFile,
	} {
		if _, ok := out.Files[want]; !ok {
			t.Errorf("missing output %s", want)
		}
	}
}

// TestGeneratedUpToDate fails when the committed output differs from what
// `go run ./cmd/flaregen` (make generate-crds) would write.
func TestGeneratedUpToDate(t *testing.T) {
	doc := pinnedSpec(t)
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, "generator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(doc, cfg, Options{Module: "github.com/chenhunghan/flare-operator"})
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := out.Diff(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("generated files are out of date; run make generate-crds:\n%s", strings.Join(diffs, "\n"))
	}
	var kinds []string
	for _, m := range out.Kinds {
		kinds = append(kinds, m.Group+"/"+m.Kind)
	}
	if want := "aigateway.cloudflare.flare.dev/AIGateway d1.cloudflare.flare.dev/D1Database kv.cloudflare.flare.dev/KVNamespace " +
		"queues.cloudflare.flare.dev/Queue r2.cloudflare.flare.dev/R2Bucket secretsstore.cloudflare.flare.dev/SecretsStore vectorize.cloudflare.flare.dev/VectorizeIndex"; strings.Join(sortedStrings(kinds), " ") != want {
		t.Errorf("kinds = %v", kinds)
	}
}

// TestGoldenCompiles type-checks the golden API packages (testdata is outside
// ./..., so go build and go vet would not see them otherwise).
func TestGoldenCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go vet")
	}
	cmd := exec.Command("go", "vet", "./testdata/golden/api/widgets/v1alpha1", "./testdata/golden/api/widgettools/v1alpha1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet golden packages: %v\n%s", err, out)
	}
}

func sortedStrings(ss []string) []string {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	return sortedKeys(m)
}
