package workerscript_test

import (
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/workerscript"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// Stale-cache safety of the create-pending record (reconcile.FreshCreatePending): the
// WorkerScript reconciler runs outside the manager, on a client whose Get serves a lagging view
// of the object (testenv.LaggingClient), while its APIReader reads the API server.

// startDirect is a harness whose manager runs only the CloudflareAccount controller (and the
// WorkerScript field index); the reconciler it returns is driven directly.
func startDirect(t *testing.T) (*harness, *testenv.LaggingClient, *workerscript.Reconciler) {
	t.Helper()
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	h := &harness{t: t, e: e, ns: ns}
	h.m = e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{ns},
		Setup: []func(ctrl.Manager, controller.Deps) error{func(mgr ctrl.Manager, _ controller.Deps) error { return workerscript.RegisterIndexes(mgr) }}})
	h.acct = e.CreateReadyAccount(t, ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	lc := &testenv.LaggingClient{Client: e.Client, Lists: h.m.Client}
	r := &workerscript.Reconciler{Client: lc, APIReader: e.Client, Accounts: e.DirectAccounts(), Tagger: reconcile.ResourceTagger{},
		ClusterName: "testenv"}
	return h, lc, r
}

func (h *harness) injectFault(f fake.Fault) {
	h.t.Helper()
	if err := h.e.Fake.InjectFault(f); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) uploadRegex(name string) string {
	return "^(/client/v4)?/accounts/" + h.acct.AccountID + "/workers/scripts/" + name + "$"
}

// A first upload is refused for good, which drops the create-pending record; someone else then
// uploads a script of that name (without an owner tag). A retry whose cached copy still shows
// the record must not take that script for its own interrupted upload (NameConflict, no
// ownership proof, no upload over it), and the object's deletion (deletionPolicy Delete, the
// finalizer seeing the stale record too) leaves it alone.
func TestWorkerScriptStaleCacheKeepsDroppedRecord(t *testing.T) {
	h, lc, r := startDirect(t)
	ws := h.newScript("stale", params(fetchModule), func(ws *workersv1alpha1.WorkerScript) { ws.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete })
	h.injectFault(fake.Fault{Method: http.MethodPut, PathRegex: h.uploadRegex("stale"), Times: 1,
		Status: http.StatusBadRequest, Code: 10021, Message: "Uncaught SyntaxError: Unexpected token"})
	if err := h.e.ReconcileDirect(t, r, ws); err != nil {
		t.Logf("refused upload: %v", err)
	}
	if _, pending := reconcile.PendingCreate(ws); pending {
		t.Fatalf("a refused upload kept its create-pending record: %v", ws.Annotations)
	}
	record := lc.LastWritten(ws, reconcile.AnnotationCreatePending)
	if record == "" {
		t.Fatal("the upload was not announced")
	}
	h.apiUpload("stale", `export default { fetch() { return new Response("foreign") } };`)
	lc.SetLag(testenv.LagAnnotation(ws, reconcile.AnnotationCreatePending, record))

	m := h.mark()
	_ = h.e.ReconcileDirect(t, r, ws)
	if !hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, reconcile.ReasonNameConflict) ||
		reconcile.HasOwnershipProof(ws, "stale") || ws.Annotations[commonv1alpha1.AnnotationExternalID] != "" {
		t.Fatalf("a stale create-pending record adopted the foreign script: annotations %v %s", ws.Annotations, condOf(ws.Status.Conditions, "Synced"))
	}
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("writes to the foreign script:\n%s", testenv.Summary(w))
	}

	if err := h.e.Client.Delete(h.ctx(), ws); err != nil {
		t.Fatal(err)
	}
	_ = h.e.ReconcileDirect(t, r, ws)
	if !h.scriptExists("stale") {
		t.Fatal("the finalizer deleted the foreign script through a stale create-pending record")
	}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(ws), ws); !apierrors.IsNotFound(err) {
		t.Fatalf("the WorkerScript was not finalized: %v (finalizers %v)", err, ws.Finalizers)
	}
}

// A first upload fails transiently but was made (a lost answer), and the retry's cached copy
// does not show the record yet: the uncached record identifies the script as the object's own
// interrupted upload, which is adopted instead of reported as a NameConflict.
func TestWorkerScriptStaleCacheMissesRecord(t *testing.T) {
	h, lc, r := startDirect(t)
	ws := h.newScript("lost", params(fetchModule), nil)
	h.injectFault(fake.Fault{Method: http.MethodPut, PathRegex: h.uploadRegex("lost"), Times: 1 + cfclient.DefaultMaxRetries, // PUTs are retried
		Status: http.StatusInternalServerError, Code: 10013, Message: "An unknown error has occurred"})
	if err := h.e.ReconcileDirect(t, r, ws); err == nil {
		t.Fatal("the failed upload returned no error")
	}
	if _, pending := reconcile.PendingCreate(ws); !pending {
		t.Fatalf("a transient failure dropped the create-pending record: %v", ws.Annotations)
	}
	h.apiUpload("lost", fetchModule) // the upload the failed answer hid
	lc.SetLag(testenv.LagAnnotation(ws, reconcile.AnnotationCreatePending, ""))

	_ = h.e.ReconcileDirect(t, r, ws)
	if !reconcile.HasOwnershipProof(ws, "lost") || ws.Status.ID != "lost" {
		t.Fatalf("the own interrupted upload was not adopted: status.id %q annotations %v %s", ws.Status.ID, ws.Annotations,
			condOf(ws.Status.Conditions, "Synced"))
	}
}
