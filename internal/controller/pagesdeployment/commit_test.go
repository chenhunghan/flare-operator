package pagesdeployment_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// pathRecorder records the paths of the requests an http.Client sends.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (r *pathRecorder) saw(suffix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// A user-set commit_hash is a git commit that other deployments carry too: a deployment lost
// to a dropped answer is not looked up by it. B, whose create answer is lost, must neither take
// A's deployment of the same commit (nor its own lost one) as its own, nor delete A's when it is
// deleted. The asset calls go through the account's HTTP client (its timeout and transport).
func TestUserSetCommitHashIdentifiesNothing(t *testing.T) {
	rec := &pathRecorder{}
	h := startWith(t, testenv.ManagerOptions{AccountsOptions: []reconcile.AccountsOption{
		reconcile.WithHTTPClient(&http.Client{Timeout: 2 * time.Second, Transport: rec})}})
	project := h.project("site")
	h.siteConfigMap("a-files", map[string]string{"index.html": "A"}, nil)
	h.siteConfigMap("b-files", map[string]string{"index.html": "B"}, nil)
	const commit = "abc123abc123"
	setCommit := func(pd *pagesv1alpha1.PagesDeployment) { pd.Spec.ForProvider.CommitHash = commit }
	h.newDeployment("a", "site", "a-preview", cmSource("a-files"), setCommit)
	a := h.waitDeployment("a", deployed)
	if !rec.saw("/pages/assets/check-missing") || !rec.saw("/pages/assets/upsert-hashes") {
		t.Fatalf("the asset calls did not use the account's HTTP client: %v", rec.paths)
	}

	// B's create is made in Cloudflare, and its answer arrives after the client gave up.
	if err := h.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost,
		PathRegex: "^(/client/v4)?/accounts/" + h.acct.AccountID + "/pages/projects/" + project + "/deployments$", Times: 1,
		FaultShape: fake.FaultShape{DelayMs: 4000, Passthrough: true}}); err != nil {
		t.Fatal(err)
	}
	m := h.mark()
	h.newDeployment("b", "site", "b-preview", cmSource("b-files"), setCommit)
	b := h.waitDeployment("b", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return deployed(pd) && pd.Status.AtProvider.Branch == "b-preview"
	})
	if b.Status.ID == a.Status.ID {
		t.Fatalf("B took A's deployment %s as its own", a.Status.ID)
	}
	h.waitEvent("b", reconcile.EventReasonExternalResourceKept, commit, "does not identify it")
	if n := h.deploys(h.since(m), project); n != 2 {
		t.Fatalf("B made %d deployments, want 2 (the lost one and its own):\n%s", n, testenv.Summary(h.since(m)))
	}

	// C's create is refused with a 500 (nothing made): the retry must not take the newest
	// deployment of the commit (A's or B's) for C's lost one.
	if err := h.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost,
		PathRegex: "^(/client/v4)?/accounts/" + h.acct.AccountID + "/pages/projects/" + project + "/deployments$", Times: 1,
		Status: http.StatusInternalServerError}); err != nil {
		t.Fatal(err)
	}
	h.siteConfigMap("c-files", map[string]string{"index.html": "C"}, nil)
	h.newDeployment("c", "site", "c-preview", cmSource("c-files"), setCommit)
	c := h.waitDeployment("c", func(pd *pagesv1alpha1.PagesDeployment) bool {
		return deployed(pd) && pd.Status.AtProvider.Branch == "c-preview"
	})
	if c.Status.ID == a.Status.ID || c.Status.ID == b.Status.ID {
		t.Fatalf("C took deployment %s (A %s, B %s) as its own", c.Status.ID, a.Status.ID, b.Status.ID)
	}

	h.delete(b)
	h.waitGone(b)
	if !h.deploymentExists(project, a.Status.ID) {
		t.Fatalf("deleting B deleted A's deployment %s", a.Status.ID)
	}
	if h.deploymentExists(project, b.Status.ID) {
		t.Fatalf("B's own deployment %s is still in Cloudflare", b.Status.ID)
	}
	h.settle("a")
	if a2 := h.waitDeployment("a", deployed); a2.Status.ID != a.Status.ID {
		t.Fatalf("A's deployment changed from %s to %s", a.Status.ID, a2.Status.ID)
	}
}

// The finalizer's half of the guard above: a deleted object whose create-pending record carries
// a user-set commit hash must not look its lost deployment up by that hash, which would find
// (and delete) another object's deployment of the same commit. D is blocked before it deploys
// (its artifact ConfigMap is missing), so its record is the one a crash right after the
// announcement leaves, and only the finalizer reads it.
func TestUserSetCommitHashFinalizerDeletesNothing(t *testing.T) {
	h := start(t)
	project := h.project("site")
	h.siteConfigMap("a-files", map[string]string{"index.html": "A"}, nil)
	const commit = "def456def456"
	setCommit := func(pd *pagesv1alpha1.PagesDeployment) { pd.Spec.ForProvider.CommitHash = commit }
	h.newDeployment("a", "site", "a-preview", cmSource("a-files"), setCommit)
	a := h.waitDeployment("a", deployed)

	h.newDeployment("d", "site", "d-preview", cmSource("d-missing"), setCommit)
	d := h.waitDeployment("d", func(pd *pagesv1alpha1.PagesDeployment) bool {
		c := meta.FindStatusCondition(pd.Status.Conditions, commonv1alpha1.ConditionReady)
		return controllerutil.ContainsFinalizer(pd, commonv1alpha1.Finalizer) && c != nil && c.Reason == commonv1alpha1.ReasonDependency
	})
	h.update(d, func() {
		metav1.SetMetaDataAnnotation(&d.ObjectMeta, reconcile.AnnotationCreatePending,
			string(d.UID)+"/"+project+";k="+strings.Repeat("0", 64)+";c="+commit)
	})
	m := h.mark()
	h.delete(d)
	h.waitGone(d)
	j := h.since(m)
	if n := testenv.Count(j, http.MethodDelete, "/deployments/"); n != 0 {
		t.Fatalf("deleting D sent %d deployment DELETEs:\n%s", n, testenv.Summary(j))
	}
	if n := testenv.CountPath(j, http.MethodGet, "/accounts/"+h.acct.AccountID+"/pages/projects/"+project+"/deployments"); n != 0 {
		t.Fatalf("deleting D looked its lost deployment up by the user-set commit hash:\n%s", testenv.Summary(j))
	}
	if !h.deploymentExists(project, a.Status.ID) {
		t.Fatalf("deleting D deleted A's deployment %s", a.Status.ID)
	}
	h.waitEvent("d", reconcile.EventReasonExternalResourceKept, commit, "does not identify it")
	var evs eventsv1.EventList
	if err := h.e.Client.List(h.ctx(), &evs, client.InNamespace(h.ns)); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs.Items {
		if e.Regarding.Name == "d" && e.Reason == reconcile.EventReasonExternalResourceKept && e.Action != "Delete" {
			t.Fatalf("D's %s Event came from %q, want only the finalizer's (Delete): %s", e.Reason, e.Action, e.Note)
		}
	}
}
