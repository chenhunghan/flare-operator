package flaregen

import (
	"encoding/json"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

type printerColumn struct{ Name, Type, JSONPath string }

func printerColumns() []printerColumn {
	return []printerColumn{
		{"READY", "string", ".status.conditions[?(@.type=='Ready')].status"},
		{"SYNCED", "string", ".status.conditions[?(@.type=='Synced')].status"},
		{"EXTERNAL-ID", "string", ".status.id"},
		{"AGE", "date", ".metadata.creationTimestamp"},
	}
}

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
		Properties: commonSpecProps(d.DefaultDeletionPolicy), Required: []string{"accountRef", "forProvider"}}
	fp := props(m.Params)
	fp.Description = "ForProvider holds the Cloudflare API fields, named exactly as in the API."
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
	}
	var cols []apiextensionsv1.CustomResourceColumnDefinition
	for _, c := range printerColumns() {
		cols = append(cols, apiextensionsv1.CustomResourceColumnDefinition{Name: c.Name, Type: c.Type, JSONPath: c.JSONPath})
	}
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: m.Plural + "." + m.Group,
			Annotations: map[string]string{"flare.dev/generated-by": "flaregen", "flare.dev/fern-group": m.Resource.FernGroup}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: m.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: m.Kind, ListKind: m.Kind + "List", Plural: m.Plural, Singular: strings.ToLower(m.Kind),
				Categories: []string{"cloudflare", m.Product},
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
