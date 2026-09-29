package generic_test

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/reconcile"
)

// TestNamesakeWithoutTagging: with ownership tagging off, nothing proves that an object owns
// a same-named resource another tool made. The object (deletionPolicy Delete) reports
// NameConflict, writes nothing and pins nothing, and deleting it leaves the resource. Only the
// object's own lost create (its create-pending record) is adopted, and then recorded as created
// by it.
func TestNamesakeWithoutTagging(t *testing.T) {
	h := newHarness(t, accountOnly)
	en := entry(t, "KVNamespace")
	rec := &recorder{}
	r := &generic.Reconciler{
		Client:      h.e.Client,
		Accounts:    reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec}), anyBaseURL),
		Tagger:      reconcile.NoopTagger{},
		ClusterName: "testenv",
		Descriptor:  en.Descriptor,
		New:         func() reconcile.ManagedObject { return en.New().(reconcile.ManagedObject) },
	}
	ctx := context.Background()
	reconcileN := func(obj reconcile.ManagedObject, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}); err != nil {
				t.Fatalf("reconcile %d: %v", i, err)
			}
		}
	}

	title := randName("flare-spike")
	foreign := h.createExternal(en, `{"title":"`+title+`"}`)
	obj := h.newObj(en, "namesake", `{"deletionPolicy":"Delete","forProvider":{"title":"`+title+`"}}`)
	h.create(obj)
	reconcileN(obj, 3)
	if err := h.get(obj); err != nil {
		t.Fatal(err)
	}
	if c := reconcile.GetCondition(obj, commonv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reconcile.ReasonNameConflict {
		t.Errorf("Ready: %+v, want False/NameConflict", c)
	}
	if a, s := obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID], obj.GetResourceStatus().ID; a != "" || s != "" {
		t.Errorf("pinned the foreign resource: annotation %q, status.id %q", a, s)
	}
	h.delete(obj)
	reconcileN(obj, 1)
	h.waitGone(obj)
	if w := writesOf(rec.since(0)); len(w) != 0 {
		t.Errorf("writes for a foreign namesake:\n%s", summary(w))
	}
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, foreign), nil); err != nil {
		t.Errorf("the foreign resource after deleting its namesake (deletionPolicy Delete): %v", err)
	}

	// The object's own lost create: its create-pending record names the title.
	lost := h.createExternal(en, `{"title":"`+title+`-lost"}`)
	own := h.newObj(en, "own", `{"deletionPolicy":"Delete","forProvider":{"title":"`+title+`-lost"}}`)
	h.create(own)
	if err := reconcile.MarkCreatePending(ctx, h.e.Client, own, title+"-lost"); err != nil {
		t.Fatal(err)
	}
	mark := rec.mark()
	reconcileN(own, 3)
	if err := h.get(own); err != nil {
		t.Fatal(err)
	}
	if !reconcile.IsReady(own) || own.GetResourceStatus().ID != lost || !reconcile.HasOwnershipProof(own, lost) {
		t.Errorf("lost create: status.id %q, proof %v, %s", own.GetResourceStatus().ID, reconcile.HasOwnershipProof(own, lost), conditions(own))
	}
	if _, pending := reconcile.PendingCreate(own); pending {
		t.Error("the create-pending record was not cleared")
	}
	if w := writesOf(rec.since(mark)); len(w) != 0 {
		t.Errorf("adopting the lost create wrote:\n%s", summary(w))
	}
}
