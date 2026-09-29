package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

// conflictingPatches fails every Patch with a Conflict while fail is set (a cache that never
// catches up within the retry budget).
type conflictingPatches struct {
	client.Client
	fail bool
}

func (c *conflictingPatches) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if c.fail {
		return apierrors.NewConflict(schema.GroupResource{Resource: "widgets"}, obj.GetName(), errors.New("stale"))
	}
	return c.Client.Patch(ctx, obj, p, opts...)
}

// A finalizer whose removal keeps conflicting after the Cloudflare delete is retried by a
// requeue; that pass must not DELETE again (the deletion is remembered per UID), and once the
// removal works the finalizer comes off. Another object (UID) with the same ID still deletes.
func TestFinalizeDeletesOncePerObject(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	// A fresh UID per run: the deletion memory is process-wide (go test -count=N).
	w.UID = types.UID(fmt.Sprintf("uid-once-%d", time.Now().UnixNano()))
	w.Finalizers = []string{commonv1alpha1.Finalizer}
	w.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "x"}
	kube := &conflictingPatches{Client: newKube(t, w), fail: true}
	if err := kube.Delete(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	calls := 0
	del := func(context.Context, string) error { calls++; return nil }
	if _, err := reconcile.Finalize(ctx, kube, w, "", del); !apierrors.IsConflict(err) {
		t.Fatalf("first pass: err %v, want the Conflict", err)
	}
	if _, err := reconcile.Finalize(ctx, kube, w, "", del); !apierrors.IsConflict(err) {
		t.Fatalf("second pass: err %v, want the Conflict", err)
	}
	if calls != 1 {
		t.Fatalf("deleteExternal called %d times over two passes, want 1", calls)
	}
	kube.fail = false
	if _, err := reconcile.Finalize(ctx, kube, w, "", del); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if calls != 1 {
		t.Fatalf("deleteExternal called %d times over three passes, want 1", calls)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &Widget{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not removed: %v", err)
	}

	other := widget(nil, "")
	other.Name = "other"
	other.UID = types.UID(fmt.Sprintf("uid-other-%d", time.Now().UnixNano()))
	other.Finalizers = []string{commonv1alpha1.Finalizer}
	other.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "x"}
	kube2 := newKube(t, other)
	if err := kube2.Delete(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := kube2.Get(ctx, client.ObjectKeyFromObject(other), other); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Finalize(ctx, kube2, other, "", del); err != nil || calls != 2 {
		t.Fatalf("another object with the same ID: err %v, %d calls, want 2", err, calls)
	}
}
