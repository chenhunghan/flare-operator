package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// TestUpToDate fails when docs/api-reference.md differs from what `make api-docs` writes.
func TestUpToDate(t *testing.T) {
	want, err := Render(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, outFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date: run make api-docs", outFile)
	}
}

// reasonConstants returns the values of the string constants named Reason* (condition reasons)
// and EventReason* (event reasons) declared in api/ and internal/ (internal/fake, the emulator,
// and tests excepted), each with the files that declare it.
func reasonConstants(t *testing.T) (conditions, events map[string][]string) {
	t.Helper()
	conditions, events = map[string][]string{}, map[string][]string{}
	for _, dir := range []string{"api", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				if rel == filepath.Join("internal", "fake") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				g, ok := decl.(*ast.GenDecl)
				if !ok || g.Tok != token.CONST {
					continue
				}
				for _, spec := range g.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue // e.g. an alias of another package's constant
						}
						v, _ := strconv.Unquote(lit.Value)
						switch {
						case strings.HasPrefix(name.Name, "EventReason"):
							events[v] = append(events[v], rel)
						case strings.HasPrefix(name.Name, "Reason"):
							conditions[v] = append(conditions[v], rel)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return conditions, events
}

// TestReasonsDocumented keeps reasons.yaml (the reasons table of the API reference) in step
// with the code.
func TestReasonsDocumented(t *testing.T) {
	r, err := LoadReasons(root)
	if err != nil {
		t.Fatal(err)
	}
	conds, events := reasonConstants(t)
	if len(conds) < 10 || len(events) == 0 {
		t.Fatalf("found only %d condition and %d event reasons: is the scan broken?", len(conds), len(events))
	}
	compare := func(what string, code map[string][]string, doc []string) {
		seen := map[string]bool{}
		for _, d := range doc {
			if seen[d] {
				t.Errorf("%s reason %s is documented twice", what, d)
			}
			seen[d] = true
			if _, ok := code[d]; !ok {
				t.Errorf("%s reason %s is documented in %s but no constant declares it", what, d, reasonsFile)
			}
		}
		var missing []string
		for v, files := range code {
			if !seen[v] {
				missing = append(missing, v+" ("+strings.Join(files, ", ")+")")
			}
		}
		sort.Strings(missing)
		for _, m := range missing {
			t.Errorf("%s reason %s is not documented in %s", what, m, reasonsFile)
		}
	}
	var cd, ed []string
	for _, c := range r.Conditions {
		cd = append(cd, c.Reason)
		if c.Condition == "" || c.Status == "" || c.Kinds == "" || c.Meaning == "" {
			t.Errorf("condition reason %s: condition, status, kinds and meaning are required", c.Reason)
		}
	}
	for _, e := range r.Events {
		ed = append(ed, e.Reason)
		if e.Type == "" || e.Kinds == "" || e.Meaning == "" {
			t.Errorf("event reason %s: type, kinds and meaning are required", e.Reason)
		}
	}
	compare("condition", conds, cd)
	compare("event", events, ed)
}

func TestHelpers(t *testing.T) {
	if got := code("a || b"); got != "` a \\|\\| b `" {
		t.Errorf("code = %q", got)
	}
	if got := code("x `y`"); got != "`` x `y` ``" {
		t.Errorf("code with backticks = %q", got)
	}
	if got := text("a\n  <b>  c"); got != "a &lt;b&gt; c" {
		t.Errorf("text = %q", got)
	}
}
