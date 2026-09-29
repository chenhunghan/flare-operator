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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
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
		if !reconcile.HasOwnershipProof(o.(client.Object), o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]) {
			t.Errorf("%s: no ownership record after create: %v", o.GetName(), o.GetAnnotations())
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
	h.waitEvent("svc", reconcile.EventReasonExternalResourceKept, vs.Status.ID, "no longer exists")
	h.waitEvent("tun", reconcile.EventReasonExternalResourceKept, tun.Status.ID, "no longer exists")
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

// TestTunnelUntaggedNoAdoptionByName: with ownership tags disabled nothing can prove that a
// same-named tunnel is ours, so a managing Tunnel does not adopt it by name (NameConflict, as
// VPCService) and runs no connectors on it, not even when an older build left its ID in
// status.id. Pinning the ID with the external-id annotation adopts it, and then (pinned, tagging
// off) its deletion deletes it. A tunnel the Tunnel created carries the ownership record and is
// deleted too.
func TestTunnelUntaggedNoAdoptionByName(t *testing.T) {
	h := startWith(t, testenv.ManagerOptions{Tagger: reconcile.NoopTagger{}})
	id, _ := h.apiCreateTunnel("foreign")
	m := h.mark()
	h.newTunnel("foreign", nil)
	conflict := func(t *tunnelsv1alpha1.Tunnel) bool {
		return hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionFalse, reconcile.ReasonNameConflict) &&
			hasCond(t.Status.Conditions, t.Generation, "Synced", metav1.ConditionFalse, reconcile.ReasonNameConflict) && t.Status.ID == ""
	}
	tun := h.waitTunnel("foreign", conflict)
	if c := meta.FindStatusCondition(tun.Status.Conditions, "Synced"); !strings.Contains(c.Message, id) || !strings.Contains(c.Message, commonv1alpha1.AnnotationExternalID) {
		t.Errorf("NameConflict message %q should name the tunnel and the annotation", c.Message)
	}
	noConnector := func() {
		t.Helper()
		var dep appsv1.Deployment
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "foreign-cloudflared"}, &dep); !apierrors.IsNotFound(err) {
			t.Errorf("connector Deployment for an unproven tunnel: %v", err)
		}
		var s corev1.Secret
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "foreign-cloudflared-token"}, &s); !apierrors.IsNotFound(err) {
			t.Errorf("token Secret for an unproven tunnel: %v", err)
		}
	}
	noConnector()
	if a := tun.Annotations; a[commonv1alpha1.AnnotationExternalID] != "" || a[reconcile.AnnotationOwnershipProof] != "" {
		t.Errorf("unproven tunnel pinned: %v", a)
	}
	for _, e := range h.since(m) {
		if strings.Contains(e.Path, "/cfd_tunnel/"+id+"/token") {
			t.Errorf("fetched the token of an unproven tunnel: %s %s", e.Method, e.Path)
		}
	}
	// An older build's untagged adoption left the ID in status.id only, and ran connectors on
	// it: still no proof, so its connector Deployment and token Secret are removed.
	ctrlRef := []metav1.OwnerReference{*metav1.NewControllerRef(tun, tunnelsv1alpha1.GroupVersion.WithKind("Tunnel"))}
	labels := map[string]string{"app": "legacy-cloudflared"}
	legacy := []client.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "foreign-cloudflared", OwnerReferences: ctrlRef},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "cloudflared", Image: "cloudflare/cloudflared"}}},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "foreign-cloudflared-token", OwnerReferences: ctrlRef},
			Data:       map[string][]byte{"token": []byte("legacy-token")},
		},
	}
	for _, o := range legacy {
		if err := h.e.Client.Create(h.ctx(), o); err != nil {
			t.Fatal(err)
		}
	}
	base := tun.DeepCopy()
	tun.Status.ID = id
	if err := h.e.Client.Status().Patch(h.ctx(), tun, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	h.poke(tun)
	h.waitTunnel("foreign", conflict)
	h.waitAllGone(30*time.Second, legacy...)
	noConnector()

	h.newTunnel("made", nil)
	h.setDeploymentStatus("made", 2, 2)
	made := h.waitTunnel("made", tunnelReady)
	if !reconcile.HasOwnershipProof(made, made.Status.ID) || made.Annotations[commonv1alpha1.AnnotationExternalID] != made.Status.ID {
		t.Errorf("created tunnel annotations %v", made.Annotations)
	}

	// Explicit adoption.
	h.updateTunnel("foreign", func(t *tunnelsv1alpha1.Tunnel) {
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}
		t.Annotations[commonv1alpha1.AnnotationExternalID] = id
	})
	h.setDeploymentStatus("foreign", 2, 2)
	tun = h.waitTunnel("foreign", func(t *tunnelsv1alpha1.Tunnel) bool { return tunnelReady(t) && t.Status.ID == id })

	m = h.mark()
	for _, o := range []client.Object{tun, made} {
		if err := h.e.Client.Delete(h.ctx(), o); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"foreign", "made"} {
		testenv.Eventually(t, 30*time.Second, func() (bool, string) {
			return *h.deployment(name).Spec.Replicas == 0, name + " not scaled down"
		})
		h.setDeploymentStatus(name, 0, 0)
	}
	h.waitAllGone(30*time.Second, tun, made)
	for _, tid := range []string{id, made.Status.ID} {
		if got := h.cfTunnel(tid); got.DeletedAt == nil {
			t.Errorf("tunnel %s not deleted", tid)
		}
	}
	if order := deleteOrder(h.since(m)); len(order) != 2 {
		t.Errorf("DELETEs %v, want both tunnels", order)
	}
}

// TestLegacyCreatedByUIDMigrated: objects of an older build recorded ownership in the
// created-by-uid annotation. It still counts as proof, and the next sync rewrites it as the
// shared ownership-proof annotation, for Tunnels and VPCServices alike.
func TestLegacyCreatedByUIDMigrated(t *testing.T) {
	h := startWith(t, testenv.ManagerOptions{Tagger: reconcile.NoopTagger{}})
	id, _ := h.apiCreateTunnel("old")
	serviceID := h.apiCreateService("old-svc", id)
	h.newTunnel("old", func(t *tunnelsv1alpha1.Tunnel) {
		t.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: id}
	})
	h.newVPC("old-svc", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.5"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str(id)}}},
		func(vs *workersvpcv1alpha1.VPCService) {
			vs.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: serviceID}
		})
	h.setDeploymentStatus("old", 2, 2)
	tun := h.waitTunnel("old", tunnelReady)
	vs := h.waitVPC("old-svc", vpcReady)
	// What the older build wrote after its create.
	for _, o := range []client.Object{tun, vs} {
		base := o.DeepCopyObject().(client.Object)
		a := o.GetAnnotations()
		a[reconcile.AnnotationLegacyCreatedByUID] = string(o.GetUID())
		o.SetAnnotations(a)
		if err := h.e.Client.Patch(h.ctx(), o, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
	}
	migrated := func(o client.Object, id string) bool {
		a := o.GetAnnotations()
		_, legacy := a[reconcile.AnnotationLegacyCreatedByUID]
		return !legacy && a[reconcile.AnnotationOwnershipProof] == string(o.GetUID())+"/"+id && a[commonv1alpha1.AnnotationExternalID] == id
	}
	h.waitTunnel("old", func(t *tunnelsv1alpha1.Tunnel) bool { return migrated(t, id) })
	h.waitVPC("old-svc", func(v *workersvpcv1alpha1.VPCService) bool { return migrated(v, serviceID) })
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
	h.waitEvent("legacy", reconcile.EventReasonExternalResourceKept, serviceID)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Errorf("deleting an unproven object wrote:\n%s", testenv.Summary(w))
	}
	var still map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+serviceID, &still); err != nil {
		t.Errorf("service deleted without proof of ownership: %v", err)
	}
}
