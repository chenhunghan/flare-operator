package tunnel_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	"flare.dev/operator/internal/testenv"
)

func TestTunnelLifecycle(t *testing.T) {
	h := start(t)
	m := h.mark()
	h.newTunnel("web", nil)

	// Created in Cloudflare; not Ready until cloudflared has a ready replica.
	tun := h.waitTunnel("web", func(t *tunnelsv1alpha1.Tunnel) bool {
		return t.Status.ID != "" && hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonCreating)
	})
	id := tun.Status.ID
	if tun.Annotations[commonv1alpha1.AnnotationExternalID] != id {
		t.Errorf("external-id annotation %q, want %q", tun.Annotations[commonv1alpha1.AnnotationExternalID], id)
	}
	if got := testenv.Count(h.since(m), http.MethodPost, "/cfd_tunnel"); got != 1 {
		t.Errorf("POST /cfd_tunnel count %d, want 1", got)
	}
	if got := testenv.Count(h.since(m), http.MethodGet, "/token"); got != 0 {
		t.Errorf("token fetched with GET although the create response carries it (%d)", got)
	}
	if tun.Status.AtProvider.ConfigSrc != "cloudflare" || tun.Status.AtProvider.Name != "web" || tun.Status.AtProvider.Status != "inactive" {
		t.Errorf("atProvider %+v", tun.Status.AtProvider)
	}

	// Token Secret: base64 JSON {a: account, t: tunnel ID, s: secret} (fake tunnels.go token()).
	var sec corev1.Secret
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "web-cloudflared-token"}, &sec); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(sec.Data["token"]))
	if err != nil {
		t.Fatalf("token is not base64: %v", err)
	}
	var tok struct{ A, T, S string }
	if err := json.Unmarshal(raw, &tok); err != nil || tok.A != h.acct.AccountID || tok.T != id || tok.S == "" {
		t.Errorf("token %s (%v)", raw, err)
	}
	if !metav1.IsControlledBy(&sec, tun) || sec.Annotations["cloudflare.flare.dev/tunnel-id"] != id {
		t.Errorf("secret meta %+v", sec.ObjectMeta)
	}

	// cloudflared Deployment (spike §2.1).
	dep := h.deployment("web")
	if !metav1.IsControlledBy(dep, tun) || *dep.Spec.Replicas != 2 {
		t.Errorf("deployment owner/replicas: %v %d", dep.OwnerReferences, *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != tunnelsv1alpha1.DefaultCloudflaredImage ||
		!reflect.DeepEqual(c.Args, []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"}) {
		t.Errorf("container image/args %s %v", c.Image, c.Args)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "TUNNEL_TOKEN" || c.Env[0].ValueFrom.SecretKeyRef.Name != "web-cloudflared-token" || c.Env[0].ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("env %+v", c.Env)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/ready" || p.HTTPGet.Port.IntValue() != 2000 {
		t.Errorf("readiness probe %+v", p)
	}
	if dep.Spec.Template.Annotations["cloudflare.flare.dev/tunnel-id"] != id {
		t.Errorf("pod template annotations %v", dep.Spec.Template.Annotations)
	}

	// Ownership tag.
	if got := h.ownerTag(id); got != "testenv/"+h.ns+"/web" {
		t.Errorf("owner tag %q", got)
	}

	// Ready once a replica is ready.
	h.setDeploymentStatus("web", 2, 2)
	tun = h.waitTunnel("web", tunnelReady)
	if tun.Status.Connector.ReadyReplicas != 2 || tun.Status.Connector.TokenSecretName != "web-cloudflared-token" || tun.Status.ObservedGeneration != tun.Generation {
		t.Errorf("status %+v", tun.Status)
	}

	// Idempotent: no Cloudflare writes and no rewrite of owned objects.
	dep = h.deployment("web")
	np := h.networkPolicy("web")
	h.assertNoWritesAfterReconcile([]client.Object{tun}, []string{"/cfd_tunnel/" + id})
	if d2 := h.deployment("web"); d2.ResourceVersion != dep.ResourceVersion {
		t.Errorf("Deployment rewritten by a no-op reconcile")
	}
	if np2 := h.networkPolicy("web"); np2.ResourceVersion != np.ResourceVersion {
		t.Errorf("NetworkPolicy rewritten by a no-op reconcile")
	}

	// Connector update: Kubernetes only, no Cloudflare writes.
	m = h.mark()
	h.updateTunnel("web", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Spec.Connector.Replicas = i32(3)
		tun.Spec.Connector.Image = "example.com/cloudflared:test"
	})
	testenv.Eventually(t, 30e9, func() (bool, string) {
		d := h.deployment("web")
		return *d.Spec.Replicas == 3 && d.Spec.Template.Spec.Containers[0].Image == "example.com/cloudflared:test", "deployment not updated"
	})
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Errorf("connector update wrote to Cloudflare:\n%s", testenv.Summary(w))
	}

	// Rename is not supported: Synced=False, Immutable.
	h.updateTunnel("web", func(tun *tunnelsv1alpha1.Tunnel) { tun.Spec.ForProvider.Name = "web-renamed" })
	h.waitTunnel("web", func(t *tunnelsv1alpha1.Tunnel) bool {
		return hasCond(t.Status.Conditions, t.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonImmutable)
	})
	h.updateTunnel("web", func(tun *tunnelsv1alpha1.Tunnel) { tun.Spec.ForProvider.Name = "" })
	h.setDeploymentStatus("web", 3, 3)
	tun = h.waitTunnel("web", tunnelReady)

	// Delete while connected: scale to 0, wait for pods, wait for connections, then delete.
	if !h.e.Fake.ConnectTunnel(h.acct.AccountID, id, 3, 4) {
		t.Fatal("ConnectTunnel")
	}
	m = h.mark()
	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30e9, func() (bool, string) {
		d := h.deployment("web")
		return *d.Spec.Replicas == 0, "not scaled to zero"
	})
	h.waitTunnel("web", func(t *tunnelsv1alpha1.Tunnel) bool {
		return hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonDeleting)
	})
	h.setDeploymentStatus("web", 0, 0)
	h.waitTunnel("web", func(t *tunnelsv1alpha1.Tunnel) bool {
		c := reconcileCond(t)
		return c != nil && strings.Contains(c.Message, "connections to drain")
	})
	if n := testenv.Count(h.since(m), http.MethodDelete, "/cfd_tunnel/"); n != 0 {
		t.Fatalf("tunnel deleted while connected (%d DELETEs)", n)
	}
	h.e.Fake.DisconnectTunnel(h.acct.AccountID, id)
	h.waitGone(&tunnelsv1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "web"}})
	if got := h.cfTunnel(id); got.DeletedAt == nil {
		t.Errorf("tunnel not soft-deleted: %+v", got)
	}
	if n := testenv.Count(h.since(m), http.MethodDelete, "/cfd_tunnel/"); n != 1 {
		t.Errorf("DELETE count %d, want 1:\n%s", n, testenv.Summary(h.since(m)))
	}
}

func reconcileCond(t *tunnelsv1alpha1.Tunnel) *metav1.Condition {
	for i := range t.Status.Conditions {
		if t.Status.Conditions[i].Type == "Ready" {
			return &t.Status.Conditions[i]
		}
	}
	return nil
}

func TestTunnelAdopt(t *testing.T) {
	h := start(t)

	// By external-id annotation: no create; the token is fetched with GET …/token.
	id, token := h.apiCreateTunnel("pre-existing")
	m := h.mark()
	h.newTunnel("pinned", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Spec.ForProvider.Name = "pre-existing"
		tun.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: id}
	})
	h.setDeploymentStatus("pinned", 2, 1)
	tun := h.waitTunnel("pinned", tunnelReady)
	if tun.Status.ID != id {
		t.Errorf("status.id %q, want %q", tun.Status.ID, id)
	}
	j := h.since(m)
	if testenv.Count(j, http.MethodPost, "/cfd_tunnel") != 0 || testenv.Count(j, http.MethodGet, "/cfd_tunnel/"+id+"/token") != 1 {
		t.Errorf("adopt by id:\n%s", testenv.Summary(j))
	}
	var sec corev1.Secret
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "pinned-cloudflared-token"}, &sec); err != nil || string(sec.Data["token"]) != token {
		t.Errorf("token secret %v %v", sec.Data, err)
	}

	// By name.
	id2, _ := h.apiCreateTunnel("by-name")
	m = h.mark()
	h.newTunnel("by-name", nil)
	tun = h.waitTunnel("by-name", func(t *tunnelsv1alpha1.Tunnel) bool { return t.Status.ID != "" })
	if tun.Status.ID != id2 || tun.Annotations[commonv1alpha1.AnnotationExternalID] != id2 {
		t.Errorf("adopt by name: id %q annotation %q, want %q", tun.Status.ID, tun.Annotations[commonv1alpha1.AnnotationExternalID], id2)
	}
	if n := testenv.Count(h.since(m), http.MethodPost, "/cfd_tunnel"); n != 0 {
		t.Errorf("adopt by name created a tunnel")
	}

	// A pinned ID that does not exist is reported, never recreated.
	m = h.mark()
	h.newTunnel("missing", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "3f7b5a52-0000-4000-8000-000000000000"}
	})
	h.waitTunnel("missing", func(t *tunnelsv1alpha1.Tunnel) bool {
		return hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonNotFound)
	})
	if n := testenv.Count(h.since(m), http.MethodPost, "/cfd_tunnel"); n != 0 {
		t.Errorf("missing pinned tunnel was recreated")
	}
}

func TestTunnelObserveOnly(t *testing.T) {
	h := start(t)
	id, _ := h.apiCreateTunnel("observed")
	m := h.mark()
	h.newTunnel("observed", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
		tun.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete
		tun.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: id}
	})
	tun := h.waitTunnel("observed", func(t *tunnelsv1alpha1.Tunnel) bool {
		return tunnelReady(t) && hasCond(t.Status.Conditions, t.Generation, "Synced", metav1.ConditionTrue, commonv1alpha1.ReasonObserveOnly)
	})
	if tun.Status.AtProvider.Name != "observed" || tun.Status.ID != id {
		t.Errorf("atProvider %+v", tun.Status.AtProvider)
	}
	var dep appsv1.Deployment
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "observed-cloudflared"}, &dep); err == nil {
		t.Error("observe-only Tunnel created a connector Deployment")
	}
	h.assertNoWritesAfterReconcile([]client.Object{tun}, []string{"/cfd_tunnel/" + id})
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("observe-only wrote:\n%s", testenv.Summary(w))
	}
	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	h.waitGone(tun)
	if got := h.cfTunnel(id); got.DeletedAt != nil {
		t.Error("observe-only Tunnel deleted the Cloudflare tunnel")
	}
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("observe-only delete wrote:\n%s", testenv.Summary(w))
	}
}

func TestTunnelOrphan(t *testing.T) {
	h := start(t)
	h.newTunnel("orphan", func(tun *tunnelsv1alpha1.Tunnel) { tun.Spec.DeletionPolicy = commonv1alpha1.DeletionOrphan })
	h.setDeploymentStatus("orphan", 2, 2)
	tun := h.waitTunnel("orphan", tunnelReady)
	id := tun.Status.ID
	m := h.mark()
	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	h.waitGone(tun)
	if got := h.cfTunnel(id); got.DeletedAt != nil {
		t.Error("orphaned tunnel was deleted")
	}
	if n := testenv.Count(h.since(m), http.MethodDelete, "/cfd_tunnel"); n != 0 {
		t.Errorf("DELETE sent for an orphaned tunnel")
	}
	// The ownership tag is released.
	if got := h.ownerTag(id); got != "" {
		t.Errorf("owner tag kept on an orphaned tunnel: %q", got)
	}
}

func TestTunnelNameValidation(t *testing.T) {
	h := start(t)
	tun := &tunnelsv1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: strings.Repeat("a", 64)},
		Spec:       tunnelsv1alpha1.TunnelSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}}},
	}
	if err := h.e.Client.Create(h.ctx(), tun); err == nil || !strings.Contains(err.Error(), "63") {
		t.Errorf("64-character name accepted: %v", err)
	}
}
