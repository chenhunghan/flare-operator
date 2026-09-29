package generic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// TestAdopt adopts existing resources by the external-id annotation and by name, then deletes
// the objects with the kinds' default deletionPolicy (Orphan): the resources stay and lose
// their owner tag.
func TestAdopt(t *testing.T) {
	for _, kc := range kindCases {
		t.Run(kc.kind, func(t *testing.T) {
			h := newHarness(t, recorded)
			en := entry(t, kc.kind)
			byIDName, byNameName := randName("flare-spike"), randName("flare-spike")
			byID := h.createExternal(en, kc.fp(kc.create, byIDName))
			byName := h.createExternal(en, kc.fp(kc.create, byNameName))
			mark := h.rec.mark()

			a := h.newObj(en, "by-id", `{"forProvider":`+kc.fp(kc.create, byIDName)+`}`)
			a.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: byID})
			b := h.newObj(en, "by-name", `{"forProvider":`+kc.fp(kc.create, byNameName)+`}`)
			h.create(a)
			h.create(b)
			h.waitSynced(a, en)
			h.waitSynced(b, en)
			if a.GetResourceStatus().ID != byID {
				t.Errorf("by id: status.id %q, want %q", a.GetResourceStatus().ID, byID)
			}
			if b.GetResourceStatus().ID != byName || b.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != byName {
				t.Errorf("by name: status.id %q annotation %q, want %q", b.GetResourceStatus().ID, b.GetAnnotations()[commonv1alpha1.AnnotationExternalID], byName)
			}
			for _, r := range writesOf(h.rec.since(mark)) {
				if r.Method == http.MethodPost && r.Path == h.path(en.CreatePath, "") {
					t.Errorf("adoption created a resource: %s %s", r.Method, r.Body)
				}
			}
			for _, x := range []struct{ id, name string }{{byID, "by-id"}, {byName, "by-name"}} {
				if o := h.ownerTag(kc.tagType, x.id); o != reconcile.OwnerValue("testenv", h.ns, x.name) {
					t.Errorf("%s: owner tag %q", x.name, o)
				}
			}
			h.assertNoWrites("adopted objects", 4*poll, h.path(en.ItemPath, byName))

			// Orphan (kind default): the resources survive, ownership is released.
			h.delete(a)
			h.delete(b)
			h.waitGone(a)
			h.waitGone(b)
			for _, id := range []string{byID, byName} {
				if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
					t.Errorf("orphaned %s: %v", id, err)
				}
				if o := h.ownerTag(kc.tagType, id); o != "" {
					t.Errorf("orphaned %s still owned by %q", id, o)
				}
			}
			h.checkSpecViolations()
		})
	}
}

// TestObserveOnly: managementPolicies [Observe] never write (not even ownership tags), need no
// forProvider (the CEL rule relaxes the create-required fields), follow external changes, and
// deletion never deletes in Cloudflare.
func TestObserveOnly(t *testing.T) {
	for _, kc := range kindCases {
		t.Run(kc.kind, func(t *testing.T) {
			h := newHarness(t, recorded)
			en := entry(t, kc.kind)
			name := randName("flare-spike")
			id := h.createExternal(en, kc.fp(kc.create, name))
			item := h.path(en.ItemPath, id)
			mark := h.rec.mark()

			obj := h.newObj(en, "observer", `{"managementPolicies":["Observe"],"deletionPolicy":"Delete"}`)
			obj.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
			h.create(obj)
			h.waitFor(obj, "observed", func() (bool, string) {
				return condIs(obj, commonv1alpha1.ConditionReady, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable) &&
					condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonObserveOnly) &&
					obj.GetResourceStatus().ID == id, "conditions"
			})
			if ap := atProvider(t, obj); ap[en.NameField] != name {
				t.Errorf("atProvider.%s = %v, want %s", en.NameField, ap[en.NameField], name)
			}
			// External change: observed, not reverted.
			var drift map[string]any
			_ = json.Unmarshal([]byte(kc.fp(kc.driftBody, name)), &drift)
			h.mustAPI(kc.driftMethod, item, drift)
			want := h.mustAPI(http.MethodGet, item, nil)
			h.waitFor(obj, "drift observed", func() (bool, string) {
				ap := atProvider(t, obj)
				return generic.Covers(pickJSON(want, drift), ap), "atProvider not updated"
			})
			h.assertNoWrites("observe-only", 4*poll, item)
			if w := writesOf(h.rec.since(mark)); len(w) != 0 {
				t.Errorf("observe-only object wrote:\n%s", summary(w))
			}
			if o := h.ownerTag(kc.tagType, id); o != "" {
				t.Errorf("observe-only object tagged the resource: %q", o)
			}

			// A missing resource is reported, not created.
			ghost := h.newObj(en, "ghost", `{"managementPolicies":["Observe"],"forProvider":`+kc.fp(kc.create, randName("flare-spike"))+`}`)
			ghost.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: strings.Repeat("0", 32)})
			h.create(ghost)
			h.waitFor(ghost, "not found", func() (bool, string) {
				return condIs(ghost, commonv1alpha1.ConditionReady, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound), "conditions"
			})

			// Deleting never deletes in Cloudflare (Observe does not allow Delete).
			h.delete(obj)
			h.delete(ghost)
			h.waitGone(obj)
			h.waitGone(ghost)
			if _, err := h.api(http.MethodGet, item, nil); err != nil {
				t.Errorf("observe-only delete removed the resource: %v", err)
			}
			if w := writesOf(h.rec.since(mark)); len(w) != 0 {
				t.Errorf("observe-only objects wrote:\n%s", summary(w))
			}
		})
	}
}

func pickJSON(obj, like map[string]any) map[string]any {
	out := map[string]any{}
	for k := range like {
		out[k] = obj[k]
	}
	return out
}

// TestCreateRequiredCEL: the fields the create body requires are required only when the
// management policies allow Create.
func TestCreateRequiredCEL(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 30*time.Second)
	for _, kc := range kindCases {
		en := entry(t, kc.kind)
		for _, c := range []struct {
			policies string
			ok       bool
		}{
			{``, false},
			{`"managementPolicies":[],`, false},
			{`"managementPolicies":["*"],`, false},
			{`"managementPolicies":["Observe","Create"],`, false},
			{`"managementPolicies":["Observe"],`, true},
			{`"managementPolicies":["Observe","Update","Delete"],`, true},
		} {
			obj := en.New().(reconcile.ManagedObject)
			if err := json.Unmarshal([]byte(`{"spec":{`+c.policies+`"accountRef":{"name":"a"},"forProvider":{}}}`), obj); err != nil {
				t.Fatal(err)
			}
			obj.SetNamespace(ns)
			obj.SetGenerateName("cel-")
			err := e.Client.Create(ctx, obj)
			if c.ok != (err == nil) {
				t.Errorf("%s %s: create err=%v, want ok=%v", kc.kind, c.policies, err, c.ok)
			}
			if err != nil && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "forProvider."+en.NameField+" is required")) {
				t.Errorf("%s: unexpected error %v", kc.kind, err)
			}
		}
	}
}

// TestOwnershipConflict: a resource tagged as owned by another object is not adopted by name
// and not written to.
func TestOwnershipConflict(t *testing.T) {
	h := newHarness(t, recorded)
	en := entry(t, "KVNamespace")
	name := randName("flare-spike")
	id := h.createExternal(en, `{"title":"`+name+`"}`)
	h.mustAPI(http.MethodPut, h.path("/accounts/{account_id}/tags", ""), map[string]any{
		"resource_type": "kv_namespace", "resource_id": id, "tags": map[string]string{reconcile.OwnerTagKey: "other/ns/obj"}})
	mark := h.rec.mark()
	obj := h.newObj(en, "late", `{"deletionPolicy":"Delete","forProvider":{"title":"`+name+`-new"}}`)
	obj.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
	h.create(obj)
	h.waitFor(obj, "conflict", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced)
		return c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "owned by"), "no conflict"
	})
	if c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced); !strings.Contains(c.Message, "remove the "+reconcile.OwnerTagKey+" tag") {
		t.Errorf("conflict message gives no recovery hint: %q", c.Message)
	}
	if w := writesOf(h.rec.since(mark)); len(w) != 0 {
		t.Errorf("wrote to a resource owned by someone else:\n%s", summary(w))
	}
	if got := h.mustAPI(http.MethodGet, h.path(en.ItemPath, id), nil); got["title"] != name {
		t.Errorf("title changed to %v", got["title"])
	}
	// Deleting the object (deletionPolicy Delete) must not delete the other owner's resource.
	h.delete(obj)
	h.waitGone(obj)
	if w := writesOf(h.rec.since(mark)); len(w) != 0 {
		t.Errorf("deletion wrote to a resource owned by someone else:\n%s", summary(w))
	}
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
		t.Errorf("the other owner's resource: %v", err)
	}
	if o := h.ownerTag("kv_namespace", id); o != "other/ns/obj" {
		t.Errorf("owner tag %q, want other/ns/obj", o)
	}

	// By name: same outcome, and the ID is not pinned.
	name2 := randName("flare-spike")
	id2 := h.createExternal(en, `{"title":"`+name2+`"}`)
	h.mustAPI(http.MethodPut, h.path("/accounts/{account_id}/tags", ""), map[string]any{
		"resource_type": "kv_namespace", "resource_id": id2, "tags": map[string]string{reconcile.OwnerTagKey: "other/ns/obj"}})
	byName := h.newObj(en, "late-by-name", `{"forProvider":{"title":"`+name2+`"}}`)
	h.create(byName)
	h.waitFor(byName, "conflict", func() (bool, string) {
		c := reconcile.GetCondition(byName, commonv1alpha1.ConditionSynced)
		return c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "owned by"), "no conflict"
	})
	if a := byName.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; a != "" {
		t.Errorf("pinned someone else's resource: %s", a)
	}
}

// TestDeleteAccountStates: deletion waits while the CloudflareAccount exists but is not Ready
// (both Delete and Orphan need it: the Orphan path releases the owner tag). A deleting account
// is held by its in-use finalizer and still serves the deletion of its dependents. Once the
// account is really gone (its finalizer removed by hand), deletion completes without touching
// Cloudflare and a Warning event says the resource was kept.
func TestDeleteAccountStates(t *testing.T) {
	h := newHarness(t, recorded)
	en := entry(t, "KVNamespace")
	orphan := h.newObj(en, "orphan", `{"deletionPolicy":"Orphan","forProvider":{"title":"`+randName("flare-spike")+`"}}`)
	del := h.newObj(en, "del", `{"deletionPolicy":"Delete","forProvider":{"title":"`+randName("flare-spike")+`"}}`)
	h.create(orphan)
	h.create(del)
	h.waitSynced(orphan, en)
	h.waitSynced(del, en)
	orphanID, delID := orphan.GetResourceStatus().ID, del.GetResourceStatus().ID

	// Account not Ready (a spec change not yet verified: points at a missing Secret).
	acct := &cloudflarev1alpha1.CloudflareAccount{}
	setSecret := func(name string) {
		t.Helper()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "acct"}, acct); err != nil {
				return err
			}
			acct.Spec.TokenSecretRef.Name = name
			return h.e.Client.Update(h.ctx(), acct)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	good := h.acct.Spec.TokenSecretRef.Name
	setSecret("missing")
	h.e.WaitAccountCondition(t, h.ns, "acct", metav1.ConditionFalse, "")
	h.delete(orphan)
	h.waitFor(orphan, "orphan waits for the account", func() (bool, string) {
		c := reconcile.GetCondition(orphan, commonv1alpha1.ConditionReady)
		return c != nil && c.Reason == commonv1alpha1.ReasonDeleting && strings.Contains(c.Message, "not Ready"), "not waiting"
	})
	if o := h.ownerTag("kv_namespace", orphanID); o != reconcile.OwnerValue("testenv", h.ns, "orphan") {
		t.Errorf("owner tag %q changed while the account was not Ready", o)
	}
	setSecret(good)
	h.waitGone(orphan)
	if o := h.ownerTag("kv_namespace", orphanID); o != "" {
		t.Errorf("orphaned resource still owned by %q", o)
	}

	// Account deleting: held by its in-use finalizer while del exists, and del's deletion
	// still deletes in Cloudflare (it owns the resource: it created it).
	kept := h.newObj(en, "kept", `{"deletionPolicy":"Delete","forProvider":{"title":"`+randName("flare-spike")+`"}}`)
	h.create(kept)
	h.waitSynced(kept, en)
	keptID := kept.GetResourceStatus().ID
	if err := h.e.Client.Delete(h.ctx(), acct); err != nil {
		t.Fatal(err)
	}
	h.delete(del)
	h.waitGone(del)
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, delID), nil); !cfclient.IsNotFound(err) {
		t.Errorf("resource of an object deleted while its account was deleting: %v, want 404", err)
	}

	// Account gone (its in-use finalizer removed by hand): kept is finalized without reaching
	// Cloudflare, with a Warning event.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "acct"}, acct); err != nil {
			return err
		}
		acct.Finalizers = nil
		return h.e.Client.Update(h.ctx(), acct)
	})
	if err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "acct"}, &cloudflarev1alpha1.CloudflareAccount{})
		return apierrors.IsNotFound(err), "account still there"
	})
	mark := h.rec.mark()
	h.delete(kept)
	h.waitGone(kept)
	if w := writesOf(h.rec.since(mark)); len(w) != 0 {
		t.Errorf("wrote without an account:\n%s", summary(w))
	}
	if _, err := h.api(http.MethodGet, h.path(en.ItemPath, keptID), nil); err != nil {
		t.Errorf("resource of the deleted account's object: %v", err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var evs eventsv1.EventList
		if err := h.e.Client.List(h.ctx(), &evs, client.InNamespace(h.ns)); err != nil {
			return false, err.Error()
		}
		for _, ev := range evs.Items {
			if ev.Regarding.Name == "kept" && ev.Reason == "ExternalResourceKept" && ev.Type == corev1.EventTypeWarning && strings.Contains(ev.Note, keptID) {
				return true, ""
			}
		}
		return false, "no ExternalResourceKept event"
	})
}

// TestSingleton runs the singleton path directly (no generated singleton kind exists yet): a
// Queue descriptor turned into a fixed settings object at one queue's item path. No create, no
// delete, updates only; a second reconcile writes nothing.
func TestSingleton(t *testing.T) {
	h := newHarness(t, accountOnly)
	en := entry(t, "Queue")
	qid := h.createExternal(en, `{"queue_name":"`+randName("flare-spike")+`"}`)
	d := en.Descriptor
	d.Singleton = true
	d.CreatePath, d.ListPath, d.IDField, d.NameField = "", "", "", ""
	d.ItemPath = "/accounts/{account_id}/queues/" + qid
	d.DefaultDeletionPolicy = "Orphan"
	rec := &recorder{}
	r := &generic.Reconciler{
		Client:      h.e.Client,
		Accounts:    reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec}), anyBaseURL),
		Tagger:      reconcile.ResourceTagger{},
		ClusterName: "testenv",
		Descriptor:  d,
		New:         func() reconcile.ManagedObject { return en.New().(reconcile.ManagedObject) },
	}
	// No Create in the policies (a singleton is never created; it also keeps the Queue CRD's CEL
	// rule from requiring queue_name). Delete is allowed and deletionPolicy is Delete, yet a
	// singleton must not be deleted.
	obj := h.newObj(en, "settings", `{"managementPolicies":["Observe","Update","Delete"],"deletionPolicy":"Delete","forProvider":{"settings":{"delivery_delay":7}}}`)
	h.create(obj)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	ctx := context.Background()
	for i := 0; i < 3; i++ { // finalizer, then sync, then an idempotent pass
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	w := writesOf(rec.since(0))
	if len(w) != 1 || w[0].Method != http.MethodPatch || !jsonEqual(w[0].Body, `{"settings":{"delivery_delay":7}}`) {
		t.Fatalf("singleton writes:\n%s", summary(w))
	}
	if err := h.get(obj); err != nil {
		t.Fatal(err)
	}
	if !reconcile.IsReady(obj) || obj.GetResourceStatus().ID != "" {
		t.Errorf("singleton status: id %q %s", obj.GetResourceStatus().ID, conditions(obj))
	}
	h.delete(obj)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	h.waitGone(obj)
	if w := writesOf(rec.since(0)); len(w) != 1 {
		t.Errorf("singleton deletion wrote:\n%s", summary(w[1:]))
	}
	if _, err := h.api(http.MethodGet, h.path(d.ItemPath, ""), nil); err != nil {
		t.Errorf("singleton target deleted: %v", err)
	}
}
