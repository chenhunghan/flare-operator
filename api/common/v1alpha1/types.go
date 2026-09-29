// Package v1alpha1 contains types shared by every flare-operator kind.
//
// CONTRACT (docs/plan-parallel.md §2.3). Frozen by the orchestrator.
//
// Conventions for every kind, generated or hand-written:
//   - API group <product>.cloudflare.flare.dev, version v1alpha1, where
//     <product> is the x-fern-sdk-group-name with "_" removed (e.g. kv, queues,
//     d1, zerotrust). CloudflareAccount lives in cloudflare.flare.dev.
//   - Go package api/<product>/v1alpha1.
//   - Spec embeds ResourceSpec inline and has forProvider (<Kind>Parameters,
//     JSON names exactly as the Cloudflare API).
//   - Status embeds ResourceStatus inline and has atProvider (<Kind>Observation).
//   - Adoption: annotation AnnotationExternalID.
//   - Cluster-scoped kinds are not used; every kind is namespaced and its
//     accountRef names a CloudflareAccount in the same namespace.
//
// +kubebuilder:object:generate=true
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// AnnotationExternalID pins (or adopts) the Cloudflare ID of the resource.
	AnnotationExternalID = "cloudflare.flare.dev/external-id"
	// Finalizer is set on every managed resource that may need external deletion.
	Finalizer = "cloudflare.flare.dev/finalizer"
)

// DeletionPolicy says what happens to the Cloudflare resource when the
// Kubernetes object is deleted.
// +kubebuilder:validation:Enum=Delete;Orphan
type DeletionPolicy string

const (
	DeletionDelete DeletionPolicy = "Delete"
	DeletionOrphan DeletionPolicy = "Orphan"
)

// ManagementAction is one Crossplane-style management policy.
// +kubebuilder:validation:Enum=Observe;Create;Update;Delete;LateInitialize;"*"
type ManagementAction string

const (
	ManageObserve        ManagementAction = "Observe"
	ManageCreate         ManagementAction = "Create"
	ManageUpdate         ManagementAction = "Update"
	ManageDelete         ManagementAction = "Delete"
	ManageLateInitialize ManagementAction = "LateInitialize"
	ManageAll            ManagementAction = "*"
)

// LocalRef names an object in the same namespace.
type LocalRef struct {
	Name string `json:"name"`
}

// ZoneRef selects a zone by ID or by name (exactly one).
type ZoneRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ResourceSpec is embedded inline in every kind's spec.
type ResourceSpec struct {
	// AccountRef names the CloudflareAccount (same namespace) to use.
	AccountRef LocalRef `json:"accountRef"`
	// ZoneRef is required for zone-scoped kinds and ignored otherwise.
	// +optional
	ZoneRef *ZoneRef `json:"zoneRef,omitempty"`
	// DeletionPolicy defaults per kind (Orphan for data-bearing kinds).
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
	// ManagementPolicies default to ["*"]. ["Observe"] makes the object read-only.
	// +optional
	ManagementPolicies []ManagementAction `json:"managementPolicies,omitempty"`
}

// Condition types and reasons used by every kind.
const (
	ConditionReady  = "Ready"
	ConditionSynced = "Synced"

	ReasonAvailable       = "Available"
	ReasonCreating        = "Creating"
	ReasonDeleting        = "Deleting"
	ReasonUnavailable     = "Unavailable"
	ReasonReconcileOK     = "ReconcileSuccess"
	ReasonReconcileError  = "ReconcileError"
	ReasonImmutable       = "Immutable"
	ReasonAccountNotReady = "AccountNotReady"
	ReasonObserveOnly     = "ObserveOnly"
	ReasonNotFound        = "ExternalNotFound"
	ReasonDependency      = "DependencyNotReady"
)

// ResourceStatus is embedded inline in every kind's status.
type ResourceStatus struct {
	// ID is the Cloudflare ID of the external resource.
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// WriteOnlyHash is a hash of write-only forProvider fields last applied.
	// +optional
	WriteOnlyHash string `json:"writeOnlyHash,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
