package generic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
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
// deletionPolicy Delete). So does a duplicate-name refusal (409, or 400, which some APIs use
// for a duplicate name) of a first attempt: another writer won the race between the list and
// the POST (two clusters or objects applying the same bucket name), and its bucket must not be
// adopted.
func TestR2BucketRefusedCreateDropsRecord(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	create := "^" + regexp.QuoteMeta(x.path(x.en.CreatePath, "")) + "$"
	for _, c := range []struct {
		status  int
		code    int
		message string
	}{
		{http.StatusForbidden, 10042, "Please enable R2 through the Cloudflare Dashboard."},
		{http.StatusConflict, 10004, "The bucket you tried to create already exists, and you own it."},
		{http.StatusBadRequest, 10004, "The bucket you tried to create already exists, and you own it."},
	} {
		name := randName("flare-spike")
		obj := x.newObj(x.en, fmt.Sprintf("bucket-%d", c.status), fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
		x.create(obj)
		if err := x.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost, PathRegex: create, Status: c.status, Code: c.code,
			Message: c.message, Times: 1}); err != nil {
			t.Fatal(err)
		}
		if err := x.reconcile(obj, 1); err == nil {
			t.Fatalf("%d: the refused create returned no error", c.status)
		}
		if _, pending := reconcile.PendingCreate(obj); pending || obj.GetResourceStatus().ID != "" {
			t.Errorf("%d: create-pending record kept %v, want dropped; status.id %q", c.status, pending, obj.GetResourceStatus().ID)
		}
		// Someone else creates the bucket (for 400/409: the other writer's bucket becomes
		// visible): it is not this object's.
		if _, err := x.bucketAPI(http.MethodPost, x.path(x.en.CreatePath, ""), "", map[string]any{"name": name}); err != nil {
			t.Fatal(err)
		}
		mark := x.rec.mark()
		x.mustReconcile(obj, 1)
		if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reconcile.ReasonNameConflict) || obj.GetResourceStatus().ID != "" {
			t.Errorf("%d: foreign bucket after a refused create: status.id %q\n%s", c.status, obj.GetResourceStatus().ID, conditions(obj))
		}
		x.delete(obj)
		x.mustReconcile(obj, 1)
		x.waitGone(obj)
		if w := writesOf(x.rec.since(mark)); len(w) != 0 {
			t.Errorf("%d: wrote to the foreign bucket:\n%s", c.status, summary(w))
		}
		if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); err != nil {
			t.Errorf("%d: the foreign bucket after the object's deletion: %v", c.status, err)
		}
	}
}

// TestR2BucketRetryDuplicateKeepsRecord: a duplicate-name refusal (409) of a retry keeps the
// create-pending record. The earlier attempt (here answered 500) may have created the bucket
// and a lagging list missed it, so when the bucket shows up it is this object's own lost
// create: adopted, and deleted by its finalizer under deletionPolicy Delete.
func TestR2BucketRetryDuplicateKeepsRecord(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	create := "^" + regexp.QuoteMeta(x.path(x.en.CreatePath, "")) + "$"
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
	x.create(obj)
	for _, f := range []fake.Fault{
		{Method: http.MethodPost, PathRegex: create, Status: http.StatusInternalServerError, Code: 10001, Message: "Internal error", Times: 1},
		{Method: http.MethodPost, PathRegex: create, Status: http.StatusConflict, Code: 10004, Message: "The bucket you tried to create already exists, and you own it.", Times: 1},
	} {
		if err := x.e.Fake.InjectFault(f); err != nil {
			t.Fatal(err)
		}
		if err := x.reconcile(obj, 1); err == nil {
			t.Fatalf("%d: the failed create returned no error", f.Status)
		}
		if key, pending := reconcile.PendingCreate(obj); !pending || key != name {
			t.Fatalf("after %d: create-pending record %q/%v, want %q kept", f.Status, key, pending, name)
		}
	}
	// The first attempt's bucket becomes visible.
	if _, err := x.bucketAPI(http.MethodPost, x.path(x.en.CreatePath, ""), "", map[string]any{"name": name}); err != nil {
		t.Fatal(err)
	}
	x.mustReconcile(obj, 1)
	if obj.GetResourceStatus().ID != name || !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonReconcileOK) {
		t.Fatalf("own lost create not adopted: status.id %q\n%s", obj.GetResourceStatus().ID, conditions(obj))
	}
	x.delete(obj)
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); !cfclient.IsNotFound(err) {
		t.Errorf("own lost create after the object's deletion (deletionPolicy Delete): %v, want 404", err)
	}
}

// TestR2BucketCORSNarrowed: the CORS PUT replaces the whole policy, so a rule narrowed in
// forProvider (allowed.headers, exposeHeaders and maxAgeSeconds dropped) is written, and the
// wider policy does not stay behind an object reporting Synced=True.
func TestR2BucketCORSNarrowed(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	wide := `{"rules":[{"allowed":{"origins":["https://example.com"],"methods":["GET"],"headers":["*"]},"exposeHeaders":["ETag"],"maxAgeSeconds":3600}]}`
	narrow := `{"rules":[{"allowed":{"origins":["https://example.com"],"methods":["GET"]}}]}`
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"forProvider":{"name":%q,"cors":%s}}`, name, wide))
	x.create(obj)
	x.mustReconcile(obj, 2)
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"cors":%s}`, name, narrow))
	mark := x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 1 || w[0].Method != http.MethodPut {
		t.Errorf("narrowed cors, want one PUT:\n%s", summary(w))
	}
	resp, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name)+"/cors", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal([]byte(narrow), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cors on Cloudflare %s, want %s", resp, narrow)
	}
	if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonReconcileOK) {
		t.Errorf("after narrowing:\n%s", conditions(obj))
	}
	// In sync now: no further writes.
	mark = x.rec.mark()
	x.mustReconcile(obj, 1)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("an in-sync cors policy was written again:\n%s", summary(w))
	}
	x.checkSpecViolations()
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

// laggingCache is the reconciler's client with a Get that serves a stale view of the object:
// lag edits every copy it returns, as an informer cache that has not seen the reconciler's
// latest writes yet would. Writes (and the APIReader) go to the API server.
type laggingCache struct {
	client.Client
	lag func(client.Object)
}

func (c *laggingCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.lag != nil {
		c.lag(obj)
	}
	return nil
}

// setAnnotation sets (v != "") or removes annotation k.
func setAnnotation(o client.Object, k, v string) {
	a := maps.Clone(o.GetAnnotations())
	if a == nil {
		a = map[string]string{}
	}
	if v == "" {
		delete(a, k)
	} else {
		a[k] = v
	}
	o.SetAnnotations(a)
}

// TestR2BucketStaleCacheKeepsDroppedRecord: a first-attempt duplicate-name refusal drops the
// create-pending record, and a retry whose cached copy still has it (the cache saw the record
// written but not dropped; the watch filters both writes out, and the error retry follows in
// milliseconds) does not take the other writer's bucket for its own lost create: NameConflict,
// no ownership proof, and the object's deletion (deletionPolicy Delete) leaves the bucket alone.
func TestR2BucketStaleCacheKeepsDroppedRecord(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	cache := &laggingCache{Client: x.e.Client}
	x.r.Client, x.r.APIReader = cache, x.e.Client
	create := "^" + regexp.QuoteMeta(x.path(x.en.CreatePath, "")) + "$"
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
	x.create(obj)
	if err := x.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost, PathRegex: create, Status: http.StatusConflict, Code: 10004,
		Message: "The bucket you tried to create already exists, and you own it.", Times: 1}); err != nil {
		t.Fatal(err)
	}
	if err := x.reconcile(obj, 1); err == nil {
		t.Fatal("the refused create returned no error")
	}
	if _, pending := reconcile.PendingCreate(obj); pending {
		t.Fatalf("first-attempt 409: create-pending record kept")
	}
	// The other writer's bucket, and a cache that still shows the dropped record.
	if _, err := x.bucketAPI(http.MethodPost, x.path(x.en.CreatePath, ""), "", map[string]any{"name": name}); err != nil {
		t.Fatal(err)
	}
	record := string(obj.GetUID()) + "/" + name
	cache.lag = func(o client.Object) { setAnnotation(o, reconcile.AnnotationCreatePending, record) }

	mark := x.rec.mark()
	x.mustReconcile(obj, 1)
	a := obj.GetAnnotations()
	if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reconcile.ReasonNameConflict) || obj.GetResourceStatus().ID != "" ||
		a[commonv1alpha1.AnnotationExternalID] != "" || a[reconcile.AnnotationOwnershipProof] != "" {
		t.Errorf("stale-cache reconcile adopted the foreign bucket: status.id %q annotations %v\n%s", obj.GetResourceStatus().ID, a, conditions(obj))
	}
	x.delete(obj)
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("wrote to the foreign bucket:\n%s", summary(w))
	}
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); err != nil {
		t.Errorf("the foreign bucket after the object's deletion (stale cache): %v", err)
	}
}

// TestR2BucketStaleCacheMissesRecord is the inverse lag: the cached copy lacks the record of an
// earlier attempt (answered 500, so it may have created the bucket). The retry's 409 is still a
// retry's refusal and keeps the record, and the bucket that shows up is adopted as the object's
// own lost create and deleted with it.
func TestR2BucketStaleCacheMissesRecord(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	cache := &laggingCache{Client: x.e.Client}
	x.r.Client, x.r.APIReader = cache, x.e.Client
	create := "^" + regexp.QuoteMeta(x.path(x.en.CreatePath, "")) + "$"
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
	x.create(obj)
	for i, f := range []fake.Fault{
		{Method: http.MethodPost, PathRegex: create, Status: http.StatusInternalServerError, Code: 10001, Message: "Internal error", Times: 1},
		{Method: http.MethodPost, PathRegex: create, Status: http.StatusConflict, Code: 10004, Message: "The bucket you tried to create already exists, and you own it.", Times: 1},
	} {
		if err := x.e.Fake.InjectFault(f); err != nil {
			t.Fatal(err)
		}
		if err := x.reconcile(obj, 1); err == nil {
			t.Fatalf("%d: the failed create returned no error", f.Status)
		}
		if key, pending := reconcile.PendingCreate(obj); !pending || key != name {
			t.Fatalf("after %d: create-pending record %q/%v, want %q kept", f.Status, key, pending, name)
		}
		if i == 0 { // from now on the cache has not seen the record
			cache.lag = func(o client.Object) { setAnnotation(o, reconcile.AnnotationCreatePending, "") }
		}
	}
	if _, err := x.bucketAPI(http.MethodPost, x.path(x.en.CreatePath, ""), "", map[string]any{"name": name}); err != nil {
		t.Fatal(err)
	}
	x.mustReconcile(obj, 1)
	if obj.GetResourceStatus().ID != name || !reconcile.HasOwnershipProof(obj, name) {
		t.Fatalf("own lost create not adopted (stale cache): status.id %q annotations %v\n%s", obj.GetResourceStatus().ID, obj.GetAnnotations(), conditions(obj))
	}
	x.delete(obj)
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); !cfclient.IsNotFound(err) {
		t.Errorf("own lost create after the object's deletion (deletionPolicy Delete): %v, want 404", err)
	}
}
