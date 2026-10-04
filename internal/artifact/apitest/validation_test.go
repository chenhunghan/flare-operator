package apitest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/chenhunghan/flare-operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, &env, testenv.Options{ExtraCRDPaths: []string{filepath.Join(testenv.RepoRoot(), "internal", "artifact", "apitest", "testdata")}})
}

// TestArtifactSourceValidation checks the OpenAPI patterns and the CEL rule of
// sharedv1alpha1.ArtifactSource against a real kube-apiserver.
func TestArtifactSourceValidation(t *testing.T) {
	if env == nil {
		t.Skip(testenv.MissingAssetsMessage())
	}
	ns := env.Namespace(t)
	sha := strings.Repeat("a", 64)
	for name, c := range map[string]struct {
		source string
		want   string // "" accepted, else a substring of the Invalid error
	}{
		"configMapRef":    {`{configMapRef: {configMaps: [{name: site}, {name: css, path: static/css, items: [{key: a.css, path: v1/a.css}]}]}}`, ""},
		"ociRef":          {`{ociRef: {image: "ghcr.io/o/site@sha256:` + sha + `", path: dist, pullSecretRef: {name: pull}}}`, ""},
		"url":             {`{url: {url: "https://example.com/site.tgz", sha256: "` + sha + `", path: public}}`, ""},
		"dot names":       {`{configMapRef: {configMaps: [{name: a, path: ".well-known/..x/..."}]}}`, ""},
		"none":            {`{}`, "set exactly one of configMapRef, ociRef or url"},
		"two":             {`{ociRef: {image: x}, url: {url: "https://e.x/a", sha256: "` + sha + `"}}`, "set exactly one of configMapRef, ociRef or url"},
		"no configMaps":   {`{configMapRef: {configMaps: []}}`, "configMaps"},
		"http url":        {`{url: {url: "http://example.com/a.tgz", sha256: "` + sha + `"}}`, "spec.source.url.url"},
		"bad sha256":      {`{url: {url: "https://example.com/a.tgz", sha256: "ABC"}}`, "spec.source.url.sha256"},
		"no sha256":       {`{url: {url: "https://example.com/a.tgz"}}`, "sha256"},
		"traversal":       {`{url: {url: "https://example.com/a.tgz", sha256: "` + sha + `", path: "a/../b"}}`, "spec.source.url.path"},
		"parent":          {`{ociRef: {image: x, path: ".."}}`, "spec.source.ociRef.path"},
		"absolute":        {`{ociRef: {image: x, path: "/dist"}}`, "spec.source.ociRef.path"},
		"dot segment":     {`{configMapRef: {configMaps: [{name: a, path: "./x"}]}}`, "path"},
		"trailing slash":  {`{configMapRef: {configMaps: [{name: a, path: "x/"}]}}`, "path"},
		"backslash":       {`{configMapRef: {configMaps: [{name: a, items: [{key: k, path: 'a\b'}]}]}}`, "path"},
		"empty item path": {`{configMapRef: {configMaps: [{name: a, items: [{key: k, path: ""}]}]}}`, "path"},
		"empty image":     {`{ociRef: {image: ""}}`, "spec.source.ociRef.image"},
		"empty secret":    {`{ociRef: {image: x, pullSecretRef: {name: ""}}}`, "spec.source.ociRef.pullSecretRef.name"},
	} {
		t.Run(name, func(t *testing.T) {
			u := &unstructured.Unstructured{}
			manifest := "apiVersion: artifacttest.flare.dev/v1\nkind: ArtifactHolder\nmetadata: {generateName: h-}\nspec: {source: " + c.source + "}\n"
			if err := yaml.Unmarshal([]byte(manifest), &u.Object); err != nil {
				t.Fatalf("manifest: %v\n%s", err, manifest)
			}
			u.SetNamespace(ns)
			err := env.Client.Create(testenv.Context(t, 10*time.Second), u)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("want accepted, got %v", err)
			case c.want != "" && err == nil:
				t.Errorf("want rejected (%q), but it was accepted", c.want)
			case c.want != "" && !apierrors.IsInvalid(err):
				t.Errorf("want an Invalid error, got %v", err)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Errorf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
