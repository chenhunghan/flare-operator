package pagesdeployment_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/pagesdeployment"
	"flare.dev/operator/internal/controller/pagesproject"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// A deployment's create is made in Cloudflare but its answer is lost (the client gives up), so
// its create-pending record stands; the retry's cached copy does not show the record yet (the
// reconciler runs outside the manager, on a client whose Get lags: testenv.LaggingClient). The
// uncached record (reconcile.FreshCreatePending) identifies the deployment by its derived
// commit hash: it is adopted, not deployed a second time.
func TestPagesDeploymentStaleCacheMissesRecord(t *testing.T) {
	// The manager runs the CloudflareAccount and PagesProject controllers; PagesDeployments are
	// reconciled only by the direct reconciler below.
	e := testenv.Require(t, env)
	h := &harness{t: t, e: e, ns: e.Namespace(t)}
	h.m = e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{h.ns}, Setup: []func(ctrl.Manager, controller.Deps) error{
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&pagesproject.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr)
		}}})
	h.acct = e.CreateReadyAccount(t, h.ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	project := h.project("site")
	loader, err := artifact.NewLoader(artifact.Options{UserAgent: "flare-operator-testenv"})
	if err != nil {
		t.Fatal(err)
	}
	lc := &testenv.LaggingClient{Client: h.e.Client, Lists: h.m.Client}
	r := &pagesdeployment.Reconciler{Client: lc, APIReader: h.e.Client, Artifacts: loader,
		Accounts: h.e.DirectAccounts(reconcile.WithHTTPClient(&http.Client{Timeout: 2 * time.Second}))}

	h.siteConfigMap("files", map[string]string{"index.html": "lost"}, nil)
	pd := h.newDeployment("lost", "site", "preview", cmSource("files"), nil)
	if err := h.e.Fake.InjectFault(fake.Fault{Method: http.MethodPost,
		PathRegex: "^(/client/v4)?/accounts/" + h.acct.AccountID + "/pages/projects/" + project + "/deployments$", Times: 1,
		FaultShape: fake.FaultShape{DelayMs: 4000, Passthrough: true}}); err != nil {
		t.Fatal(err)
	}
	m := h.mark()
	if err := h.e.ReconcileDirect(t, r, pd); err == nil {
		t.Fatal("the create whose answer was lost returned no error")
	}
	if _, pending := reconcile.PendingCreate(pd); !pending {
		t.Fatalf("a lost answer dropped the create-pending record: %v", pd.Annotations)
	}
	var lost struct {
		ID string `json:"id"`
	}
	testenv.Eventually(t, 15*time.Second, func() (bool, string) {
		var ds []struct {
			ID string `json:"id"`
		}
		resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + "/pages/projects/" + project + "/deployments"})
		if err != nil {
			return false, err.Error()
		}
		if err := json.Unmarshal(resp.Result, &ds); err != nil || len(ds) != 1 {
			return false, "waiting for the lost deployment"
		}
		lost.ID = ds[0].ID
		return true, ""
	})
	lc.SetLag(testenv.LagAnnotation(pd, reconcile.AnnotationCreatePending, ""))

	_ = h.e.ReconcileDirect(t, r, pd)
	if n := h.deploys(h.since(m), project); n != 1 {
		t.Fatalf("%d deployment creates, want 1 (the lost one, adopted):\n%s", n, testenv.Summary(h.since(m)))
	}
	if pd.Status.ID != lost.ID || !reconcile.HasOwnershipProof(pd, lost.ID) {
		t.Fatalf("the lost deployment %s was not adopted: status.id %q annotations %v %s", lost.ID, pd.Status.ID, pd.Annotations,
			condOf(pd.Status.Conditions, "Synced"))
	}
}
