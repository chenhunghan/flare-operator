package flaregen

import (
	"bytes"
	"fmt"
	"sort"
)

// emitFakeKinds renders internal/fake/zz_generated_generic.go: the kinds that
// generator.yaml marks `emulate: generic`, as fake.GenericKind values. The
// emulator package cannot import internal/generic (it would pull the operator
// runtime into cmd/flarefake), so the descriptor fields it needs are repeated.
func emitFakeKinds(kinds []*KindModel) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// %s\n\npackage fake\n\n", GeneratedHeader)
	b.WriteString("// generatedGenericKinds are the generator.yaml kinds marked `emulate: generic`. flarefake\n")
	b.WriteString("// emulates them with the descriptor-driven generic profile (generic.go) when they are passed in\n")
	b.WriteString("// Options.Generic; GeneratedGenericKinds returns a copy.\n")
	b.WriteString("var generatedGenericKinds = []GenericKind{\n")
	sorted := append([]*KindModel(nil), kinds...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Group != sorted[j].Group {
			return sorted[i].Group < sorted[j].Group
		}
		return sorted[i].Kind < sorted[j].Kind
	})
	for _, m := range sorted {
		if m.Emulate != "generic" {
			continue
		}
		d := m.Descriptor
		b.WriteString("\t{\n")
		fmt.Fprintf(&b, "\t\tGroup: %q, Kind: %q, Scope: %q,\n", d.Group, d.Kind, d.Scope)
		for _, kv := range [][2]string{{"CreatePath", d.CreatePath}, {"ItemPath", d.ItemPath}, {"ListPath", d.ListPath},
			{"IDField", d.IDField}, {"NameField", d.NameField}, {"UpdateMethod", d.UpdateMethod}} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "\t\t%s: %q,\n", kv[0], kv[1])
			}
		}
		if len(d.WriteOnly) > 0 {
			fmt.Fprintf(&b, "\t\tWriteOnly: %s,\n", goStrings(d.WriteOnly))
		}
		if d.Singleton {
			b.WriteString("\t\tSingleton: true,\n")
		}
		b.WriteString(extensionLiteral("", "GenericHeader", "GenericSubResource", "Headers", "SubResources", m.Extension, "\t\t"))
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	return formatGo(b.Bytes())
}
