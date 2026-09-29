package reconcile_test

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/reconcile"
)

// Finalize with a stale copy (the cache had not shown the object's last status write): the
// Cloudflare resource is deleted once and the finalizer still comes off, instead of a Conflict
// whose requeue deletes again. If the fresh object pins another resource, the Conflict stands.
func TestFinalizeStaleCopy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		newID       string // external ID written concurrently ("": status write only)
		wantRemoved bool
	}{
		{name: "status write", wantRemoved: true},
		{name: "external id changed", newID: "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := widget(nil, "")
			w.Finalizers = []string{commonv1alpha1.Finalizer}
			w.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "x"}
			kube := newKube(t, w)
			if err := kube.Delete(ctx, w); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
				t.Fatal(err)
			}
			stale := w.DeepCopyObject().(*Widget)
			// A concurrent write the stale copy does not show.
			cur := w.DeepCopyObject().(*Widget)
			if tc.newID == "" {
				base := cur.DeepCopyObject().(*Widget)
				reconcile.SetReady(cur, metav1.ConditionFalse, commonv1alpha1.ReasonDeleting, "waiting")
				if err := kube.Status().Patch(ctx, cur, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
			} else {
				cur.Annotations[commonv1alpha1.AnnotationExternalID] = tc.newID
				if err := kube.Update(ctx, cur); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			_, err := reconcile.Finalize(ctx, kube, stale, "", func(context.Context, string) error { calls++; return nil })
			if calls != 1 {
				t.Fatalf("deleteExternal called %d times", calls)
			}
			var stored Widget
			removed := apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(w), &stored))
			if removed != tc.wantRemoved {
				t.Fatalf("finalizer removed = %v, want %v (err %v)", removed, tc.wantRemoved, err)
			}
			if tc.wantRemoved && err != nil {
				t.Fatalf("err %v", err)
			}
			if !tc.wantRemoved && !apierrors.IsConflict(err) {
				t.Fatalf("err %v, want a Conflict", err)
			}
		})
	}
}
