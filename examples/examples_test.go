// Package examples_test checks that every manifest in examples/ is accepted by a real API server
// (envtest) with the CRDs of config/crd/bases: OpenAPI schema, CEL rules and strict field
// validation (unknown fields fail), through a server-side dry-run create. It also requires an
// example for every kind in config/crd/bases. For examples/fullstack it validates every file
// and the kustomize output, and runs the whole app against the operator's controllers and
// flarefake (fullstack_test.go).
package examples_test

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	flag.Parse()
	opts := testenv.Options{}
	if !testing.Short() {
		// R2 buckets (TestFullStack) are served by the fake's generic profile, which loads the
		// pinned spec; TestFullStack skips with -short. The fake also checks every request
		// against the spec (as the e2e flarefake does), and TestFullStack fails on violations.
		spec, err := fake.LoadDefaultSpec()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load spec:", err)
			os.Exit(1)
		}
		opts.Fake.Spec = spec
		opts.Fake.Generic = fake.GeneratedGenericKinds()
	}
	testenv.Main(m, &env, opts)
}

// docs returns the non-empty YAML documents of file.
func docs(t *testing.T, file string) []*unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	for i := 0; ; i++ {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: document %d: %v", file, i, err)
		}
		var m map[string]any
		if err := yaml.UnmarshalStrict(raw, &m); err != nil {
			t.Fatalf("%s: document %d: %v", file, i, err)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
}

// crdKinds returns "<group>/<kind>" of every CRD in config/crd/bases.
func crdKinds(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testenv.RepoRoot(), "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs found: %v", err)
	}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, crd.Spec.Group+"/"+crd.Spec.Names.Kind)
	}
	sort.Strings(out)
	return out
}

func exampleFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	return files
}

// TestEveryKindHasAnExample fails when a CRD kind has no manifest in examples/.
func TestEveryKindHasAnExample(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range exampleFiles(t) {
		for _, u := range docs(t, f) {
			seen[u.GroupVersionKind().Group+"/"+u.GetKind()] = true
		}
	}
	for _, k := range crdKinds(t) {
		if !seen[k] {
			t.Errorf("no example for %s in examples/", k)
		}
	}
}

// TestExamplesValidate dry-run creates every example document with strict field validation.
func TestExamplesValidate(t *testing.T) {
	e := testenv.Require(t, env)
	ctx := testenv.Context(t, time.Minute)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "examples"}}
	if err := e.Client.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	for _, f := range exampleFiles(t) {
		for i, u := range docs(t, f) {
			t.Run(fmt.Sprintf("%s#%d-%s", strings.TrimSuffix(f, ".yaml"), i, u.GetName()), func(t *testing.T) {
				if u.GetNamespace() != "" {
					t.Fatalf("examples must not set metadata.namespace (got %q)", u.GetNamespace())
				}
				u.SetNamespace(ns.Name)
				if err := e.Client.Create(ctx, u, client.DryRunAll, client.FieldValidation("Strict")); err != nil {
					t.Fatalf("%s %s: %v", u.GetKind(), u.GetName(), err)
				}
			})
		}
	}
}
