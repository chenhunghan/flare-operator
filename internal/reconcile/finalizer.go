package reconcile

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// EnsureFinalizer adds commonv1alpha1.Finalizer to obj (a merge patch with optimistic locking)
// and reports whether it had to. obj is updated in place from the server's answer.
func EnsureFinalizer(ctx context.Context, c client.Client, obj client.Object) (bool, error) {
	if controllerutil.ContainsFinalizer(obj, commonv1alpha1.Finalizer) {
		return false, nil
	}
	return true, patchMeta(ctx, c, obj, func(o client.Object) {
		controllerutil.AddFinalizer(o, commonv1alpha1.Finalizer)
	})
}

// RemoveFinalizer removes commonv1alpha1.Finalizer from obj if present.
func RemoveFinalizer(ctx context.Context, c client.Client, obj client.Object) error {
	if !controllerutil.ContainsFinalizer(obj, commonv1alpha1.Finalizer) {
		return nil
	}
	return patchMeta(ctx, c, obj, func(o client.Object) {
		controllerutil.RemoveFinalizer(o, commonv1alpha1.Finalizer)
	})
}

// patchMeta applies mutate to a copy of obj, sends a merge patch with optimistic locking, and
// copies back only metadata (resourceVersion, finalizers, annotations) so in-memory status
// changes on obj survive (the patch response carries the stored, older status).
func patchMeta(ctx context.Context, c client.Client, obj client.Object, mutate func(client.Object)) error {
	cp, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		panic("reconcile: DeepCopyObject did not return a client.Object")
	}
	base, _ := cp.DeepCopyObject().(client.Object)
	mutate(cp)
	if err := c.Patch(ctx, cp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	obj.SetResourceVersion(cp.GetResourceVersion())
	obj.SetFinalizers(cp.GetFinalizers())
	obj.SetAnnotations(cp.GetAnnotations())
	obj.SetGeneration(cp.GetGeneration())
	return nil
}

// ManagedObject is a Managed that is also a controller-runtime client.Object (every kind is).
type ManagedObject interface {
	commonv1alpha1.Managed
	client.Object
}

// Finalize runs the deletion flow for mg once its deletionTimestamp is set:
//
//   - If ShouldDeleteExternal (Delete policy and a management policy that allows Delete) and
//     the object has an external ID, deleteExternal is called; a cfclient 404 counts as gone.
//     An error keeps the finalizer, sets Ready=False/Deleting and is returned for requeue.
//   - Otherwise (Orphan, Observe-only, or no Delete in managementPolicies) the Cloudflare
//     resource is left alone.
//   - Finally the finalizer is removed.
//
// It returns done=false when mg is not being deleted (nothing was done).
func Finalize(ctx context.Context, c client.Client, mg ManagedObject, kindDefault commonv1alpha1.DeletionPolicy,
	deleteExternal func(ctx context.Context, externalID string) error) (done bool, err error) {
	if mg.GetDeletionTimestamp().IsZero() {
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(mg, commonv1alpha1.Finalizer) {
		return true, nil
	}
	if ShouldDeleteExternal(mg, kindDefault) {
		if id := ExternalID(mg); id != "" && deleteExternal != nil {
			if err := deleteExternal(ctx, id); err != nil && !cfclient.IsNotFound(err) {
				MarkDeleting(mg, err.Error())
				return true, err
			}
		}
	}
	return true, RemoveFinalizer(ctx, c, mg)
}
