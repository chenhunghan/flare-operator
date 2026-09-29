package reconcile_test

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/reconcile"
)

// stubTagger is a Tagger and OwnerReader answering one fixed owner read.
type stubTagger struct {
	reconcile.NoopTagger
	owner  string
	tagged bool
	err    error
}

func (s stubTagger) Owner(context.Context, cfclient.Client, string, reconcile.TagTarget) (string, bool, error) {
	return s.owner, s.tagged, s.err
}

// writeOnlyTagger is a Tagger that cannot read tags without writing.
type writeOnlyTagger struct{ reconcile.NoopTagger }

func TestMayDeleteExternal(t *testing.T) {
	const me, id = "c/ns/w", "res-1"
	errRead := errors.New("tags: 500 and the tag index failed")
	notFound := &cfclient.APIError{Status: 404}
	type obj struct{ record, pin, statusID bool }
	cases := []struct {
		name   string
		tagger reconcile.Tagger
		obj    obj
		ok     bool
		err    bool
	}{
		{"tag names the object", stubTagger{owner: me, tagged: true}, obj{}, true, false},
		{"foreign tag", stubTagger{owner: "c/ns/other", tagged: true}, obj{record: true, pin: true, statusID: true}, false, false},
		{"untagged, no record", stubTagger{tagged: true}, obj{pin: true, statusID: true}, false, false},
		{"ambiguous 500, status.id only", stubTagger{}, obj{pin: true, statusID: true}, false, false},
		{"ambiguous 500, recorded", stubTagger{}, obj{record: true}, true, false},
		{"read fails, no record: retry", stubTagger{err: errRead}, obj{pin: true, statusID: true}, false, true},
		{"read fails, recorded", stubTagger{err: errRead}, obj{record: true}, true, false},
		{"tags 404, no record", stubTagger{err: notFound}, obj{pin: true}, false, false},
		{"tagging off, pinned", reconcile.NoopTagger{}, obj{pin: true}, true, false},
		{"tagging off, recorded", reconcile.NoopTagger{}, obj{record: true}, true, false},
		{"tagging off, status.id only", reconcile.NoopTagger{}, obj{statusID: true}, false, false},
		{"unreadable tagger, recorded", writeOnlyTagger{}, obj{record: true}, true, false},
		{"unreadable tagger, pinned", writeOnlyTagger{}, obj{pin: true, statusID: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Widget{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns", UID: types.UID("uid-1"), Annotations: map[string]string{}}}
			if tc.obj.record {
				w.Annotations[reconcile.AnnotationOwnershipProof] = "uid-1/" + id
			}
			if tc.obj.pin {
				w.Annotations[commonv1alpha1.AnnotationExternalID] = id
			}
			if tc.obj.statusID {
				w.Status.ID = id
			}
			ok, why, err := reconcile.MayDeleteExternal(context.Background(), tc.tagger, nil, "acct", reconcile.TagTarget{Type: "queue", ID: id}, me, w, id)
			if (err != nil) != tc.err || ok != tc.ok {
				t.Fatalf("ok=%v why=%q err=%v, want ok=%v err=%v", ok, why, err, tc.ok, tc.err)
			}
			if !ok && err == nil && why == "" {
				t.Error("refusal without a reason")
			}
		})
	}
}

func TestRecordOwnership(t *testing.T) {
	ctx := context.Background()
	w := widget(nil, "")
	w.UID = "uid-7"
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
	rv := w.ResourceVersion
	if err := reconcile.RecordOwnership(ctx, kube, w, "cf-9"); err != nil || w.ResourceVersion != rv {
		t.Errorf("second RecordOwnership wrote (err %v, rv %s → %s)", err, rv, w.ResourceVersion)
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
}
