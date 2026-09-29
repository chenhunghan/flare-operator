package tunnel_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller/tunnelnet"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// waitAllGone waits (up to timeout) until every object is NotFound.
func (h *harness) waitAllGone(timeout time.Duration, objs ...client.Object) {
	h.t.Helper()
	testenv.Eventually(h.t, timeout, func() (bool, string) {
		var left []string
		for _, o := range objs {
			err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(o), o)
			if !apierrors.IsNotFound(err) {
				left = append(left, fmt.Sprintf("%T %s (err %v, finalizers %v)", o, o.GetName(), err, o.GetFinalizers()))
			}
		}
		return len(left) == 0, "still present: " + strings.Join(left, "; ")
	})
}

func deleteOrder(j []fake.JournalEntry) []string {
	var order []string
	for _, e := range testenv.Writes(j) {
		if e.Method == http.MethodDelete {
			order = append(order, e.Path)
		}
	}
	return order
}

// TestNamespaceDeletion deletes a whole namespace holding a CloudflareAccount, its token Secret,
// a Tunnel and a VPCService that references it. envtest runs no namespace controller or garbage
// collector, so the test deletes every object the way the namespace controller would, the
// Secret and the account first. The account's in-use finalizer keeps it (and its cached client)
// until both managed objects have cleaned up, so Cloudflare sees the VPC service deleted
// before the tunnel, and nothing is left behind.
func TestNamespaceDeletion(t *testing.T) {
	h := start(t)
	h.service("marker", map[string]string{"app": "marker"},
		corev1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)})
	h.newVPC("web", hostnameParams("marker."+h.ns+".svc.cluster.local", 80, "tun"), nil)
	h.newTunnel("tun", nil)
	h.setDeploymentStatus("tun", 2, 2)
	tun := h.waitTunnel("tun", tunnelReady)
	vs := h.waitVPC("web", vpcReady)
	tunnelID, serviceID := tun.Status.ID, vs.Status.ID
	for _, o := range []metav1.Object{tun, vs} {
		if o.GetLabels()[reconcile.AccountLabel] != "acct" {
			t.Fatalf("%s has no account label: %v", o.GetName(), o.GetLabels())
		}
		if o.GetAnnotations()[tunnelnet.AnnotationCreatedByUID] != string(o.GetUID()) {
			t.Errorf("%s: created-by-uid %q, want its UID %s", o.GetName(), o.GetAnnotations()[tunnelnet.AnnotationCreatedByUID], o.GetUID())
		}
	}

	m := h.mark()
	if err := h.e.Client.Delete(h.ctx(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: h.ns}}); err != nil {
		t.Fatal(err)
	}
	acct := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "acct"}}
	objs := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "acct-token"}},
		acct,
		&tunnelsv1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "tun"}},
		&workersvpcv1alpha1.VPCService{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "web"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "tun-cloudflared"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "tun-cloudflared"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "tun-cloudflared-token"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "marker"}},
	}
	for _, o := range objs {
		if err := h.e.Client.Delete(h.ctx(), o); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete %T %s: %v", o, o.GetName(), err)
		}
	}
	// The account waits for its users (in-use finalizer) instead of vanishing with the Secret.
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(acct), acct); err != nil {
		t.Fatalf("account gone before its managed objects cleaned up: %v", err)
	}
	h.waitAllGone(90*time.Second, objs...)

	var gone map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+serviceID, &gone); !cfclient.IsNotFound(err) {
		t.Errorf("VPC service still exists: %v %v", gone, err)
	}
	if got := h.cfTunnel(tunnelID); got.DeletedAt == nil {
		t.Errorf("tunnel not deleted: %+v", got)
	}
	order := deleteOrder(h.since(m))
	if len(order) != 2 || !strings.Contains(order[0], "/connectivity/directory/services/"+serviceID) || !strings.Contains(order[1], "/cfd_tunnel/"+tunnelID) {
		t.Errorf("Cloudflare delete order %v, want the VPC service, then the tunnel", order)
	}
}

// TestAccountGoneKeepsExternal: when the CloudflareAccount no longer exists (here its in-use
// finalizer was removed by hand), the finalizers do not hang. The Cloudflare resources cannot
// be reached, so they are kept, each with a Warning Event, and nothing is written.
func TestAccountGoneKeepsExternal(t *testing.T) {
	h := start(t)
	id, _ := h.apiCreateTunnel("raw")
	h.newVPC("svc", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.5"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str(id)}}}, nil)
	h.newTunnel("tun", nil)
	h.setDeploymentStatus("tun", 2, 2)
	tun := h.waitTunnel("tun", tunnelReady)
	vs := h.waitVPC("svc", vpcReady)

	acct := &cloudflarev1alpha1.CloudflareAccount{}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "acct"}, acct); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Client.Delete(h.ctx(), acct); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var a cloudflarev1alpha1.CloudflareAccount
		err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(acct), &a)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err == nil && len(a.Finalizers) > 0 {
			base := a.DeepCopy()
			a.Finalizers = nil
			err = h.e.Client.Patch(h.ctx(), &a, client.MergeFrom(base))
		}
		return false, fmt.Sprint("account still present: ", err)
	})

	m := h.mark()
	for _, o := range []client.Object{vs, tun} {
		if err := h.e.Client.Delete(h.ctx(), o); err != nil {
			t.Fatal(err)
		}
	}
	h.waitAllGone(30*time.Second, vs, tun)
	h.waitEvent("svc", tunnelnet.EventReasonExternalResourceKept, vs.Status.ID, "no longer exists")
	h.waitEvent("tun", tunnelnet.EventReasonExternalResourceKept, tun.Status.ID, "no longer exists")
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Errorf("writes after the account was gone:\n%s", testenv.Summary(w))
	}
	if got := h.cfTunnel(tun.Status.ID); got.DeletedAt != nil {
		t.Error("tunnel deleted")
	}
	var still map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+vs.Status.ID, &still); err != nil {
		t.Errorf("VPC service deleted: %v", err)
	}
}

// TestTunnelObserveOnlyByNameNotPinned: an observe-only Tunnel that finds a tunnel by name
// records the ID only in status.atProvider: no external-id annotation, no status.id, no
// writes, and its deletion (Delete policy) leaves the tunnel alone. A VPCService can still
// reference it.
func TestTunnelObserveOnlyByNameNotPinned(t *testing.T) {
	h := start(t)
	id, _ := h.apiCreateTunnel("seen")
	m := h.mark()
	h.newTunnel("seen", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
		tun.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete
	})
	tun := h.waitTunnel("seen", func(t *tunnelsv1alpha1.Tunnel) bool { return tunnelReady(t) && t.Status.AtProvider.ID == id })
	if tun.Status.ID != "" || tun.Annotations[commonv1alpha1.AnnotationExternalID] != "" {
		t.Errorf("name lookup recorded as the external ID: status.id %q annotation %q", tun.Status.ID, tun.Annotations[commonv1alpha1.AnnotationExternalID])
	}
	h.newVPC("svc", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.6")}, TunnelRef: &commonv1alpha1.LocalRef{Name: "seen"}}, nil)
	vs := h.waitVPC("svc", vpcReady)
	if vs.Status.TunnelID != id {
		t.Errorf("VPCService tunnel_id %q, want the observed %s", vs.Status.TunnelID, id)
	}
	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	h.waitGone(vs)
	m2 := h.mark()
	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	h.waitGone(tun)
	if w := testenv.Writes(h.since(m2)); len(w) != 0 {
		t.Errorf("observe-only Tunnel deletion wrote:\n%s", testenv.Summary(w))
	}
	for _, e := range testenv.Writes(h.since(m)) {
		if strings.Contains(e.Path, "/cfd_tunnel") || strings.Contains(e.Path, "/tags") {
			t.Errorf("observe-only Tunnel wrote %s %s", e.Method, e.Path)
		}
	}
	if got := h.cfTunnel(id); got.DeletedAt != nil {
		t.Error("observed tunnel deleted")
	}
}

// TestTunnelUntaggedAdoptionNotDeleted: with ownership tags disabled, a tunnel adopted by name
// is never proven to be ours, so it is tracked in status.id only and kept on deletion
// (ExternalResourceKept). A tunnel the Tunnel created (created-by-uid) is deleted.
func TestTunnelUntaggedAdoptionNotDeleted(t *testing.T) {
	h := startWith(t, testenv.ManagerOptions{Tagger: reconcile.NoopTagger{}})
	id, _ := h.apiCreateTunnel("legacy")
	h.newTunnel("legacy", nil)
	h.setDeploymentStatus("legacy", 2, 2)
	tun := h.waitTunnel("legacy", func(t *tunnelsv1alpha1.Tunnel) bool { return tunnelReady(t) && t.Status.ID == id })
	if a := tun.Annotations; a[commonv1alpha1.AnnotationExternalID] != "" || a[tunnelnet.AnnotationCreatedByUID] != "" {
		t.Errorf("untagged adoption pinned the tunnel: %v", a)
	}
	h.newTunnel("made", nil)
	h.setDeploymentStatus("made", 2, 2)
	made := h.waitTunnel("made", tunnelReady)
	if made.Annotations[tunnelnet.AnnotationCreatedByUID] != string(made.UID) || made.Annotations[commonv1alpha1.AnnotationExternalID] != made.Status.ID {
		t.Errorf("created tunnel annotations %v", made.Annotations)
	}

	m := h.mark()
	for _, o := range []client.Object{tun, made} {
		if err := h.e.Client.Delete(h.ctx(), o); err != nil {
			t.Fatal(err)
		}
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		return *h.deployment("made").Spec.Replicas == 0, "made not scaled down"
	})
	h.setDeploymentStatus("made", 0, 0)
	h.waitAllGone(30*time.Second, tun, made)
	h.waitEvent("legacy", tunnelnet.EventReasonExternalResourceKept, id)
	if got := h.cfTunnel(id); got.DeletedAt != nil {
		t.Error("tunnel adopted without an ownership tag was deleted")
	}
	if got := h.cfTunnel(made.Status.ID); got.DeletedAt == nil {
		t.Error("tunnel created by the Tunnel was not deleted")
	}
	if order := deleteOrder(h.since(m)); len(order) != 1 || !strings.Contains(order[0], made.Status.ID) {
		t.Errorf("DELETEs %v, want only %s", order, made.Status.ID)
	}
}

// TestVPCServiceStatusIDIsNoProof: an object that knows a service only through status.id (as
// an older build could leave it) manages it but, having neither created nor pinned it, never
// deletes it.
func TestVPCServiceStatusIDIsNoProof(t *testing.T) {
	h := start(t)
	id, _ := h.apiCreateTunnel("raw")
	serviceID := h.apiCreateService("legacy", id)
	h.newVPC("legacy", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.5"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str(id)}}}, nil)
	vs := h.waitVPC("legacy", func(v *workersvpcv1alpha1.VPCService) bool {
		return hasCond(v.Status.Conditions, v.Generation, "Synced", metav1.ConditionFalse, "NameConflict")
	})
	base := vs.DeepCopy()
	vs.Status.ID = serviceID
	if err := h.e.Client.Status().Patch(h.ctx(), vs, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	h.poke(vs)
	vs = h.waitVPC("legacy", func(v *workersvpcv1alpha1.VPCService) bool { return vpcReady(v) && v.Status.ID == serviceID })

	m := h.mark()
	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	h.waitGone(vs)
	h.waitEvent("legacy", tunnelnet.EventReasonExternalResourceKept, serviceID)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Errorf("deleting an unproven object wrote:\n%s", testenv.Summary(w))
	}
	var still map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+serviceID, &still); err != nil {
		t.Errorf("service deleted without proof of ownership: %v", err)
	}
}
