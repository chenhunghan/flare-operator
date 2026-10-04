// Command flaregen generates CRD Go types, CRD YAML and generic.Descriptor
// values from the pinned Cloudflare OpenAPI spec and generator.yaml.
//
//	go run ./cmd/flaregen             # write api/<product>/v1alpha1, config/crd/bases, internal/generic/descriptors
//	go run ./cmd/flaregen -check      # exit 1 if the committed output is not up to date
//	go run ./cmd/flaregen -list       # print every resource the model finds in the spec
//
// See internal/flaregen/doc.go for the model and the schema flattening rules.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenhunghan/flare-operator/internal/flaregen"
)

func main() {
	root := flag.String("root", ".", "repository root")
	specPath := flag.String("spec", "spec/openapi.json.gz", "OpenAPI spec (relative to -root unless absolute)")
	configPath := flag.String("config", "generator.yaml", "generator config (relative to -root unless absolute)")
	check := flag.Bool("check", false, "do not write; exit 1 if generated files are out of date")
	list := flag.Bool("list", false, "print the resource model of the whole spec and exit")
	verbose := flag.Bool("v", false, "print schema flattening warnings")
	flag.Parse()

	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(*root, p)
	}
	doc, err := flaregen.LoadSpec(abs(*specPath))
	if err != nil {
		fatal(err)
	}
	if *list {
		for _, r := range flaregen.Discover(doc) {
			state := "ok"
			if r.Unsupported != "" {
				state = "unsupported: " + r.Unsupported
			}
			kind := "crud"
			if r.Singleton {
				kind = "singleton"
			}
			fmt.Printf("%-9s %-7s %s  [%s]\n", kind, r.Scope, r.Key(), state)
		}
		return
	}
	cfg, err := flaregen.LoadConfig(abs(*configPath))
	if err != nil {
		fatal(err)
	}
	module, err := modulePath(abs("go.mod"))
	if err != nil {
		fatal(err)
	}
	out, err := flaregen.Generate(doc, cfg, flaregen.Options{Module: module})
	if err != nil {
		fatal(err)
	}
	if *verbose {
		for _, w := range out.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	}
	if *check {
		diffs, err := out.Diff(*root)
		if err != nil {
			fatal(err)
		}
		if len(diffs) > 0 {
			for _, d := range diffs {
				fmt.Fprintln(os.Stderr, d)
			}
			fatal(fmt.Errorf("generated files are out of date; run make generate-crds"))
		}
		return
	}
	if err := out.Write(*root); err != nil {
		fatal(err)
	}
	fmt.Printf("flaregen: %d kinds, %d files\n", len(out.Kinds), len(out.Files))
}

func modulePath(goMod string) (string, error) {
	f, err := os.Open(goMod)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.TrimSpace(m), nil
		}
	}
	return "", fmt.Errorf("%s: no module line", goMod)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "flaregen:", err)
	os.Exit(1)
}
