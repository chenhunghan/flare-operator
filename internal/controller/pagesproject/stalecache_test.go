package pagesproject_test

import (
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/pagesproject"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// Stale-cache safety of the create-pending record (reconcile.FreshCreatePending): the
// PagesProject reconciler runs outside the manager, on a client whose Get serves a lagging view
// of the object (testenv.LaggingClient), while its APIReader reads the API server.

// startDirect is a harness whose manager runs only the CloudflareAccount controller (and the
// PagesProject field index); the reconciler it returns is driven directly.
func startDirect(t *testing.T) (*harness, *testenv.LaggingClient, *pagesproject.Reconciler) {
	t.Helper()
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	h := &harness{t: t, e: e, ns: ns}
	h.m = e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{ns},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, _ controller.Deps) error { return pagesproject.RegisterIndexes(mgr) }}})
	h.acct = e.CreateReadyAccount(t, ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	lc := &testenv.LaggingClient{Client: e.Client, Lists: h.m.Client}
	r := &pagesproject.Reconciler{Client: lc, APIReader: e.Client, Accounts: e.DirectAccounts(), Tagger: reconcile.ResourceTagger{},
		ClusterName: "testenv"}
	return h, lc, r
}

func (h *harness) injectCreateFault(status, code int, msg string) {
	h.t.Helper()
	if err := h.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost, PathRegex: "^(/client/v4)?/accounts/" + h.acct.AccountID + "/pages/projects$",
		Times: 1, Status: status, Code: code, Message: msg}); err != nil {
		h.t.Fatal(err)
	}
}

// A create is refused for good, which drops the create-pending record; someone else then
// creates a project of that name (without an owner tag). A retry whose cached copy still shows
// the record must not take that project for its own lost create (NameConflict, no ownership
// proof, no writes to it), and the object's deletion (deletionPolicy Delete, the finalizer
// seeing the stale record too) leaves it alone.
func TestPagesProjectStaleCacheKeepsDroppedRecord(t *testing.T) {
	h, lc, r := startDirect(t)
	name := h.projectName("stale")
	pp := h.newProject("stale", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main"},
		func(pp *pagesv1alpha1.PagesProject) { pp.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete })
	h.injectCreateFault(http.StatusForbidden, 10000, "Authentication error")
	_ = h.e.ReconcileDirect(t, r, pp)
	if _, pending := reconcile.PendingCreate(pp); pending {
		t.Fatalf("a refused create kept its create-pending record: %v", pp.Annotations)
	}
	record := lc.LastWritten(pp, reconcile.AnnotationCreatePending)
	if record == "" {
		t.Fatal("the create was not announced")
	}
	if _, err := h.apiDo(http.MethodPost, "/pages/projects", map[string]any{"name": name, "production_branch": "dev"}); err != nil {
		t.Fatal(err)
	}
	lc.SetLag(testenv.LagAnnotation(pp, reconcile.AnnotationCreatePending, record))

	m := h.mark()
	_ = h.e.ReconcileDirect(t, r, pp)
	if !hasCond(pp.Status.Conditions, pp.Generation, "Synced", metav1.ConditionFalse, reconcile.ReasonNameConflict) ||
		reconcile.HasOwnershipProof(pp, name) || pp.Annotations[commonv1alpha1.AnnotationExternalID] != "" {
		t.Fatalf("a stale create-pending record adopted the foreign project: annotations %v %s", pp.Annotations, condOf(pp.Status.Conditions, "Synced"))
	}
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("writes to the foreign project:\n%s", testenv.Summary(w))
	}

	if err := h.e.Client.Delete(h.ctx(), pp); err != nil {
		t.Fatal(err)
	}
	_ = h.e.ReconcileDirect(t, r, pp)
	if _, err := h.apiDo(http.MethodGet, "/pages/projects/"+name, nil); err != nil {
		t.Fatalf("the finalizer deleted the foreign project through a stale create-pending record: %v", err)
	}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(pp), pp); !apierrors.IsNotFound(err) {
		t.Fatalf("the PagesProject was not finalized: %v (finalizers %v)", err, pp.Finalizers)
	}
}

// A create fails transiently but was made (a lost answer), and the retry's cached copy does not
// show the record yet: the uncached record identifies the project as the object's own lost
// create, which is adopted instead of reported as a NameConflict.
func TestPagesProjectStaleCacheMissesRecord(t *testing.T) {
	h, lc, r := startDirect(t)
	name := h.projectName("lost")
	pp := h.newProject("lost", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main"}, nil)
	h.injectCreateFault(http.StatusInternalServerError, 8000000, "An unknown error occurred")
	if err := h.e.ReconcileDirect(t, r, pp); err == nil {
		t.Fatal("the failed create returned no error")
	}
	if _, pending := reconcile.PendingCreate(pp); !pending {
		t.Fatalf("a transient failure dropped the create-pending record: %v", pp.Annotations)
	}
	if _, err := h.apiDo(http.MethodPost, "/pages/projects", map[string]any{"name": name, "production_branch": "main"}); err != nil {
		t.Fatal(err) // the create the failed answer hid
	}
	lc.SetLag(testenv.LagAnnotation(pp, reconcile.AnnotationCreatePending, ""))

	_ = h.e.ReconcileDirect(t, r, pp)
	if !reconcile.HasOwnershipProof(pp, name) || pp.Status.ID != name {
		t.Fatalf("the own lost create was not adopted: status.id %q annotations %v %s", pp.Status.ID, pp.Annotations,
			condOf(pp.Status.Conditions, "Synced"))
	}
}
