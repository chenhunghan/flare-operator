package reconcile_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// stubTagger is a Tagger answering one fixed owner read.
type stubTagger struct {
	reconcile.NoopTagger
	owner  string
	tagged bool
	err    error
	reads  *int
}

func (s stubTagger) Owner(context.Context, cfclient.Client, string, reconcile.TagTarget) (string, bool, error) {
	if s.reads != nil {
		*s.reads++
	}
	return s.owner, s.tagged, s.err
}

func TestMayDeleteExternal(t *testing.T) {
	const me, id = "c/ns/w", "res-1"
	errRead := errors.New("tags: 500 and the tag index failed")
	notFound := &cfclient.APIError{Status: 404}
	forbidden := &cfclient.APIError{Status: 403, Errors: []cfclient.ErrorDetail{{Code: 10000, Message: "Authentication error"}}}
	type obj struct{ record, legacy, pin, statusID bool }
	type verdict int
	const (
		del verdict = iota
		keep
		gone
		retry
	)
	exists := func(ok bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) { return ok, err }
	}
	cases := []struct {
		name    string
		tagger  reconcile.Tagger
		obj     obj
		exists  func(context.Context) (bool, error)
		want    verdict
		foreign bool
	}{
		{"tag names the object", stubTagger{owner: me, tagged: true}, obj{}, nil, del, false},
		{"foreign tag", stubTagger{owner: "c/ns/other", tagged: true}, obj{record: true, pin: true, statusID: true}, nil, keep, true},
		{"foreign tag, resource gone check not consulted", stubTagger{owner: "c/ns/other", tagged: true}, obj{}, exists(false, nil), keep, true},
		{"untagged, no record", stubTagger{tagged: true}, obj{pin: true, statusID: true}, nil, keep, false},
		{"untagged, no record, resource gone", stubTagger{tagged: true}, obj{pin: true}, exists(false, nil), gone, false},
		{"untagged, no record, resource exists", stubTagger{tagged: true}, obj{pin: true}, exists(true, nil), keep, false},
		{"untagged, no record, exists check fails", stubTagger{tagged: true}, obj{pin: true}, exists(false, errRead), retry, false},
		{"untagged, exists check 404", stubTagger{tagged: true}, obj{pin: true}, exists(false, notFound), gone, false},
		{"ambiguous 500, status.id only", stubTagger{}, obj{pin: true, statusID: true}, nil, keep, false},
		{"ambiguous 500, recorded", stubTagger{}, obj{record: true}, nil, del, false},
		{"ambiguous 500, legacy created-by-uid", stubTagger{}, obj{legacy: true, pin: true}, nil, del, false},
		{"legacy created-by-uid without the pin", stubTagger{}, obj{legacy: true}, nil, keep, false},
		{"read fails, no record: retry", stubTagger{err: errRead}, obj{pin: true, statusID: true}, nil, retry, false},
		{"read fails, no record, resource gone", stubTagger{err: errRead}, obj{pin: true}, exists(false, nil), gone, false},
		{"read fails, no record, resource exists: retry", stubTagger{err: errRead}, obj{pin: true}, exists(true, nil), retry, false},
		{"read fails, recorded", stubTagger{err: errRead}, obj{record: true}, nil, del, false},
		{"tags 404, no record", stubTagger{err: notFound}, obj{pin: true}, nil, keep, false},
		{"tags 404, no record, resource gone", stubTagger{err: notFound}, obj{pin: true}, exists(false, nil), gone, false},
		{"tags 404, recorded", stubTagger{err: notFound}, obj{record: true}, nil, del, false},
		{"tags 403, no record: keep, no retry", stubTagger{err: forbidden}, obj{pin: true, statusID: true}, nil, keep, false},
		{"tags 403, recorded", stubTagger{err: forbidden}, obj{record: true}, nil, del, false},
		{"tags 403, no record, exists check 403: keep, no retry", stubTagger{err: forbidden}, obj{pin: true}, exists(false, forbidden), keep, false},
		{"untagged, no record, exists check 403: keep", stubTagger{tagged: true}, obj{pin: true}, exists(false, forbidden), keep, false},
		{"read fails, no record, exists check 403: retry", stubTagger{err: errRead}, obj{pin: true}, exists(false, forbidden), retry, false},
		{"tagging off, pinned", reconcile.NoopTagger{}, obj{pin: true}, nil, del, false},
		{"tagging off, recorded", reconcile.NoopTagger{}, obj{record: true}, nil, del, false},
		{"tagging off, legacy", reconcile.NoopTagger{}, obj{legacy: true}, nil, keep, false}, // legacy needs the pin; the pin alone suffices
		{"tagging off, status.id only", reconcile.NoopTagger{}, obj{statusID: true}, nil, keep, false},
		{"tagging off, status.id only, gone", reconcile.NoopTagger{}, obj{statusID: true}, exists(false, nil), gone, false},
		{"nil tagger, pinned", nil, obj{pin: true}, nil, del, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Widget{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns", UID: types.UID("uid-1"), Annotations: map[string]string{}}}
			if tc.obj.record {
				w.Annotations[reconcile.AnnotationOwnershipProof] = "uid-1/" + id
			}
			if tc.obj.legacy {
				w.Annotations[reconcile.AnnotationLegacyCreatedByUID] = "uid-1"
			}
			if tc.obj.pin {
				w.Annotations[commonv1alpha1.AnnotationExternalID] = id
			}
			if tc.obj.statusID {
				w.Status.ID = id
			}
			dec, err := reconcile.MayDeleteExternal(context.Background(), tc.tagger, nil, "acct", reconcile.TagTarget{Type: "queue", ID: id}, me, w, id, tc.exists)
			var got verdict
			switch {
			case err != nil:
				got = retry
			case dec.Delete:
				got = del
			case dec.Gone:
				got = gone
			default:
				got = keep
			}
			if got != tc.want || (dec.Foreign != "") != tc.foreign {
				t.Fatalf("decision %+v err=%v, want verdict %d (foreign %v)", dec, err, tc.want, tc.foreign)
			}
			if got == keep && dec.Why == "" {
				t.Error("kept without a reason")
			}
		})
	}
}

func TestMayDeleteExternalPermanentReason(t *testing.T) {
	w := &Widget{ObjectMeta: metav1.ObjectMeta{UID: "u", Annotations: map[string]string{commonv1alpha1.AnnotationExternalID: "r"}}}
	forbidden := &cfclient.APIError{Status: 403, Errors: []cfclient.ErrorDetail{{Code: 10000, Message: "Authentication error"}}}
	dec, err := reconcile.MayDeleteExternal(context.Background(), stubTagger{err: forbidden}, nil, "a", reconcile.TagTarget{ID: "r"}, "o", w, "r", nil)
	if err != nil || dec.Delete || dec.Gone || !strings.Contains(dec.Why, "403") {
		t.Errorf("decision %+v err %v, want kept citing the 403", dec, err)
	}
}

func TestRecordOwnership(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	w.UID = "uid-7"
	w.Annotations = map[string]string{reconcile.AnnotationLegacyCreatedByUID: "uid-7"}
	kube := newKube(t, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	w.Status.Note = "in-memory"
	if err := reconcile.RecordOwnership(ctx, kube, w, "cf-9"); err != nil {
		t.Fatal(err)
	}
	if w.Status.Note != "in-memory" || w.Status.ID != "cf-9" {
		t.Errorf("in-memory status lost: %+v", w.Status)
	}
	var stored Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	if !reconcile.HasOwnershipProof(&stored, "cf-9") || stored.Annotations[commonv1alpha1.AnnotationExternalID] != "cf-9" {
		t.Errorf("annotations %v", stored.Annotations)
	}
	if _, ok := stored.Annotations[reconcile.AnnotationLegacyCreatedByUID]; ok {
		t.Errorf("legacy created-by-uid kept: %v", stored.Annotations)
	}
	rv := w.ResourceVersion
	if err := reconcile.RecordOwnership(ctx, kube, w, "cf-9"); err != nil || w.ResourceVersion != rv {
		t.Errorf("second RecordOwnership wrote (err %v, rv %s → %s)", err, rv, w.ResourceVersion)
	}
}

// TestRecordCreatedStaleObject: the ownership record written right after a create survives a
// concurrent change of the object (RecordOwnership's optimistic lock would answer Conflict and
// lose the new ID); other annotations are kept and in-memory status survives.
func TestRecordCreatedStaleObject(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	w.UID = "uid-8"
	w.Annotations = map[string]string{reconcile.AnnotationLegacyCreatedByUID: "uid-8"}
	kube := newKube(t, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	// Someone else changes the object: w is now stale.
	var other Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &other); err != nil {
		t.Fatal(err)
	}
	other.Annotations["team"] = "blue"
	other.Labels = map[string]string{"l": "v"}
	if err := kube.Update(ctx, &other); err != nil {
		t.Fatal(err)
	}
	stale := w.DeepCopyObject().(*Widget)
	if err := reconcile.RecordOwnership(ctx, kube, stale, "cf-1"); err == nil {
		t.Fatal("RecordOwnership on a stale object did not conflict (the test premise is wrong)")
	}

	w.Status.Note = "in-memory"
	if err := reconcile.RecordCreated(ctx, kube, w, "cf-1"); err != nil {
		t.Fatalf("RecordCreated on a stale object: %v", err)
	}
	if w.Status.Note != "in-memory" || w.Status.ID != "cf-1" || w.Labels["l"] != "v" {
		t.Errorf("in-memory object: status %+v labels %v", w.Status, w.Labels)
	}
	var stored Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	a := stored.Annotations
	if !reconcile.HasOwnershipProof(&stored, "cf-1") || a[commonv1alpha1.AnnotationExternalID] != "cf-1" || a["team"] != "blue" {
		t.Errorf("annotations %v", a)
	}
	if _, ok := a[reconcile.AnnotationLegacyCreatedByUID]; ok {
		t.Errorf("legacy created-by-uid kept: %v", a)
	}
	// w was stale: it keeps its resourceVersion, so a later optimistically locked write from it
	// (a finalizer, the status) conflicts instead of overwriting what w does not show.
	if w.ResourceVersion != stale.ResourceVersion || w.ResourceVersion == stored.ResourceVersion {
		t.Errorf("resourceVersion %s, want the stale %s (stored %s)", w.ResourceVersion, stale.ResourceVersion, stored.ResourceVersion)
	}
	// Idempotent.
	rv := w.ResourceVersion
	if err := reconcile.RecordCreated(ctx, kube, w, "cf-1"); err != nil || w.ResourceVersion != rv {
		t.Errorf("second RecordCreated wrote (err %v)", err)
	}
}

// Two consecutive unlocked fallback writes from one stale copy (MarkCreatePending, then
// RecordCreated, as around a create): both land, and the copy keeps its old resourceVersion
// through both, so a later locked write from it (the status) still conflicts.
func TestConsecutiveFallbackWritesKeepStaleResourceVersion(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	w.UID = "uid-twice"
	kube := newKube(t, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	staleRV := w.ResourceVersion
	var other Widget // someone else changes the object: w is now stale
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &other); err != nil {
		t.Fatal(err)
	}
	other.Labels = map[string]string{"team": "blue"}
	if err := kube.Update(ctx, &other); err != nil {
		t.Fatal(err)
	}
	base := w.DeepCopyObject().(*Widget)

	if err := reconcile.MarkCreatePending(ctx, kube, w, "name"); err != nil {
		t.Fatalf("MarkCreatePending on a stale copy: %v", err)
	}
	if w.ResourceVersion != staleRV {
		t.Fatalf("after the first fallback write: resourceVersion %s, want the stale %s", w.ResourceVersion, staleRV)
	}
	if _, ok := reconcile.PendingCreate(w); !ok || w.Labels["team"] != "blue" {
		t.Fatalf("the first fallback write did not refresh the metadata: %v %v", w.Annotations, w.Labels)
	}
	if err := reconcile.RecordCreated(ctx, kube, w, "cf-2"); err != nil {
		t.Fatalf("RecordCreated on a stale copy: %v", err)
	}
	if w.ResourceVersion != staleRV {
		t.Fatalf("after the second fallback write: resourceVersion %s, want the stale %s", w.ResourceVersion, staleRV)
	}
	var stored Widget
	if err := kube.Get(ctx, client.ObjectKeyFromObject(w), &stored); err != nil {
		t.Fatal(err)
	}
	if !reconcile.HasOwnershipProof(&stored, "cf-2") || stored.Labels["team"] != "blue" {
		t.Fatalf("stored annotations %v labels %v", stored.Annotations, stored.Labels)
	}
	if _, pending := reconcile.PendingCreate(&stored); pending {
		t.Fatalf("RecordCreated kept the create-pending record: %v", stored.Annotations)
	}
	if err := reconcile.PatchStatus(ctx, kube, w, base); !apierrors.IsConflict(err) {
		t.Fatalf("a status write from the stale copy after two fallback writes: %v, want a Conflict", err)
	}
}

func TestMigrateLegacyOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		ann     map[string]string
		migrate bool
	}{
		{"valid legacy record", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "uid-3", commonv1alpha1.AnnotationExternalID: "x"}, true},
		{"copied manifest (other UID)", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "uid-other", commonv1alpha1.AnnotationExternalID: "x"}, false},
		{"legacy record for another ID", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "uid-3", commonv1alpha1.AnnotationExternalID: "y"}, false},
		{"no legacy record", map[string]string{commonv1alpha1.AnnotationExternalID: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := widget(nil, "")
			w.UID = "uid-3"
			w.Annotations = tc.ann
			kube := newKube(t, w)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
				t.Fatal(err)
			}
			rv := w.ResourceVersion
			if err := reconcile.MigrateLegacyOwnership(ctx, kube, w, "x"); err != nil {
				t.Fatal(err)
			}
			if wrote := w.ResourceVersion != rv; wrote != tc.migrate {
				t.Fatalf("wrote=%v, want %v", wrote, tc.migrate)
			}
			if tc.migrate && (w.Annotations[reconcile.AnnotationOwnershipProof] != "uid-3/x" || w.Annotations[reconcile.AnnotationLegacyCreatedByUID] != "") {
				t.Errorf("annotations %v", w.Annotations)
			}
		})
	}
}

func TestHasOwnershipProof(t *testing.T) {
	w := &Widget{ObjectMeta: metav1.ObjectMeta{UID: "uid-1", Annotations: map[string]string{reconcile.AnnotationOwnershipProof: "uid-1/a/b"}}}
	if !reconcile.HasOwnershipProof(w, "a/b") {
		t.Error("record for an ID with a slash not accepted")
	}
	if reconcile.HasOwnershipProof(w, "a") || reconcile.HasOwnershipProof(w, "") {
		t.Error("record accepted for another ID")
	}
	w.UID = "uid-2" // a copy of the manifest (new object)
	if reconcile.HasOwnershipProof(w, "a/b") {
		t.Error("record of another UID accepted")
	}
	// Legacy (older Tunnel/VPCService builds): created-by-uid=<uid> plus the external-id annotation.
	for _, tc := range []struct {
		name string
		uid  string
		ann  map[string]string
		want bool
	}{
		{"legacy, matching UID and ID", "u1", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "u1", commonv1alpha1.AnnotationExternalID: "x"}, true},
		{"legacy, copied manifest (other UID)", "u2", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "u1", commonv1alpha1.AnnotationExternalID: "x"}, false},
		{"legacy, re-pointed external-id", "u1", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "u1", commonv1alpha1.AnnotationExternalID: "y"}, false},
		{"legacy, no UID yet", "", map[string]string{reconcile.AnnotationLegacyCreatedByUID: "", commonv1alpha1.AnnotationExternalID: "x"}, false},
	} {
		o := &Widget{ObjectMeta: metav1.ObjectMeta{UID: types.UID(tc.uid), Annotations: tc.ann}}
		if got := reconcile.HasOwnershipProof(o, "x"); got != tc.want {
			t.Errorf("%s: HasOwnershipProof = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTaggingEnabled(t *testing.T) {
	for _, tc := range []struct {
		t    reconcile.Tagger
		want bool
	}{{nil, false}, {reconcile.NoopTagger{}, false}, {&reconcile.NoopTagger{}, false}, {reconcile.ResourceTagger{}, true}, {&reconcile.ResourceTagger{}, true}} {
		if got := reconcile.TaggingEnabled(tc.t); got != tc.want {
			t.Errorf("TaggingEnabled(%T) = %v", tc.t, got)
		}
	}
	if o, tagged, err := (reconcile.NoopTagger{}).Owner(context.Background(), nil, "", reconcile.TagTarget{}); o != "" || tagged || err != nil {
		t.Errorf("NoopTagger.Owner = %q %v %v", o, tagged, err)
	}
}

func TestDeletionResult(t *testing.T) {
	w := widget(nil, "")
	if res, err := reconcile.DeletionResult(w, nil); err != nil || res.RequeueAfter != 0 {
		t.Errorf("nil: %v %v", res, err)
	}
	res, err := reconcile.DeletionResult(w, &reconcile.WaitError{After: time.Second, Reason: "draining"})
	if err != nil || res.RequeueAfter != time.Second {
		t.Errorf("wait: %v %v", res, err)
	}
	if c := reconcile.GetCondition(w, commonv1alpha1.ConditionReady); c == nil || c.Reason != commonv1alpha1.ReasonDeleting || c.Message != "draining" {
		t.Errorf("Ready %+v", c)
	}
	boom := errors.New("boom")
	if _, err := reconcile.DeletionResult(w, boom); !errors.Is(err, boom) {
		t.Errorf("error not returned: %v", err)
	}
	if c := reconcile.GetCondition(w, commonv1alpha1.ConditionReady); c.Message != "boom" {
		t.Errorf("Ready %+v", c)
	}
}

// TestFinalizeAccount covers the account step shared by every kind's finalizer.
func TestFinalizeAccount(t *testing.T) {
	ctx := context.Background()
	deleting := func() *Widget {
		w := widget(nil, "Delete")
		w.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "x"}
		now := metav1.Now()
		w.DeletionTimestamp = &now
		w.Finalizers = []string{commonv1alpha1.Finalizer}
		return w
	}
	policy := reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true})

	// Usable account.
	kube := newKube(t, readyAccount(1, 1, true), tokenSecret("t"))
	rec := events.NewFakeRecorder(5)
	acct, err := reconcile.FinalizeAccount(ctx, reconcile.NewAccounts(kube, policy), kube, rec, deleting(), "Delete", "kept")
	if err != nil || acct == nil {
		t.Fatalf("usable account: %v %v", acct, err)
	}

	// The account exists but is not Ready: wait.
	kube = newKube(t, readyAccount(1, 1, false), tokenSecret("t"))
	acct, err = reconcile.FinalizeAccount(ctx, reconcile.NewAccounts(kube, policy), kube, rec, deleting(), "Delete", "kept")
	we, ok := reconcile.AsWait(err)
	if acct != nil || !ok || we.After != reconcile.AccountRetryInterval || !strings.Contains(we.Reason, "not Ready") {
		t.Fatalf("not-Ready account: %v %v", acct, err)
	}

	// The account is gone: nil, nil and a Warning event (none without a note).
	kube = newKube(t)
	acct, err = reconcile.FinalizeAccount(ctx, reconcile.NewAccounts(kube, policy), kube, rec, deleting(), "Delete", "Widget x was left in Cloudflare")
	if acct != nil || err != nil {
		t.Fatalf("gone account: %v %v", acct, err)
	}
	select {
	case e := <-rec.Events:
		if !strings.HasPrefix(e, "Warning ExternalResourceKept") || !strings.Contains(e, "Widget x was left in Cloudflare") || !strings.Contains(e, `"acct" no longer exists`) {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no ExternalResourceKept event")
	}
	if _, err := reconcile.FinalizeAccount(ctx, reconcile.NewAccounts(kube, policy), kube, rec, deleting(), "Orphan", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("event without a note: %q", e)
	default:
	}

	// The cache still shows a usable account that is gone (uncached): gone, nothing returned
	// to reach Cloudflare with.
	cached := newKube(t, readyAccount(1, 1, true), tokenSecret("t"))
	acct, err = reconcile.FinalizeAccount(ctx, reconcile.NewAccounts(cached, policy), newKube(t), rec, deleting(), "Delete", "kept")
	if acct != nil || err != nil {
		t.Fatalf("account gone but cached: %v %v", acct, err)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, `"acct" no longer exists`) {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no ExternalResourceKept event for the account gone but cached")
	}
}
