package reconcile_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/reconcile"
)

// TestCreatePending: the record names the object's UID and key, survives as metadata, is not
// inherited by a copy with another UID, and is cleared by RecordCreated, RecordOwnership and
// PersistExternalID.
func TestCreatePending(t *testing.T) {
	ctx := context.Background()
	for name, record := range map[string]func(context.Context, client.Client, reconcile.ManagedObject, string) error{
		"RecordCreated": reconcile.RecordCreated, "RecordOwnership": reconcile.RecordOwnership, "PersistExternalID": reconcile.PersistExternalID,
	} {
		t.Run(name, func(t *testing.T) {
			w := widget(nil, "")
			w.UID = "uid-p"
			kube := newKube(t, w)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
				t.Fatal(err)
			}
			w.Status.Note = "in-memory"
			if err := reconcile.MarkCreatePending(ctx, kube, w, "my-name"); err != nil {
				t.Fatal(err)
			}
			if k, ok := reconcile.PendingCreate(w); !ok || k != "my-name" || w.Status.Note != "in-memory" {
				t.Fatalf("PendingCreate = %q, %v; status %+v", k, ok, w.Status)
			}
			rv := w.ResourceVersion
			if err := reconcile.MarkCreatePending(ctx, kube, w, "my-name"); err != nil || w.ResourceVersion != rv {
				t.Errorf("a second identical mark wrote (err %v)", err)
			}
			var stored Widget
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
				t.Fatal(err)
			}
			if k, ok := reconcile.PendingCreate(&stored); !ok || k != "my-name" {
				t.Errorf("stored record %v", stored.Annotations)
			}
			copied := stored
			copied.UID = "uid-other"
			if _, ok := reconcile.PendingCreate(&copied); ok {
				t.Error("a copy with another UID inherits the record")
			}
			if err := record(ctx, kube, w, "cf-1"); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
				t.Fatal(err)
			}
			if _, ok := stored.Annotations[reconcile.AnnotationCreatePending]; ok || stored.Annotations[commonv1alpha1.AnnotationExternalID] != "cf-1" {
				t.Errorf("annotations after %s: %v", name, stored.Annotations)
			}
		})
	}
	w := widget(nil, "")
	w.UID = "uid-q"
	kube := newKube(t, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	if err := reconcile.MarkCreatePending(ctx, kube, w, "a\nb"); err == nil {
		t.Error("a key with a newline was accepted")
	}
	if err := reconcile.MarkCreatePending(ctx, kube, w, "n"); err != nil {
		t.Fatal(err)
	}
	if err := reconcile.ClearCreatePending(ctx, kube, w); err != nil {
		t.Fatal(err)
	}
	if _, ok := reconcile.PendingCreate(w); ok {
		t.Error("ClearCreatePending left the record")
	}
}

func TestThrottled(t *testing.T) {
	if _, ok := reconcile.Throttled(errors.New("x")); ok {
		t.Error("a plain error is throttled")
	}
	if _, ok := reconcile.Throttled(&cfclient.APIError{Status: 500}); ok {
		t.Error("a 500 is throttled")
	}
	if d, ok := reconcile.Throttled(&cfclient.APIError{Status: http.StatusTooManyRequests}); !ok || d != reconcile.MinThrottleRequeue {
		t.Errorf("429 without Retry-After: %v %v", d, ok)
	}
	err := &cfclient.APIError{Status: http.StatusTooManyRequests, RetryAfter: 90 * time.Second, Errors: []cfclient.ErrorDetail{{Code: 971, Message: "slow"}}}
	if d, ok := reconcile.Throttled(err); !ok || d != 90*time.Second {
		t.Errorf("429 with Retry-After 90s: %v %v", d, ok)
	}
	w := widget(nil, "")
	reconcile.MarkSyncError(w, "", err)
	c := reconcile.GetCondition(w, commonv1alpha1.ConditionSynced)
	if c.Status != metav1.ConditionFalse || c.Reason != reconcile.ReasonRateLimited || !strings.Contains(c.Message, "1m30s") {
		t.Errorf("Synced = %+v", c)
	}
	reconcile.MarkSyncError(w, "Custom", err)
	if c := reconcile.GetCondition(w, commonv1alpha1.ConditionSynced); c.Reason != "Custom" {
		t.Errorf("an explicit reason was replaced: %+v", c)
	}
	res, derr := reconcile.DeletionResult(w, err)
	if derr != nil || res.RequeueAfter != 90*time.Second {
		t.Errorf("DeletionResult(429) = %+v, %v; want a 90s requeue without error", res, derr)
	}
}
