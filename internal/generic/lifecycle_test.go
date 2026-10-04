package generic_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/generic/descriptors"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// kindCase describes one generated kind for the lifecycle tests. forProvider values are JSON
// with {name} standing for the resource name.
type kindCase struct {
	kind, tagType string
	create        string // initial forProvider
	update        string // an allowed change
	immutable     string // a change of an immutable field (plus an allowed one)
	// drift changes the resource behind the operator's back (method, body).
	driftMethod string
	driftBody   string
	// createBody lists the top-level fields the create request must carry.
	createBody []string
	// postCreate is the body the update right after create must carry ("" = no such update).
	postCreate string
}

var kindCases = []kindCase{
	{
		kind: "KVNamespace", tagType: "kv_namespace",
		create:      `{"title":"{name}"}`,
		update:      `{"title":"{name}-renamed"}`,
		immutable:   `{"title":"{name}-other","jurisdiction":"eu"}`, // jurisdiction is create-only
		driftMethod: http.MethodPut, driftBody: `{"title":"{name}-drifted"}`,
		createBody: []string{"title"},
	},
	{
		kind: "Queue", tagType: "queue",
		// Every settable setting is covered; delivery_paused is never read back (write-only).
		create:      `{"queue_name":"{name}","settings":{"delivery_delay":5,"delivery_paused":false}}`,
		update:      `{"queue_name":"{name}","settings":{"delivery_delay":10,"delivery_paused":true,"message_retention_period":3600}}`,
		immutable:   `{"queue_name":"{name}","jurisdiction":"eu","settings":{"delivery_delay":20}}`,
		driftMethod: http.MethodPatch, driftBody: `{"settings":{"delivery_delay":0}}`,
		createBody: []string{"queue_name"},
		// settings are not in the create body (0028): applied by PATCH right after create.
		postCreate: `{"settings":{"delivery_delay":5,"delivery_paused":false}}`,
	},
	{
		kind: "D1Database", tagType: "d1_database",
		create:      `{"name":"{name}","primary_location_hint":"WEUR","read_replication":{"mode":"disabled"}}`,
		update:      `{"name":"{name}","primary_location_hint":"WEUR","read_replication":{"mode":"auto"}}`,
		immutable:   `{"name":"{name}","primary_location_hint":"ENAM","read_replication":{"mode":"disabled"}}`, // write-only + immutable
		driftMethod: http.MethodPatch, driftBody: `{"read_replication":{"mode":"disabled"}}`,
		createBody: []string{"name", "primary_location_hint", "read_replication"},
	},
}

func (k kindCase) fp(tmpl, name string) string { return strings.ReplaceAll(tmpl, "{name}", name) }

func jsonEqual(a []byte, b string) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

// TestLifecycle drives each kind through create, idempotent re-reconciles, update, drift
// correction, an immutable change, external deletion (→ recreate) and deletion with
// deletionPolicy Delete.
func TestLifecycle(t *testing.T) {
	for _, kc := range kindCases {
		t.Run(kc.kind, func(t *testing.T) {
			h := newHarness(t, recorded)
			en := entry(t, kc.kind)
			name := randName("flare-spike")
			obj := h.newObj(en, "obj", `{"deletionPolicy":"Delete","forProvider":`+kc.fp(kc.create, name)+`}`)

			// Create.
			h.create(obj)
			h.waitSynced(obj, en)
			id := obj.GetResourceStatus().ID
			if id == "" || obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != id {
				t.Fatalf("status.id %q, annotation %q", id, obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID])
			}
			item := h.path(en.ItemPath, id)
			creates := 0
			var postCreate []request
			for _, r := range writesOf(h.rec.since(0)) {
				switch {
				case r.Method == http.MethodPost && r.Path == h.path(en.CreatePath, ""):
					creates++
					var body map[string]any
					_ = json.Unmarshal(r.Body, &body)
					for _, f := range kc.createBody {
						if _, ok := body[f]; !ok {
							t.Errorf("create body %s lacks %s", r.Body, f)
						}
					}
					for f := range body {
						if !has(en.CreateFields, f) {
							t.Errorf("create body carries %s, not a CreateField", f)
						}
					}
				case r.Path == item:
					postCreate = append(postCreate, r)
				}
			}
			if creates != 1 {
				t.Errorf("%d creates, want 1", creates)
			}
			if kc.postCreate != "" {
				if len(postCreate) != 1 || postCreate[0].Method != en.UpdateMethod || !jsonEqual(postCreate[0].Body, kc.postCreate) {
					t.Errorf("post-create update: got\n%s want %s %s", summary(postCreate), en.UpdateMethod, kc.postCreate)
				}
			} else if len(postCreate) != 0 {
				t.Errorf("unexpected item writes after create:\n%s", summary(postCreate))
			}
			if o := h.ownerTag(kc.tagType, id); o != reconcile.OwnerValue("testenv", h.ns, "obj") {
				t.Errorf("owner tag %q", o)
			}

			// Idempotent: repeated reconciles of an unchanged object make zero writes.
			h.assertNoWrites("in-sync object", 3, item)

			// Update.
			mark := h.rec.mark()
			h.setForProvider(obj, kc.fp(kc.update, name))
			h.waitSynced(obj, en)
			ups := writesOf(h.rec.since(mark))
			if len(ups) != 1 || ups[0].Method != en.UpdateMethod || ups[0].Path != item {
				t.Fatalf("update: got\n%s", summary(ups))
			}
			var body map[string]any
			_ = json.Unmarshal(ups[0].Body, &body)
			for f := range body {
				if !has(en.UpdateFields, f) {
					t.Errorf("update body carries %s, not an UpdateField", f)
				}
			}
			if en.UpdateMethod == http.MethodPut && len(body) != len(en.UpdateFields) {
				t.Errorf("PUT body %s is not the full UpdateFields body %v", ups[0].Body, en.UpdateFields)
			}
			h.assertNoWrites("after update", 2, item)

			// Drift: a change made outside the operator is reverted.
			mark = h.rec.mark()
			var drift map[string]any
			_ = json.Unmarshal([]byte(kc.fp(kc.driftBody, name)), &drift)
			h.mustAPI(kc.driftMethod, item, drift)
			h.waitFor(obj, "drift corrected", func() (bool, string) {
				for _, w := range writesOf(h.rec.since(mark)) {
					if w.Method == en.UpdateMethod && w.Path == item {
						return true, ""
					}
				}
				return false, "no " + en.UpdateMethod + " " + item + " yet"
			})
			h.waitSynced(obj, en)
			// The recorder sees a request when it is sent, before the fake applies it: wait
			// for the correction to land rather than read the resource once.
			var got map[string]any
			testenv.Eventually(t, time.Minute, func() (bool, string) {
				got = h.mustAPI(http.MethodGet, item, nil)
				return covers(t, kc.fp(kc.update, name), got, en), fmt.Sprintf("after drift correction the resource is %v", got)
			})

			// Immutable change: Synced=False/Immutable and no write at all (made past the CRD's
			// CEL immutability rules, which reject an in-place change at once).
			h.setImmutable(obj, kc.fp(kc.immutable, name))
			h.waitFor(obj, "Immutable", func() (bool, string) {
				return condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, commonv1alpha1.ReasonImmutable), "not Immutable"
			})
			h.assertNoWrites("immutable change", 2, item)
			h.setImmutable(obj, kc.fp(kc.update, name))
			h.waitSynced(obj, en)

			// External delete → recreated (new ID, annotation follows).
			h.mustAPI(http.MethodDelete, item, nil)
			h.waitFor(obj, "recreated", func() (bool, string) {
				nid := obj.GetResourceStatus().ID
				return nid != "" && nid != id && obj.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == nid, "id " + nid
			})
			h.waitSynced(obj, en)
			id = obj.GetResourceStatus().ID
			item = h.path(en.ItemPath, id)
			h.assertNoWrites("after recreate", 2, item)

			// Delete (deletionPolicy Delete): DELETE without a body, then gone.
			mark = h.rec.mark()
			h.delete(obj)
			h.waitGone(obj)
			var dels []request
			for _, r := range writesOf(h.rec.since(mark)) {
				if r.Method == http.MethodDelete && r.Path == item {
					dels = append(dels, r)
				}
			}
			if len(dels) != 1 || len(dels[0].Body) != 0 {
				t.Errorf("delete: got\n%s", summary(dels))
			}
			if _, err := h.api(http.MethodGet, item, nil); !cfclient.IsNotFound(err) {
				t.Errorf("GET after delete: %v, want 404", err)
			}
			h.checkSpecViolations()
		})
	}
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// covers reports whether an API object matches forProvider JSON (write-only fields skipped).
func covers(t *testing.T, forProvider string, got map[string]any, en descriptors.Entry) bool {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal([]byte(forProvider), &want); err != nil {
		t.Fatal(err)
	}
	stripWriteOnly(want, en.WriteOnly)
	return coversJSON(want, got)
}

func coversJSON(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !coversJSON(v, g[k]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(want) == fmt.Sprint(got)
	}
}

// TestRegisteredControllers runs the controllers as cmd/manager registers them.
func TestRegisteredControllers(t *testing.T) {
	h := newHarness(t, registered)
	for _, kc := range kindCases {
		en := entry(t, kc.kind)
		obj := h.newObj(en, strings.ToLower(kc.kind), `{"forProvider":`+kc.fp(kc.create, randName("flare-spike"))+`}`)
		h.create(obj)
		h.waitSynced(obj, en)
		if obj.GetResourceStatus().ID == "" {
			t.Errorf("%s: no status.id", kc.kind)
		}
		// Default deletionPolicy of these data-bearing kinds is Orphan: the resource stays.
		id := obj.GetResourceStatus().ID
		h.delete(obj)
		h.waitGone(obj)
		if _, err := h.api(http.MethodGet, h.path(en.ItemPath, id), nil); err != nil {
			t.Errorf("%s: orphaned resource: %v", kc.kind, err)
		}
	}
}
