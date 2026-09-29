package flaregen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

type printerColumn struct {
	Name, Type, JSONPath string
	Priority             int32
	Description          string
}

// printerColumns are the kubectl columns of a kind: READY, SYNCED and EXTERNAL-ID (the same for
// every managed kind, hand-written ones included), the kind's own generator.yaml printColumns,
// then AGE.
func printerColumns(m *KindModel) []printerColumn {
	cols := []printerColumn{
		{Name: "READY", Type: "string", JSONPath: ".status.conditions[?(@.type=='Ready')].status"},
		{Name: "SYNCED", Type: "string", JSONPath: ".status.conditions[?(@.type=='Synced')].status"},
		{Name: "EXTERNAL-ID", Type: "string", JSONPath: ".status.id"},
	}
	for _, c := range m.PrintColumns {
		cols = append(cols, printerColumn(c))
	}
	return append(cols, printerColumn{Name: "AGE", Type: "date", JSONPath: ".metadata.creationTimestamp"})
}

// Categories every managed kind belongs to: `kubectl get cloudflare` lists every flare-operator
// object (CloudflareAccount included), `kubectl get managed` every managed resource, as with
// Crossplane providers. Each kind is also in the category of its product (e.g. queues).
const (
	CategoryCloudflare = "cloudflare"
	CategoryManaged    = "managed"
)

// AccountRefRule is the CEL rule on spec that every managed kind (generated or hand-written)
// carries: accountRef.name must be a non-empty object name.
const AccountRefRule = "size(self.accountRef.name) > 0 && size(self.accountRef.name) <= 253"

// AccountRefMessage is the message of AccountRefRule.
const AccountRefMessage = "accountRef.name must name a CloudflareAccount in this namespace (1-253 characters)"

func props(t *Type) apiextensionsv1.JSONSchemaProps {
	s := apiextensionsv1.JSONSchemaProps{Description: t.Description}
	switch t.Kind {
	case KString:
		s.Type = "string"
	case KInteger:
		s.Type, s.Format = "integer", "int64"
	case KNumber:
		s.Type = "number"
	case KBool:
		s.Type = "boolean"
	case KObject:
		s.Type = "object"
		s.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		for _, f := range t.Fields {
			p := props(f.Type)
			if f.Description != "" {
				p.Description = f.Description
			}
			if f.Deprecated {
				p.Description = strings.TrimSpace(p.Description + " (deprecated in the Cloudflare API)")
			}
			s.Properties[f.JSONName] = p
			if f.Required {
				s.Required = append(s.Required, f.JSONName)
			}
		}
	case KArray:
		s.Type = "array"
		e := props(t.Elem)
		s.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &e}
		s.MinItems, s.MaxItems = t.MinItems, t.MaxItems
	case KMap:
		s.Type = "object"
		e := props(t.Elem)
		s.AdditionalProperties = &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &e}
	case KAny:
		s.XPreserveUnknownFields = ptr(true)
		if t.AnyObject {
			s.Type = "object"
		}
	}
	for _, e := range t.Enum {
		raw, _ := json.Marshal(e)
		s.Enum = append(s.Enum, apiextensionsv1.JSON{Raw: raw})
	}
	s.Pattern = t.Pattern
	s.MinLength, s.MaxLength = t.MinLength, t.MaxLength
	s.Minimum, s.Maximum = t.Minimum, t.Maximum
	s.ExclusiveMinimum, s.ExclusiveMaximum = t.ExclusiveMinimum, t.ExclusiveMaximum
	return s
}

func ptr[T any](v T) *T { return &v }

func str(desc string) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "string", Description: desc}
}

func enumOf(vals ...string) []apiextensionsv1.JSON {
	out := make([]apiextensionsv1.JSON, len(vals))
	for i, v := range vals {
		out[i] = apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf("%q", v))}
	}
	return out
}

// conditionSchema mirrors what controller-gen emits for metav1.Condition.
func conditionSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type:        "object",
		Description: "Condition contains details for one aspect of the current state of this API Resource.",
		Required:    []string{"lastTransitionTime", "message", "reason", "status", "type"},
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"lastTransitionTime": {Type: "string", Format: "date-time", Description: "lastTransitionTime is the last time the condition transitioned from one status to another."},
			"message":            {Type: "string", MaxLength: ptr[int64](32768), Description: "message is a human readable message indicating details about the transition."},
			"observedGeneration": {Type: "integer", Format: "int64", Minimum: ptr[float64](0), Description: "observedGeneration represents the .metadata.generation that the condition was set based upon."},
			"reason": {Type: "string", MaxLength: ptr[int64](1024), MinLength: ptr[int64](1),
				Pattern: `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`, Description: "reason contains a programmatic identifier indicating the reason for the condition's last transition."},
			"status": {Type: "string", Enum: enumOf("True", "False", "Unknown"), Description: "status of the condition, one of True, False, Unknown."},
			"type": {Type: "string", MaxLength: ptr[int64](316),
				Pattern:     `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`,
				Description: "type of condition in CamelCase or in foo.example.com/CamelCase."},
		},
	}
}

// commonSpecProps is the schema of commonv1alpha1.ResourceSpec (inlined into spec).
// TestCommonSchemaMatchesGoTypes keeps it in sync with the Go types.
func commonSpecProps(defaultDeletion string) map[string]apiextensionsv1.JSONSchemaProps {
	dp := apiextensionsv1.JSONSchemaProps{Type: "string", Enum: enumOf("Delete", "Orphan"),
		Description: "DeletionPolicy says what happens to the Cloudflare resource when this object is deleted."}
	if defaultDeletion != "" {
		dp.Default = &apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf("%q", defaultDeletion))}
	}
	return map[string]apiextensionsv1.JSONSchemaProps{
		"accountRef": {Type: "object", Description: "AccountRef names the CloudflareAccount (same namespace) to use.",
			Required:   []string{"name"},
			Properties: map[string]apiextensionsv1.JSONSchemaProps{"name": str("")}},
		"zoneRef": {Type: "object", Description: "ZoneRef is required for zone-scoped kinds and ignored otherwise. Exactly one of id or name.",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{"id": str(""), "name": str("")}},
		"deletionPolicy": dp,
		"managementPolicies": {Type: "array", Description: `ManagementPolicies default to ["*"]. ["Observe"] makes the object read-only.`,
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{Type: "string",
				Enum: enumOf("Observe", "Create", "Update", "Delete", "LateInitialize", "*")}}},
	}
}

// commonStatusProps is the schema of commonv1alpha1.ResourceStatus (inlined into status).
func commonStatusProps() map[string]apiextensionsv1.JSONSchemaProps {
	return map[string]apiextensionsv1.JSONSchemaProps{
		"id":                 str("ID is the Cloudflare ID of the external resource."),
		"observedGeneration": {Type: "integer", Format: "int64"},
		"writeOnlyHash":      str("WriteOnlyHash is a hash of write-only forProvider fields last applied."),
		"conditions": {Type: "array", XListType: ptr("map"), XListMapKeys: []string{"type"},
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: ptr(conditionSchema())}},
	}
}

// BuildCRD returns the CustomResourceDefinition of a kind.
func BuildCRD(m *KindModel) *apiextensionsv1.CustomResourceDefinition {
	d := m.Descriptor
	spec := apiextensionsv1.JSONSchemaProps{Type: "object", Description: fmt.Sprintf("%sSpec defines the desired state of a %s.", m.Kind, m.Kind),
		Properties: commonSpecProps(d.DefaultDeletionPolicy), Required: []string{"accountRef"}}
	fp := props(m.Params)
	fp.Description = "ForProvider holds the Cloudflare API fields, named exactly as in the API."
	var celFields []string
	for _, f := range m.CreateRequired {
		if CELAccessible(f) {
			celFields = append(celFields, f)
		} else {
			fp.Required = append(fp.Required, f) // no CEL rule can name it: plain required
		}
	}
	spec.XValidations = append(createRequiredRules(celFields), specRules(m)...)
	spec.Properties["forProvider"] = fp
	if d.Scope == "zone" {
		spec.Required = append(spec.Required, "zoneRef")
	}
	status := apiextensionsv1.JSONSchemaProps{Type: "object", Description: fmt.Sprintf("%sStatus defines the observed state of a %s.", m.Kind, m.Kind),
		Properties: commonStatusProps()}
	ap := props(m.Observation)
	ap.Description = "AtProvider is the resource as last read from the Cloudflare API."
	status.Properties["atProvider"] = ap

	desc := fmt.Sprintf("%s is a Cloudflare %s (x-fern-sdk-group-name %q).", m.Kind, strings.ReplaceAll(m.Resource.FernGroup, ".", " "), m.Resource.FernGroup)
	root := apiextensionsv1.JSONSchemaProps{
		Type: "object", Description: desc, Required: []string{"spec"},
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"apiVersion": str("APIVersion defines the versioned schema of this representation of an object."),
			"kind":       str("Kind is a string value representing the REST resource this object represents."),
			"metadata":   {Type: "object"},
			"spec":       spec,
			"status":     status,
		},
		XValidations: immutableRules(m),
	}
	var cols []apiextensionsv1.CustomResourceColumnDefinition
	for _, c := range printerColumns(m) {
		cols = append(cols, apiextensionsv1.CustomResourceColumnDefinition{Name: c.Name, Type: c.Type, JSONPath: c.JSONPath,
			Priority: c.Priority, Description: c.Description})
	}
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: m.Plural + "." + m.Group,
			Annotations: map[string]string{"flare.dev/generated-by": "flaregen", "flare.dev/fern-group": m.Resource.FernGroup}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: m.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: m.Kind, ListKind: m.Kind + "List", Plural: m.Plural, Singular: strings.ToLower(m.Kind),
				ShortNames: m.ShortNames,
				Categories: categories(m),
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: m.Version, Served: true, Storage: true,
				Subresources:             &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				AdditionalPrinterColumns: cols,
				Schema:                   &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &root},
			}},
		},
	}
}

func categories(m *KindModel) []string {
	out := []string{CategoryCloudflare, CategoryManaged}
	if m.Product != CategoryCloudflare && m.Product != CategoryManaged {
		out = append(out, m.Product)
	}
	return out
}

// specRules are the CEL rules on spec besides the create-required ones: a non-empty
// accountRef.name and, for zone-scoped kinds, exactly one of zoneRef.id and zoneRef.name.
func specRules(m *KindModel) apiextensionsv1.ValidationRules {
	out := apiextensionsv1.ValidationRules{{
		Rule: AccountRefRule, Message: AccountRefMessage,
		FieldPath: ".accountRef.name", Reason: ptr(apiextensionsv1.FieldValueInvalid),
	}}
	if m.Descriptor.Scope == "zone" {
		out = append(out, apiextensionsv1.ValidationRule{
			Rule:      "has(self.zoneRef.id) != has(self.zoneRef.name)",
			Message:   "zoneRef needs exactly one of id or name",
			FieldPath: ".zoneRef", Reason: ptr(apiextensionsv1.FieldValueInvalid),
		})
	}
	return out
}

// ExistsCEL is true (in a CEL rule on the object root, oldSelf being the stored object) once
// the Cloudflare resource exists: the controller has recorded its ID in status.id.
const ExistsCEL = "has(oldSelf.status) && has(oldSelf.status.id) && size(oldSelf.status.id) > 0"

// immutableRules returns one transition rule (on the object root) per Immutable top-level
// forProvider field: once the resource exists (status.id is set), a value that is set in both
// the stored and the new object cannot change. The controller still compares the fields with
// Cloudflare and reports Synced=False, reason Immutable; the rule only gives immediate
// feedback. It deliberately does not cover adding or removing the field (the controller then
// compares the new value with Cloudflare), so an object that adopted a resource with other
// values can be corrected. A new value equal to status.atProvider (a scalar the API reads
// back) is also allowed, for the same reason.
func immutableRules(m *KindModel) apiextensionsv1.ValidationRules {
	var out apiextensionsv1.ValidationRules
	for _, f := range m.Descriptor.Immutable {
		if strings.Contains(f, ".") || !CELAccessible(f) || m.Params.Field(f) == nil {
			continue // nested or unnameable: the controller's check alone applies
		}
		cf := CELField(f)
		newV, oldV := "self.spec.forProvider."+cf, "oldSelf.spec.forProvider."+cf
		rule := fmt.Sprintf("!(%s) || !has(oldSelf.spec.forProvider) || !has(%s) || !has(self.spec.forProvider) || !has(%s) || %s == %s",
			ExistsCEL, oldV, newV, newV, oldV)
		msg := fmt.Sprintf("forProvider.%s is immutable once the resource exists (status.id is set): recreate the object to change it", f)
		if atProviderComparable(m, f) {
			at := "oldSelf.status.atProvider." + cf
			rule += fmt.Sprintf(" || (has(oldSelf.status.atProvider) && has(%s) && %s == %s)", at, at, newV)
			msg += fmt.Sprintf(", or set it to the value Cloudflare reports (status.atProvider.%s)", f)
		}
		r := apiextensionsv1.ValidationRule{Rule: rule, Message: msg, Reason: ptr(apiextensionsv1.FieldValueInvalid)}
		if celIdentRe.MatchString(f) && !celReserved[f] {
			r.FieldPath = ".spec.forProvider." + f
		}
		out = append(out, r)
	}
	return out
}

// atProviderComparable: status.atProvider has field f with the same scalar type as
// forProvider.f, and the API reads it back (it is not write-only).
func atProviderComparable(m *KindModel, f string) bool {
	pf, of := m.Params.Field(f), m.Observation.Field(f)
	if pf == nil || of == nil || !pf.Type.Kind.scalar() || pf.Type.Kind != of.Type.Kind {
		return false
	}
	for _, w := range m.Descriptor.WriteOnly {
		if w == f {
			return false
		}
	}
	return true
}

// checkPrintColumns fails when a printer column's JSONPath names no scalar field of the schema.
func checkPrintColumns(m *KindModel, root *apiextensionsv1.JSONSchemaProps) error {
	for _, c := range m.PrintColumns {
		s := root
		for _, seg := range strings.Split(strings.TrimPrefix(c.JSONPath, "."), ".") {
			p, ok := s.Properties[seg]
			if !ok {
				return fmt.Errorf("%s: printColumns %s: %s is not in the CRD schema", m.Kind, c.Name, c.JSONPath)
			}
			s = &p
		}
		if s.Type == "object" || s.Type == "array" {
			return fmt.Errorf("%s: printColumns %s: %s is an %s, not a scalar", m.Kind, c.Name, c.JSONPath, s.Type)
		}
	}
	return nil
}

// MarshalCRD renders a CRD as YAML with the generated-file header, without
// the empty status and creationTimestamp that the typed struct carries.
func MarshalCRD(crd *apiextensionsv1.CustomResourceDefinition) ([]byte, error) {
	j, err := json.Marshal(crd)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(j, &obj); err != nil {
		return nil, err
	}
	delete(obj, "status")
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	y, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return append([]byte("# "+GeneratedHeader+"\n---\n"), y...), nil
}

// CanCreateCEL is true (in a CEL rule on spec) when the management policies allow
// Create: unset or empty means ["*"].
const CanCreateCEL = "!has(self.managementPolicies) || size(self.managementPolicies) == 0 || " +
	"'*' in self.managementPolicies || 'Create' in self.managementPolicies"

// createRequiredRules returns one CEL rule per top-level forProvider field the
// create body requires: required only when the object may create, so an
// Observe-only object that adopts through the external-id annotation need not
// supply fields it never sends.
func createRequiredRules(fields []string) apiextensionsv1.ValidationRules {
	var out apiextensionsv1.ValidationRules
	for _, f := range fields {
		r := apiextensionsv1.ValidationRule{
			Rule:    fmt.Sprintf("!(%s) || (has(self.forProvider) && has(self.forProvider.%s))", CanCreateCEL, CELField(f)),
			Message: fmt.Sprintf("forProvider.%s is required unless managementPolicies exclude Create (e.g. [\"Observe\"])", f),
			Reason:  ptr(apiextensionsv1.FieldValueRequired),
		}
		if celIdentRe.MatchString(f) && !celReserved[f] {
			r.FieldPath = ".forProvider." + f
		}
		out = append(out, r)
	}
	return out
}

var (
	celIdentRe      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	celAccessibleRe = regexp.MustCompile(`^[a-zA-Z_.\-/][a-zA-Z0-9_.\-/]*$`)
)

// CELAccessible reports whether Kubernetes CEL can select a property of this name.
func CELAccessible(name string) bool { return celAccessibleRe.MatchString(name) }

// celReserved are the CEL keywords Kubernetes escapes as __{keyword}__.
var celReserved = map[string]bool{
	"true": true, "false": true, "null": true, "in": true, "as": true, "break": true, "const": true,
	"continue": true, "else": true, "for": true, "function": true, "if": true, "import": true,
	"let": true, "loop": true, "package": true, "namespace": true, "return": true, "var": true,
	"void": true, "while": true,
}

// CELField escapes a property name for CEL field selection the way the
// Kubernetes CEL environment does: reserved words become __{word}__, and
// "__", ".", "-", "/" become __underscores__, __dot__, __dash__, __slash__.
func CELField(name string) string {
	if celReserved[name] {
		return "__" + name + "__"
	}
	r := strings.NewReplacer("__", "__underscores__", ".", "__dot__", "-", "__dash__", "/", "__slash__")
	return r.Replace(name)
}
