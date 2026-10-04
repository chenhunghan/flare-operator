package tunnel_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "github.com/chenhunghan/flare-operator/api/workersvpc/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/controller/tunnel"
	"github.com/chenhunghan/flare-operator/internal/controller/tunnelnet"
	"github.com/chenhunghan/flare-operator/internal/controller/vpcservice"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// Stale-cache safety of the create-pending record (reconcile.FreshCreatePending). The
// reconciler under test runs outside the manager, on a client whose Get serves a lagging view
// of the object (testenv.LaggingClient), while its APIReader reads the API server.

// startDirect is a harness whose manager runs the CloudflareAccount controller and setup
// (nothing else): the kind under test is reconciled directly.
func startDirect(t *testing.T, setup ...func(ctrl.Manager, controller.Deps) error) *harness {
	t.Helper()
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	h := &harness{t: t, e: e, ns: ns}
	// The field indexes the Tunnel and VPCService reconcilers list by (as SetupWithManager
	// registers them).
	setup = append(setup, func(mgr ctrl.Manager, _ controller.Deps) error {
		return tunnelnet.RegisterIndexes(context.Background(), mgr.GetFieldIndexer(), tunnelnet.ClusterDNS{}.WithDefaults().Domain)
	})
	h.m = e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{ns, "kube-system"}, Setup: setup})
	h.ensureKubeDNS()
	h.acct = e.CreateReadyAccount(t, ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	return h
}

// laggingClient is the direct reconciler's client: it Gets from the API server through the
// lag and Lists from the manager's cache (which has the field indexes).
func (h *harness) laggingClient() *testenv.LaggingClient {
	return &testenv.LaggingClient{Client: h.e.Client, Lists: h.m.Client}
}

func (h *harness) directAccounts() *reconcile.Accounts { return h.e.DirectAccounts() }

func (h *harness) reconcileDirect(r reconcileDirector, obj client.Object) error {
	h.t.Helper()
	return h.e.ReconcileDirect(h.t, r, obj)
}

type reconcileDirector interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

func (h *harness) injectFault(f fake.Fault) {
	h.t.Helper()
	if err := h.e.Fake.InjectFault(f); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) accountPathRegex(p string) string {
	return "^(/client/v4)?/accounts/" + h.acct.AccountID + p + "$"
}

// A Tunnel's create is refused for good, which drops the create-pending record; someone else
// then creates a tunnel of that name. A retry whose cached copy still shows the record must not
// take that tunnel for its own lost create (NameConflict, no ownership proof), and the object's
// deletion (deletionPolicy Delete, the finalizer seeing the stale record too) leaves it alone.
func TestTunnelStaleCacheKeepsDroppedRecord(t *testing.T) {
	h := startDirect(t)
	lc := h.laggingClient()
	r := &tunnel.Reconciler{Client: lc, APIReader: h.e.Client, Accounts: h.directAccounts(), Tagger: reconcile.NoopTagger{},
		ClusterName: "testenv", DrainInterval: 200 * time.Millisecond}
	tun := h.newTunnel("stale", nil)
	h.injectFault(fake.Fault{Method: http.MethodPost, PathRegex: h.accountPathRegex("/cfd_tunnel"), Times: 1,
		Status: http.StatusForbidden, Code: 10000, Message: "Authentication error"})
	if err := h.reconcileDirect(r, tun); err == nil {
		t.Fatal("the refused create returned no error")
	}
	if _, pending := reconcile.PendingCreate(tun); pending {
		t.Fatalf("a refused create kept its create-pending record: %v", tun.Annotations)
	}
	record := lc.LastWritten(tun, reconcile.AnnotationCreatePending)
	if record == "" {
		t.Fatal("the create was not announced")
	}
	foreign, _ := h.apiCreateTunnel(tun.TunnelName())
	lc.SetLag(testenv.LagAnnotation(tun, reconcile.AnnotationCreatePending, record))

	_ = h.reconcileDirect(r, tun)
	if !hasCond(tun.Status.Conditions, tun.Generation, "Synced", metav1.ConditionFalse, reconcile.ReasonNameConflict) || tun.Status.ID != "" ||
		reconcile.HasOwnershipProof(tun, foreign) || tun.Annotations[commonv1alpha1.AnnotationExternalID] != "" {
		t.Fatalf("a stale create-pending record adopted the foreign tunnel %s: status.id %q annotations %v %s",
			foreign, tun.Status.ID, tun.Annotations, condOf(tun.Status.Conditions, "Synced"))
	}

	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // a drain would take several
		_ = h.reconcileDirect(r, tun)
	}
	if got := h.cfTunnel(foreign); got.DeletedAt != nil {
		t.Fatalf("the finalizer deleted the foreign tunnel %s through a stale create-pending record", foreign)
	}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(tun), tun); !apierrors.IsNotFound(err) {
		t.Fatalf("the Tunnel was not finalized: %v (finalizers %v)", err, tun.Finalizers)
	}
}

// A Tunnel's create fails transiently but was made (a lost answer), and the retry's cached copy
// does not show the record yet: the uncached record identifies the tunnel as the object's own
// lost create, which is adopted instead of reported as a NameConflict.
func TestTunnelStaleCacheMissesRecord(t *testing.T) {
	h := startDirect(t)
	lc := h.laggingClient()
	r := &tunnel.Reconciler{Client: lc, APIReader: h.e.Client, Accounts: h.directAccounts(), Tagger: reconcile.NoopTagger{},
		ClusterName: "testenv", DrainInterval: 200 * time.Millisecond}
	tun := h.newTunnel("lost", func(t *tunnelsv1alpha1.Tunnel) { t.Spec.DeletionPolicy = commonv1alpha1.DeletionOrphan })
	h.injectFault(fake.Fault{Method: http.MethodPost, PathRegex: h.accountPathRegex("/cfd_tunnel"), Times: 1,
		Status: http.StatusInternalServerError, Code: 1002, Message: "Internal Server Error"})
	if err := h.reconcileDirect(r, tun); err == nil {
		t.Fatal("the failed create returned no error")
	}
	if key, pending := reconcile.PendingCreate(tun); !pending || key != tun.TunnelName() {
		t.Fatalf("a transient failure dropped the create-pending record: %v", tun.Annotations)
	}
	// The create the failed answer hid, and a cache that has not seen its record.
	own, _ := h.apiCreateTunnel(tun.TunnelName())
	lc.SetLag(testenv.LagAnnotation(tun, reconcile.AnnotationCreatePending, ""))

	_ = h.reconcileDirect(r, tun)
	if tun.Status.ID != own || !reconcile.HasOwnershipProof(tun, own) {
		t.Fatalf("the own lost create %s was not adopted: status.id %q annotations %v %s", own, tun.Status.ID, tun.Annotations,
			condOf(tun.Status.Conditions, "Synced"))
	}
}

// startWithTunnel is startDirect with the Tunnel controller in the manager, and a Tunnel "tun"
// that has its Cloudflare ID.
func startWithTunnel(t *testing.T) (*harness, *tunnelsv1alpha1.Tunnel) {
	h := startDirect(t, func(mgr ctrl.Manager, d controller.Deps) error {
		return (&tunnel.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
			DrainInterval: 200 * time.Millisecond, Recorder: mgr.GetEventRecorder(tunnel.Name), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr)
	})
	h.newTunnel("tun", nil)
	return h, h.waitTunnel("tun", func(t *tunnelsv1alpha1.Tunnel) bool { return t.Status.ID != "" })
}

// The VPCService half of TestTunnelStaleCacheKeepsDroppedRecord: a create refused for good (not
// for a duplicate name) drops the record; a same-named service someone else creates is neither
// adopted by a retry that still sees the record, nor deleted by the object's finalizer.
func TestVPCServiceStaleCacheKeepsDroppedRecord(t *testing.T) {
	h, tun := startWithTunnel(t)
	lc := h.laggingClient()
	r := &vpcservice.Reconciler{Client: lc, APIReader: h.e.Client, Accounts: h.directAccounts()}
	vs := h.newVPC("stale", hostnameParams("api.example.com", 80, "tun"), func(v *workersvpcv1alpha1.VPCService) {
		v.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete
	})
	h.injectFault(fake.Fault{Method: http.MethodPost, PathRegex: h.accountPathRegex("/connectivity/directory/services"), Times: 1,
		Status: http.StatusForbidden, Code: 10000, Message: "Authentication error"})
	if err := h.reconcileDirect(r, vs); err == nil {
		t.Fatal("the refused create returned no error")
	}
	if _, pending := reconcile.PendingCreate(vs); pending {
		t.Fatalf("a refused create kept its create-pending record: %v", vs.Annotations)
	}
	record := lc.LastWritten(vs, reconcile.AnnotationCreatePending)
	if record == "" {
		t.Fatal("the create was not announced")
	}
	foreign := h.apiCreateService(vs.ServiceName(), tun.Status.ID)
	lc.SetLag(testenv.LagAnnotation(vs, reconcile.AnnotationCreatePending, record))

	_ = h.reconcileDirect(r, vs)
	if !hasCond(vs.Status.Conditions, vs.Generation, "Synced", metav1.ConditionFalse, vpcservice.ReasonNameConflict) || vs.Status.ID != "" ||
		reconcile.HasOwnershipProof(vs, foreign) || vs.Annotations[commonv1alpha1.AnnotationExternalID] != "" {
		t.Fatalf("a stale create-pending record adopted the foreign service %s: status.id %q annotations %v %s",
			foreign, vs.Status.ID, vs.Annotations, condOf(vs.Status.Conditions, "Synced"))
	}

	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	_ = h.reconcileDirect(r, vs)
	var svc map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+foreign, &svc); err != nil {
		t.Fatalf("the finalizer deleted the foreign service %s through a stale create-pending record: %v", foreign, err)
	}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(vs), vs); !apierrors.IsNotFound(err) {
		t.Fatalf("the VPCService was not finalized: %v (finalizers %v)", err, vs.Finalizers)
	}
}

// The VPCService half of TestTunnelStaleCacheMissesRecord.
func TestVPCServiceStaleCacheMissesRecord(t *testing.T) {
	h, tun := startWithTunnel(t)
	lc := h.laggingClient()
	r := &vpcservice.Reconciler{Client: lc, APIReader: h.e.Client, Accounts: h.directAccounts()}
	vs := h.newVPC("lost", hostnameParams("api.example.com", 80, "tun"), nil)
	h.injectFault(fake.Fault{Method: http.MethodPost, PathRegex: h.accountPathRegex("/connectivity/directory/services"), Times: 1,
		Status: http.StatusInternalServerError, Code: 10001, Message: "Internal error"})
	if err := h.reconcileDirect(r, vs); err == nil {
		t.Fatal("the failed create returned no error")
	}
	if key, pending := reconcile.PendingCreate(vs); !pending || key != vs.ServiceName() {
		t.Fatalf("a transient failure dropped the create-pending record: %v", vs.Annotations)
	}
	own := h.apiCreateService(vs.ServiceName(), tun.Status.ID)
	lc.SetLag(testenv.LagAnnotation(vs, reconcile.AnnotationCreatePending, ""))

	_ = h.reconcileDirect(r, vs)
	if vs.Status.ID != own || !reconcile.HasOwnershipProof(vs, own) {
		t.Fatalf("the own lost create %s was not adopted: status.id %q annotations %v %s", own, vs.Status.ID, vs.Annotations,
			condOf(vs.Status.Conditions, "Synced"))
	}
}
