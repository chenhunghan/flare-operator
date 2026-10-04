package workerscript_test

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

func (h *harness) r2(name, jurisdiction string, policy commonv1alpha1.DeletionPolicy) *r2v1alpha1.R2Bucket {
	o := &r2v1alpha1.R2Bucket{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: r2v1alpha1.R2BucketSpec{ResourceSpec: accountRef(), ForProvider: r2v1alpha1.R2BucketParameters{Name: str(h.ns + "-" + name)}}}
	if jurisdiction != "" {
		o.Spec.ForProvider.Jurisdiction = str(jurisdiction)
	}
	o.Spec.DeletionPolicy = policy
	h.create(o)
	return o
}

// An r2_bucket binding's r2BucketRef waits (DependencyNotReady) for the R2Bucket, then binds its
// bucket name and jurisdiction ("default" is bound as none). The R2Bucket (deletionPolicy
// Delete) waits for the WorkerScript before its own Cloudflare delete.
func TestR2BucketRefBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("R2 buckets are served by the fake's generic profile, which needs the pinned spec")
	}
	h := startWith(t, testenv.ManagerOptions{Controllers: []string{"r2bucket"}})
	fp := params(fetchModule)
	fp.Bindings = []workersv1alpha1.WorkerBinding{
		{Name: "MEDIA", Type: "r2_bucket", R2BucketRef: &commonv1alpha1.LocalRef{Name: "media"}},
		{Name: "PLAIN", Type: "r2_bucket", R2BucketRef: &commonv1alpha1.LocalRef{Name: "plain"}},
	}
	h.newScript("user", fp, nil)
	h.waitScript("user", func(ws *workersv1alpha1.WorkerScript) bool {
		c := condOf(ws.Status.Conditions, "Synced")
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonDependency) &&
			strings.Contains(c, "R2Bucket media not found")
	})
	if h.scriptExists("user") {
		t.Fatal("uploaded before the R2Bucket existed")
	}
	media := wait(h, h.r2("media", "eu", commonv1alpha1.DeletionDelete), ready[*r2v1alpha1.R2Bucket])
	plain := wait(h, h.r2("plain", "default", commonv1alpha1.DeletionOrphan), ready[*r2v1alpha1.R2Bucket])
	ws := h.waitScript("user", scriptReady)
	s := h.settings("user")
	if b := binding(s, "MEDIA"); b["type"] != "r2_bucket" || b["bucket_name"] != media.Status.ID || b["jurisdiction"] != "eu" {
		t.Fatalf("r2 binding %v, want bucket %s in eu", b, media.Status.ID)
	}
	if b := binding(s, "PLAIN"); b["bucket_name"] != plain.Status.ID || b["jurisdiction"] != nil {
		t.Fatalf("r2 binding %v, want bucket %s without a jurisdiction", b, plain.Status.ID)
	}
	h.assertNoWrites("user")

	m := h.mark()
	h.delete(media)
	wait(h, media, func(o *r2v1alpha1.R2Bucket) bool {
		c := condOf(o.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "WorkerScript user")
	})
	if n := testenv.Count(h.since(m), http.MethodDelete, "/r2/buckets/"); n != 0 {
		t.Fatalf("deleted while bound:\n%s", testenv.Summary(h.since(m)))
	}
	h.delete(ws)
	h.waitGone(ws)
	h.waitGone(media)
	if n := testenv.Count(h.since(m), http.MethodDelete, "/r2/buckets/"+media.Status.ID); n != 1 {
		t.Fatalf("bucket deletes:\n%s", testenv.Summary(h.since(m)))
	}
}
