package flaregen

import (
	"fmt"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidateCRD checks that every version schema of a v1 CRD is structural, with
// the API server's own structural-schema code. (The full CRD validation in
// k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation is not used:
// it pulls in cgo-only metrics packages that do not link on this toolchain.)
func ValidateCRD(in *apiextensionsv1.CustomResourceDefinition) error {
	if len(in.Spec.Versions) == 0 {
		return fmt.Errorf("%s: no versions", in.Name)
	}
	for i, v := range in.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return fmt.Errorf("%s: version %s has no schema", in.Name, v.Name)
		}
		var internal apiextensions.JSONSchemaProps
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v.Schema.OpenAPIV3Schema, &internal, nil); err != nil {
			return fmt.Errorf("convert %s: %w", in.Name, err)
		}
		fld := field.NewPath("spec", "versions").Index(i).Child("schema", "openAPIV3Schema")
		s, err := structuralschema.NewStructural(&internal)
		if err != nil {
			return fmt.Errorf("%s: not structural: %w", in.Name, err)
		}
		if errs := structuralschema.ValidateStructural(fld, s); len(errs) > 0 {
			return fmt.Errorf("%s: not structural: %w", in.Name, errs.ToAggregate())
		}
	}
	return nil
}
