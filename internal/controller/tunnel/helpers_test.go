package tunnel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/tunnel"
	"flare.dev/operator/internal/controller/vpcservice"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

func TestMain(m *testing.M) { testenv.Main(m, &env, testenv.Options{}) }

// harness is one test's manager, namespace and Ready account.
type harness struct {
	t    *testing.T
	e    *testenv.Env
	ns   string
	acct *testenv.Account
	cf   cfclient.Client
}

func start(t *testing.T) *harness {
	t.Helper()
	e := testenv.Require(t, env)
	e.StartManager(t, testenv.ManagerOptions{Setup: []func(ctrl.Manager, controller.Deps) error{
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&tunnel.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				DrainInterval: 200 * time.Millisecond, Recorder: mgr.GetEventRecorder(tunnel.Name)}).SetupWithManager(mgr)
		},
		func(mgr ctrl.Manager, d controller.Deps) error {
			return (&vpcservice.Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts}).SetupWithManager(mgr)
		},
	}})
	h := &harness{t: t, e: e}
	h.ensureKubeDNS()
	h.ns = e.Namespace(t)
	h.acct = e.CreateReadyAccount(t, h.ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	return h
}

func (h *harness) ctx() context.Context { return testenv.Context(h.t, 20*time.Second) }

// ensureKubeDNS creates kube-system/kube-dns (envtest runs no DNS) and returns its ClusterIP.
func (h *harness) ensureKubeDNS() string {
	h.t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"k8s-app": "kube-dns"},
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)},
			},
		},
	}
	if err := h.e.Client.Create(h.ctx(), svc); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create kube-dns: %v", err)
	}
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(svc), svc); err != nil {
		h.t.Fatal(err)
	}
	return svc.Spec.ClusterIP
}

func (h *harness) service(name string, selector map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	h.t.Helper()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: name}, Spec: corev1.ServiceSpec{Selector: selector, Ports: ports}}
	if err := h.e.Client.Create(h.ctx(), svc); err != nil {
		h.t.Fatalf("create Service %s: %v", name, err)
	}
	return svc
}

func (h *harness) newTunnel(name string, mut func(*tunnelsv1alpha1.Tunnel)) *tunnelsv1alpha1.Tunnel {
	h.t.Helper()
	tun := &tunnelsv1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: name},
		Spec:       tunnelsv1alpha1.TunnelSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}}},
	}
	if mut != nil {
		mut(tun)
	}
	if err := h.e.Client.Create(h.ctx(), tun); err != nil {
		h.t.Fatalf("create Tunnel %s: %v", name, err)
	}
	return tun
}

func (h *harness) newVPC(name string, fp *workersvpcv1alpha1.VPCServiceParameters, mut func(*workersvpcv1alpha1.VPCService)) *workersvpcv1alpha1.VPCService {
	h.t.Helper()
	vs := &workersvpcv1alpha1.VPCService{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: name},
		Spec: workersvpcv1alpha1.VPCServiceSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}},
			ForProvider:  fp,
		},
	}
	if mut != nil {
		mut(vs)
	}
	if err := h.e.Client.Create(h.ctx(), vs); err != nil {
		h.t.Fatalf("create VPCService %s: %v", name, err)
	}
	return vs
}

func condOf(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return typ + "=<none>"
	}
	return fmt.Sprintf("%s=%s/%s %q (gen %d)", typ, c.Status, c.Reason, c.Message, c.ObservedGeneration)
}

func hasCond(conds []metav1.Condition, gen int64, typ string, st metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(conds, typ)
	return c != nil && c.Status == st && (reason == "" || c.Reason == reason) && c.ObservedGeneration == gen
}

// waitTunnel polls the Tunnel until ok returns true.
func (h *harness) waitTunnel(name string, ok func(*tunnelsv1alpha1.Tunnel) bool) *tunnelsv1alpha1.Tunnel {
	h.t.Helper()
	var tun tunnelsv1alpha1.Tunnel
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &tun); err != nil {
			return false, err.Error()
		}
		return ok(&tun), fmt.Sprintf("Tunnel %s: %s %s id=%q connector=%+v", name, condOf(tun.Status.Conditions, "Ready"),
			condOf(tun.Status.Conditions, "Synced"), tun.Status.ID, tun.Status.Connector)
	})
	return &tun
}

func (h *harness) waitVPC(name string, ok func(*workersvpcv1alpha1.VPCService) bool) *workersvpcv1alpha1.VPCService {
	h.t.Helper()
	var vs workersvpcv1alpha1.VPCService
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &vs); err != nil {
			return false, err.Error()
		}
		return ok(&vs), fmt.Sprintf("VPCService %s: %s %s id=%q", name, condOf(vs.Status.Conditions, "Ready"),
			condOf(vs.Status.Conditions, "Synced"), vs.Status.ID)
	})
	return &vs
}

func tunnelReady(t *tunnelsv1alpha1.Tunnel) bool {
	return hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionTrue, "") &&
		hasCond(t.Status.Conditions, t.Generation, "Synced", metav1.ConditionTrue, "")
}

func vpcReady(v *workersvpcv1alpha1.VPCService) bool {
	return hasCond(v.Status.Conditions, v.Generation, "Ready", metav1.ConditionTrue, "") &&
		hasCond(v.Status.Conditions, v.Generation, "Synced", metav1.ConditionTrue, "")
}

func (h *harness) waitGone(obj client.Object) {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(obj), obj)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		return false, fmt.Sprintf("%s still exists (err %v, finalizers %v)", obj.GetName(), err, obj.GetFinalizers())
	})
}

func (h *harness) deployment(tun string) *appsv1.Deployment {
	h.t.Helper()
	var dep appsv1.Deployment
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: tun + "-cloudflared"}, &dep)
		return err == nil, fmt.Sprint(err)
	})
	return &dep
}

// markDeploymentReady simulates the Deployment controller (envtest runs none).
func (h *harness) setDeploymentStatus(tun string, replicas, ready int32) {
	h.t.Helper()
	dep := h.deployment(tun)
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: replicas, ReadyReplicas: ready,
		AvailableReplicas: ready, UpdatedReplicas: replicas}
	if err := h.e.Client.Status().Update(h.ctx(), dep); err != nil {
		h.t.Fatalf("update Deployment status: %v", err)
	}
}

func (h *harness) networkPolicy(tun string) *networkingv1.NetworkPolicy {
	h.t.Helper()
	var np networkingv1.NetworkPolicy
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: tun + "-cloudflared"}, &np); err != nil {
		h.t.Fatalf("get NetworkPolicy: %v", err)
	}
	return &np
}

// mark returns the current journal length; since(mark) returns this account's entries after it.
func (h *harness) mark() int { return len(h.e.Journal(h.t)) }

func (h *harness) since(mark int) []fake.JournalEntry {
	j := h.e.Journal(h.t)
	if mark > len(j) {
		mark = len(j)
	}
	return testenv.ForAccount(j[mark:], h.acct.AccountID)
}

// poke changes an annotation so the object is reconciled again without a spec change.
func (h *harness) poke(obj client.Object) {
	h.t.Helper()
	base := obj.DeepCopyObject().(client.Object)
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a["test.flare.dev/poke"] = time.Now().Format(time.RFC3339Nano)
	obj.SetAnnotations(a)
	if err := h.e.Client.Patch(h.ctx(), obj, client.MergeFrom(base)); err != nil {
		h.t.Fatalf("poke %s: %v", obj.GetName(), err)
	}
}

// assertNoWritesAfterReconcile pokes objs, waits until each was re-read from Cloudflare (a GET
// whose path contains the matching readPaths entry), lets stragglers finish and asserts that
// no write reached the fake.
func (h *harness) assertNoWritesAfterReconcile(objs []client.Object, readPaths []string) {
	h.t.Helper()
	m := h.mark()
	for _, o := range objs {
		h.poke(o)
	}
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		j := h.since(m)
		for _, p := range readPaths {
			if testenv.Count(j, http.MethodGet, p) == 0 {
				return false, "no GET " + p + " yet:\n" + testenv.Summary(j)
			}
		}
		return true, ""
	})
	time.Sleep(time.Second)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		h.t.Fatalf("a reconcile without spec changes wrote to Cloudflare:\n%s", testenv.Summary(w))
	}
}

func (h *harness) apiGet(p string, out any) error {
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + p})
	if err != nil {
		return err
	}
	return json.Unmarshal(resp.Result, out)
}

// ownerTag returns the tunnel's flare.dev/owner tag ("" when untagged).
func (h *harness) ownerTag(id string) string {
	h.t.Helper()
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + h.acct.AccountID + "/tags",
		Query: url.Values{"resource_type": {"cloudflared_tunnel"}, "resource_id": {id}}})
	if err != nil {
		if ae, ok := cfclient.AsAPIError(err); ok && ae.Status == http.StatusInternalServerError {
			return "" // never tagged
		}
		h.t.Fatalf("GET tags: %v", err)
	}
	var out struct{ Tags map[string]string }
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.Tags["flare.dev/owner"]
}

func (h *harness) apiCreateTunnel(name string) (id, token string) {
	h.t.Helper()
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodPost, Path: "/accounts/" + h.acct.AccountID + "/cfd_tunnel",
		Body: map[string]string{"name": name, "config_src": "cloudflare"}})
	if err != nil {
		h.t.Fatal(err)
	}
	var out struct{ ID, Token string }
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.ID, out.Token
}

type cfTunnel struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	DeletedAt *string `json:"deleted_at"`
}

func (h *harness) cfTunnel(id string) cfTunnel {
	h.t.Helper()
	var out cfTunnel
	if err := h.apiGet("/cfd_tunnel/"+id, &out); err != nil {
		h.t.Fatalf("GET tunnel %s: %v", id, err)
	}
	return out
}

func (h *harness) updateTunnel(name string, mut func(*tunnelsv1alpha1.Tunnel)) {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var tun tunnelsv1alpha1.Tunnel
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &tun); err != nil {
			return err
		}
		mut(&tun)
		return h.e.Client.Update(h.ctx(), &tun)
	})
	if err != nil {
		h.t.Fatalf("update Tunnel %s: %v", name, err)
	}
}

func (h *harness) updateVPC(name string, mut func(*workersvpcv1alpha1.VPCService)) {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var vs workersvpcv1alpha1.VPCService
		if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: name}, &vs); err != nil {
			return err
		}
		mut(&vs)
		return h.e.Client.Update(h.ctx(), &vs)
	})
	if err != nil {
		h.t.Fatalf("update VPCService %s: %v", name, err)
	}
}

func str(s string) *string { return &s }
func i32(i int32) *int32   { return &i }

func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
