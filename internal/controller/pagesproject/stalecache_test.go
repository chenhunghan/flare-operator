package pagesproject_test

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/controller/pagesproject"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
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

// snapshotClient is a LaggingClient that keeps a copy of the object every Patch answered
// with: the object as the informer cache would serve it at that resourceVersion (the status
// subresource is written through Status(), which it does not record).
type snapshotClient struct {
	*testenv.LaggingClient
	snaps []*pagesv1alpha1.PagesProject
}

func (c *snapshotClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.LaggingClient.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	if pp, ok := obj.(*pagesv1alpha1.PagesProject); ok {
		c.snaps = append(c.snaps, pp.DeepCopy())
	}
	return nil
}

// serve returns a lag that serves snap (a whole, older copy of the object) instead of the
// stored object: an informer cache that has not seen the reconciler's latest writes yet.
func serve(snap *pagesv1alpha1.PagesProject) func(client.Object) {
	return func(o client.Object) {
		if pp, ok := o.(*pagesv1alpha1.PagesProject); ok && pp.UID == snap.UID {
			*pp = *snap.DeepCopy()
		}
	}
}

// The reconcile that follows a create sees the cache as it was when the create was announced
// (the create-pending annotation is what queued it), without the recorded ownership or the
// Ready status; the one after that sees the cache caught up with the create's reconcile but not
// with the second one's writes. Neither may leave a stale status behind: a status computed from
// the stale copy must not replace the stored Ready=True, and a reconcile whose stale copy
// already looks right must not skip the write that repairs it. Before status writes were
// optimistically locked (reconcile.PatchStatus), the second reconcile's ownership record failed
// with a Conflict and its status patch (a merge patch of the whole conditions list) dropped
// Ready; the third found its cached status already Ready and wrote nothing, so the object
// stayed not Ready until the resync. Now the second reconcile's status write conflicts too.
func TestPagesProjectStaleCacheKeepsReadyStatus(t *testing.T) {
	h, lc, r := startDirect(t)
	sc := &snapshotClient{LaggingClient: lc}
	r.Client = sc
	name := h.projectName("ready")
	pp := h.newProject("ready", &pagesv1alpha1.PagesProjectParameters{Name: name, ProductionBranch: "main"}, nil)
	if err := h.e.ReconcileDirect(t, r, pp); err != nil {
		t.Fatal(err)
	}
	if !ready(pp) {
		t.Fatalf("the create did not make the object Ready: %s %s", condOf(pp.Status.Conditions, "Ready"), condOf(pp.Status.Conditions, "Synced"))
	}
	afterCreate := pp.DeepCopy()
	var announced *pagesv1alpha1.PagesProject
	for _, s := range sc.snaps {
		if _, ok := reconcile.PendingCreate(s); ok {
			announced = s
		}
	}
	if announced == nil {
		t.Fatal("the create was not announced")
	}

	lc.SetLag(serve(announced))
	_ = h.e.ReconcileDirect(t, r, pp)
	lc.SetLag(serve(afterCreate))
	_ = h.e.ReconcileDirect(t, r, pp)
	lc.SetLag(nil)
	if !ready(pp) {
		t.Fatalf("reconciles on a lagging cache left a stale status: %s %s", condOf(pp.Status.Conditions, "Ready"), condOf(pp.Status.Conditions, "Synced"))
	}
}
