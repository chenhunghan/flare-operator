package reconcile

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// Ownership proof and the deletion decision shared by every managed kind.
//
// Policy: an object deletes its Cloudflare resource only when it provably owns it:
//   - it created the resource, or its EnsureOwner wrote (or confirmed) its owner tag: recorded by
//     RecordOwnership in AnnotationOwnershipProof, or
//   - a readable owner tag names the object, or
//   - tagging is disabled and the object pins the ID through the external-id annotation.
//
// status.id is not proof (it is set whenever a resource is observed, including Observe-only
// objects and adoption), and neither is a tags read that failed or answered 500. A readable owner
// tag naming another object always wins. Observe-only objects never call RecordOwnership.

// AnnotationOwnershipProof records that the object provably owns a Cloudflare resource. Its
// value is "<metadata.uid>/<external ID>": the UID ties the record to this very object, so a
// copied manifest (GitOps export, backup restore) does not inherit it, and the ID ties it to one
// resource, so re-pointing the external-id annotation does not carry it over.
const AnnotationOwnershipProof = "cloudflare.flare.dev/ownership-proof"

func ownershipProofValue(mg client.Object, id string) string { return string(mg.GetUID()) + "/" + id }

// HasOwnershipProof reports whether mg's AnnotationOwnershipProof names id (and mg's UID).
func HasOwnershipProof(mg client.Object, id string) bool {
	return id != "" && mg.GetUID() != "" && mg.GetAnnotations()[AnnotationOwnershipProof] == ownershipProofValue(mg, id)
}

// RecordOwnership durably records that mg owns resource id: it writes AnnotationOwnershipProof
// and the external-id annotation in one metadata patch (status.id is set in memory, as by
// PersistExternalID). Call it only right after mg created the resource, or right after
// EnsureOwner succeeded with a real (non-Noop) Tagger; never for an Observe-only object. It
// writes nothing when both annotations already say so.
func RecordOwnership(ctx context.Context, c client.Client, mg ManagedObject, id string) error {
	mg.GetResourceStatus().ID = id
	if HasOwnershipProof(mg, id) && mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == id {
		return nil
	}
	return patchMeta(ctx, c, mg, func(o client.Object) {
		a := o.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[commonv1alpha1.AnnotationExternalID] = id
		a[AnnotationOwnershipProof] = ownershipProofValue(mg, id)
		o.SetAnnotations(a)
	})
}

// OwnerReader reads the owner tag without writing (ResourceTagger implements it). tagged=false
// means the tags endpoint answered 500 and the tag index did not list the resource.
type OwnerReader interface {
	Owner(ctx context.Context, cf cfclient.Client, accountID string, t TagTarget) (owner string, tagged bool, err error)
}

// MayDeleteExternal applies the ownership policy above to the finalization of mg with deletion
// policy Delete: ok reports whether resource id (target) may be deleted; otherwise why explains
// it for a log line and a Warning event (the caller removes the finalizer and keeps the
// resource). owner is mg's owner tag value; tagger is the kind's effective Tagger (NoopTagger
// when tagging is disabled or the kind cannot be tagged). A non-nil err is a failed tag read
// with no other proof: retry. It never writes to Cloudflare.
func MayDeleteExternal(ctx context.Context, tagger Tagger, cf cfclient.Client, accountID string, target TagTarget,
	owner string, mg ManagedObject, id string) (ok bool, why string, err error) {
	proof := HasOwnershipProof(mg, id)
	if _, noop := tagger.(NoopTagger); noop || tagger == nil {
		if proof || mg.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == id {
			return true, "", nil
		}
		return false, fmt.Sprintf("ownership tagging is off and this object neither created %s nor pins it with the %s annotation",
			id, commonv1alpha1.AnnotationExternalID), nil
	}
	rd, readable := tagger.(OwnerReader)
	if !readable {
		if proof {
			return true, "", nil
		}
		return false, fmt.Sprintf("this object has no ownership record for %s and its tagger cannot read the %s tag", id, OwnerTagKey), nil
	}
	o, tagged, err := rd.Owner(ctx, cf, accountID, target)
	switch {
	case err != nil && proof:
		return true, "", nil // the record suffices; the tag only guards against a later re-tag
	case err != nil && cfclient.IsNotFound(err):
		return false, fmt.Sprintf("this object has no ownership record for %s and its %s tag cannot be read (404)", id, OwnerTagKey), nil
	case err != nil:
		return false, "", err
	case o != "" && o != owner:
		return false, fmt.Sprintf("it is owned by %q (tag %s)", o, OwnerTagKey), nil
	case o == owner || proof:
		return true, "", nil
	case !tagged:
		return false, fmt.Sprintf("this object has no ownership record for %s and the resource has no readable %s tag (the tags read answered 500)", id, OwnerTagKey), nil
	}
	return false, fmt.Sprintf("this object has no ownership record for %s and the resource has no %s tag", id, OwnerTagKey), nil
}
