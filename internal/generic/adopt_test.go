package generic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
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
	obj := h.newObj(en, "late", `{"forProvider":{"title":"`+name+`-new"}}`)
	obj.SetAnnotations(map[string]string{commonv1alpha1.AnnotationExternalID: id})
	h.create(obj)
	h.waitFor(obj, "conflict", func() (bool, string) {
		c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced)
		return c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "owned by"), "no conflict"
	})
	if w := writesOf(h.rec.since(mark)); len(w) != 0 {
		t.Errorf("wrote to a resource owned by someone else:\n%s", summary(w))
	}
	if got := h.mustAPI(http.MethodGet, h.path(en.ItemPath, id), nil); got["title"] != name {
		t.Errorf("title changed to %v", got["title"])
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
		Accounts:    reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec})),
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
