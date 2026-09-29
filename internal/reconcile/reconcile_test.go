package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cloudflarev1alpha1.AddToScheme, addWidget} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func newKube(t *testing.T, objs ...client.Object) client.Client {
	return fakeclient.NewClientBuilder().WithScheme(scheme(t)).
		WithStatusSubresource(&Widget{}, &cloudflarev1alpha1.CloudflareAccount{}).
		WithObjects(objs...).Build()
}

func widget(policies []commonv1alpha1.ManagementAction, del commonv1alpha1.DeletionPolicy) *Widget {
	return &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w", Generation: 3},
		Spec: WidgetSpec{ResourceSpec: commonv1alpha1.ResourceSpec{
			AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, ManagementPolicies: policies, DeletionPolicy: del,
		}},
	}
}

type A = commonv1alpha1.ManagementAction

func TestPolicies(t *testing.T) {
	all := []A{"Create", "Update", "Delete", "LateInitialize"}
	for _, tc := range []struct {
		name        string
		list        []A
		want        []bool // create, update, delete, lateinit
		observeOnly bool
	}{
		{"default", nil, []bool{true, true, true, true}, false},
		{"star", []A{"*"}, []bool{true, true, true, true}, false},
		{"observe", []A{"Observe"}, []bool{false, false, false, false}, true},
		{"no-delete", []A{"Observe", "Create", "Update", "LateInitialize"}, []bool{true, true, false, true}, false},
		{"no-update", []A{"Observe", "Create", "Delete"}, []bool{true, false, true, false}, false},
		{"observe-lateinit", []A{"Observe", "LateInitialize"}, []bool{false, false, false, true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := reconcile.PoliciesOf(widget(tc.list, ""))
			got := []bool{p.CanCreate(), p.CanUpdate(), p.CanDelete(), p.CanLateInitialize()}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("%s: got %v want %v", all[i], got[i], tc.want[i])
				}
			}
			if p.ObserveOnly() != tc.observeOnly {
				t.Errorf("ObserveOnly=%v", p.ObserveOnly())
			}
			if p.CanWrite() != (tc.want[0] || tc.want[1] || tc.want[2]) {
				t.Errorf("CanWrite=%v", p.CanWrite())
			}
		})
	}
}

func TestDeletionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       commonv1alpha1.DeletionPolicy
		kind       commonv1alpha1.DeletionPolicy
		policies   []A
		wantPolicy commonv1alpha1.DeletionPolicy
		wantDelete bool
	}{
		{"default-delete", "", "", nil, "Delete", true},
		{"kind-orphan", "", "Orphan", nil, "Orphan", false},
		{"spec-overrides-kind", "Delete", "Orphan", nil, "Delete", true},
		{"spec-orphan", "Orphan", "Delete", nil, "Orphan", false},
		{"observe-only-never-deletes", "Delete", "", []A{"Observe"}, "Delete", false},
		{"no-delete-policy", "Delete", "", []A{"Observe", "Create", "Update"}, "Delete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := widget(tc.policies, tc.spec)
			if got := reconcile.EffectiveDeletionPolicy(w, tc.kind); got != tc.wantPolicy {
				t.Errorf("policy %s", got)
			}
			if got := reconcile.ShouldDeleteExternal(w, tc.kind); got != tc.wantDelete {
				t.Errorf("ShouldDeleteExternal %v", got)
			}
		})
	}
}

func TestConditions(t *testing.T) {
	w := widget(nil, "")
	reconcile.MarkCreating(w, "creating")
	reconcile.MarkSynced(w)
	if reconcile.IsReady(w) {
		t.Error("ready while creating")
	}
	reconcile.MarkAvailable(w)
	if !reconcile.IsReady(w) {
		t.Error("not ready")
	}
	c := reconcile.GetCondition(w, commonv1alpha1.ConditionReady)
	if c.ObservedGeneration != 3 || c.Reason != commonv1alpha1.ReasonAvailable {
		t.Errorf("%+v", c)
	}
	if s := reconcile.GetCondition(w, commonv1alpha1.ConditionSynced); s.Reason != commonv1alpha1.ReasonReconcileOK || s.Status != metav1.ConditionTrue {
		t.Errorf("synced %+v", s)
	}
	w.Generation = 4
	if reconcile.IsReady(w) {
		t.Error("Ready for an older generation counts as ready")
	}
	reconcile.MarkSyncError(w, "", errors.New("boom"))
	if s := reconcile.GetCondition(w, commonv1alpha1.ConditionSynced); s.Reason != commonv1alpha1.ReasonReconcileError || s.Message != "boom" || s.ObservedGeneration != 4 {
		t.Errorf("sync error %+v", s)
	}
	reconcile.MarkAccountNotReady(w, errors.New("acct"))
	for _, typ := range []string{commonv1alpha1.ConditionReady, commonv1alpha1.ConditionSynced} {
		if c := reconcile.GetCondition(w, typ); c.Reason != commonv1alpha1.ReasonAccountNotReady || c.Status != metav1.ConditionFalse {
			t.Errorf("%s %+v", typ, c)
		}
	}
	reconcile.SetObservedGeneration(w)
	if w.Status.ObservedGeneration != 4 {
		t.Error("observedGeneration")
	}
	ro := widget([]A{"Observe"}, "")
	reconcile.MarkSynced(ro)
	if s := reconcile.GetCondition(ro, commonv1alpha1.ConditionSynced); s.Reason != commonv1alpha1.ReasonObserveOnly {
		t.Errorf("observe-only synced reason %q", s.Reason)
	}
}

func TestFinalizerAndExternalIDPreserveStatus(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	kube := newKube(t, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	w.Status.Note = "in-memory"
	reconcile.MarkCreating(w, "x")
	added, err := reconcile.EnsureFinalizer(ctx, kube, w)
	if err != nil || !added {
		t.Fatalf("EnsureFinalizer %v %v", added, err)
	}
	if added, _ := reconcile.EnsureFinalizer(ctx, kube, w); added {
		t.Error("finalizer added twice")
	}
	if err := reconcile.PersistExternalID(ctx, kube, w, "cf-123"); err != nil {
		t.Fatal(err)
	}
	if w.Status.Note != "in-memory" || w.Status.ID != "cf-123" || reconcile.GetCondition(w, "Ready") == nil {
		t.Errorf("in-memory status lost: %+v", w.Status)
	}
	var stored Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Finalizers) != 1 || stored.Finalizers[0] != commonv1alpha1.Finalizer {
		t.Errorf("finalizers %v", stored.Finalizers)
	}
	if reconcile.ExternalID(&stored) != "cf-123" || !reconcile.HasExternalIDAnnotation(&stored) {
		t.Errorf("annotation %v", stored.Annotations)
	}
	if stored.ResourceVersion != w.ResourceVersion {
		t.Errorf("resourceVersion not refreshed: %s vs %s", stored.ResourceVersion, w.ResourceVersion)
	}
	// status.id is the fallback when there is no annotation.
	other := widget(nil, "")
	other.Status.ID = "from-status"
	if reconcile.ExternalID(other) != "from-status" {
		t.Error("status.id fallback")
	}
	reconcile.SetExternalID(other, "pinned")
	if reconcile.ExternalID(other) != "pinned" || other.Status.ID != "pinned" {
		t.Error("SetExternalID")
	}
	if err := reconcile.RemoveFinalizer(ctx, kube, w); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Finalizers) != 0 {
		t.Errorf("finalizer not removed: %v", stored.Finalizers)
	}
}

func TestFinalize(t *testing.T) {
	notFound := &cfclient.APIError{Status: 404, Errors: []cfclient.ErrorDetail{{Code: 10007, Message: "gone"}}}
	for _, tc := range []struct {
		name        string
		policies    []A
		spec, kind  commonv1alpha1.DeletionPolicy
		externalID  string
		deleteErr   error
		wantCalled  bool
		wantErr     bool
		wantRemoved bool
		wantRequeue time.Duration
	}{
		{name: "delete", externalID: "x", wantCalled: true, wantRemoved: true},
		{name: "orphan-spec", spec: "Orphan", externalID: "x", wantRemoved: true},
		{name: "orphan-kind-default", kind: "Orphan", externalID: "x", wantRemoved: true},
		{name: "observe-only", policies: []A{"Observe"}, spec: "Delete", externalID: "x", wantRemoved: true},
		{name: "no-external-id", wantRemoved: true},
		{name: "already-gone", externalID: "x", deleteErr: notFound, wantCalled: true, wantRemoved: true},
		{name: "delete-fails", externalID: "x", deleteErr: errors.New("500"), wantCalled: true, wantErr: true},
		// A multi-step delete in progress: no error, a requeue, the finalizer stays.
		{name: "delete-waits", externalID: "x", deleteErr: &reconcile.WaitError{After: 7 * time.Second, Reason: "draining"},
			wantCalled: true, wantRequeue: 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := widget(tc.policies, tc.spec)
			w.Finalizers = []string{commonv1alpha1.Finalizer}
			if tc.externalID != "" {
				w.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: tc.externalID}
			}
			kube := newKube(t, w)
			if err := kube.Delete(ctx, w); err != nil { // sets deletionTimestamp (finalizer present)
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
				t.Fatal(err)
			}
			called := false
			res, err := reconcile.Finalize(ctx, kube, w, tc.kind, func(_ context.Context, id string) error {
				called = true
				if id != tc.externalID {
					t.Errorf("deleteExternal(%q)", id)
				}
				return tc.deleteErr
			})
			if called != tc.wantCalled || (err != nil) != tc.wantErr || res.RequeueAfter != tc.wantRequeue {
				t.Fatalf("called=%v err=%v requeue=%v", called, err, res.RequeueAfter)
			}
			var stored Widget
			getErr := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored)
			removed := getErr != nil // the fake client deletes the object once finalizers are gone
			if removed != tc.wantRemoved {
				t.Errorf("finalizer removed=%v (get err %v)", removed, getErr)
			}
			if tc.wantErr || tc.wantRequeue > 0 {
				if c := reconcile.GetCondition(w, commonv1alpha1.ConditionReady); c == nil || c.Reason != commonv1alpha1.ReasonDeleting {
					t.Errorf("Ready %+v", c)
				}
			}
			if we, ok := reconcile.AsWait(tc.deleteErr); ok {
				if c := reconcile.GetCondition(w, commonv1alpha1.ConditionReady); c == nil || c.Message != we.Reason {
					t.Errorf("Ready message %+v, want the wait reason %q", c, we.Reason)
				}
			}
		})
	}
	// Not being deleted: nothing happens.
	w := widget(nil, "")
	res, err := reconcile.Finalize(context.Background(), newKube(t, w), w, "", func(context.Context, string) error {
		t.Error("deleteExternal called for a live object")
		return nil
	})
	if res.RequeueAfter != 0 || err != nil {
		t.Errorf("res=%v err=%v", res, err)
	}
}

func readyAccount(gen int64, condGen int64, ready bool) *cloudflarev1alpha1.CloudflareAccount {
	st := metav1.ConditionFalse
	if ready {
		st = metav1.ConditionTrue
	}
	return &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "acct", Generation: gen},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID:      "0123456789abcdef0123456789abcdef",
			TokenSecretRef: cloudflarev1alpha1.SecretKeySelector{Name: "tok", Key: "token"},
			BaseURL:        "http://127.0.0.1:1/client/v4",
		},
		Status: cloudflarev1alpha1.CloudflareAccountStatus{Conditions: []metav1.Condition{{
			Type: "Ready", Status: st, Reason: "Available", ObservedGeneration: condGen, LastTransitionTime: metav1.Now(),
		}}},
	}
}

func tokenSecret(tok string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tok"}, Data: map[string][]byte{"token": []byte(tok)}}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")

	// Missing account.
	_, err := reconcile.NewAccounts(newKube(t)).Resolve(ctx, w)
	var ae *reconcile.AccountError
	if !errors.As(err, &ae) || ae.Reason != commonv1alpha1.ReasonAccountNotReady {
		t.Fatalf("missing account: %v", err)
	}
	// Empty accountRef.
	empty := widget(nil, "")
	empty.Spec.AccountRef.Name = ""
	if _, err := reconcile.NewAccounts(newKube(t)).Resolve(ctx, empty); !reconcile.IsAccountNotReady(err) {
		t.Fatalf("empty ref: %v", err)
	}
	// Not Ready / Ready for an older generation.
	for _, acct := range []*cloudflarev1alpha1.CloudflareAccount{readyAccount(1, 1, false), readyAccount(2, 1, true)} {
		if _, err := reconcile.NewAccounts(newKube(t, acct, tokenSecret("t"))).Resolve(ctx, w); !reconcile.IsAccountNotReady(err) {
			t.Fatalf("not ready account resolved: %v", err)
		}
	}
	// Ready: client is cached; a rotated token builds a new one.
	kube := newKube(t, readyAccount(1, 1, true), tokenSecret("t1"))
	builds := 0
	accts := reconcile.NewAccounts(kube, reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}), reconcile.WithClientFactory(func(o cfclient.Options) (cfclient.Client, error) {
		builds++
		if o.Token == "" || o.BaseURL == "" {
			t.Errorf("options %+v", o)
		}
		return cfclient.New(o)
	}))
	r1, err := accts.Resolve(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := accts.Resolve(ctx, w)
	if r1.Client != r2.Client || builds != 1 || r1.AccountID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("cache: builds=%d same=%v id=%s", builds, r1.Client == r2.Client, r1.AccountID)
	}
	if err := kube.Update(ctx, tokenSecret("t2")); err != nil {
		t.Fatal(err)
	}
	r3, _ := accts.Resolve(ctx, w)
	if r3.Client == r1.Client || builds != 2 {
		t.Errorf("rotation did not rebuild: builds=%d", builds)
	}
	// Missing Secret on a Ready account → AccountNotReady.
	accts2 := reconcile.NewAccounts(newKube(t, readyAccount(1, 1, true)))
	if _, err := accts2.Resolve(ctx, w); !reconcile.IsAccountNotReady(err) {
		t.Errorf("missing secret: %v", err)
	}
}

func TestAccountsOptions(t *testing.T) {
	a := readyAccount(1, 1, true)
	zero := int32(0)
	a.Spec.RateLimit = &cloudflarev1alpha1.RateLimitSpec{RequestsPerFiveMinutes: 600, Burst: 7, MaxRetries: &zero,
		ListCacheTTL: &metav1.Duration{Duration: time.Minute}}
	o := reconcile.NewAccounts(nil, reconcile.WithUserAgent("ua")).Options(a, "tok")
	if o.RPS != 2 || o.Burst != 7 || o.MaxRetries != -1 || o.ListTTL != time.Minute || o.UserAgent != "ua" || o.Token != "tok" {
		t.Errorf("%+v", o)
	}
}

// ---- ownership tags against flarefake ----------------------------------------------------------

func TestResourceTagger(t *testing.T) {
	fs := fake.New(fake.Options{})
	hs := httptest.NewServer(fs)
	defer hs.Close()
	cf, err := cfclient.New(cfclient.Options{Token: "tagger-" + t.Name(), BaseURL: hs.URL + "/client/v4", RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const acct = "0123456789abcdef0123456789abcdef"
	resp, err := cf.Do(ctx, cfclient.Request{Method: "POST", Path: "/accounts/" + acct + "/storage/kv/namespaces", Body: map[string]string{"title": "owned"}})
	if err != nil {
		t.Fatal(err)
	}
	var ns struct{ ID string }
	if err := json.Unmarshal(resp.Result, &ns); err != nil {
		t.Fatal(err)
	}
	target := reconcile.TagTarget{Type: "kv_namespace", ID: ns.ID}
	owner := reconcile.OwnerValue("c1", "ns", "w")
	if owner != "c1/ns/w" {
		t.Fatal(owner)
	}
	tg := reconcile.ResourceTagger{}
	writes := func() int {
		n := 0
		for _, e := range fs.Journal() {
			if e.Method != http.MethodGet {
				n++
			}
		}
		return n
	}

	// Never tagged (500) → one PUT.
	base := writes()
	if err := tg.EnsureOwner(ctx, cf, acct, target, owner); err != nil {
		t.Fatal(err)
	}
	if writes()-base != 1 {
		t.Errorf("first EnsureOwner writes=%d", writes()-base)
	}
	// Idempotent: second call makes zero writes.
	base = writes()
	if err := tg.EnsureOwner(ctx, cf, acct, target, owner); err != nil {
		t.Fatal(err)
	}
	if writes() != base {
		t.Errorf("second EnsureOwner wrote %d times", writes()-base)
	}
	// Foreign tags survive (GET-merge-PUT): someone adds a tag, we re-own after a removal.
	if _, err := cf.Do(ctx, cfclient.Request{Method: "PUT", Path: "/accounts/" + acct + "/tags",
		Body: map[string]any{"resource_type": "kv_namespace", "resource_id": ns.ID, "tags": map[string]string{"team": "x", reconcile.OwnerTagKey: owner}}}); err != nil {
		t.Fatal(err)
	}
	if err := tg.RemoveOwner(ctx, cf, acct, target, owner); err != nil {
		t.Fatal(err)
	}
	tags, tagged, err := tg.Get(ctx, cf, acct, target)
	if err != nil || !tagged || tags["team"] != "x" || tags[reconcile.OwnerTagKey] != "" {
		t.Fatalf("after RemoveOwner: %v %v %v", tags, tagged, err)
	}
	if err := tg.EnsureOwner(ctx, cf, acct, target, owner); err != nil {
		t.Fatal(err)
	}
	tags, _, _ = tg.Get(ctx, cf, acct, target)
	if tags["team"] != "x" || tags[reconcile.OwnerTagKey] != owner {
		t.Fatalf("merge lost tags: %v", tags)
	}
	// Someone else's ownership → conflict, no write.
	base = writes()
	err = tg.EnsureOwner(ctx, cf, acct, target, "c2/ns/other")
	var conflict *reconcile.OwnershipConflictError
	if !errors.As(err, &conflict) || conflict.Owner != owner {
		t.Fatalf("conflict: %v", err)
	}
	if writes() != base {
		t.Error("conflict wrote")
	}
	// RemoveOwner by a non-owner is a no-op.
	if err := tg.RemoveOwner(ctx, cf, acct, target, "c2/ns/other"); err != nil || writes() != base {
		t.Errorf("non-owner remove: %v writes=%d", err, writes()-base)
	}
	// Last tag removed → DELETE (204).
	if _, err := cf.Do(ctx, cfclient.Request{Method: "PUT", Path: "/accounts/" + acct + "/tags",
		Body: map[string]any{"resource_type": "kv_namespace", "resource_id": ns.ID, "tags": map[string]string{reconcile.OwnerTagKey: owner}}}); err != nil {
		t.Fatal(err)
	}
	if err := tg.RemoveOwner(ctx, cf, acct, target, owner); err != nil {
		t.Fatal(err)
	}
	j := fs.Journal()
	if last := j[len(j)-1]; last.Method != http.MethodDelete || last.Status != http.StatusNoContent {
		t.Errorf("last call %s %d", last.Method, last.Status)
	}
	// RemoveOwner on a never-tagged resource does nothing.
	base = writes()
	if err := tg.RemoveOwner(ctx, cf, acct, reconcile.TagTarget{Type: "queue", ID: "nope"}, owner); err != nil || writes() != base {
		t.Errorf("never-tagged remove: %v", err)
	}
	// NoopTagger never calls the API.
	n := len(fs.Journal())
	var noop reconcile.Tagger = reconcile.NoopTagger{}
	_ = noop.EnsureOwner(ctx, cf, acct, target, owner)
	_ = noop.RemoveOwner(ctx, cf, acct, target, owner)
	if len(fs.Journal()) != n {
		t.Error("NoopTagger made calls")
	}
}
