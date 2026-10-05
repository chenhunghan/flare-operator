package reconcile

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Views makes a reconciler start from a copy of its object that is no older than the
// reconciler's own last write of it.
//
// The informer cache lags behind the reconciler's writes, and the next reconcile of an object
// can start within milliseconds of the last one (a retry after an error, or an event that was
// queued meanwhile). A reconcile that works on a copy older than its own last write goes wrong
// in two ways that nothing repairs before the resync, as the watches filter status-only
// changes out:
//   - Its status patch (a merge patch from the stale copy, which replaces the whole conditions
//     list) writes conditions back that the last reconcile had replaced, such as Ready=False
//     over a newer Ready=True.
//   - It skips its status write when the stale copy already shows the status it computed,
//     although the stored status differs.
//
// Get reads the cache, and reads again uncached when the cached copy is not the one the
// reconciler last finished with (its resourceVersion differs: the cache lags, or someone else
// wrote since); Done records the copy a reconcile finished with. resourceVersions are compared
// for equality only. In steady state the cache has caught up and Get costs nothing extra.
//
// The zero value is ready to use.
type Views struct {
	m sync.Map // client.ObjectKey -> resourceVersion
}

// Get reads key into obj from cached and, when the cached copy is not the last one Done
// recorded for key, from uncached as well (nil: the cached copy is used). A NotFound from
// either read forgets key and is returned.
func (v *Views) Get(ctx context.Context, cached, uncached client.Reader, key client.ObjectKey, obj client.Object) error {
	err := cached.Get(ctx, key, obj)
	if err == nil {
		last, ok := v.m.Load(key)
		if !ok || last.(string) == obj.GetResourceVersion() || uncached == nil {
			return nil
		}
		// A fresh object, not obj: decoding into the cached copy would keep its map entries
		// (annotations, labels) that the stored object no longer has.
		fresh, ok := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(client.Object)
		if !ok {
			return fmt.Errorf("read %s uncached: %T is not a client.Object", key, reflect.New(reflect.TypeOf(obj).Elem()).Interface())
		}
		fresh.GetObjectKind().SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		if err = uncached.Get(ctx, key, fresh); err == nil {
			reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(fresh).Elem())
		}
	}
	if apierrors.IsNotFound(err) {
		v.Forget(key)
	}
	return err
}

// Done records the resourceVersion obj has at the end of a reconcile: the one it was read at,
// or the answer to the reconcile's last write of it.
func (v *Views) Done(obj client.Object) {
	if rv := obj.GetResourceVersion(); rv != "" {
		v.m.Store(client.ObjectKeyFromObject(obj), rv)
	}
}

// Forget drops what Done recorded for key (the object is gone).
func (v *Views) Forget(key client.ObjectKey) { v.m.Delete(key) }
