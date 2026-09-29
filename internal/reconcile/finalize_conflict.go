package reconcile

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	finalizerConflictRetries = 10
	finalizerConflictWait    = 20 * time.Millisecond
)

// removeFinalizerAfterDelete removes the finalizer of mg after Finalize deleted its Cloudflare
// resource deletedID. The removal patch is optimistically locked, so it fails with Conflict
// when mg was a stale cache copy, typically because the cache had not yet shown the status
// write of the object's previous reconcile. A requeue would then run the deletion again and
// send a second DELETE (answered 404, which is ignored, but still a needless write), so on a
// Conflict it re-reads the object, waiting briefly for the cache to catch up, and removes the
// finalizer from the fresh copy if that copy still pins deletedID. If it pins something else
// (the external ID changed concurrently), or the cache does not catch up, the Conflict is
// returned for a normal requeue, which decides again on the fresh object.
func removeFinalizerAfterDelete(ctx context.Context, c client.Client, mg ManagedObject, deletedID string) error {
	err := RemoveFinalizer(ctx, c, mg)
	if deletedID == "" || !apierrors.IsConflict(err) {
		return err
	}
	conflict := err
	stale := mg.GetResourceVersion()
	for range finalizerConflictRetries {
		fresh, ok := mg.DeepCopyObject().(ManagedObject)
		if !ok {
			return conflict
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(mg), fresh); err != nil {
			return err
		}
		if fresh.GetResourceVersion() == stale {
			t := time.NewTimer(finalizerConflictWait)
			select {
			case <-ctx.Done():
				t.Stop()
				return conflict
			case <-t.C:
			}
			continue
		}
		if fresh.GetDeletionTimestamp().IsZero() || ExternalID(fresh) != deletedID {
			return conflict
		}
		err := RemoveFinalizer(ctx, c, fresh)
		if err == nil {
			mg.SetResourceVersion(fresh.GetResourceVersion())
			mg.SetFinalizers(fresh.GetFinalizers())
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		conflict, stale = err, fresh.GetResourceVersion()
	}
	return conflict
}
