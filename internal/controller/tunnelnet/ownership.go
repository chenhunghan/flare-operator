package tunnelnet

// Deletion and ownership policy shared by the Tunnel and VPCService controllers. These helpers
// are deliberately small and local; they can be swapped for shared ones in internal/reconcile.
//
// A Cloudflare resource is deleted only when ownership is proven:
//   - this object created it (AnnotationCreatedByUID names the object's UID; set only in the
//     patch that persists the ID right after a successful create), or
//   - a readable owner tag names this object (Tunnel only; a failed or ambiguous tag read is
//     never proof), or
//   - the kind has no ownership tag (tagging disabled, or VPC services, which Resource Tagging
//     cannot tag) and the object pins the ID with the external-id annotation.
//
// status.id alone is never proof. If the CloudflareAccount no longer exists when the object is
// finalized, the resource cannot be reached: it is kept (Warning Event ExternalResourceKept)
// and the finalizer is removed. An account that exists but is being deleted or not Ready is
// waited for (it carries an in-use finalizer while labelled objects exist).

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// AnnotationCreatedByUID records that the object with this UID created the Cloudflare resource
// named by its external-id annotation.
const AnnotationCreatedByUID = "cloudflare.flare.dev/created-by-uid"

// EventReasonExternalResourceKept is the reason of the Warning Event recorded when an object
// with the Delete policy is deleted but its Cloudflare resource is kept (ownership not proven,
// or the CloudflareAccount is gone).
const EventReasonExternalResourceKept = "ExternalResourceKept"

// CreatedByThis reports whether o's AnnotationCreatedByUID names o's UID. A copied manifest
// carries the annotation but not the UID, so it proves nothing.
func CreatedByThis(o metav1.Object) bool {
	uid := o.GetUID()
	return uid != "" && o.GetAnnotations()[AnnotationCreatedByUID] == string(uid)
}

// PersistCreated writes the external-id annotation and AnnotationCreatedByUID in one merge
// patch (optimistic lock) right after a successful create, so a crash before the status
// update can neither orphan the new resource nor lose the proof of ownership. status.id is set
// in memory; in-memory status changes are preserved.
func PersistCreated(ctx context.Context, c client.Client, mg reconcile.ManagedObject, id string) error {
	mg.GetResourceStatus().ID = id
	uid := string(mg.GetUID())
	if a := mg.GetAnnotations(); a[commonv1alpha1.AnnotationExternalID] == id && a[AnnotationCreatedByUID] == uid {
		return nil
	}
	cp, ok := mg.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("tunnelnet: DeepCopyObject of %T is not a client.Object", mg)
	}
	base, _ := cp.DeepCopyObject().(client.Object)
	a := cp.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[commonv1alpha1.AnnotationExternalID] = id
	a[AnnotationCreatedByUID] = uid
	cp.SetAnnotations(a)
	if err := c.Patch(ctx, cp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	mg.SetResourceVersion(cp.GetResourceVersion())
	mg.SetAnnotations(cp.GetAnnotations())
	mg.SetFinalizers(cp.GetFinalizers())
	mg.SetLabels(cp.GetLabels())
	mg.SetGeneration(cp.GetGeneration())
	return nil
}

// ResolveForDelete resolves mg's account for its finalizer.
//   - acct != nil: the account is usable.
//   - gone: the CloudflareAccount does not exist (confirmed with reader, which should be
//     uncached); the caller keeps the Cloudflare resource and removes its finalizer.
//   - err is an *reconcile.AccountError: the account exists but is not usable yet (deleting,
//     not Ready); the caller waits (MarkAccountNotReady and requeue).
//   - any other err: retry.
func ResolveForDelete(ctx context.Context, accounts *reconcile.Accounts, reader client.Reader, mg reconcile.ManagedObject) (acct *reconcile.Resolved, gone bool, err error) {
	name := mg.GetResourceSpec().AccountRef.Name
	if name == "" {
		return nil, true, nil
	}
	acct, err = accounts.Resolve(ctx, mg)
	if err == nil || !reconcile.IsAccountNotReady(err) {
		return acct, false, err
	}
	var a cloudflarev1alpha1.CloudflareAccount
	switch gerr := reader.Get(ctx, client.ObjectKey{Namespace: mg.GetNamespace(), Name: name}, &a); {
	case apierrors.IsNotFound(gerr):
		return nil, true, nil
	case gerr != nil:
		return nil, false, gerr
	}
	return nil, false, err
}
