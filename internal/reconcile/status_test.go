package reconcile_test

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// A status write built from a copy older than the stored object (a lagging cache) conflicts
// instead of replacing the newer conditions; one built from the current copy, or from a copy
// whose only newer versions are its own reconcile's metadata writes, lands.
func TestPatchStatusStaleBase(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		obj  client.Object
		set  func(client.Object, metav1.ConditionStatus, string)
		get  func(client.Object) *metav1.Condition
	}{
		{name: "managed kind", obj: widget(nil, ""),
			set: func(o client.Object, s metav1.ConditionStatus, r string) {
				reconcile.SetReady(o.(*Widget), s, r, "")
			},
			get: func(o client.Object) *metav1.Condition {
				return reconcile.GetCondition(o.(*Widget), commonv1alpha1.ConditionReady)
			}},
		{name: "CloudflareAccount", obj: &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "acct"}},
			set: func(o client.Object, s metav1.ConditionStatus, r string) {
				a := o.(*cloudflarev1alpha1.CloudflareAccount)
				meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: commonv1alpha1.ConditionReady, Status: s, Reason: r})
			},
			get: func(o client.Object) *metav1.Condition {
				return meta.FindStatusCondition(o.(*cloudflarev1alpha1.CloudflareAccount).Status.Conditions, commonv1alpha1.ConditionReady)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newKube(t, tc.obj)
			key := client.ObjectKeyFromObject(tc.obj)
			read := func() client.Object {
				t.Helper()
				o := tc.obj.DeepCopyObject().(client.Object)
				if err := kube.Get(ctx, key, o); err != nil {
					t.Fatal(err)
				}
				return o
			}
			stale := read() // the copy the cache still serves

			// The previous reconcile, from the current copy: Ready=True lands.
			cur := read()
			base := cur.DeepCopyObject().(client.Object)
			tc.set(cur, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
			if err := reconcile.PatchStatus(ctx, kube, cur, base); err != nil {
				t.Fatalf("status write from the current copy: %v", err)
			}

			// The next reconcile starts from the stale copy and computes Ready=False.
			base = stale.DeepCopyObject().(client.Object)
			tc.set(stale, metav1.ConditionFalse, commonv1alpha1.ReasonCreating)
			err := reconcile.PatchStatus(ctx, kube, stale, base)
			if !apierrors.IsConflict(err) {
				t.Fatalf("status write from a stale copy: %v, want a Conflict", err)
			}
			if c := tc.get(read()); c == nil || c.Status != metav1.ConditionTrue {
				t.Fatalf("the stale write replaced Ready=True: %+v", c)
			}

			// A copy advanced by its own reconcile's metadata write (the response's
			// resourceVersion; status kept in memory) is current: its status write lands.
			own := read()
			base = own.DeepCopyObject().(client.Object)
			if _, err := reconcile.EnsureFinalizer(ctx, kube, own); err != nil {
				t.Fatal(err)
			}
			tc.set(own, metav1.ConditionFalse, commonv1alpha1.ReasonUnavailable)
			if err := reconcile.PatchStatus(ctx, kube, own, base); err != nil {
				t.Fatalf("status write after the reconcile's own metadata write: %v", err)
			}
			if c := tc.get(read()); c == nil || c.Reason != commonv1alpha1.ReasonUnavailable {
				t.Fatalf("Ready after the write: %+v", c)
			}
		})
	}
}

func TestStatusWritten(t *testing.T) {
	ctx := context.Background()
	gr := schema.GroupResource{Group: "flare.dev", Resource: "widgets"}
	conflict := apierrors.NewConflict(gr, "w", errors.New("modified"))
	gone := apierrors.NewNotFound(gr, "w")
	boom := errors.New("boom")
	poll := ctrl.Result{RequeueAfter: 42}
	quiet := ctrl.Result{RequeueAfter: reconcile.StatusConflictRetry}
	for _, tc := range []struct {
		name      string
		err, perr error
		wantRes   ctrl.Result
		wantErr   func(error) bool
	}{
		{"written", nil, nil, poll, func(e error) bool { return e == nil }},
		{"written, reconcile error", boom, nil, poll, func(e error) bool { return e == boom }},
		{"gone", nil, gone, poll, func(e error) bool { return e == nil }},
		{"conflict: quiet requeue", nil, conflict, quiet, func(e error) bool { return e == nil }},
		{"conflict after a conflicting metadata write", conflict, conflict, quiet, func(e error) bool { return e == nil }},
		{"conflict keeps another reconcile error", boom, conflict, poll, func(e error) bool { return errors.Is(e, boom) }},
		{"other write error", nil, boom, poll, func(e error) bool { return errors.Is(e, boom) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := reconcile.StatusWritten(ctx, poll, tc.err, tc.perr)
			if res != tc.wantRes || !tc.wantErr(err) {
				t.Fatalf("got %+v, %v", res, err)
			}
		})
	}
}
