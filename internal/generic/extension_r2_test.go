package generic_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
)

// TestR2BucketCORSClear: an empty forProvider.cors ({"rules": []}, which the generated type
// turns into {}, or {} itself) removes the bucket's CORS policy with one DELETE of the
// sub-resource, and is in sync once the policy is gone. Covers alone would see {} as covering
// any policy: no write, the old rules kept, and Synced=True.
func TestR2BucketCORSClear(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"forProvider":{"name":%q,"cors":%s}}`, name, corsRule))
	x.create(obj)
	x.mustReconcile(obj, 2)
	if _, ok := atProvider(t, obj)["cors"]; !ok {
		t.Fatalf("no cors after create: %s\n%s", atJSON(t, obj), conditions(obj))
	}
	item := x.path(x.en.ItemPath, name)

	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"cors":{"rules":[]}}`, name))
	mark := x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 1 || w[0].Method != http.MethodDelete || w[0].Path != item+"/cors" {
		t.Errorf("clearing cors, want one DELETE of the policy:\n%s", summary(w))
	}
	if _, ok := atProvider(t, obj)["cors"]; ok || !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") {
		t.Errorf("after clearing: atProvider %s\n%s", atJSON(t, obj), conditions(obj))
	}
	if _, err := x.bucketAPI(http.MethodGet, item+"/cors", "", nil); !cfclient.IsNotFound(err) {
		t.Errorf("GET cors after clearing: %v, want 404", err)
	}

	// {} is the same request, and a cleared policy is in sync: reads only.
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"cors":{}}`, name))
	mark = x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("cleared cors wrote:\n%s", summary(w))
	}
	// A policy added outside the operator is removed again.
	if _, err := x.bucketAPI(http.MethodPut, item+"/cors", "", json.RawMessage(corsRule)); err != nil {
		t.Fatal(err)
	}
	mark = x.rec.mark()
	x.mustReconcile(obj, 1)
	if w := writesOf(x.rec.since(mark)); len(w) != 1 || w[0].Method != http.MethodDelete {
		t.Errorf("drifted cors, want one DELETE:\n%s", summary(w))
	}

	// Without Update in managementPolicies the policy is not cleared: Synced=False names the field.
	if _, err := x.bucketAPI(http.MethodPut, item+"/cors", "", json.RawMessage(corsRule)); err != nil {
		t.Fatal(err)
	}
	if err := x.tryForProvider(obj, fmt.Sprintf(`{"name":%q,"cors":{}}`, name), []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve, commonv1alpha1.ManageCreate, commonv1alpha1.ManageDelete}); err != nil {
		t.Fatal(err)
	}
	mark = x.rec.mark()
	x.mustReconcile(obj, 1)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("an object without Update wrote:\n%s", summary(w))
	}
	if c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "cors") {
		t.Errorf("without Update, a policy to clear: Synced %+v", c)
	}
	x.checkSpecViolations()
}

// TestR2BucketRefusedCreateDropsRecord: a create the API refuses for good (403, e.g. R2 not
// enabled) drops the create-pending record, so a bucket of that name that someone else creates
// later is a NameConflict, not this object's "own lost create" (adopted, and deleted with
// deletionPolicy Delete). A refusal that may answer a lost create (409, or 400, which some
// APIs use for a duplicate name) keeps the record.
func TestR2BucketRefusedCreateDropsRecord(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	create := "^" + regexp.QuoteMeta(x.path(x.en.CreatePath, "")) + "$"
	for _, c := range []struct {
		status int
		kept   bool
	}{{http.StatusForbidden, false}, {http.StatusConflict, true}, {http.StatusBadRequest, true}} {
		name := randName("flare-spike")
		obj := x.newObj(x.en, fmt.Sprintf("bucket-%d", c.status), fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
		x.create(obj)
		if err := x.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost, PathRegex: create, Status: c.status, Code: 10042,
			Message: "Please enable R2 through the Cloudflare Dashboard.", Times: 1}); err != nil {
			t.Fatal(err)
		}
		if err := x.reconcile(obj, 1); err == nil {
			t.Fatalf("%d: the refused create returned no error", c.status)
		}
		_, pending := reconcile.PendingCreate(obj)
		if pending != c.kept || obj.GetResourceStatus().ID != "" {
			t.Errorf("%d: create-pending record kept %v, want %v; status.id %q", c.status, pending, c.kept, obj.GetResourceStatus().ID)
		}
		if c.kept {
			continue
		}
		// Someone else creates the bucket: it is not this object's.
		if _, err := x.bucketAPI(http.MethodPost, x.path(x.en.CreatePath, ""), "", map[string]any{"name": name}); err != nil {
			t.Fatal(err)
		}
		mark := x.rec.mark()
		x.mustReconcile(obj, 1)
		if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reconcile.ReasonNameConflict) || obj.GetResourceStatus().ID != "" {
			t.Errorf("foreign bucket after a refused create: status.id %q\n%s", obj.GetResourceStatus().ID, conditions(obj))
		}
		x.delete(obj)
		x.mustReconcile(obj, 1)
		x.waitGone(obj)
		if w := writesOf(x.rec.since(mark)); len(w) != 0 {
			t.Errorf("wrote to the foreign bucket:\n%s", summary(w))
		}
		if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); err != nil {
			t.Errorf("the foreign bucket after the object's deletion: %v", err)
		}
	}
}

// TestR2BucketFinalizerJurisdiction: should a jurisdiction change get past the CRD rule, the
// finalizer still deletes the bucket in the jurisdiction it was read from, instead of finding
// nothing in the new one and dropping the bucket silently.
func TestR2BucketFinalizerJurisdiction(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
	x.create(obj)
	x.mustReconcile(obj, 2)
	base := obj.DeepCopyObject().(reconcile.ManagedObject)
	obj.GetResourceStatus().ID = "" // lifts the CEL rule (the external-id annotation stays)
	if err := x.e.Client.Status().Patch(x.ctx(), obj, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if err := x.tryForProvider(obj, fmt.Sprintf(`{"name":%q,"jurisdiction":"eu"}`, name), nil); err != nil {
		t.Fatal(err)
	}
	mark := x.rec.mark()
	x.delete(obj)
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	for _, r := range x.rec.since(mark) {
		if strings.Contains(r.Path, "/r2/buckets") && r.Header.Get("Cf-R2-Jurisdiction") != "" {
			t.Errorf("%s %s: cf-r2-jurisdiction %q, want none (the default jurisdiction)", r.Method, r.Path, r.Header.Get("Cf-R2-Jurisdiction"))
		}
	}
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); !cfclient.IsNotFound(err) {
		t.Errorf("the bucket after the object's deletion: %v, want 404", err)
	}
}
