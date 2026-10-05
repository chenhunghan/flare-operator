package reconcile

import (
	"context"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StatusConflictRetry is how soon a reconcile whose status write met a Conflict runs again.
const StatusConflictRetry = 200 * time.Millisecond

// PatchStatus writes obj's status as a merge patch from base, the copy the reconcile started
// from, preconditioned on obj's resourceVersion (optimistic locking): the version the
// reconcile read, advanced only by its own metadata writes that found the object at it
// (optimistically locked patches, patchMetadata), so obj's content still is that version's.
//
// The informer cache lags behind writes, and a reconcile can start within milliseconds of the
// previous one (a retry, an event queued meanwhile) from a copy older than that reconcile's
// own status write. A merge patch replaces the whole conditions list, so a status built from
// such a copy would put older conditions back (Ready=False over a newer Ready=True), and as the
// watches ignore status-only changes nothing would repair it before the resync. With the
// precondition the API server refuses that write with a Conflict instead; the reconcile runs
// again (StatusWritten) from a newer copy.
func PatchStatus(ctx context.Context, c client.Client, obj, base client.Object) error {
	from, ok := base.DeepCopyObject().(client.Object)
	if !ok {
		panic("reconcile: DeepCopyObject did not return a client.Object")
	}
	from.SetResourceVersion(obj.GetResourceVersion()) // MergeFromWithOptimisticLock sends from's
	return c.Status().Patch(ctx, obj, client.MergeFromWithOptions(from, client.MergeFromWithOptimisticLock{}))
}

// StatusWritten folds the error of a reconcile's status write (PatchStatus, nil when none was
// needed) into the reconcile's result and error.
//
// NotFound (the object is gone) is ignored. A Conflict means the reconcile worked on an
// outdated copy: it is not an error to report (no error log, and no condition, which could
// only be written by another status write from the same outdated copy), just a short requeue.
// A Conflict the reconcile itself returned (an optimistically locked metadata write from that
// copy) is treated alike. Any other reconcile error stands, so its backoff applies.
func StatusWritten(ctx context.Context, res ctrl.Result, err, perr error) (ctrl.Result, error) {
	switch {
	case perr == nil || apierrors.IsNotFound(perr):
		return res, err
	case apierrors.IsConflict(perr) && (err == nil || apierrors.IsConflict(err)):
		log.FromContext(ctx).V(1).Info("status write conflicted (outdated copy); requeueing", "error", perr.Error())
		return ctrl.Result{RequeueAfter: StatusConflictRetry}, nil
	default:
		return res, errors.Join(err, perr)
	}
}
