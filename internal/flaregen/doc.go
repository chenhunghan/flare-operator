// Package flaregen generates CRD kinds from the pinned Cloudflare OpenAPI spec.
//
// # Resource model
//
// Operations are grouped by x-fern-sdk-group-name (e.g. "kv.namespaces") and
// classified by x-fern-sdk-method-name:
//
//   - CRUD resource: a "create" POST on a collection path C, plus "get",
//     "update"/"edit" and "delete" on the item path C/{param}, and "list" GET on C.
//     Required: get and delete. The item parameter becomes {id} in the Descriptor.
//   - Singleton: "get" plus "update"/"edit" (PUT or PATCH) on one path that no
//     CRUD resource of the group claims, with no create.
//   - Scope: paths under /accounts/{x} are account-scoped, /zones/{x} zone-scoped;
//     the scope parameter becomes {account_id}/{zone_id}.
//   - Not generated yet (Resource.Unsupported says why): other scopes, nested
//     resources (extra parent path parameters), upsert-style item paths without
//     create, non-JSON request bodies.
//
// Per kind (generator.yaml can override each):
//
//   - forProvider = create body ∪ chosen update body, readOnly properties removed.
//     Top-level required = required by the create body (nothing for singletons).
//   - atProvider = the "result" of the get response envelope (the create response
//     if there is no get), writeOnly properties removed.
//   - UpdateMethod: PATCH ("edit", or an "update" that is PATCH) is preferred
//     because it never resets fields the object does not mention; else PUT.
//   - Immutable: create-body fields the chosen update body does not accept.
//   - WriteOnly: forProvider fields that atProvider does not have.
//   - IDField: the item parameter name, else id, uuid, <singular>_id, tag — the
//     first present in atProvider. NameField: name, <singular>_name, title — the
//     first present in forProvider.
//   - DefaultDeletionPolicy: Delete (Orphan for singletons); data-bearing kinds
//     set Orphan in generator.yaml.
//
// # Flattening rules (OpenAPI → structural CRD schema)
//
// Kubernetes requires structural schemas: every node has one type, and
// oneOf/anyOf/allOf may not introduce types or properties. Convert therefore
// flattens:
//
//  1. $ref is resolved. A schema that refers to itself (recursion) becomes
//     preserve-unknown-fields at the point of recursion; so does nesting deeper
//     than maxDepth.
//  2. allOf: all members are intersected with the base schema — properties are
//     merged, a property is required if any member requires it, scalar
//     constraints are kept from the first member that has them.
//  3. oneOf/anyOf: members that are only null are dropped; the rest are unioned:
//     - all objects → one object with every member's properties; a property is
//     required only if every member requires it (e.g. discriminated unions keep
//     the discriminator, with the union of its enums);
//     - all the same scalar type → that type; enums are unioned (dropped if any
//     member has none); other constraints are kept only when all members agree;
//     - integer and number → number; arrays → array of the union of the items;
//     - anything else (string | object, object | map, …) → preserve-unknown-fields.
//     The union is then intersected with the base schema (a common pattern is an
//     object with properties plus oneOf naming required subsets).
//  4. type lists (OpenAPI 3.1): "null" is dropped; several remaining types →
//     preserve-unknown-fields.
//  5. Objects: properties → a Go struct; no properties but additionalProperties
//     with a schema → map[string]T; neither → free-form object
//     (type: object + x-kubernetes-preserve-unknown-fields). An object whose
//     properties are all readOnly (forProvider) or writeOnly (atProvider) also
//     becomes free-form.
//  6. x-kubernetes-preserve-unknown-fields is the last resort; Go type
//     apiextensionsv1.JSON. Every use is reported as a warning (flaregen -v).
//  7. Validations (enum, pattern, min/max length, minimum/maximum, min/max items)
//     are kept in forProvider only, where the API would reject the value anyway;
//     atProvider never validates what Cloudflare returns. Patterns that Go's RE2
//     cannot compile (ECMA lookarounds) are dropped. format, default,
//     uniqueItems and nullable are never emitted (the API server enforces formats
//     such as date-time more strictly than Cloudflare's own output; defaults are
//     applied by the API, not by Kubernetes).
//  8. Field names in JSON are exactly the API's; Go names are CamelCase with
//     common initialisms (queue_id → QueueID). Optional scalars and objects are
//     pointers; required ones are values.
//
// Output: api/<product>/v1alpha1 (types, groupversion_info.go, deepcopy),
// config/crd/bases/<group>_<plural>.yaml, and
// internal/generic/descriptors/zz_generated.go. flaregen writes deepcopy and
// CRD YAML itself (no controller-gen); the kubebuilder markers in the Go types
// document the same schema.
package flaregen
