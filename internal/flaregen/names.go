package flaregen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// DefaultGroup is the API group of every flare-operator kind (generator.yaml group).
const DefaultGroup = "flare.dev"

// KindNames are the names one kind takes in the API group: its Kind, and the resource names
// the API server and kubectl resolve to it (plural, singular, short names).
type KindNames struct {
	Kind       string
	Plural     string
	Singular   string
	ShortNames []string
	// Source says where the kind comes from, for error messages: a generator.yaml entry or a
	// hand-written CRD file.
	Source string
	// Product is the kind's product category (informational; "" when it has none).
	Product string
}

// NamesOf returns the names of a generated kind.
func NamesOf(m *KindModel) KindNames {
	return KindNames{Kind: m.Kind, Plural: m.Plural, Singular: strings.ToLower(m.Kind), ShortNames: m.ShortNames,
		Source: "generator.yaml " + m.Resource.Key(), Product: m.Product}
}

// NameCollision is one name that several kinds of the API group claim.
type NameCollision struct {
	// What is "kind" (two kinds with the same Kind) or "resource name" (a plural, singular or
	// short name of one kind equals one of another kind).
	What  string
	Name  string
	Kinds []KindNames // at least two, in input order
}

func (c NameCollision) String() string {
	var who []string
	for _, k := range c.Kinds {
		who = append(who, fmt.Sprintf("%s (%s)", k.Kind, k.Source))
	}
	return fmt.Sprintf("%s %q is claimed by %s", c.What, c.Name, strings.Join(who, " and "))
}

// CheckNames reports every name that two or more kinds share. All kinds share one API group,
// so a Kind must be unique, and a plural, singular or short name may belong to one kind only
// (kubectl and the API server resolve all three to a resource). A kind's own names may repeat
// (a plural equal to its singular). The result is sorted by What, then Name.
func CheckNames(kinds []KindNames) []NameCollision {
	type claim struct {
		what, name string
	}
	owners := map[claim][]int{}
	add := func(c claim, i int) {
		if o := owners[c]; len(o) > 0 && o[len(o)-1] == i {
			return
		}
		owners[c] = append(owners[c], i)
	}
	for i, k := range kinds {
		add(claim{"kind", k.Kind}, i)
		for _, n := range append([]string{k.Plural, k.Singular}, k.ShortNames...) {
			if n != "" {
				add(claim{"resource name", n}, i)
			}
		}
	}
	var out []NameCollision
	for c, idx := range owners {
		if len(idx) < 2 {
			continue
		}
		nc := NameCollision{What: c.what, Name: c.name}
		for _, i := range idx {
			nc.Kinds = append(nc.Kinds, kinds[i])
		}
		out = append(out, nc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].What != out[j].What {
			return out[i].What < out[j].What
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CollisionError is the error Generate returns when kinds share a name in the API group.
type CollisionError struct {
	Group      string
	Collisions []NameCollision
}

func (e *CollisionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d name collision(s) in API group %s:\n", len(e.Collisions), e.Group)
	for _, c := range e.Collisions {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	b.WriteString("every kind shares the API group, so its Kind, plural, singular and short names must be unique.\n" +
		"Fix: give the NEW kind an explicit name in generator.yaml (kind: and plural:, e.g. prefixed with its\n" +
		"product: kind: ZeroTrustPolicy, plural: zerotrustpolicies) or other shortNames; flaregen never renames\n" +
		"a kind by itself, and an existing kind must keep its names (renaming one breaks every object of it).")
	return b.String()
}

// LoadHandWrittenNames reads the names of the hand-written kinds of group from the CRDs in dir
// (config/crd/bases, written by controller-gen): every CRD file without the flaregen header.
func LoadHandWrittenNames(dir, group string) ([]KindNames, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []KindNames
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(b, []byte(GeneratedHeader)) {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(b, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Spec.Group != group {
			continue
		}
		n := crd.Spec.Names
		singular := n.Singular
		if singular == "" {
			singular = strings.ToLower(n.Kind)
		}
		product := ""
		for _, c := range n.Categories {
			if c != CategoryCloudflare && c != CategoryManaged {
				product = c
			}
		}
		out = append(out, KindNames{Kind: n.Kind, Plural: n.Plural, Singular: singular, ShortNames: n.ShortNames,
			Source: "hand-written CRD " + filepath.ToSlash(f), Product: product})
	}
	return out, nil
}
