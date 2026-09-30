package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Crash consistency of creates (docs/resilience.md).
//
// A create in Cloudflare and the write of its ID to the object (RecordCreated) are two steps: a
// manager that dies (or loses the API server) between them leaves a Cloudflare resource that
// the object does not know. Kinds whose API has unique names, or client-chosen IDs, find it
// again by name or ID on the next reconcile and adopt it. Kinds that never adopt by name
// (VPCService; Tunnel and WorkerScript without a readable owner tag) could not tell their own
// lost create from someone else's resource, and reported NameConflict for their own resource.
//
// For those, the create is announced first: MarkCreatePending durably records, before the
// Cloudflare call, that this object (its UID) is about to create the resource identified by key
// (its name) and saw no such resource just before. After a crash, a resource with that name is
// taken to be the object's own lost create (PendingCreate) and adopted with RecordCreated, which
// also clears the record.
//
// The record also covers deletion: an object deleted before a restarted manager adopted its
// lost create has no ID, and its finalizer would drop the resource silently.
// AdoptPendingCreate lets the finalizer find it by the record's key and record it as the
// object's own create first, so deletionPolicy Delete deletes it. An object whose known
// resource was found gone and recreated still carries the old ID next to the record;
// AdoptPendingCreateReplacing checks that ID and, when it is gone, adopts the record's resource
// in its place. Every kind that can find a
// resource by name or client-chosen ID announces its creates this way (the generic reconciler,
// Tunnel, VPCService, WorkerScript).
//
// Residual risk: the record stands from the lookup that found nothing until a create succeeds
// or the API refuses it for good (a permanent 4xx that proves nothing was made). While creates
// keep failing transiently (5xx, timeouts, lost answers; each retry repeats the lookup), a
// same-named resource that someone else creates in that time, however long it lasts, is taken
// for the object's own lost create and adopted. Without the record a lost create could not be
// told from such a resource at all. The owner tag, where the kind has one, still refuses a
// resource another object has tagged.

// AnnotationCreatePending records a create in progress: "<metadata.uid>/<key>".
const AnnotationCreatePending = "cloudflare.flare.dev/create-pending"

// MarkCreatePending durably records that mg is about to create the Cloudflare resource
// identified by key (a name, or name plus content hash; it must not contain a newline). Call it
// right before the create call, after a lookup found no such resource. Like RecordCreated it
// sends a merge patch of just this annotation with mg's UID as a precondition (no optimistic
// lock), and refreshes mg's metadata from the answer. It writes nothing when the record already
// says so.
func MarkCreatePending(ctx context.Context, c client.Client, mg ManagedObject, key string) error {
	if strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("create-pending key %q contains a newline", key)
	}
	want := string(mg.GetUID()) + "/" + key
	if mg.GetAnnotations()[AnnotationCreatePending] == want {
		return nil
	}
	return patchAnnotations(ctx, c, mg, map[string]any{AnnotationCreatePending: want})
}

// PendingCreate returns the key of mg's create-pending record, if the record belongs to mg (its
// UID; a copied manifest does not inherit it).
func PendingCreate(mg client.Object) (key string, ok bool) {
	v := mg.GetAnnotations()[AnnotationCreatePending]
	uid := string(mg.GetUID())
	if uid == "" || !strings.HasPrefix(v, uid+"/") {
		return "", false
	}
	return strings.TrimPrefix(v, uid+"/"), true
}

// FreshCreatePending reads mg through r, which must bypass the informer cache (the manager's
// API reader), and replaces mg's in-memory create-pending record and ownership proof
// (AnnotationCreatePending, AnnotationOwnershipProof) with the stored ones; it returns the
// uncached copy. The stored object must still be mg (its UID).
//
// Call it wherever the record decides something: before MarkCreatePending (whose no-op check
// reads it), where a same-named resource may be taken for the object's own lost create, and in
// the finalizer before AdoptPendingCreate. The cache can lag behind the reconciler's own writes
// of the record (the watches filter metadata-only changes out, and the retry after an error
// runs within milliseconds):
//   - A refused create drops the record. A retry that still saw it would take a same-named
//     resource someone else made for its own lost create: adopt it, record an ownership proof,
//     manage it and, under deletionPolicy Delete, delete it (its finalizer, too). It would also
//     not write the record again before the next create (MarkCreatePending's no-op check), so
//     a crash after that create would lose it.
//   - A retry that missed the record of an earlier attempt would not recognize that attempt's
//     create as its own (NameConflict).
//   - A RecordCreated that the cache does not show yet leaves no record but an ownership proof,
//     which the uncached copy has.
func FreshCreatePending(ctx context.Context, r client.Reader, mg ManagedObject) (ManagedObject, error) {
	t := reflect.TypeOf(mg)
	if t == nil || t.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("read the create-pending record: %T is not a pointer", mg)
	}
	cur, ok := reflect.New(t.Elem()).Interface().(ManagedObject)
	if !ok {
		return nil, fmt.Errorf("read the create-pending record: %T is not a ManagedObject", reflect.New(t.Elem()).Interface())
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(mg), cur); err != nil {
		return nil, fmt.Errorf("read the create-pending record: %w", err)
	}
	if cur.GetUID() != mg.GetUID() {
		return nil, fmt.Errorf("read the create-pending record: the object was replaced (UID %s, want %s)", cur.GetUID(), mg.GetUID())
	}
	a := maps.Clone(mg.GetAnnotations())
	if a == nil {
		a = map[string]string{}
	}
	for _, k := range []string{AnnotationCreatePending, AnnotationOwnershipProof} {
		if v, ok := cur.GetAnnotations()[k]; ok {
			a[k] = v
		} else {
			delete(a, k)
		}
	}
	mg.SetAnnotations(a)
	return cur, nil
}

// ErrAmbiguousName is wrapped by lookups that found several resources with the wanted name.
var ErrAmbiguousName = errors.New("several resources have this name")

// AdoptPendingCreate is the finalizer's half of the create-pending record, for an object that
// is being deleted with deletionPolicy Delete. When mg has no external ID but a create-pending
// record of its own, the manager may have died between the create and RecordCreated: lookup
// (ctx, key) finds the resource the record names (its ID, "" when there is none), and a
// resource found is recorded as mg's own create (RecordCreated, which also clears the record),
// so the finalizer goes on to delete it like any resource mg created. Ownership checks that
// follow (a readable owner tag naming another object) still apply.
//
// It returns mg's external ID: the existing one, the adopted one, or "" when there is nothing
// to delete. A lookup that can never succeed (a permanent 4xx, or ErrAmbiguousName) is given up
// with a Warning event ExternalResourceKept, so the finalizer does not wait forever; any other
// lookup error is returned for a retry.
func AdoptPendingCreate(ctx context.Context, c client.Client, rec events.EventRecorder, mg ManagedObject, kind string,
	lookup func(ctx context.Context, key string) (string, error)) (string, error) {
	return AdoptPendingCreateReplacing(ctx, c, rec, mg, kind, nil, lookup)
}

// AdoptPendingCreateReplacing is AdoptPendingCreate for an object that may still carry the ID
// of a resource that went away. A reconcile that finds the known resource gone (404) creates a
// new one under a fresh create-pending record; a manager that dies before RecordCreated leaves
// the object with the old, gone ID and that record. Deleted before a restarted manager adopts
// the new resource, the object's finalizer would delete only the gone ID and leak the new one.
//
// gone reports whether the resource with mg's current external ID no longer exists (nil: the
// ID is always taken as it is, which is AdoptPendingCreate). When mg has an external ID and a
// create-pending record of its own and gone says the ID's resource is gone, the record's
// resource (lookup) is recorded as mg's own create (RecordCreated) and returned in its place.
// Otherwise mg's ID is returned unchanged: without a record, when the ID's resource exists, or
// when the lookup finds nothing (the finalizer then finds the old one gone). A gone check
// refused for good (a permanent 4xx) keeps the ID, whose own deletion path handles that; any
// other gone or lookup error is returned for a retry.
func AdoptPendingCreateReplacing(ctx context.Context, c client.Client, rec events.EventRecorder, mg ManagedObject, kind string,
	gone func(ctx context.Context, id string) (bool, error), lookup func(ctx context.Context, key string) (string, error)) (string, error) {
	known := ExternalID(mg)
	key, ok := PendingCreate(mg)
	if !ok {
		return known, nil
	}
	if known != "" {
		if gone == nil {
			return known, nil
		}
		g, err := gone(ctx, known)
		switch {
		case err != nil && IsPermanent(err):
			return known, nil
		case err != nil:
			return "", fmt.Errorf("check whether the %s %s still exists: %w", kind, known, err)
		case !g:
			return known, nil
		}
	}
	id, err := lookup(ctx, key)
	switch {
	case err != nil && (IsPermanent(err) || errors.Is(err, ErrAmbiguousName)):
		WarnExternalKept(rec, mg, "Delete", fmt.Sprintf("the %s this object may have created before a restart (create-pending %q) "+
			"cannot be looked up and may be left in Cloudflare: %v", kind, key, err))
		return known, nil
	case err != nil:
		return "", fmt.Errorf("look up the %s of an interrupted create (%q): %w", kind, key, err)
	case id == "" || id == known:
		return known, nil
	}
	if err := RecordCreated(ctx, c, mg, id); err != nil {
		return "", fmt.Errorf("record the %s %s created before a restart: %w", kind, id, err)
	}
	log.FromContext(ctx).Info("found the resource of an interrupted create; it is deleted with the object", "kind", kind, "id", id, "replaces", known)
	return id, nil
}

// ClearCreatePending drops the create-pending record (a no-op without one).
func ClearCreatePending(ctx context.Context, c client.Client, mg ManagedObject) error {
	if _, ok := mg.GetAnnotations()[AnnotationCreatePending]; !ok {
		return nil
	}
	return patchAnnotations(ctx, c, mg, map[string]any{AnnotationCreatePending: nil})
}

// patchAnnotations merge-patches annotations (nil deletes) with mg's UID as precondition and
// refreshes mg's metadata (resourceVersion, annotations, labels, finalizers) from the answer;
// generation, spec and in-memory status are kept.
func patchAnnotations(ctx context.Context, c client.Client, mg ManagedObject, ann map[string]any) error {
	meta := map[string]any{"annotations": ann}
	if uid := mg.GetUID(); uid != "" {
		meta["uid"] = string(uid)
	}
	data, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return err
	}
	cp, ok := mg.DeepCopyObject().(client.Object)
	if !ok {
		panic("reconcile: DeepCopyObject did not return a client.Object")
	}
	if err := c.Patch(ctx, cp, client.RawPatch(types.MergePatchType, data)); err != nil {
		return err
	}
	if cp.GetUID() != mg.GetUID() {
		return fmt.Errorf("annotate %s/%s: the object was replaced (UID %s, want %s)", mg.GetNamespace(), mg.GetName(), cp.GetUID(), mg.GetUID())
	}
	mg.SetResourceVersion(cp.GetResourceVersion())
	mg.SetAnnotations(cp.GetAnnotations())
	mg.SetLabels(cp.GetLabels())
	mg.SetFinalizers(cp.GetFinalizers())
	return nil
}
