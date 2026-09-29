package reconcile

import (
	"context"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	finalizerConflictRetries = 10
	finalizerConflictWait    = 20 * time.Millisecond
)

// deletedExternal remembers, per object UID, the Cloudflare resource its finalizer deleted
// (Finalize), so that a later pass of the same finalizer does not send the DELETE again. Such a
// pass happens whenever the finalizer removal does not stick at once: its optimistically locked
// patch conflicts with a stale cache copy, and the cache has not caught up within
// removeFinalizerAfterDelete's short retry budget (a loaded machine), or a queued event
// re-runs the reconcile on a cache copy that still shows the finalizer. Every such pass would
// DELETE again (answered 404, which is ignored, but still a needless write and a
// nondeterministic request count).
//
// A deleting object never stops deleting, and its UID is never reused, so a remembered
// (UID, ID) pair stays true for the object's lifetime: the resource was deleted with a 2xx or
// found gone (404). The memory is in-process only: after a restart the first pass deletes
// again (404). Entries expire after deletedExternalTTL.
var deletedExternal = struct {
	mu sync.Mutex
	m  map[types.UID]deletedEntry
}{m: map[types.UID]deletedEntry{}}

type deletedEntry struct {
	id string
	at time.Time
}

// deletedExternalTTL bounds how long a deletion is remembered; a finalizer that is still not
// removed that long after its DELETE just deletes again (404).
const deletedExternalTTL = 30 * time.Minute

// rememberDeleted records that uid's finalizer deleted (or found gone) id.
func rememberDeleted(uid types.UID, id string) {
	if uid == "" || id == "" {
		return
	}
	now := time.Now()
	deletedExternal.mu.Lock()
	defer deletedExternal.mu.Unlock()
	for k, e := range deletedExternal.m {
		if now.Sub(e.at) > deletedExternalTTL {
			delete(deletedExternal.m, k)
		}
	}
	deletedExternal.m[uid] = deletedEntry{id: id, at: now}
}

// alreadyDeleted reports whether uid's finalizer already deleted id in this process.
func alreadyDeleted(uid types.UID, id string) bool {
	if uid == "" || id == "" {
		return false
	}
	deletedExternal.mu.Lock()
	defer deletedExternal.mu.Unlock()
	e, ok := deletedExternal.m[uid]
	return ok && e.id == id && time.Since(e.at) <= deletedExternalTTL
}

// removeFinalizerAfterDelete removes the finalizer of mg after Finalize deleted its Cloudflare
// resource deletedID. The removal patch is optimistically locked, so it fails with Conflict
// when mg was a stale cache copy, typically because the cache had not yet shown the status
// write of the object's previous reconcile. On a Conflict it re-reads the object, waiting
// briefly for the cache to catch up, and removes the finalizer from the fresh copy if that
// copy still pins deletedID. If it pins something else (the external ID changed concurrently),
// or the cache does not catch up, the Conflict is returned for a normal requeue, which decides
// again on the fresh object; that requeue does not DELETE deletedID again (deletedExternal).
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
