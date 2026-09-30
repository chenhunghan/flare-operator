package kustomizelite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: app
resources: [ns.yaml, cm.yaml, sub]
generatorOptions:
  disableNameSuffixHash: true
  labels: {a: "1"}
configMapGenerator:
  - name: site
    files: [public/index.html, renamed.js=src/main.js, bin/blob]
    options:
      labels: {b: "2"}
`,
		"ns.yaml":                "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other\n",
		"cm.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plain\n---\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: cr\n",
		"public/index.html":      "<h1>hi</h1>\n",
		"src/main.js":            "export {}\n",
		"bin/blob":               "\xff\xfe",
		"sub/kustomization.yaml": "resources: [s.yaml]\n",
		"sub/s.yaml":             "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n",
	})
	objs, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range objs {
		got[o.GetKind()+"/"+o.GetName()] = o.GetNamespace()
	}
	want := map[string]string{"Namespace/app": "", "ConfigMap/plain": "app", "ClusterRole/cr": "", "Secret/s": "app", "ConfigMap/site": "app"}
	if len(got) != len(want) {
		t.Fatalf("objects %v, want %v", got, want)
	}
	for k, ns := range want {
		if g, ok := got[k]; !ok || g != ns {
			t.Errorf("%s: namespace %q (present %v), want %q", k, g, ok, ns)
		}
	}
	site := objs[len(objs)-1]
	if site.GetName() != "site" {
		t.Fatalf("last object %s, want the generated ConfigMap", site.GetName())
	}
	if l := site.GetLabels(); l["a"] != "1" || l["b"] != "2" {
		t.Errorf("labels %v", l)
	}
	data := site.Object["data"].(map[string]any)
	if data["index.html"] != "<h1>hi</h1>\n" || data["renamed.js"] != "export {}\n" || len(data) != 2 {
		t.Errorf("data %v", data)
	}
	if b := site.Object["binaryData"].(map[string]any); b["blob"] != "//4=" {
		t.Errorf("binaryData %v", b)
	}
}

func TestBuildRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ kustomization, want string }{
		"unknown field":    {"commonLabels: {a: b}\n", "unknown field"},
		"hashed names":     {"configMapGenerator:\n  - name: x\n    files: [f]\n", "disableNameSuffixHash"},
		"file outside":     {"generatorOptions: {disableNameSuffixHash: true}\nconfigMapGenerator:\n  - name: x\n    files: [../f]\n", "outside"},
		"resource outside": {"resources: [../r.yaml]\n", "outside"},
		"duplicate object": {"resources: [f, f]\n", "defined twice"},
		"wrong kind":       {"kind: Component\n", "kind"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, map[string]string{"kustomization.yaml": tc.kustomization, "f": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: f\n"})
			if _, err := Build(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
