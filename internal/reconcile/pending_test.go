package reconcile_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
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

// TestAdoptPendingCreate: a found resource is recorded as the object's own create; nothing to
// look up without a record or with an ID; a lookup that can never succeed is given up with a
// Warning event; a transient one is returned for a retry.
func TestAdoptPendingCreate(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, pending bool) (client.Client, *Widget) {
		w := widget(nil, "")
		w.UID = "uid-a"
		kube := newKube(t, w)
		if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
			t.Fatal(err)
		}
		if pending {
			if err := reconcile.MarkCreatePending(ctx, kube, w, "my-name"); err != nil {
				t.Fatal(err)
			}
		}
		return kube, w
	}
	lookup := func(id string, err error) func(context.Context, string) (string, error) {
		return func(_ context.Context, key string) (string, error) {
			if key != "my-name" {
				t.Errorf("lookup of %q, want my-name", key)
			}
			return id, err
		}
	}
	t.Run("found", func(t *testing.T) {
		kube, w := setup(t, true)
		id, err := reconcile.AdoptPendingCreate(ctx, kube, nil, w, "thing", lookup("cf-9", nil))
		if err != nil || id != "cf-9" {
			t.Fatalf("= %q, %v", id, err)
		}
		var stored Widget
		if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
			t.Fatal(err)
		}
		if a := stored.Annotations; a[commonv1alpha1.AnnotationExternalID] != "cf-9" || !reconcile.HasOwnershipProof(&stored, "cf-9") {
			t.Errorf("stored annotations %v, want the ID and its ownership proof", a)
		}
		if _, ok := reconcile.PendingCreate(&stored); ok {
			t.Error("the record was not cleared")
		}
	})
	t.Run("no record", func(t *testing.T) {
		kube, w := setup(t, false)
		if id, err := reconcile.AdoptPendingCreate(ctx, kube, nil, w, "thing", lookup("cf-9", nil)); err != nil || id != "" {
			t.Errorf("= %q, %v; want nothing", id, err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		kube, w := setup(t, true)
		if id, err := reconcile.AdoptPendingCreate(ctx, kube, nil, w, "thing", lookup("", nil)); err != nil || id != "" {
			t.Errorf("= %q, %v; want nothing", id, err)
		}
	})
	for name, lerr := range map[string]error{
		"permanent": &cfclient.APIError{Status: http.StatusForbidden, Errors: []cfclient.ErrorDetail{{Code: 10000, Message: "no"}}},
		"ambiguous": errors.Join(errors.New("2 things"), reconcile.ErrAmbiguousName),
	} {
		t.Run(name, func(t *testing.T) {
			kube, w := setup(t, true)
			rec := events.NewFakeRecorder(5)
			if id, err := reconcile.AdoptPendingCreate(ctx, kube, rec, w, "thing", lookup("", lerr)); err != nil || id != "" {
				t.Errorf("= %q, %v; want given up", id, err)
			}
			select {
			case ev := <-rec.Events:
				if !strings.Contains(ev, reconcile.EventReasonExternalResourceKept) {
					t.Errorf("event %q", ev)
				}
			default:
				t.Error("no Warning event")
			}
		})
	}
	t.Run("transient", func(t *testing.T) {
		kube, w := setup(t, true)
		lerr := &cfclient.APIError{Status: http.StatusServiceUnavailable}
		if _, err := reconcile.AdoptPendingCreate(ctx, kube, nil, w, "thing", lookup("", lerr)); !errors.Is(err, lerr) {
			t.Errorf("err %v, want the lookup error for a retry", err)
		}
	})
}
