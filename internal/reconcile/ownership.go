package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// Ownership proof and the deletion decision shared by every managed kind (the generic
// reconciler, Tunnel and VPCService).
//
// Policy: an object deletes its Cloudflare resource only when it provably owns it:
//   - it created the resource, or its EnsureOwner wrote (or confirmed) its owner tag: recorded by
//     RecordCreated / RecordOwnership in AnnotationOwnershipProof (an older build's
//     AnnotationLegacyCreatedByUID counts as the same record), or
//   - a readable owner tag names the object, or
//   - no ownership tag can exist (tagging is disabled, or the resource type cannot be tagged,
//     such as VPC services) and the object pins the ID through the external-id annotation.
//
// status.id is not proof (it is set whenever a resource is observed, including Observe-only
// objects and adoption), and neither is a tags read that failed or answered 500. A readable owner
// tag naming another object always wins. Observe-only objects never record ownership.

// AnnotationOwnershipProof records that the object provably owns a Cloudflare resource. Its
// value is "<metadata.uid>/<external ID>": the UID ties the record to this very object, so a
// copied manifest (GitOps export, backup restore) does not inherit it, and the ID ties it to one
// resource, so re-pointing the external-id annotation does not carry it over.
const AnnotationOwnershipProof = "cloudflare.flare.dev/ownership-proof"

// AnnotationLegacyCreatedByUID is the ownership record of older Tunnel and VPCService builds:
// "<metadata.uid>" of the object that created the resource named by its external-id
// annotation. HasOwnershipProof honours it; RecordOwnership and RecordCreated replace it with
// AnnotationOwnershipProof (MigrateLegacyOwnership does so for an object that needs no other
// write).
const AnnotationLegacyCreatedByUID = "cloudflare.flare.dev/created-by-uid"

func ownershipProofValue(mg client.Object, id string) string { return string(mg.GetUID()) + "/" + id }

// HasOwnershipProof reports whether mg records that it owns resource id: its
// AnnotationOwnershipProof names mg's UID and id, or (legacy) its AnnotationLegacyCreatedByUID
// names mg's UID and its external-id annotation is id.
func HasOwnershipProof(mg client.Object, id string) bool {
	if id == "" || mg.GetUID() == "" {
		return false
	}
	a := mg.GetAnnotations()
	return a[AnnotationOwnershipProof] == ownershipProofValue(mg, id) || hasLegacyProof(mg, id)
}

func hasLegacyProof(mg client.Object, id string) bool {
	a := mg.GetAnnotations()
	return id != "" && mg.GetUID() != "" && a[AnnotationLegacyCreatedByUID] == string(mg.GetUID()) &&
		a[commonv1alpha1.AnnotationExternalID] == id
}

// recorded reports whether mg's annotations already say exactly what RecordOwnership writes.
func recorded(mg client.Object, id string) bool {
	a := mg.GetAnnotations()
	_, legacy := a[AnnotationLegacyCreatedByUID]
	return !legacy && a[AnnotationOwnershipProof] == ownershipProofValue(mg, id) && a[commonv1alpha1.AnnotationExternalID] == id
}

// RecordOwnership durably records that mg owns resource id: it writes AnnotationOwnershipProof
// and the external-id annotation (and drops AnnotationLegacyCreatedByUID) in one metadata merge
// patch with optimistic locking; status.id is set in memory, as by PersistExternalID. Call it
// right after EnsureOwner succeeded with a real (non-Noop) Tagger, or to migrate a legacy
// record; never for an Observe-only object. Right after a create use RecordCreated, which
// cannot lose the new ID to a Conflict. It writes nothing when the annotations already say so.
func RecordOwnership(ctx context.Context, c client.Client, mg ManagedObject, id string) error {
	mg.GetResourceStatus().ID = id
	if recorded(mg, id) {
		return nil
	}
	return patchMeta(ctx, c, mg, func(o client.Object) {
		a := o.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[commonv1alpha1.AnnotationExternalID] = id
		a[AnnotationOwnershipProof] = ownershipProofValue(mg, id)
		delete(a, AnnotationLegacyCreatedByUID)
		o.SetAnnotations(a)
	})
}

// RecordCreated is RecordOwnership for the moment right after mg created resource id in
// Cloudflare. The create cannot be undone, so the ID must not be lost to a Conflict because
// the object changed in between (a spec edit, a label, another controller): instead of an
// optimistic lock it sends a merge patch of just these annotations, carrying mg's UID as a
// precondition (the API server refuses to change metadata.uid), so a namesake object that
// replaced mg is never annotated. Other metadata is left alone. mg's metadata
// (resourceVersion, annotations, labels, finalizers) is refreshed from the answer; its
// generation, spec and in-memory status are kept.
func RecordCreated(ctx context.Context, c client.Client, mg ManagedObject, id string) error {
	mg.GetResourceStatus().ID = id
	if recorded(mg, id) {
		return nil
	}
	meta := map[string]any{"annotations": map[string]any{
		commonv1alpha1.AnnotationExternalID: id,
		AnnotationOwnershipProof:            ownershipProofValue(mg, id),
		AnnotationLegacyCreatedByUID:        nil,
	}}
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
		return fmt.Errorf("record ownership of %s: the object was replaced (UID %s, want %s)", id, cp.GetUID(), mg.GetUID())
	}
	mg.SetResourceVersion(cp.GetResourceVersion())
	mg.SetAnnotations(cp.GetAnnotations())
	mg.SetLabels(cp.GetLabels())
	mg.SetFinalizers(cp.GetFinalizers())
	return nil
}

// MigrateLegacyOwnership rewrites a valid AnnotationLegacyCreatedByUID record for id as
// AnnotationOwnershipProof (RecordOwnership). It writes nothing when mg has no legacy record
// for id. Call it only for objects that may write (not Observe-only).
func MigrateLegacyOwnership(ctx context.Context, c client.Client, mg ManagedObject, id string) error {
	if !hasLegacyProof(mg, id) {
		return nil
	}
	return RecordOwnership(ctx, c, mg, id)
}

// IsPermanent reports whether a Cloudflare error will not go away by retrying: a 4xx other than
// 408, 409 and 429.
func IsPermanent(err error) bool {
	ae, ok := cfclient.AsAPIError(err)
	return ok && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusRequestTimeout && ae.Status != http.StatusConflict && ae.Status != http.StatusTooManyRequests
}

// DeleteDecision is the verdict of MayDeleteExternal.
type DeleteDecision struct {
	// Delete: ownership is proven; the resource may be deleted.
	Delete bool
	// Gone: ownership is not proven, but the resource no longer exists (checked with the exists
	// callback): there is nothing to delete or keep, and no warning is due.
	Gone bool
	// Foreign is the other owner a readable owner tag names ("" otherwise).
	Foreign string
	// Why explains, when neither Delete nor Gone, why the resource is kept (for a log line and a
	// Warning event ExternalResourceKept). The caller removes the finalizer.
	Why string
}

// MayDeleteExternal applies the ownership policy above to the finalization of mg with deletion
// policy Delete for resource id (target). owner is mg's owner tag value; tagger is the kind's
// effective Tagger (NoopTagger when tagging is disabled or the kind cannot be tagged; cf may
// then be nil). It never writes to Cloudflare.
//
// exists (optional) reports whether the resource still exists. It is consulted only when
// ownership is not proven, so an already-deleted resource yields Gone instead of a spurious
// "kept" warning or an endless retry. A permanent (4xx, not 404) error from exists counts as
// "still exists": the resource is kept with the warning rather than retried forever.
//
// A non-nil error is a transient failure of the tag read (or of exists) with no other proof:
// retry. A permanent refusal of the tag read (a 4xx such as 403 when the token lacks Resource
// Tagging permission, or 404) with no ownership record keeps the resource: waiting would never
// end.
func MayDeleteExternal(ctx context.Context, tagger Tagger, cf cfclient.Client, accountID string, target TagTarget,
	owner string, mg ManagedObject, id string, exists func(context.Context) (bool, error)) (DeleteDecision, error) {
	proof := HasOwnershipProof(mg, id)
	gone := func() (bool, error) {
		if exists == nil {
			return false, nil
		}
		ok, err := exists(ctx)
		switch {
		case err == nil:
			return !ok, nil
		case cfclient.IsNotFound(err):
			return true, nil
		case IsPermanent(err):
			// The check only spares a warning: a read that will keep failing (403, ...) must not
			// turn a keep verdict into an endless retry, so the resource is taken to exist.
			return false, nil
		}
		return false, fmt.Errorf("check whether %s %s still exists: %w", target.Type, id, err)
	}
	keep := func(why string) (DeleteDecision, error) {
		g, err := gone()
		switch {
		case err != nil:
			return DeleteDecision{}, err
		case g:
			return DeleteDecision{Gone: true}, nil
		}
		return DeleteDecision{Why: why}, nil
	}
	if !TaggingEnabled(tagger) {
		if proof || mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == id {
			return DeleteDecision{Delete: true}, nil
		}
		return keep(fmt.Sprintf("this object neither created %s nor pins it with the %s annotation, and no ownership tag can prove "+
			"that it owns it (tagging is off, or the resource type cannot be tagged)", id, commonv1alpha1.AnnotationExternalID))
	}
	o, tagged, err := tagger.Owner(ctx, cf, accountID, target)
	switch {
	case err != nil && proof:
		return DeleteDecision{Delete: true}, nil // the record suffices; the tag only guards against a later re-tag
	case err != nil && cfclient.IsNotFound(err):
		return keep(fmt.Sprintf("this object has no ownership record for %s and its %s tag cannot be read (404)", id, OwnerTagKey))
	case err != nil && IsPermanent(err):
		return keep(fmt.Sprintf("this object has no ownership record for %s and its %s tag cannot be read (%v); "+
			"a token with Resource Tagging read access lets deletion check the tag", id, OwnerTagKey, err))
	case err != nil:
		if g, gerr := gone(); gerr == nil && g {
			return DeleteDecision{Gone: true}, nil // nothing left to decide about
		}
		return DeleteDecision{}, err
	case o != "" && o != owner:
		return DeleteDecision{Foreign: o, Why: fmt.Sprintf("it is owned by %q (tag %s)", o, OwnerTagKey)}, nil
	case o == owner || proof:
		return DeleteDecision{Delete: true}, nil
	case !tagged:
		return keep(fmt.Sprintf("this object has no ownership record for %s and the resource has no readable %s tag (the tags read answered 500)", id, OwnerTagKey))
	}
	return keep(fmt.Sprintf("this object has no ownership record for %s and the resource has no %s tag", id, OwnerTagKey))
}
