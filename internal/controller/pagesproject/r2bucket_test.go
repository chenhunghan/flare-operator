package pagesproject_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// An r2_buckets binding's r2BucketRef waits (DependencyNotReady) for the R2Bucket, then binds
// its bucket name and jurisdiction. The R2Bucket (deletionPolicy Delete) waits for the
// PagesProject before its own Cloudflare delete.
func TestProjectR2BucketRef(t *testing.T) {
	if testing.Short() {
		t.Skip("R2 buckets are served by the fake's generic profile, which needs the pinned spec")
	}
	h := startWith(t, testenv.ManagerOptions{Controllers: []string{"r2bucket"}})
	name := h.projectName("files")
	h.newProject("files", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main",
		DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Preview: &pagesv1alpha1.PagesDeploymentConfig{
			R2Buckets: []pagesv1alpha1.PagesR2Binding{{Name: "FILES", R2BucketRef: &commonv1alpha1.LocalRef{Name: "bucket"}}},
		}}}, nil)
	h.waitProject("files", func(pp *pagesv1alpha1.PagesProject) bool {
		return hasCond(pp.Status.Conditions, pp.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonDependency) &&
			strings.Contains(condOf(pp.Status.Conditions, "Synced"), "R2Bucket bucket not found")
	})
	rb := &r2v1alpha1.R2Bucket{ObjectMeta: metav1.ObjectMeta{Name: "bucket"},
		Spec: r2v1alpha1.R2BucketSpec{ResourceSpec: accountRef(), ForProvider: r2v1alpha1.R2BucketParameters{
			Name: str(h.ns + "-bucket"), Jurisdiction: str("eu")}}}
	rb.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete
	h.create(rb)
	rb = wait(h, rb, ready[*r2v1alpha1.R2Bucket])
	pp := h.waitProject("files", projectReady)
	if got, want := fmt.Sprint(h.config(name, "preview")["r2_buckets"]), fmt.Sprintf("map[FILES:map[jurisdiction:eu name:%s]]", rb.Status.ID); got != want {
		t.Fatalf("r2_buckets = %s, want %s", got, want)
	}
	h.assertNoWrites("files", name)

	m := h.mark()
	h.delete(rb)
	wait(h, rb, func(o *r2v1alpha1.R2Bucket) bool {
		c := condOf(o.Status.Conditions, "Ready")
		return strings.Contains(c, commonv1alpha1.ReasonDependency) && strings.Contains(c, "PagesProject files")
	})
	if n := testenv.Count(h.since(m), http.MethodDelete, "/r2/buckets/"); n != 0 {
		t.Fatalf("deleted while bound:\n%s", testenv.Summary(h.since(m)))
	}
	h.delete(pp)
	h.waitGone(pp)
	h.waitGone(rb)
	if n := testenv.Count(h.since(m), http.MethodDelete, "/r2/buckets/"+rb.Status.ID); n != 1 {
		t.Fatalf("bucket deletes:\n%s", testenv.Summary(h.since(m)))
	}
}

// The CRD's CEL rules reject an r2_buckets binding without exactly one of bucket_name and
// r2BucketRef, and a jurisdiction next to an r2BucketRef.
func TestProjectR2BindingValidation(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	for i, tc := range []struct {
		b    pagesv1alpha1.PagesR2Binding
		want string
	}{
		{pagesv1alpha1.PagesR2Binding{Name: "B"}, "set exactly one of bucket_name or r2BucketRef"},
		{pagesv1alpha1.PagesR2Binding{Name: "B", BucketName: str("flare-spike-b"), R2BucketRef: &commonv1alpha1.LocalRef{Name: "b"}},
			"set exactly one of bucket_name or r2BucketRef"},
		{pagesv1alpha1.PagesR2Binding{Name: "B", R2BucketRef: &commonv1alpha1.LocalRef{Name: "b"}, Jurisdiction: str("eu")},
			"an r2BucketRef binds the R2Bucket's own jurisdiction"},
	} {
		pp := &pagesv1alpha1.PagesProject{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("v%d", i)},
			Spec: pagesv1alpha1.PagesProjectSpec{ResourceSpec: accountRef(), ForProvider: &pagesv1alpha1.PagesProjectParameters{
				Name: fmt.Sprintf("%s-v%d", ns, i), ProductionBranch: "main",
				DeploymentConfigs: &pagesv1alpha1.PagesDeploymentConfigs{Production: &pagesv1alpha1.PagesDeploymentConfig{
					R2Buckets: []pagesv1alpha1.PagesR2Binding{tc.b}}}}}}
		err := e.Client.Create(testenv.Context(t, 10e9), pp)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: create error %v, want %q", i, err, tc.want)
		}
	}
}
