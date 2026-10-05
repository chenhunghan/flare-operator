package reconcile

import (
	"context"
	"errors"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StatusConflictRetry is how soon a reconcile whose status write met a first Conflict runs
// again; each further consecutive Conflict of the same object doubles it, up to
// MaxStatusConflictRetry.
const StatusConflictRetry = 200 * time.Millisecond

// MaxStatusConflictRetry caps the requeue delay after consecutive status write Conflicts.
const MaxStatusConflictRetry = time.Minute

// statusConflicts counts each object's consecutive status write Conflicts (object key →
// count). A status write that lands, or finds the object gone, drops the entry.
var statusConflicts sync.Map

// statusConflict is the Conflict PatchStatus returns: it carries the requeue delay for the
// object's consecutive Conflicts (apierrors.IsConflict still holds through Unwrap).
type statusConflict struct {
	err   error
	retry time.Duration
}

func (e *statusConflict) Error() string { return e.err.Error() }
func (e *statusConflict) Unwrap() error { return e.err }

// statusConflictRetry is the delay after the n-th consecutive Conflict (n ≥ 1).
func statusConflictRetry(n int) time.Duration {
	d := StatusConflictRetry
	for i := 1; i < n && d < MaxStatusConflictRetry; i++ {
		d *= 2
	}
	return min(d, MaxStatusConflictRetry)
}

func conflictKey(obj client.Object) string {
	if uid := obj.GetUID(); uid != "" {
		return string(uid)
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

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
// again (StatusWritten) from a newer copy, after a delay that grows with the object's
// consecutive Conflicts (a cache that does not catch up, or a writer that keeps changing the
// object, must not make it re-observe Cloudflare several times a second).
func PatchStatus(ctx context.Context, c client.Client, obj, base client.Object) error {
	from, ok := base.DeepCopyObject().(client.Object)
	if !ok {
		panic("reconcile: DeepCopyObject did not return a client.Object")
	}
	from.SetResourceVersion(obj.GetResourceVersion()) // MergeFromWithOptimisticLock sends from's
	key := conflictKey(obj)
	err := c.Status().Patch(ctx, obj, client.MergeFromWithOptions(from, client.MergeFromWithOptimisticLock{}))
	switch {
	case err == nil || apierrors.IsNotFound(err):
		statusConflicts.Delete(key)
	case apierrors.IsConflict(err):
		n := 1
		if v, ok := statusConflicts.Load(key); ok {
			n = v.(int) + 1
		}
		statusConflicts.Store(key, n)
		return &statusConflict{err: err, retry: statusConflictRetry(n)}
	}
	return err
}

// StatusWritten folds the error of a reconcile's status write (PatchStatus, nil when none was
// needed) into the reconcile's result and error.
//
// NotFound (the object is gone) is ignored. A Conflict means the reconcile worked on an
// outdated copy: it is not an error to report (no error log, and no condition, which could
// only be written by another status write from the same outdated copy), just a requeue after
// the delay PatchStatus chose (StatusConflictRetry, doubling per consecutive Conflict of the
// object up to MaxStatusConflictRetry). A Conflict the reconcile itself returned (an
// optimistically locked metadata write from that copy) is treated alike. Any other reconcile
// error stands, so its backoff applies.
func StatusWritten(ctx context.Context, res ctrl.Result, err, perr error) (ctrl.Result, error) {
	switch {
	case perr == nil || apierrors.IsNotFound(perr):
		return res, err
	case apierrors.IsConflict(perr) && (err == nil || apierrors.IsConflict(err)):
		retry := StatusConflictRetry
		var sc *statusConflict
		if errors.As(perr, &sc) {
			retry = sc.retry
		}
		log.FromContext(ctx).V(1).Info("status write conflicted (outdated copy); requeueing", "after", retry, "error", perr.Error())
		return ctrl.Result{RequeueAfter: retry}, nil
	default:
		return res, errors.Join(err, perr)
	}
}
