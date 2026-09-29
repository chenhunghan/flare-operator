package generic_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/reconcile"
)

// TestDeleteRequiresOwnership: finalizing an object with deletionPolicy Delete deletes the
// resource only with proof that the object owns it: its owner tag, or status.id (set only after
// the object created or claimed the resource). A tags read that answers 500 (a never-tagged
// resource, or a transient failure: indistinguishable) is not proof. Each case builds an object
// that already carries the finalizer and the external-id annotation, deletes it and runs one
// reconcile directly.
func TestDeleteRequiresOwnership(t *testing.T) {
	h := newHarness(t, accountOnly)
	en := entry(t, "KVNamespace")
	tagsFault := func(t *testing.T) {
		t.Helper()
		// Every tags read of this account answers 500 (Times is far above what one finalize uses).
		if err := h.e.Control.InjectFault(h.ctx(), fake.Fault{Method: http.MethodGet, PathRegex: "^/accounts/" + h.acct.AccountID + "/tags$", Status: 500, Times: 50}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.e.Control.ClearFaults(context.Background()) })
	}
	setTag := func(id, owner string) {
		h.mustAPI(http.MethodPut, h.path("/accounts/{account_id}/tags", ""), map[string]any{
			"resource_type": "kv_namespace", "resource_id": id, "tags": map[string]string{reconcile.OwnerTagKey: owner}})
	}
	cases := []struct {
		name string
		// setup prepares resource id; statusID says whether status.id records it.
		setup    func(t *testing.T, id, me string)
		statusID bool
		deleted  bool
	}{
		{name: "foreign owner, tags unreadable", setup: func(t *testing.T, id, _ string) { setTag(id, "other/ns/obj"); tagsFault(t) }},
		{name: "never tagged, never synced", setup: func(*testing.T, string, string) {}},
		{name: "tags unreadable, never synced", setup: func(t *testing.T, _, _ string) { tagsFault(t) }},
		{name: "tags unreadable, ownership proven by status.id", setup: func(t *testing.T, _, _ string) { tagsFault(t) }, statusID: true, deleted: true},
		{name: "owner tag names the object", setup: func(_ *testing.T, id, me string) { setTag(id, me) }, deleted: true},
		{name: "foreign owner despite status.id", setup: func(_ *testing.T, id, _ string) { setTag(id, "other/ns/obj") }, statusID: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := h.createExternal(en, `{"title":"`+randName("flare-spike")+`"}`)
			name := "own-" + string(rune('a'+i))
			me := reconcile.OwnerValue("testenv", h.ns, name)
			rec := &recorder{}
			ev := events.NewFakeRecorder(10)
			r := &generic.Reconciler{
				Client:          h.e.Client,
				Accounts:        reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec})),
				Tagger:          reconcile.ResourceTagger{},
				ClusterName:     "testenv",
				Descriptor:      en.Descriptor,
				TagResourceType: en.TagResourceType,
				Recorder:        ev,
				New:             func() reconcile.ManagedObject { return en.New().(reconcile.ManagedObject) },
			}
			obj := h.newObj(en, name, `{"deletionPolicy":"Delete","forProvider":{"title":"x"}}`)
			obj.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
			obj.SetFinalizers([]string{commonv1alpha1.Finalizer})
			h.create(obj)
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
