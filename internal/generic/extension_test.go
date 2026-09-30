package generic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/reconcile"
)

// extHarness runs the generic reconciler of one kind by hand (no controller), so a test sees
// exactly the requests of the reconciles it asks for.
type extHarness struct {
	*harness
	en  descriptors.Entry
	rec *recorder
	r   *generic.Reconciler
}

func newExtHarness(t *testing.T, kind string) *extHarness {
	t.Helper()
	if testing.Short() {
		t.Skip("the generic profile (R2Bucket) needs the pinned spec")
	}
	h := newHarness(t, accountOnly)
	en := entry(t, kind)
	rec := &recorder{}
	r := kindsReconciler(h, en, rec)
	return &extHarness{harness: h, en: en, rec: rec, r: r}
}

func kindsReconciler(h *harness, en descriptors.Entry, rec *recorder) *generic.Reconciler {
	return &generic.Reconciler{
		Client:      h.e.Client,
		Accounts:    reconcile.NewAccounts(h.e.Client, reconcile.WithHTTPClient(&http.Client{Transport: rec}), anyBaseURL),
		Tagger:      reconcile.NoopTagger{},
		ClusterName: "testenv",
		Descriptor:  en.Descriptor,
		Extension:   en.Extension,
		New:         func() reconcile.ManagedObject { return en.New().(reconcile.ManagedObject) },
	}
}

// reconcile runs n reconciles of obj and returns the last error; obj is re-read afterwards.
func (x *extHarness) reconcile(obj reconcile.ManagedObject, n int) error {
	x.t.Helper()
	var err error
	for i := 0; i < n; i++ {
		_, err = x.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	}
	if gerr := x.get(obj); gerr != nil && !apierrors.IsNotFound(gerr) {
		x.t.Fatal(gerr)
	}
	return err
}

func (x *extHarness) mustReconcile(obj reconcile.ManagedObject, n int) {
	x.t.Helper()
	if err := x.reconcile(obj, n); err != nil {
		x.t.Fatalf("reconcile: %v\n%s", err, conditions(obj))
	}
}

// bucketAPI calls the fake as someone outside the operator, with R2's jurisdiction header.
func (x *extHarness) bucketAPI(method, p, jurisdiction string, body any) (json.RawMessage, error) {
	x.t.Helper()
	req := cfclient.Request{Method: method, Path: p, Body: body}
	if jurisdiction != "" {
		req.Header = http.Header{"Cf-R2-Jurisdiction": {jurisdiction}}
	}
	resp, err := x.cf.Do(x.ctx(), req)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func atJSON(t *testing.T, obj reconcile.ManagedObject) string {
	b, err := json.Marshal(atProvider(t, obj))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const corsRule = `{"rules":[{"allowed":{"origins":["https://example.com"],"methods":["GET"]},"maxAgeSeconds":3600}]}`

// TestR2BucketExtensions: the generator.yaml extensions of R2Bucket in the generic reconciler.
// The jurisdiction travels as cf-r2-jurisdiction on every bucket request (and on none of the
// others); the storage class is updated by a bodiless PATCH with cf-r2-storage-class and read
// back as storage_class (no drift); the CORS policy is a sub-resource (GET into atProvider.cors,
// PUT when it differs, unset = unmanaged); in-sync objects make no writes.
func TestR2BucketExtensions(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q,"jurisdiction":"eu","storageClass":"Standard","cors":%s}}`, name, corsRule))
	x.create(obj)
	x.mustReconcile(obj, 2)
	if !reconcile.IsReady(obj) || obj.GetResourceStatus().ID != name {
		t.Fatalf("not created: status.id %q\n%s", obj.GetResourceStatus().ID, conditions(obj))
	}
	item := x.path(x.en.ItemPath, name)
	var sawBucket int
	for _, r := range x.rec.since(0) {
		if !strings.Contains(r.Path, "/r2/buckets") {
			if r.Header.Get("Cf-R2-Jurisdiction") != "" {
				t.Errorf("%s %s carries the jurisdiction header", r.Method, r.Path)
			}
			continue
		}
		sawBucket++
		if got := r.Header.Get("Cf-R2-Jurisdiction"); got != "eu" {
			t.Errorf("%s %s: cf-r2-jurisdiction %q, want eu", r.Method, r.Path, got)
		}
	}
	if w := writesOf(x.rec.since(0)); len(w) != 2 || w[0].Method != http.MethodPost || w[1].Method != http.MethodPut || w[1].Path != item+"/cors" {
		t.Errorf("create writes, want POST then PUT cors:\n%s", summary(w))
	}
	if sawBucket == 0 {
		t.Fatal("no bucket request recorded")
	}
	at := atProvider(t, obj)
	if at["jurisdiction"] != "eu" || at["storage_class"] != "Standard" || !strings.Contains(atJSON(t, obj), `"cors":{"rules":[{"allowed":{"methods":["GET"],"origins":["https://example.com"]},"maxAgeSeconds":3600}]}`) {
		t.Errorf("atProvider %s", atJSON(t, obj))
	}
	// The bucket lives in the EU jurisdiction only.
	if _, err := x.bucketAPI(http.MethodGet, item, "", nil); !cfclient.IsNotFound(err) {
		t.Errorf("GET without the jurisdiction header: %v, want 404", err)
	}
	if _, err := x.bucketAPI(http.MethodGet, item, "eu", nil); err != nil {
		t.Errorf("GET in the EU jurisdiction: %v", err)
	}

	// In sync: reads only.
	mark := x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("in-sync bucket wrote:\n%s", summary(w))
	}

	// Storage class: one bodiless PATCH carrying the class in its header.
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"jurisdiction":"eu","storageClass":"InfrequentAccess","cors":%s}`, name, corsRule))
	mark = x.rec.mark()
	x.mustReconcile(obj, 2)
	w := writesOf(x.rec.since(mark))
	if len(w) != 1 || w[0].Method != http.MethodPatch || w[0].Path != item || len(w[0].Body) != 0 ||
		w[0].Header.Get("Cf-R2-Storage-Class") != "InfrequentAccess" || w[0].Header.Get("Cf-R2-Jurisdiction") != "eu" {
		t.Errorf("storage class update, want one bodiless PATCH with both headers:\n%s%v", summary(w), headersOf(w))
	}
	if atProvider(t, obj)["storage_class"] != "InfrequentAccess" || !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") {
		t.Errorf("after the update: atProvider %s\n%s", atJSON(t, obj), conditions(obj))
	}

	// CORS: a change is one PUT of the sub-resource; dropping the field leaves it alone.
	newRule := strings.ReplaceAll(corsRule, "3600", "60")
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"jurisdiction":"eu","storageClass":"InfrequentAccess","cors":%s}`, name, newRule))
	mark = x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 1 || w[0].Method != http.MethodPut || w[0].Path != item+"/cors" || string(w[0].Body) != strings.ReplaceAll(`{"rules":[{"allowed":{"methods":["GET"],"origins":["https://example.com"]},"maxAgeSeconds":60}]}`, " ", "") {
		t.Errorf("cors update, want one PUT of the new policy:\n%s", summary(w))
	}
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q,"jurisdiction":"eu","storageClass":"InfrequentAccess"}`, name))
	mark = x.rec.mark()
	x.mustReconcile(obj, 2)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("unset cors wrote:\n%s", summary(w))
	}
	if !strings.Contains(atJSON(t, obj), `"maxAgeSeconds":60`) || !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") {
		t.Errorf("unmanaged cors: atProvider %s\n%s", atJSON(t, obj), conditions(obj))
	}
	x.checkSpecViolations()
}

func headersOf(rs []request) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%s %s: %v\n", r.Method, r.Path, r.Header)
	}
	return b.String()
}

// TestR2BucketJurisdictionImmutable: the jurisdiction header selects where the bucket lives.
// Once it exists the CRD refuses to set, change or remove forProvider.jurisdiction, and should
// a change get past the rule (here: status.id cleared), the controller refuses it before any
// request instead of looking in the other jurisdiction and creating a second bucket there.
func TestR2BucketJurisdictionImmutable(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q}}`, name))
	x.create(obj)
	x.mustReconcile(obj, 2)
	if !reconcile.IsReady(obj) || atProvider(t, obj)["jurisdiction"] != "default" {
		t.Fatalf("not created in the default jurisdiction: %s\n%s", atJSON(t, obj), conditions(obj))
	}
	for _, fp := range []string{
		fmt.Sprintf(`{"name":%q,"jurisdiction":"eu"}`, name),      // set
		fmt.Sprintf(`{"name":%q,"jurisdiction":"default"}`, name), // the default, spelled out: allowed below
	} {
		err := x.tryForProvider(obj, fp, nil)
		if strings.Contains(fp, `"eu"`) {
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "jurisdiction is immutable") {
				t.Errorf("setting jurisdiction eu: %v, want the CRD's immutable error", err)
			}
		} else if err != nil {
			t.Errorf("spelling out the default jurisdiction: %v", err)
		}
	}

	// Past the rule: without status.id the CRD allows the change; the controller does not.
	if err := x.get(obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(reconcile.ManagedObject)
	obj.GetResourceStatus().ID = ""
	if err := x.e.Client.Status().Patch(x.ctx(), obj, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if err := x.tryForProvider(obj, fmt.Sprintf(`{"name":%q,"jurisdiction":"eu"}`, name), nil); err != nil {
		t.Fatalf("change without status.id: %v", err)
	}
	mark := x.rec.mark()
	x.mustReconcile(obj, 2)
	if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, commonv1alpha1.ReasonImmutable) {
		t.Errorf("want Synced=False/Immutable:\n%s", conditions(obj))
	}
	for _, r := range x.rec.since(mark) {
		if strings.Contains(r.Path, "/r2/buckets") {
			t.Errorf("request after a jurisdiction change: %s %s", r.Method, r.Path)
		}
	}
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "eu", nil); !cfclient.IsNotFound(err) {
		t.Errorf("a bucket appeared in the EU jurisdiction: %v", err)
	}
	// Back to the bucket's jurisdiction: in sync again.
	x.setForProvider(obj, fmt.Sprintf(`{"name":%q}`, name))
	x.mustReconcile(obj, 2)
	if !condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") || obj.GetResourceStatus().ID != name {
		t.Errorf("after reverting: status.id %q\n%s", obj.GetResourceStatus().ID, conditions(obj))
	}
	x.checkSpecViolations()
}

// TestR2BucketDeleteRefused: the API refuses to delete a bucket that still holds objects. The
// error is surfaced as Synced=False, reason DeleteFailed, with the API's message; the finalizer
// stays and the next attempt that the API accepts completes the deletion. The error itself is
// injected (409/10008 is UNVERIFIED: flarefake models no objects).
func TestR2BucketDeleteRefused(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"deletionPolicy":"Delete","forProvider":{"name":%q,"jurisdiction":"eu"}}`, name))
	x.create(obj)
	x.mustReconcile(obj, 2)
	item := x.path(x.en.ItemPath, name)
	if err := x.e.Fake.InjectFault(fake.Fault{Method: http.MethodDelete, PathRegex: "^" + regexp.QuoteMeta(item) + "$",
		Status: http.StatusConflict, Code: 10008, Message: "The bucket you tried to delete is not empty.", Times: 1}); err != nil {
		t.Fatal(err)
	}
	x.delete(obj)
	if err := x.reconcile(obj, 1); err == nil {
		t.Fatal("the refused delete returned no error (no retry)")
	}
	c := reconcile.GetCondition(obj, commonv1alpha1.ConditionSynced)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reconcile.ReasonDeleteFailed || !strings.Contains(c.Message, "10008") {
		t.Errorf("Synced: %+v, want False/DeleteFailed with the API error", c)
	}
	if len(obj.GetFinalizers()) == 0 {
		t.Error("the finalizer was removed although the bucket still exists")
	}
	if _, err := x.bucketAPI(http.MethodGet, item, "eu", nil); err != nil {
		t.Errorf("the bucket after the refused delete: %v", err)
	}
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	if _, err := x.bucketAPI(http.MethodGet, item, "eu", nil); !cfclient.IsNotFound(err) {
		t.Errorf("the bucket after the accepted delete: %v, want 404", err)
	}
}

// TestR2BucketDefaultOrphan: R2Bucket is data-bearing, so without deletionPolicy deleting the
// object keeps the bucket.
func TestR2BucketDefaultOrphan(t *testing.T) {
	x := newExtHarness(t, "R2Bucket")
	name := randName("flare-spike")
	obj := x.newObj(x.en, "bucket", fmt.Sprintf(`{"forProvider":{"name":%q}}`, name))
	x.create(obj)
	x.mustReconcile(obj, 2)
	mark := x.rec.mark()
	x.delete(obj)
	x.mustReconcile(obj, 1)
	x.waitGone(obj)
	if w := writesOf(x.rec.since(mark)); len(w) != 0 {
		t.Errorf("orphaning wrote:\n%s", summary(w))
	}
	if _, err := x.bucketAPI(http.MethodGet, x.path(x.en.ItemPath, name), "", nil); err != nil {
		t.Errorf("the orphaned bucket: %v", err)
	}
}
