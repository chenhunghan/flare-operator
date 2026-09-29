package generic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Referrers: deletion ordering between kinds that Cloudflare itself does not enforce.
//
// Cloudflare lets a resource be deleted while another one still uses it (a Worker keeps its
// vpc_service binding to a deleted VPC service, recording 0091; KV namespaces, queues and D1
// databases bound by a Worker are not checked either, UNVERIFIED). A kind whose objects
// reference objects of another kind (WorkerScript bindings → KVNamespace, Queue, D1Database,
// VPCService) registers a Referrer for the referenced kind from an init function. Before the
// referenced object's Cloudflare resource is deleted, its finalizer waits (Ready=False, reason
// DependencyNotReady) until no referrer object in the namespace still names it.
//
// The generic reconciler checks the registered referrers of its kind in finalize and watches
// every referrer kind, so a removed reference or a deleted referrer wakes the waiting object;
// hand-written controllers (VPCService) call BlockingReferrers and ReferrerWatch themselves.

// Referrer describes a kind whose objects reference objects of another kind by name (same
// namespace).
type Referrer struct {
	// Kind names the referrer kind in messages (e.g. "WorkerScript").
	Kind string
	// Object is an empty referrer object (the watch source).
	Object client.Object
	// NewList returns an empty list of the referrer kind.
	NewList func() client.ObjectList
	// Referenced returns the names of the referenced kind's objects that o references.
	Referenced func(o client.Object) []string
}

// ReferrerRetry is how often a deletion blocked by referrers is re-checked when no watch event
// arrives.
var ReferrerRetry = 30 * time.Second

var (
	referrersMu sync.RWMutex
	referrers   = map[schema.GroupKind][]Referrer{}
)

// RegisterReferrer records that objects of r's kind reference objects of kind gk (call it from
// an init function; registrations made after a controller's setup are not watched).
func RegisterReferrer(gk schema.GroupKind, r Referrer) {
	if r.Object == nil || r.NewList == nil || r.Referenced == nil {
		panic("generic.RegisterReferrer: Object, NewList and Referenced are required")
	}
	referrersMu.Lock()
	defer referrersMu.Unlock()
	referrers[gk] = append(referrers[gk], r)
}

// ReferrersOf returns the registered referrers of kind gk.
func ReferrersOf(gk schema.GroupKind) []Referrer {
	referrersMu.RLock()
	defer referrersMu.RUnlock()
	return append([]Referrer(nil), referrers[gk]...)
}

// BlockingReferrers lists ("<Kind> <name>", sorted) the objects in obj's namespace that still
// reference obj through a registered referrer of kind gk. A referrer that is itself being deleted
// still counts: its finalizer removes the reference from Cloudflare. A referrer kind whose CRD is
// not installed has no objects.
func BlockingReferrers(ctx context.Context, c client.Reader, gk schema.GroupKind, obj client.Object) ([]string, error) {
	var out []string
	for _, r := range ReferrersOf(gk) {
		list := r.NewList()
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return nil, fmt.Errorf("list %ss that may reference %s: %w", r.Kind, obj.GetName(), err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			o, ok := it.(client.Object)
			if !ok {
				continue
			}
			for _, n := range r.Referenced(o) {
				if n == obj.GetName() {
					out = append(out, r.Kind+" "+o.GetName())
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ReferrerWaitMessage is the DependencyNotReady message of a deletion blocked by refs.
func ReferrerWaitMessage(refs []string) string {
	return fmt.Sprintf("waiting for %d object(s) that reference this one to be deleted or to drop the reference "+
		"(Cloudflare does not check dependencies on delete): %s", len(refs), strings.Join(refs, ", "))
}

// ReferrerWatch returns an event handler for referrer r that enqueues the referenced objects
// (same namespace) a referrer stops naming: every reference of a deleted referrer, and the
// references an update drops. Those are the events that can unblock a waiting deletion; new
// references and status updates of referrers enqueue nothing.
func ReferrerWatch(r Referrer) handler.EventHandler {
	add := func(q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request], ns string, names []string) {
		for _, n := range names {
			q.Add(ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: n}})
		}
	}
	return handler.Funcs{
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return
			}
			kept := map[string]bool{}
			for _, n := range r.Referenced(e.ObjectNew) {
				kept[n] = true
			}
			var dropped []string
			for _, n := range r.Referenced(e.ObjectOld) {
				if !kept[n] {
					dropped = append(dropped, n)
				}
			}
			add(q, e.ObjectNew.GetNamespace(), dropped)
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			if e.Object != nil {
				add(q, e.Object.GetNamespace(), r.Referenced(e.Object))
			}
		},
	}
}
