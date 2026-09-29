package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
// Residual risk: someone else creating a resource with the same name between the object's
// "not found" check and its own create call (a window of one API round trip) would be adopted
// as the object's own. The owner tag, where the kind has one, still refuses a resource another
// object has tagged.

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
