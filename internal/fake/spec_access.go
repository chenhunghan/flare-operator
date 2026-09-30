package fake

import (
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
)

// liftAccessRequired makes kin-openapi apply OpenAPI's readOnly/writeOnly rule for required
// properties ("if the property is marked as writeOnly being true and is in the required list,
// the required will take effect on the request only", and the converse for readOnly) when the
// required list and the property live in different schemas of an allOf.
//
// kin-openapi looks the property up only in the schema that lists it as required. The spec
// often splits them, e.g. hyperdrive_hyperdrive-database-full is
// {allOf: [hyperdrive_hyperdrive-database], required: [..., password]} with the writeOnly
// password declared inside the allOf member, so every real Hyperdrive config response (0195:
// origin without password) fails its origin oneOf, although the spec says it must not have
// the password.
//
// For such a schema the property's schema is copied into the requiring schema's properties.
// That changes nothing else: allOf already applies the same property schema to the same value,
// and schemas that forbid additional properties are left alone (a new property would admit a
// value they reject). It returns the number of properties lifted.
func liftAccessRequired(doc *openapi3.T) int {
	seen := map[*openapi3.Schema]bool{}
	n := 0
	var walk func(ref *openapi3.SchemaRef)
	walk = func(ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		s := ref.Value
		seen[s] = true
		n += liftSchema(s)
		for _, p := range s.Properties {
			walk(p)
		}
		walk(s.Items)
		walk(s.Not)
		if s.AdditionalProperties.Schema != nil {
			walk(s.AdditionalProperties.Schema)
		}
		for _, list := range []openapi3.SchemaRefs{s.AllOf, s.AnyOf, s.OneOf} {
			for _, m := range list {
				walk(m)
			}
		}
	}
	if doc.Components != nil {
		for _, s := range doc.Components.Schemas {
			walk(s)
		}
		for _, rb := range doc.Components.RequestBodies {
			if rb != nil && rb.Value != nil {
				for _, mt := range rb.Value.Content {
					walk(mt.Schema)
				}
			}
		}
		for _, r := range doc.Components.Responses {
			if r != nil && r.Value != nil {
				for _, mt := range r.Value.Content {
					walk(mt.Schema)
				}
			}
		}
		for _, p := range doc.Components.Parameters {
			if p != nil && p.Value != nil {
				walk(p.Value.Schema)
			}
		}
	}
	if doc.Paths != nil {
		for _, pi := range doc.Paths.Map() {
			for _, p := range pi.Parameters {
				if p != nil && p.Value != nil {
					walk(p.Value.Schema)
				}
			}
			for _, op := range pi.Operations() {
				for _, p := range op.Parameters {
					if p != nil && p.Value != nil {
						walk(p.Value.Schema)
					}
				}
				if op.RequestBody != nil && op.RequestBody.Value != nil {
					for _, mt := range op.RequestBody.Value.Content {
						walk(mt.Schema)
					}
				}
				if op.Responses != nil {
					for _, r := range op.Responses.Map() {
						if r != nil && r.Value != nil {
							for _, mt := range r.Value.Content {
								walk(mt.Schema)
							}
						}
					}
				}
			}
		}
	}
	return n
}

// liftSchema lifts, into s.Properties, every readOnly or writeOnly property that s requires but
// only an allOf member (at any allOf depth) declares.
func liftSchema(s *openapi3.Schema) int {
	if len(s.Required) == 0 || len(s.AllOf) == 0 {
		return 0
	}
	if ap := s.AdditionalProperties.Has; ap != nil && !*ap {
		return 0
	}
	n := 0
	for _, k := range s.Required {
		if s.Properties[k] != nil {
			continue
		}
		p := allOfProperty(s.AllOf, k, map[*openapi3.Schema]bool{})
		if p == nil || p.Value == nil || !(p.Value.ReadOnly || p.Value.WriteOnly) {
			continue
		}
		if s.Properties == nil {
			s.Properties = openapi3.Schemas{}
		}
		s.Properties[k] = p
		n++
	}
	return n
}

func allOfProperty(members openapi3.SchemaRefs, name string, seen map[*openapi3.Schema]bool) *openapi3.SchemaRef {
	for _, m := range members {
		if m == nil || m.Value == nil || seen[m.Value] {
			continue
		}
		seen[m.Value] = true
		if p := m.Value.Properties[name]; p != nil {
			return p
		}
		if p := allOfProperty(m.Value.AllOf, name, seen); p != nil {
			return p
		}
	}
	return nil
}

// declareSecuritySchemes declares, as HTTP bearer schemes, the security schemes that
// operations require but the spec's components never define. The pinned spec's Pages asset
// operations (/pages/assets/check-missing, upload, upsert-hashes) require
// "pages_upload_token", which is not declared. kin-openapi then fails every such request with
// "security scheme ... is not declared", and, having read the request body for the
// authentication function first, validates the body as missing
// (openapi3filter.validateSecurityRequirement returns before restoring it). The emulator
// authenticates requests itself (AuthenticationFunc is a no-op), so the declaration changes
// nothing but that. It returns the names declared.
func declareSecuritySchemes(doc *openapi3.T) []string {
	if doc.Components == nil {
		doc.Components = &openapi3.Components{}
	}
	if doc.Components.SecuritySchemes == nil {
		doc.Components.SecuritySchemes = openapi3.SecuritySchemes{}
	}
	var added []string
	declare := func(reqs *openapi3.SecurityRequirements) {
		if reqs == nil {
			return
		}
		for _, req := range *reqs {
			for name := range req {
				if _, ok := doc.Components.SecuritySchemes[name]; !ok {
					doc.Components.SecuritySchemes[name] = &openapi3.SecuritySchemeRef{Value: openapi3.NewJWTSecurityScheme()}
					added = append(added, name)
				}
			}
		}
	}
	declare(&doc.Security)
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			declare(op.Security)
		}
	}
	sort.Strings(added)
	return added
}
