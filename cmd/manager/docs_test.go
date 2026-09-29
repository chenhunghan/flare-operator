package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const repoRoot = "../.."

// markedTable returns the rows ("| ... |" lines) between <!-- name:begin --> and
// <!-- name:end --> in file, split into trimmed cells.
func markedTable(t *testing.T, file, name string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, file))
	if err != nil {
		t.Fatal(err)
	}
	begin, end := "<!-- "+name+":begin -->", "<!-- "+name+":end -->"
	var rows [][]string
	in := false
	for i, line := range strings.Split(string(b), "\n") {
		switch strings.TrimSpace(line) {
		case begin:
			in = true
			continue
		case end:
			if !in {
				t.Fatalf("%s:%d: %s before %s", file, i+1, end, begin)
			}
			return rows
		}
		if !in || !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		rows = append(rows, cells)
	}
	t.Fatalf("%s: no %s ... %s block", file, begin, end)
	return nil
}

// codeSpan returns the content of s when s is exactly one `code span`.
func codeSpan(s string) (string, bool) {
	if len(s) < 2 || s[0] != '`' || s[len(s)-1] != '`' || strings.Count(s, "`") != 2 {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// TestManagerFlagsChild is not a test: TestFlagsDocumented runs the test binary with
// FLARE_MANAGER_FLAGS_CHILD=1 so that main() registers the manager's flags in a fresh process,
// parses -version (which returns without starting anything) and this prints every flag with its
// default as JSON.
func TestManagerFlagsChild(t *testing.T) {
	if os.Getenv("FLARE_MANAGER_FLAGS_CHILD") != "1" {
		t.Skip("helper process for TestFlagsDocumented")
	}
	os.Args = []string{"manager", "-version"}
	main()
	defs := map[string]string{}
	flag.VisitAll(func(f *flag.Flag) {
		if !strings.HasPrefix(f.Name, "test.") {
			defs[f.Name] = f.DefValue
		}
	})
	b, _ := json.Marshal(defs)
	fmt.Printf("\nFLAGS:%s\n", b)
}

// managerFlags returns the manager binary's flags and their defaults.
func managerFlags(t *testing.T) map[string]string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagerFlagsChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "FLARE_MANAGER_FLAGS_CHILD=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper process: %v\n%s\n%s", err, out, stderr.String())
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "FLAGS:"); ok {
			var defs map[string]string
			if err := json.Unmarshal([]byte(rest), &defs); err != nil {
				t.Fatal(err)
			}
			return defs
		}
	}
	t.Fatalf("helper process printed no FLAGS line:\n%s", out)
	return nil
}

// sameDefault compares a documented default with a flag's DefValue: durations by value (60s ==
// 1m0s), "" as the empty string, everything else literally.
func sameDefault(doc, def string) bool {
	if doc == `""` {
		doc = ""
	}
	if doc == def {
		return true
	}
	a, errA := time.ParseDuration(doc)
	b, errB := time.ParseDuration(def)
	return errA == nil && errB == nil && a == b
}

// TestFlagsDocumented keeps the README's manager-flags table in step with the binary: every
// flag has a row, every row is a flag, and a Default written as code is the flag's default.
func TestFlagsDocumented(t *testing.T) {
	flags := managerFlags(t)
	rows := markedTable(t, "README.md", "manager-flags")
	seen := map[string]bool{}
	for _, r := range rows {
		if len(r) != 4 {
			t.Errorf("README.md manager-flags: want 4 cells (Flag | Default | Chart value | Meaning), got %d: %q", len(r), r)
			continue
		}
		name, ok := codeSpan(r[0])
		if !ok || !strings.HasPrefix(name, "--") {
			t.Errorf("README.md manager-flags: the Flag cell must be one `--flag` code span: %q", r[0])
			continue
		}
		name = strings.TrimPrefix(name, "--")
		if seen[name] {
			t.Errorf("README.md manager-flags: --%s is listed twice", name)
		}
		seen[name] = true
		def, isFlag := flags[name]
		if !isFlag {
			t.Errorf("README.md manager-flags: --%s is not a manager flag", name)
			continue
		}
		if doc, ok := codeSpan(r[1]); ok && !sameDefault(doc, def) {
			t.Errorf("README.md manager-flags: --%s default %s, the binary says %q", name, r[1], def)
		}
	}
	var missing []string
	for name := range flags {
		if !seen[name] {
			missing = append(missing, "--"+name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("README.md manager-flags: no row for %s", strings.Join(missing, ", "))
	}
}

// metricDef matches a Prometheus collector definition with its Name, e.g.
// prometheus.NewCounterVec(prometheus.CounterOpts{ Name: "x_total", ...
var (
	metricDef = regexp.MustCompile(`prometheus\.New(?:Counter|Gauge|Histogram|Summary)(?:Vec|Func)?\(\s*prometheus\.(?:Counter|Gauge|Histogram|Summary)Opts\{[^}]*?Name:\s*"([a-z_:][a-z0-9_:]*)"`)
	anyDef    = regexp.MustCompile(`prometheus\.New(?:Counter|Gauge|Histogram|Summary)(?:Vec|Func)?\(`)
)

// definedMetrics scans the non-test Go files of cmd/ and internal/ for Prometheus metric
// definitions. A definition whose name the pattern cannot read fails the test, so a new metric
// cannot escape the check by being written differently.
func definedMetrics(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			named := metricDef.FindAllSubmatch(b, -1)
			if all := anyDef.FindAll(b, -1); len(all) != len(named) {
				t.Errorf("%s: %d Prometheus collector definitions, but only %d with a readable Name (keep the prometheus.New*(prometheus.*Opts{Name: \"...\"}) form)", p, len(all), len(named))
			}
			for _, m := range named {
				out[string(m[1])] = p
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestMetricsDocumented keeps the README's metrics table in step with the metrics the manager
// defines (cmd/ and internal/; controller-runtime's own are described in prose).
func TestMetricsDocumented(t *testing.T) {
	defined := definedMetrics(t)
	if len(defined) == 0 {
		t.Fatal("no metric definitions found")
	}
	seen := map[string]bool{}
	for _, r := range markedTable(t, "README.md", "metrics") {
		name, ok := codeSpan(r[0])
		if !ok {
			t.Errorf("README.md metrics: the Metric cell must be one code span: %q", r[0])
			continue
		}
		seen[name] = true
		if _, ok := defined[name]; !ok {
			t.Errorf("README.md metrics: %s is not defined in cmd/ or internal/", name)
		}
	}
	for name, file := range defined {
		if !seen[name] {
			t.Errorf("metric %s (%s) has no row in the README metrics table", name, file)
		}
	}
}
