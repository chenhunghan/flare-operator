package generic_test

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// TestRecordCreatedSurvivesConflict runs reconcile.RecordCreated against a real API server: the
// ownership record written right after a Cloudflare create survives a concurrent change of the
// object (where RecordOwnership's optimistic lock answers Conflict and would lose the new ID),
// keeps that change, and is refused for a namesake object that replaced the original (UID
// precondition).
func TestRecordCreatedSurvivesConflict(t *testing.T) {
	h := newHarness(t, accountOnly)
	en := entry(t, "KVNamespace")
	obj := h.newObj(en, "rc", `{"forProvider":{"title":"x"}}`)
	h.create(obj)
	stale := obj.DeepCopyObject().(reconcile.ManagedObject)

	// Someone else changes the object meanwhile.
	fresh := obj.DeepCopyObject().(reconcile.ManagedObject)
	if err := h.get(fresh); err != nil {
		t.Fatal(err)
	}
	fresh.SetLabels(map[string]string{"team": "blue"})
	if err := h.e.Client.Update(h.ctx(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := reconcile.RecordOwnership(h.ctx(), h.e.Client, stale.DeepCopyObject().(reconcile.ManagedObject), "id-1"); !apierrors.IsConflict(err) {
		t.Fatalf("RecordOwnership on a stale object: %v, want a Conflict (test premise)", err)
	}
	if err := reconcile.RecordCreated(h.ctx(), h.e.Client, stale, "id-1"); err != nil {
		t.Fatalf("RecordCreated on a stale object: %v", err)
	}
	if err := h.get(obj); err != nil {
		t.Fatal(err)
	}
	if !reconcile.HasOwnershipProof(obj, "id-1") || obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != "id-1" || obj.GetLabels()["team"] != "blue" {
		t.Errorf("stored annotations %v labels %v", obj.GetAnnotations(), obj.GetLabels())
	}
	if stale.GetResourceVersion() != obj.GetResourceVersion() || stale.GetLabels()["team"] != "blue" {
		t.Errorf("in-memory metadata not refreshed: rv %s (stored %s), labels %v", stale.GetResourceVersion(), obj.GetResourceVersion(), stale.GetLabels())
	}

	// The object is replaced by a namesake (new UID): the old object's record is refused.
	h.delete(obj)
	h.waitGone(obj)
	again := h.newObj(en, "rc", `{"forProvider":{"title":"x"}}`)
	h.create(again)
	if err := reconcile.RecordCreated(h.ctx(), h.e.Client, stale, "id-2"); err == nil {
		t.Fatal("RecordCreated annotated a namesake object with another UID")
	}
	if err := h.get(again); err != nil {
		t.Fatal(err)
	}
	if a := again.GetAnnotations(); a[commonv1alpha1.AnnotationExternalID] != "" || a[reconcile.AnnotationOwnershipProof] != "" {
		t.Errorf("namesake annotated: %v", a)
	}
}
