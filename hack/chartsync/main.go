// Command chartsync keeps charts/flare-operator in step with the generated manifests, so the chart
// is never maintained by hand:
//
//   - config/crd/bases/*.yaml are copied verbatim into charts/flare-operator/crds/ (stale files
//     there are removed);
//   - the rules of the ClusterRole in config/rbac/role.yaml (controller-gen output) are rendered
//     into charts/flare-operator/templates/clusterrole-manager.yaml;
//   - the CRD groups and plurals are rendered into aggregated view/edit ClusterRoles
//     (templates/clusterrole-aggregate.yaml), Crossplane-style.
//
// Run it with `make chart-sync`; `make chart-check` (-check) writes nothing and exits 1 when the
// chart is out of date.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	crdSrcDir   = "config/crd/bases"
	roleSrc     = "config/rbac/role.yaml"
	chartDir    = "charts/flare-operator"
	crdDstDir   = chartDir + "/crds"
	roleDst     = chartDir + "/templates/clusterrole-manager.yaml"
	aggregateTo = chartDir + "/templates/clusterrole-aggregate.yaml"
)

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "write nothing; exit 1 if the chart is out of sync")
	flag.Parse()

	want, err := Render(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chartsync:", err)
		os.Exit(2)
	}
	diffs, err := Diff(*root, want)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chartsync:", err)
		os.Exit(2)
	}
	if *check {
		if len(diffs) > 0 {
			fmt.Fprintln(os.Stderr, "chart out of sync with config/ (run `make chart-sync`):")
			for _, d := range diffs {
				fmt.Fprintln(os.Stderr, "  "+d)
			}
			os.Exit(1)
		}
		fmt.Println("chart in sync")
		return
	}
	if err := Apply(*root, want); err != nil {
		fmt.Fprintln(os.Stderr, "chartsync:", err)
		os.Exit(2)
	}
	for _, d := range diffs {
		fmt.Println(d)
	}
	fmt.Printf("chart synced (%d change(s))\n", len(diffs))
}

// Files is the desired content of every managed chart file, keyed by repo-relative path.
type Files map[string][]byte

// Render computes the managed chart files from config/ under root.
func Render(root string) (Files, error) {
	out := Files{}
	crdPaths, err := filepath.Glob(filepath.Join(root, crdSrcDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(crdPaths) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", crdSrcDir)
	}
	var crds []crdInfo
	for _, p := range crdPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out[filepath.Join(crdDstDir, filepath.Base(p))] = b
		infos, err := parseCRDs(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		crds = append(crds, infos...)
	}

	roleYAML, err := os.ReadFile(filepath.Join(root, roleSrc))
	if err != nil {
		return nil, err
	}
	rules, err := parseRules(roleYAML)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", roleSrc, err)
	}
	role, err := renderManagerRole(rules)
	if err != nil {
		return nil, err
	}
	out[roleDst] = role
	out[aggregateTo] = renderAggregateRoles(crds)
	return out, nil
}

// Diff lists the managed files whose content differs from want, plus stale chart CRDs.
func Diff(root string, want Files) ([]string, error) {
	var diffs []string
	for _, p := range sortedKeys(want) {
		have, err := os.ReadFile(filepath.Join(root, p))
		switch {
		case os.IsNotExist(err):
			diffs = append(diffs, "missing: "+p)
		case err != nil:
			return nil, err
		case !bytes.Equal(have, want[p]):
			diffs = append(diffs, "differs: "+p)
		}
	}
	stale, err := staleCRDs(root, want)
	if err != nil {
		return nil, err
	}
	for _, p := range stale {
		diffs = append(diffs, "stale:   "+p)
	}
	return diffs, nil
}

// Apply writes want and removes stale chart CRDs.
func Apply(root string, want Files) error {
	for _, p := range sortedKeys(want) {
		dst := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, want[p], 0o644); err != nil {
			return err
		}
	}
	stale, err := staleCRDs(root, want)
	if err != nil {
		return err
	}
	for _, p := range stale {
		if err := os.Remove(filepath.Join(root, p)); err != nil {
			return err
		}
	}
	return nil
}

func staleCRDs(root string, want Files) ([]string, error) {
	have, err := filepath.Glob(filepath.Join(root, crdDstDir, "*"))
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, p := range have {
		rel := filepath.Join(crdDstDir, filepath.Base(p))
		if _, ok := want[rel]; !ok {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func sortedKeys(f Files) []string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitDocs splits a multi-document YAML stream, dropping empty documents.
func splitDocs(b []byte) [][]byte {
	var docs [][]byte
	for _, d := range bytes.Split(append([]byte("\n"), b...), []byte("\n---")) {
		if len(bytes.TrimSpace(stripComments(d))) > 0 {
			docs = append(docs, d)
		}
	}
	return docs
}

func stripComments(b []byte) []byte {
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(l), []byte("#")) {
			out = append(out, l)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

// parseRules returns the rules of the single ClusterRole in a controller-gen RBAC file. A
// namespaced Role (from a +kubebuilder:rbac marker with namespace=) is rejected: the chart has no
// generated template for it yet.
func parseRules(b []byte) ([]rbacv1.PolicyRule, error) {
	var rules []rbacv1.PolicyRule
	found := 0
	for _, d := range splitDocs(b) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(d, &meta); err != nil {
			return nil, err
		}
		if meta.Kind != "ClusterRole" {
			return nil, fmt.Errorf("unsupported kind %q (chartsync renders only the manager ClusterRole)", meta.Kind)
		}
		var cr rbacv1.ClusterRole
		if err := yaml.UnmarshalStrict(d, &cr); err != nil {
			return nil, err
		}
		rules = append(rules, cr.Rules...)
		found++
	}
	if found != 1 {
		return nil, fmt.Errorf("want exactly one ClusterRole, found %d", found)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("ClusterRole has no rules")
	}
	return rules, nil
}

const header = `{{- /* Code generated by hack/chartsync (make chart-sync) from %s. DO NOT EDIT. */ -}}
`

func renderManagerRole(rules []rbacv1.PolicyRule) ([]byte, error) {
	ry, err := yaml.Marshal(rules)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(ry, []byte("{{")) {
		return nil, fmt.Errorf("RBAC rules contain template delimiters")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, header, roleSrc)
	b.WriteString(`{{- if .Values.rbac.create }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "flare-operator.fullname" . }}-manager
  labels:
    {{- include "flare-operator.labels" . | nindent 4 }}
rules:
`)
	b.Write(ry)
	b.WriteString("{{- end }}\n")
	return b.Bytes(), nil
}

type crdInfo struct{ Group, Plural string }

func parseCRDs(b []byte) ([]crdInfo, error) {
	var infos []crdInfo
	for _, d := range splitDocs(b) {
		var crd struct {
			Kind string `json:"kind"`
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Plural string `json:"plural"`
				} `json:"names"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(d, &crd); err != nil {
			return nil, err
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Spec.Group == "" || crd.Spec.Names.Plural == "" {
			return nil, fmt.Errorf("not a CustomResourceDefinition with group and plural")
		}
		infos = append(infos, crdInfo{crd.Spec.Group, crd.Spec.Names.Plural})
	}
	return infos, nil
}

// renderAggregateRoles emits view and edit ClusterRoles for every CRD, labelled so Kubernetes
// aggregates them into the default view/edit/admin roles.
func renderAggregateRoles(crds []crdInfo) []byte {
	byGroup := map[string][]string{}
	for _, c := range crds {
		byGroup[c.Group] = append(byGroup[c.Group], c.Plural)
	}
	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
		sort.Strings(byGroup[g])
	}
	sort.Strings(groups)

	rules := func(verbs []string, withStatus bool) string {
		var s strings.Builder
		for _, g := range groups {
			fmt.Fprintf(&s, "- apiGroups:\n  - %s\n  resources:\n", g)
			for _, p := range byGroup[g] {
				fmt.Fprintf(&s, "  - %s\n", p)
				if withStatus {
					fmt.Fprintf(&s, "  - %s/status\n", p)
				}
			}
			s.WriteString("  verbs:\n")
			for _, v := range verbs {
				fmt.Fprintf(&s, "  - %s\n", v)
			}
		}
		return s.String()
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, header, crdSrcDir)
	b.WriteString(`{{- if and .Values.rbac.create .Values.rbac.aggregateToDefaultRoles }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "flare-operator.fullname" . }}-aggregate-to-view
  labels:
    {{- include "flare-operator.labels" . | nindent 4 }}
    rbac.authorization.k8s.io/aggregate-to-view: "true"
rules:
`)
	b.WriteString(rules([]string{"get", "list", "watch"}, true))
	b.WriteString(`---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "flare-operator.fullname" . }}-aggregate-to-edit
  labels:
    {{- include "flare-operator.labels" . | nindent 4 }}
    rbac.authorization.k8s.io/aggregate-to-edit: "true"
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
rules:
`)
	b.WriteString(rules([]string{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"}, false))
	b.WriteString("{{- end }}\n")
	return b.Bytes()
}
