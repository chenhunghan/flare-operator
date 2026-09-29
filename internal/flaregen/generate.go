package flaregen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Options configures Generate.
type Options struct {
	// Module is the Go module path of the repository (imports of generated code).
	Module string
}

// Output is everything one generator run produces.
type Output struct {
	Files    map[string][]byte // repo-relative, slash-separated path → content
	Kinds    []*KindModel
	Warnings []string
}

// Output locations, relative to the repository root.
const (
	APIDir         = "api"
	CRDDir         = "config/crd/bases"
	DescriptorFile = "internal/generic/descriptors/zz_generated.go"
	// RBACFile carries the kubebuilder RBAC markers of the generic controllers
	// (make manifests collects them into config/rbac).
	RBACFile = "internal/generic/kinds/zz_generated.rbac.go"
	// FakeKindsFile lists the kinds flarefake emulates with its generic profile
	// (generator.yaml emulate: generic).
	FakeKindsFile = "internal/fake/zz_generated_generic.go"
)

// SelectResource finds the resource a generator.yaml entry names.
func SelectResource(resources []*Resource, kc KindConfig) (*Resource, error) {
	var matches []*Resource
	var inGroup []string
	for _, r := range resources {
		if r.FernGroup != kc.FernGroup {
			continue
		}
		inGroup = append(inGroup, r.Key())
		if kc.Path == "" || kc.Path == r.CollectionPath || (r.Singleton && kc.Path == r.ItemPath) {
			matches = append(matches, r)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) == 0:
		return nil, fmt.Errorf("fern group %q path %q: no resource (resources in group: %v)", kc.FernGroup, kc.Path, inGroup)
	default:
		return nil, fmt.Errorf("fern group %q has several resources, set path to one of: %v", kc.FernGroup, inGroup)
	}
}

// Generate runs the whole pipeline: model, Go types, CRDs, descriptors.
func Generate(doc *openapi3.T, cfg *Config, opts Options) (*Output, error) {
	if opts.Module == "" {
		return nil, fmt.Errorf("flaregen: Options.Module is required")
	}
	resources := Discover(doc)
	out := &Output{Files: map[string][]byte{}}
	seen := map[string]bool{}
	for _, kc := range cfg.Kinds {
		r, err := SelectResource(resources, kc)
		if err != nil {
			return nil, err
		}
		m, err := BuildKind(r, kc, cfg.GroupSuffix, cfg.Version)
		if err != nil {
			return nil, err
		}
		id := m.Group + "/" + m.Kind
		if seen[id] {
			return nil, fmt.Errorf("duplicate kind %s", id)
		}
		seen[id] = true
		out.Kinds = append(out.Kinds, m)
		for _, w := range m.Warnings {
			out.Warnings = append(out.Warnings, m.Kind+": "+w)
		}
	}

	// Go packages, one per product.
	byProduct := map[string][]*KindModel{}
	for _, m := range out.Kinds {
		byProduct[m.Product] = append(byProduct[m.Product], m)
	}
	for _, product := range sortedKeys(byProduct) {
		kinds := byProduct[product]
		dir := filepath.ToSlash(filepath.Join(APIDir, product, cfg.Version))
		pkg := newGoPkg()
		var kindNames, fernGroups []string
		for _, m := range kinds {
			pkg.used[m.Kind], pkg.used[m.Kind+"List"], pkg.used[m.Kind+"Spec"], pkg.used[m.Kind+"Status"] = true, true, true, true
		}
		for _, m := range kinds {
			start := len(pkg.structs)
			r := m.Resource
			k := &kindGo{m: m}
			k.params = pkg.addStruct(m.Params, m.Kind, "Parameters", paramsDoc(m))
			k.obs = pkg.addStruct(m.Observation, m.Kind, "Observation",
				fmt.Sprintf("%sObservation is the %s as returned by %s %s.", m.Kind, m.Kind, opMethod(r.Get), opPath(r.Get)))
			src, err := emitTypesFile(opts.Module, k, pkg.structs[start:])
			if err != nil {
				return nil, err
			}
			out.Files[dir+"/"+strings.ToLower(m.Kind)+"_types.go"] = src
			kindNames = append(kindNames, m.Kind)
			if !contains(fernGroups, r.FernGroup) {
				fernGroups = append(fernGroups, r.FernGroup)
			}
		}
		sort.Strings(fernGroups)
		gv, err := emitGroupVersion(product, kinds[0].Group, cfg.Version, fernGroups, kindNames)
		if err != nil {
			return nil, err
		}
		out.Files[dir+"/groupversion_info.go"] = gv
		dc, err := emitDeepCopy(cfg.Version, pkg.structs, kindNames)
		if err != nil {
			return nil, err
		}
		out.Files[dir+"/zz_generated.deepcopy.go"] = dc
	}

	shortNames := map[string]string{}
	for _, m := range out.Kinds {
		for _, sn := range m.ShortNames {
			if other, dup := shortNames[sn]; dup {
				return nil, fmt.Errorf("shortName %q of %s is also used by %s", sn, m.Kind, other)
			}
			shortNames[sn] = m.Kind
		}
		crd := BuildCRD(m)
		if err := ValidateCRD(crd); err != nil {
			return nil, err
		}
		if err := checkPrintColumns(m, crd.Spec.Versions[0].Schema.OpenAPIV3Schema); err != nil {
			return nil, err
		}
		y, err := MarshalCRD(crd)
		if err != nil {
			return nil, err
		}
		out.Files[CRDDir+"/"+m.Group+"_"+m.Plural+".yaml"] = y
	}

	desc, err := emitDescriptors(opts.Module, out.Kinds)
	if err != nil {
		return nil, err
	}
	out.Files[DescriptorFile] = desc
	rbac, err := emitRBAC(out.Kinds)
	if err != nil {
		return nil, err
	}
	out.Files[RBACFile] = rbac
	fk, err := emitFakeKinds(out.Kinds)
	if err != nil {
		return nil, err
	}
	out.Files[FakeKindsFile] = fk
	return out, nil
}

func opMethod(op *Operation) string {
	if op == nil {
		return ""
	}
	return op.Method
}

func opPath(op *Operation) string {
	if op == nil {
		return ""
	}
	return op.Path
}

func paramsDoc(m *KindModel) string {
	r := m.Resource
	var ops []string
	if r.Create != nil {
		ops = append(ops, r.Create.Method+" "+r.Create.Path)
	}
	for _, op := range []*Operation{r.Edit, r.Update} {
		if op != nil && op.Method == m.Descriptor.UpdateMethod {
			ops = append(ops, op.Method+" "+op.Path)
			break
		}
	}
	return fmt.Sprintf("%sParameters are the configurable fields of a %s: the request\nbodies of %s.", m.Kind, m.Kind, strings.Join(ops, " and "))
}

func isGenerated(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := f.Read(head)
	return bytes.Contains(head[:n], []byte(GeneratedHeader))
}

// existingGenerated lists repo-relative files under root that carry the
// flaregen header in the locations flaregen writes to.
func existingGenerated(root string) ([]string, error) {
	var out []string
	patterns := []string{
		filepath.Join(root, APIDir, "*", "*", "*.go"),
		filepath.Join(root, CRDDir, "*.yaml"),
		filepath.Join(root, filepath.FromSlash(DescriptorFile)),
		filepath.Join(root, filepath.FromSlash(RBACFile)),
		filepath.Join(root, filepath.FromSlash(FakeKindsFile)),
	}
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if isGenerated(m) {
				rel, err := filepath.Rel(root, m)
				if err != nil {
					return nil, err
				}
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Diff compares the output with the files under root: changed or missing files
// and stale generated files, as sorted repo-relative paths with a reason.
func (o *Output) Diff(root string) ([]string, error) {
	var diffs []string
	for _, p := range sortedKeys(o.Files) {
		cur, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		switch {
		case os.IsNotExist(err):
			diffs = append(diffs, p+": missing")
		case err != nil:
			return nil, err
		case !bytes.Equal(cur, o.Files[p]):
			diffs = append(diffs, p+": out of date")
		}
	}
	existing, err := existingGenerated(root)
	if err != nil {
		return nil, err
	}
	for _, p := range existing {
		if _, ok := o.Files[p]; !ok {
			diffs = append(diffs, p+": stale (no longer generated)")
		}
	}
	return diffs, nil
}

// Write writes the output under root and removes stale generated files.
func (o *Output) Write(root string) error {
	existing, err := existingGenerated(root)
	if err != nil {
		return err
	}
	for _, p := range existing {
		if _, ok := o.Files[p]; !ok {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(p))); err != nil {
				return err
			}
		}
	}
	for _, p := range sortedKeys(o.Files) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if cur, err := os.ReadFile(full); err == nil && bytes.Equal(cur, o.Files[p]) {
			continue
		}
		if err := os.WriteFile(full, o.Files[p], 0o644); err != nil {
			return err
		}
	}
	return nil
}
