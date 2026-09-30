// Package r2bind resolves a reference to an R2Bucket into what a binding to it names, for the
// WorkerScript (r2_bucket bindings) and PagesProject (r2_buckets) controllers.
package r2bind

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	r2v1alpha1 "flare.dev/operator/api/r2/v1alpha1"
)

// Kind is the group kind of R2Bucket.
var Kind = schema.GroupKind{Group: r2v1alpha1.GroupVersion.Group, Kind: "R2Bucket"}

// DefaultJurisdiction is R2's jurisdiction value for a bucket without one.
const DefaultJurisdiction = "default"

// Of returns the bucket name (status.id: an R2 bucket's ID is its name, "" before the bucket
// exists) and the jurisdiction ("" for none) of an R2Bucket. The jurisdiction is the observed
// one, else forProvider's (it is immutable, and sent as the cf-r2-jurisdiction header, so both
// agree once the bucket exists); R2's "default" is none: the r2_bucket binding's jurisdiction
// enum (the pinned spec's workers_binding_kind_r2_bucket: eu, fedramp, fedramp-high, us) has no
// such value, and a bucket without a jurisdiction is bound without one.
func Of(rb *r2v1alpha1.R2Bucket) (bucket, jurisdiction string) {
	bucket = rb.Status.ID
	switch {
	case rb.Status.AtProvider.Jurisdiction != nil:
		jurisdiction = *rb.Status.AtProvider.Jurisdiction
	case rb.Spec.ForProvider.Jurisdiction != nil:
		jurisdiction = *rb.Spec.ForProvider.Jurisdiction
	}
	if jurisdiction == DefaultJurisdiction {
		jurisdiction = ""
	}
	return bucket, jurisdiction
}
