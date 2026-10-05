package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
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
// copies back only metadata (resourceVersion, finalizers, annotations, labels) so in-memory status
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
	obj.SetLabels(cp.GetLabels())
	obj.SetGeneration(cp.GetGeneration())
	return nil
}

// ManagedObject is a Managed that is also a controller-runtime client.Object (every kind is).
type ManagedObject interface {
	commonv1alpha1.Managed
	client.Object
}

// WaitError is returned by a deletion step (the deleteExternal of Finalize, FinalizeAccount)
// that cannot finish yet but is not failing: a multi-step delete in progress (scale down, drain,
// then delete), or an account that is not usable yet. The finalizer stays, Ready=False/Deleting
// carries Reason, and the object is requeued after After without an error (no rate-limited
// backoff, no error log). See DeletionResult.
type WaitError struct {
	After  time.Duration
	Reason string
}

func (e *WaitError) Error() string { return "waiting: " + e.Reason }

// AsWait returns err as a *WaitError, if it is one.
func AsWait(err error) (*WaitError, bool) {
	var we *WaitError
	ok := errors.As(err, &we)
	return we, ok
}

// DeletionResult turns the outcome of a deletion step into a reconcile result: nil → no
// requeue; a *WaitError → Ready=False/Deleting with its Reason and a requeue after its After,
// with no error; a Cloudflare 429 (Throttled) → Ready=False/Deleting and a requeue after its
// Retry-After, with no error; any other error → Ready=False/Deleting with the error, returned
// for a rate-limited retry.
func DeletionResult(mg commonv1alpha1.Managed, err error) (ctrl.Result, error) {
	if err == nil {
		return ctrl.Result{}, nil
	}
	if we, ok := AsWait(err); ok {
		MarkDeleting(mg, we.Reason)
		return ctrl.Result{RequeueAfter: we.After}, nil
	}
	if wait, ok := Throttled(err); ok {
		MarkDeleting(mg, throttleMessage(err, wait))
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	MarkDeleting(mg, err.Error())
	return ctrl.Result{}, err
}

// Finalize runs the deletion flow for mg once its deletionTimestamp is set:
//
//   - If ShouldDeleteExternal (Delete policy and a management policy that allows Delete) and
//     the object has an external ID, deleteExternal is called; a cfclient 404 counts as gone.
//     A *WaitError keeps the finalizer and requeues after its After with no error; any other
//     error keeps the finalizer, sets Ready=False/Deleting and is returned for requeue (see
//     DeletionResult).
//   - Otherwise (Orphan, Observe-only, no Delete in managementPolicies, or deleteExternal nil
//     because the resource is kept or cannot be reached) the Cloudflare resource is left alone.
//   - Finally the finalizer is removed (an object already gone is not an error). After a
//     deletion, a Conflict from a stale mg is retried on the fresh object while it still pins
//     the deleted resource (removeFinalizerAfterDelete). Should the removal still not stick,
//     a later pass for the same object and ID skips deleteExternal: the deletion is remembered
//     in-process per object UID (deletedExternal), so it is never sent twice.
//
// It does nothing when mg is not being deleted or has no finalizer. Callers learn whether the
// finalizer was removed from controllerutil.ContainsFinalizer(mg, commonv1alpha1.Finalizer).
func Finalize(ctx context.Context, c client.Client, mg ManagedObject, kindDefault commonv1alpha1.DeletionPolicy,
	deleteExternal func(ctx context.Context, externalID string) error) (ctrl.Result, error) {
	if mg.GetDeletionTimestamp().IsZero() || !controllerutil.ContainsFinalizer(mg, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	deleted := ""
	if ShouldDeleteExternal(mg, kindDefault) {
		if id := ExternalID(mg); id != "" && deleteExternal != nil {
			// A pass after an earlier one deleted id but could not remove the finalizer yet
			// (deletedExternal) does not send the DELETE again.
			if !alreadyDeleted(mg.GetUID(), id) {
				if err := deleteExternal(ctx, id); err != nil && !cfclient.IsNotFound(err) {
					return DeletionResult(mg, err)
				}
				rememberDeleted(mg.GetUID(), id)
			}
			deleted = id
		}
	}
	return ctrl.Result{}, client.IgnoreNotFound(removeFinalizerAfterDelete(ctx, c, mg, deleted))
}

// EventReasonExternalResourceKept is the reason of the Warning event recorded when an object
// with deletionPolicy Delete is deleted but its Cloudflare resource is kept (ownership not
// proven, or the CloudflareAccount is gone), or an orphaned resource keeps its owner tag.
const EventReasonExternalResourceKept = "ExternalResourceKept"

// WarnExternalKept records a Warning event ExternalResourceKept on obj (no-op without rec).
// action is the event's action (Delete or Orphan).
func WarnExternalKept(rec events.EventRecorder, obj runtime.Object, action, note string) {
	if rec != nil {
		rec.Eventf(obj, nil, corev1.EventTypeWarning, EventReasonExternalResourceKept, action, "%s", note)
	}
}

// FinalizeAccount resolves the CloudflareAccount of mg, which is being finalized and needs
// Cloudflare to clean up. It is the account step of every kind's finalizer:
//
//   - The account is usable, and reader confirms that it still exists (the cache may show one
//     that is gone): it is returned.
//   - The CloudflareAccount does not exist (confirmed through reader: pass an uncached reader
//     such as the manager's APIReader), or spec.accountRef is empty: nothing can reach
//     Cloudflare. It returns nil, nil after recording, when keptNote is not "", a Warning event
//     ExternalResourceKept "<keptNote>: CloudflareAccount <name> no longer exists (...)". The
//     caller leaves the Cloudflare resource alone and removes the finalizer, so finalization
//     never hangs. (The account's in-use finalizer normally keeps it until its users are gone,
//     so this happens only when that finalizer was removed by hand.)
//   - The account exists but is not usable (not Ready, or deleting with its token gone): a
//     *WaitError (AccountRetryInterval) with the account's problem as its Reason.
//   - Any other error is returned.
func FinalizeAccount(ctx context.Context, accounts *Accounts, reader client.Reader, rec events.EventRecorder, mg ManagedObject,
	action, keptNote string) (*Resolved, error) {
	name := mg.GetResourceSpec().AccountRef.Name
	acct, err := accounts.Resolve(ctx, mg)
	if err == nil {
		// Resolve reads the account from the cache, which may still show an account that is
		// gone (its in-use finalizer removed by hand): confirm it uncached, so that the
		// finalizer does not reach Cloudflare through an account that no longer exists.
		var a cloudflarev1alpha1.CloudflareAccount
		switch gerr := reader.Get(ctx, client.ObjectKey{Namespace: mg.GetNamespace(), Name: name}, &a); {
		case gerr == nil:
			return acct, nil
		case !apierrors.IsNotFound(gerr):
			return nil, gerr
		}
	} else if name != "" {
		if !IsAccountNotReady(err) {
			return nil, err
		}
		var a cloudflarev1alpha1.CloudflareAccount
		switch gerr := reader.Get(ctx, client.ObjectKey{Namespace: mg.GetNamespace(), Name: name}, &a); {
		case gerr == nil:
			return nil, &WaitError{After: AccountRetryInterval, Reason: err.Error()}
		case !apierrors.IsNotFound(gerr):
			return nil, gerr
		}
	}
	log.FromContext(ctx).Info("CloudflareAccount is gone: leaving the Cloudflare resource in place", "id", ExternalID(mg), "account", name)
	if keptNote != "" {
		WarnExternalKept(rec, mg, action,
			fmt.Sprintf("%s: CloudflareAccount %q no longer exists (delete managed objects before their account)", keptNote, name))
	}
	return nil, nil
}
