package generic_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/generic"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// TestDeleteRequiresOwnership: finalizing an object with deletionPolicy Delete deletes the
// resource only with proof that the object owns it (reconcile.MayDeleteExternal): its ownership
// record (reconcile.AnnotationOwnershipProof, written after create or a successful EnsureOwner)
// or its owner tag. status.id is not proof (sync sets it for any observed resource, e.g. on the
// Observe-only path), and neither is a tags read that answers 500. Each case builds an object
// that already carries the finalizer and the external-id annotation, deletes it and runs one
// reconcile directly.
func TestDeleteRequiresOwnership(t *testing.T) {
	h := newHarness(t, accountOnly)
	en := entry(t, "KVNamespace")
	tagsFaultStatus := func(t *testing.T, status int) {
		t.Helper()
		// Every tags read of this account fails (Times is far above what one finalize uses).
		if err := h.e.Control.InjectFault(h.ctx(), fake.Fault{Method: http.MethodGet, PathRegex: "^/accounts/" + h.acct.AccountID + "/tags$", Status: status, Times: 50}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.e.Control.ClearFaults(context.Background()) })
	}
	tagsFault := func(t *testing.T) { t.Helper(); tagsFaultStatus(t, 500) }
	deleteExternal := func(t *testing.T, id string) {
		t.Helper()
		h.mustAPI(http.MethodDelete, h.path(en.ItemPath, id), nil)
	}
	setTag := func(id, owner string) {
		h.mustAPI(http.MethodPut, h.path("/accounts/{account_id}/tags", ""), map[string]any{
			"resource_type": "kv_namespace", "resource_id": id, "tags": map[string]string{reconcile.OwnerTagKey: owner}})
	}
	cases := []struct {
		name string
		// setup prepares resource id; statusID says whether status.id names it, record whether
		// the object carries its ownership record for it.
		setup            func(t *testing.T, id, me string)
		statusID, record bool
		deleted          bool
		// gone: the resource no longer exists before the finalizer runs; nothing is deleted
		// and no Warning is due.
		gone bool
	}{
		{name: "foreign owner, tags unreadable", setup: func(t *testing.T, id, _ string) { setTag(id, "other/ns/obj"); tagsFault(t) }},
		{name: "never tagged, never synced", setup: func(*testing.T, string, string) {}},
		{name: "tags unreadable, never synced", setup: func(t *testing.T, _, _ string) { tagsFault(t) }},
		// The reviewer's case: status.id was set by an observe (no EnsureOwner), then the tags
		// read answers 500. That is not proof.
		{name: "tags unreadable, status.id only", setup: func(t *testing.T, _, _ string) { tagsFault(t) }, statusID: true},
		{name: "untagged, status.id only", setup: func(*testing.T, string, string) {}, statusID: true},
		{name: "tags unreadable, ownership recorded", setup: func(t *testing.T, _, _ string) { tagsFault(t) }, statusID: true, record: true, deleted: true},
		{name: "owner tag names the object", setup: func(_ *testing.T, id, me string) { setTag(id, me) }, deleted: true},
		{name: "foreign owner despite status.id", setup: func(_ *testing.T, id, _ string) { setTag(id, "other/ns/obj") }, statusID: true},
		{name: "foreign owner despite the ownership record", setup: func(_ *testing.T, id, _ string) { setTag(id, "other/ns/obj") }, statusID: true, record: true},
		// A token without Resource Tagging permission: the tag can never be read, so the resource
		// is kept (with a Warning) instead of retrying forever.
		{name: "tags read forbidden, no record", setup: func(t *testing.T, _, _ string) { tagsFaultStatus(t, http.StatusForbidden) }, statusID: true},
		{name: "tags read forbidden, ownership recorded", setup: func(t *testing.T, _, _ string) { tagsFaultStatus(t, http.StatusForbidden) }, statusID: true, record: true, deleted: true},
		// Already gone, no record: no spurious ExternalResourceKept, whatever the tags read says.
		{name: "already gone, tags 404", setup: func(t *testing.T, id, _ string) { deleteExternal(t, id); tagsFaultStatus(t, http.StatusNotFound) }, statusID: true, gone: true},
		{name: "already gone, never tagged", setup: func(t *testing.T, id, _ string) { deleteExternal(t, id) }, statusID: true, gone: true},
		{name: "already gone, tags unreadable", setup: func(t *testing.T, id, _ string) { deleteExternal(t, id); tagsFault(t) }, gone: true},
		{name: "tags 404, resource exists", setup: func(t *testing.T, _, _ string) { tagsFaultStatus(t, http.StatusNotFound) }, statusID: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := h.createExternal(en, `{"title":"`+randName("flare-spike")+`"}`)
			name := "own-" + string(rune('a'+i))
			me := reconcile.OwnerValue("testenv", h.ns, name)
			rec := &recorder{}
			ev := events.NewFakeRecorder(10)
			r := &generic.Reconciler{
				Client:      h.e.Client,
				Accounts:    reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec}), anyBaseURL),
				Tagger:      reconcile.ResourceTagger{},
				ClusterName: "testenv",
				Descriptor:  en.Descriptor,
				Recorder:    ev,
				New:         func() reconcile.ManagedObject { return en.New().(reconcile.ManagedObject) },
			}
			obj := h.newObj(en, name, `{"deletionPolicy":"Delete","forProvider":{"title":"x"}}`)
			obj.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
			obj.SetFinalizers([]string{commonv1alpha1.Finalizer})
			h.create(obj)
			if tc.record {
				// What reconcile.RecordOwnership writes (it needs the UID the API server assigned).
				base := obj.DeepCopyObject().(client.Object)
				a := obj.GetAnnotations()
				a[reconcile.AnnotationOwnershipProof] = string(obj.GetUID()) + "/" + id
				obj.SetAnnotations(a)
				if err := h.e.Client.Patch(h.ctx(), obj, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
				if !reconcile.HasOwnershipProof(obj, id) {
					t.Fatal("HasOwnershipProof does not accept the record")
				}
			}
			if tc.statusID {
				base := obj.DeepCopyObject().(client.Object)
				obj.GetResourceStatus().ID = id
				if err := h.e.Client.Status().Patch(h.ctx(), obj, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
			}
			tc.setup(t, id, me)
			h.delete(obj)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			h.waitGone(obj)
			_ = h.e.Control.ClearFaults(h.ctx())
			var dels []request
			for _, w := range writesOf(rec.since(0)) {
				if w.Method == http.MethodDelete && strings.HasSuffix(w.Path, id) {
					dels = append(dels, w)
				}
			}
			_, gerr := h.api(http.MethodGet, h.path(en.ItemPath, id), nil)
			if tc.gone {
				if len(dels) != 0 {
					t.Errorf("DELETE sent for a resource that was already gone:\n%s", summary(rec.since(0)))
				}
				select {
				case e := <-ev.Events:
					t.Errorf("event for a resource that was already gone: %q", e)
				default:
				}
				return
			}
			if tc.deleted {
				if len(dels) != 1 || gerr == nil {
					t.Errorf("not deleted (DELETEs %d, GET err %v):\n%s", len(dels), gerr, summary(rec.since(0)))
				}
				return
			}
			if len(dels) != 0 || gerr != nil {
				t.Errorf("deleted without proof of ownership (GET err %v):\n%s", gerr, summary(rec.since(0)))
			}
			select {
			case e := <-ev.Events:
				if !strings.HasPrefix(e, "Warning ExternalResourceKept") || !strings.Contains(e, id) {
					t.Errorf("event %q", e)
				}
			default:
				t.Error("no Warning event for the resource left in Cloudflare")
			}
		})
	}
}

// TestCreateWithoutUpdate: with Update not allowed, the fields only an update can apply (Queue
// settings, 0028) are not sent after create, so the object must not claim Synced=True, and the
// write-only hash must not record delivery_paused as applied.
func TestCreateWithoutUpdate(t *testing.T) {
	h := newHarness(t, recorded)
	en := entry(t, "Queue")
	mark := h.rec.mark()
	obj := h.newObj(en, "no-update", `{"managementPolicies":["Observe","Create","Delete"],"forProvider":{"queue_name":"`+randName("flare-spike")+`","settings":{"delivery_paused":true}}}`)
	h.create(obj)
	h.waitFor(obj, "pending settings reported", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced)
		return c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "settings") &&
			strings.Contains(c.Message, "do not allow Update"), conditions(obj)
	})
	if wo := obj.GetResourceStatus().WriteOnlyHash; strings.Contains(wo, "delivery_paused") {
		t.Errorf("writeOnlyHash %q records a field that was never sent", wo)
	}
	for _, w := range writesOf(h.rec.since(mark)) {
		if w.Method == http.MethodPatch || (w.Method == http.MethodPut && !strings.HasSuffix(w.Path, "/tags")) {
			t.Errorf("update despite managementPolicies: %s %s %s", w.Method, w.Path, w.Body)
		}
	}
}

// tagReadsFail makes every tags read of the harness's account answer 500: the tags GET, and
// (with index) the tag index that disambiguates it.
func (h *harness) tagReadsFail(index bool) {
	h.t.Helper()
	re := "^/accounts/" + h.acct.AccountID + "/tags$"
	if index {
		re = "^/accounts/" + h.acct.AccountID + "/tags(/resources)?$"
	}
	if err := h.e.Control.InjectFault(h.ctx(), fake.Fault{Method: http.MethodGet, PathRegex: re, Status: 500, Times: 100000}); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = h.e.Control.ClearFaults(context.Background()) })
}

// TestObservedIsNotOwned is the reviewer's scenario: an Observe-only object adopts a resource by
// name (status.id is set, nothing is written), is switched to full management and deletionPolicy
// Delete but hits an ownership conflict, and is deleted while every tags read fails. status.id
// is not proof of ownership, so the finalizer waits for a readable tag instead of deleting, and
// once the foreign owner tag is readable again the resource is kept.
func TestObservedIsNotOwned(t *testing.T) {
	h := newHarness(t, recorded)
	en := entry(t, "KVNamespace")
	name := randName("flare-spike")
	id := h.createExternal(en, `{"title":"`+name+`"}`)
	h.mustAPI(http.MethodPut, h.path("/accounts/{account_id}/tags", ""), map[string]any{
		"resource_type": "kv_namespace", "resource_id": id, "tags": map[string]string{reconcile.OwnerTagKey: "other/ns/obj"}})
	mark := h.rec.mark()
	obj := h.newObj(en, "watcher", `{"managementPolicies":["Observe"],"forProvider":{"title":"`+name+`"}}`)
	h.create(obj)
	h.waitFor(obj, "observed by name", func() (bool, string) {
		return obj.GetResourceStatus().ID == id && condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonObserveOnly), conditions(obj)
	})
	for _, k := range []string{commonv1alpha1.AnnotationExternalID, reconcile.AnnotationOwnershipProof} {
		if v := obj.GetAnnotations()[k]; v != "" {
			t.Errorf("Observe-only object wrote annotation %s=%s", k, v)
		}
	}
	if w := writesOf(h.rec.since(mark)); len(w) != 0 {
		t.Errorf("Observe-only object wrote:\n%s", summary(w))
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := h.get(obj); err != nil {
			return err
		}
		obj.GetResourceSpec().ManagementPolicies = nil
		obj.GetResourceSpec().DeletionPolicy = commonv1alpha1.DeletionDelete
		return h.e.Client.Update(h.ctx(), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(obj, "conflict", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced)
		return c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "owned by"), conditions(obj)
	})
	if obj.GetResourceStatus().ID != id {
		t.Fatalf("status.id %q, want %s (the scenario needs it set)", obj.GetResourceStatus().ID, id)
	}
	if reconcile.HasOwnershipProof(obj, id) {
		t.Fatal("ownership recorded despite the conflict")
	}

	h.tagReadsFail(true)
	j0 := len(h.e.Journal(t))
	h.delete(obj)
	// The finalizer ran, found the tags unreadable and said so, and retried at least once.
	h.waitFor(obj, "finalizer waiting for a readable tag", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == commonv1alpha1.ReasonDeleting &&
			strings.Contains(c.Message, "ownership tag"), conditions(obj)
	})
	h.e.WaitJournal(t, j0, 2*time.Minute, func(j []fake.JournalEntry) (bool, string) {
		n := len(testenv.Filter(testenv.ForAccount(j, h.acct.AccountID), func(e fake.JournalEntry) bool {
			return e.Method == http.MethodGet && e.Fault && strings.Contains(e.Path, "/tags")
		}))
		return n >= 2, fmt.Sprintf("%d failed tag reads, waiting for a retry", n)
	})
	if err := h.get(obj); err != nil {
		t.Fatalf("finalized while ownership was unknown: %v", err)
	}
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
		t.Fatalf("deleted a resource the object never owned: %v", err)
	}
	if err := h.e.Control.ClearFaults(h.ctx()); err != nil {
		t.Fatal(err)
	}
	h.waitGone(obj)
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
		t.Errorf("deleted a resource the object never owned: %v", err)
	}
	if o := h.ownerTag("kv_namespace", id); o != "other/ns/obj" {
		t.Errorf("owner tag %q, want other/ns/obj", o)
	}
	for _, w := range writesOf(h.rec.since(mark)) {
		if w.Method == http.MethodDelete || strings.HasSuffix(w.Path, "/tags") {
			t.Errorf("wrote to a resource owned by someone else: %s %s", w.Method, w.Path)
		}
	}
}

// TestOrphanReleaseRetried: an orphaned object releases its owner tag even when the tags GET
// answers 500. While the tag index cannot be read either, the release is retried (the finalizer
// stays); with only the GET failing, the index supplies the tags and etag and the release
// completes.
func TestOrphanReleaseRetried(t *testing.T) {
	h := newHarness(t, recorded)
	en := entry(t, "KVNamespace")
	obj := h.newObj(en, "orphan", `{"deletionPolicy":"Orphan","forProvider":{"title":"`+randName("flare-spike")+`"}}`)
	h.create(obj)
	h.waitSynced(obj, en)
	id := obj.GetResourceStatus().ID
	me := reconcile.OwnerValue("testenv", h.ns, "orphan")
	if !reconcile.HasOwnershipProof(obj, id) {
		t.Errorf("no ownership record after create: %v", obj.GetAnnotations())
	}
	if o := h.ownerTag("kv_namespace", id); o != me {
		t.Fatalf("owner tag %q, want %s", o, me)
	}

	h.tagReadsFail(true)
	h.delete(obj)
	h.waitFor(obj, "release retried", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionReady)
		return c != nil && c.Reason == commonv1alpha1.ReasonDeleting && strings.Contains(c.Message, "release ownership tag"), conditions(obj)
	})
	if err := h.e.Control.ClearFaults(h.ctx()); err != nil {
		t.Fatal(err)
	}
	if o := h.ownerTag("kv_namespace", id); o != me {
		t.Fatalf("owner tag %q changed while it was unreadable", o)
	}
	h.tagReadsFail(false)
	h.waitGone(obj)
	if err := h.e.Control.ClearFaults(h.ctx()); err != nil {
		t.Fatal(err)
	}
	if o := h.ownerTag("kv_namespace", id); o != "" {
		t.Errorf("orphaned resource still owned by %q", o)
	}
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
		t.Errorf("orphaned resource: %v", err)
	}
}
