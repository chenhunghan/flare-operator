package flaregen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

func goStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[]string{" + strings.Join(q, ", ") + "}"
}

func pkgAlias(product, version string) string { return product + version }

// emitDescriptors renders internal/generic/descriptors/zz_generated.go.
func emitDescriptors(module string, kinds []*KindModel) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// %s\n\npackage descriptors\n\n", GeneratedHeader)
	imports := map[string]string{}
	for _, m := range kinds {
		imports[pkgAlias(m.Product, m.Version)] = fmt.Sprintf("%s/api/%s/%s", module, m.Product, m.Version)
	}
	b.WriteString("import (\n\t\"k8s.io/apimachinery/pkg/runtime\"\n\n")
	fmt.Fprintf(&b, "\tcommonv1alpha1 %q\n", module+"/api/common/v1alpha1")
	for _, a := range sortedKeys(imports) {
		fmt.Fprintf(&b, "\t%s %q\n", a, imports[a])
	}
	fmt.Fprintf(&b, "\t%q\n)\n\n", module+"/internal/generic")

	sorted := append([]*KindModel(nil), kinds...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Group != sorted[j].Group {
			return sorted[i].Group < sorted[j].Group
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	b.WriteString("var generated = []Entry{\n")
	for _, m := range sorted {
		d := m.Descriptor
		a := pkgAlias(m.Product, m.Version)
		b.WriteString("\t{\n\t\tDescriptor: generic.Descriptor{\n")
		fmt.Fprintf(&b, "\t\t\tGroup: %q, Version: %q, Kind: %q,\n", d.Group, d.Version, d.Kind)
		fmt.Fprintf(&b, "\t\t\tScope: %q,\n", d.Scope)
		for _, kv := range [][2]string{{"CreatePath", d.CreatePath}, {"ItemPath", d.ItemPath}, {"ListPath", d.ListPath},
			{"IDField", d.IDField}, {"NameField", d.NameField}, {"UpdateMethod", d.UpdateMethod}} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "\t\t\t%s: %q,\n", kv[0], kv[1])
			}
		}
		if len(d.Immutable) > 0 {
			fmt.Fprintf(&b, "\t\t\tImmutable: %s,\n", goStrings(d.Immutable))
		}
		if len(d.WriteOnly) > 0 {
			fmt.Fprintf(&b, "\t\t\tWriteOnly: %s,\n", goStrings(d.WriteOnly))
		}
		if d.Singleton {
			b.WriteString("\t\t\tSingleton: true,\n")
		}
		if d.ListOrder != "" {
			fmt.Fprintf(&b, "\t\t\tListOrder: %q,\n", d.ListOrder)
		}
		if len(d.CreateFields) > 0 {
			fmt.Fprintf(&b, "\t\t\tCreateFields: %s,\n", goStrings(d.CreateFields))
		}
		if len(d.UpdateFields) > 0 {
			fmt.Fprintf(&b, "\t\t\tUpdateFields: %s,\n", goStrings(d.UpdateFields))
		}
		if d.TagResourceType != "" {
			fmt.Fprintf(&b, "\t\t\tTagResourceType: %q,\n", d.TagResourceType)
		}
		fmt.Fprintf(&b, "\t\t\tDefaultDeletionPolicy: %q,\n\t\t},\n", d.DefaultDeletionPolicy)
		fmt.Fprintf(&b, "\t\tFernGroup: %q,\n", m.Resource.FernGroup)
		fmt.Fprintf(&b, "\t\tNew: func() commonv1alpha1.Managed { return &%s.%s{} },\n", a, m.Kind)
		fmt.Fprintf(&b, "\t\tNewList: func() runtime.Object { return &%s.%sList{} },\n\t},\n", a, m.Kind)
	}
	b.WriteString("}\n\nvar schemeBuilder = runtime.SchemeBuilder{\n")
	for _, a := range sortedKeys(imports) {
		fmt.Fprintf(&b, "\t%s.AddToScheme,\n", a)
	}
	b.WriteString("}\n")
	return formatGo(b.Bytes())
}

// emitRBAC renders internal/generic/kinds/zz_generated.rbac.go: the RBAC markers the
// generic controllers of the generated kinds need.
func emitRBAC(kinds []*KindModel) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// %s\n\npackage kinds\n\n", GeneratedHeader)
	b.WriteString("// RBAC of the generic controllers, one block per generated kind.\n//\n")
	sorted := append([]*KindModel(nil), kinds...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Group != sorted[j].Group {
			return sorted[i].Group < sorted[j].Group
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	for _, m := range sorted {
		fmt.Fprintf(&b, "// +kubebuilder:rbac:groups=%s,resources=%s,verbs=get;list;watch;update;patch\n", m.Group, m.Plural)
		fmt.Fprintf(&b, "// +kubebuilder:rbac:groups=%s,resources=%s/status,verbs=get;update;patch\n", m.Group, m.Plural)
		fmt.Fprintf(&b, "// +kubebuilder:rbac:groups=%s,resources=%s/finalizers,verbs=update\n", m.Group, m.Plural)
	}
	b.WriteString("// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch\n")
	b.WriteString("// +kubebuilder:rbac:groups=\"\",resources=secrets,verbs=get;list;watch\n")
	b.WriteString("\n// generatedRBAC documents that the free-floating markers above are generated\n// (controller-gen collects them from ./internal/generic/kinds).\nconst generatedRBAC = true\n")
	return formatGo(b.Bytes())
}
