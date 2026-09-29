package reconcile

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
)

// ExternalID returns the Cloudflare ID of mg: the cloudflare.flare.dev/external-id annotation
// if set (it pins or adopts a resource), else status.id.
func ExternalID(mg commonv1alpha1.Managed) string {
	if id := mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; id != "" {
		return id
	}
	return mg.GetResourceStatus().ID
}

// HasExternalIDAnnotation reports whether the annotation is set (the object pins an ID).
func HasExternalIDAnnotation(mg commonv1alpha1.Managed) bool {
	return mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != ""
}

// SetExternalID records id in memory: annotation and status.id.
func SetExternalID(mg commonv1alpha1.Managed, id string) {
	a := mg.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[commonv1alpha1.AnnotationExternalID] = id
	mg.SetAnnotations(a)
	mg.GetResourceStatus().ID = id
}

// PersistExternalID writes the annotation to the API server right after a create, so a crash
// before the status update cannot orphan the new Cloudflare resource. status.id is set in
// memory; in-memory status changes are preserved.
func PersistExternalID(ctx context.Context, c client.Client, mg ManagedObject, id string) error {
	mg.GetResourceStatus().ID = id
	_, pending := mg.GetAnnotations()[AnnotationCreatePending]
	if mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == id && !pending {
		return nil
	}
	return patchMeta(ctx, c, mg, func(o client.Object) {
		a := o.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[commonv1alpha1.AnnotationExternalID] = id
		delete(a, AnnotationCreatePending)
		o.SetAnnotations(a)
	})
}
