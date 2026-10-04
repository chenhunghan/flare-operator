// Package generic holds the generic reconciler that runs any Descriptor.
//
// Stable contract shared by all controllers; change deliberately.
package generic

// Descriptor is emitted by cmd/flaregen and consumed by the generic reconciler.
// Paths are relative to the API base URL and use the placeholders
// {account_id}, {zone_id} and {id}. DELETE never sends a body.
type Descriptor struct {
	Group, Version, Kind string

	Scope string // "account" | "zone"

	CreatePath, ItemPath, ListPath string

	IDField   string // field of the API result holding the external ID, e.g. "id", "uuid", "queue_id"
	NameField string // field used for adoption by name; "" disables adoption by name

	UpdateMethod string // "PUT" (full replace) | "PATCH" | "" (no update endpoint)

	Immutable []string // JSON paths under forProvider; a change → Synced=False, reason Immutable
	WriteOnly []string // JSON paths never returned by reads; drift detected via status.writeOnlyHash

	Singleton bool   // no create/delete; the object configures a fixed settings resource at ItemPath
	ListOrder string // informational

	// CreateFields / UpdateFields are the top-level forProvider JSON fields
	// accepted by the create and update request bodies. A field that is set in
	// forProvider, is in UpdateFields but not in CreateFields, is applied by an
	// update immediately after create (e.g. Queue settings).
	CreateFields, UpdateFields []string

	// TagResourceType is the Resource Tagging resource_type for ownership tags
	// (/accounts/{id}/tags); "" means the kind cannot be tagged.
	TagResourceType string

	// DefaultDeletionPolicy applies when spec.deletionPolicy is empty:
	// "Orphan" for data-bearing kinds, else "Delete".
	DefaultDeletionPolicy string
}
