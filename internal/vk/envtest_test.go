package vk

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/testenv"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, &env, testenv.Options{AddToScheme: []func(*runtime.Scheme) error{workersv1alpha1.AddToScheme}})
}

// hasTaint reports whether the node carries the virtual node's taint (the API server's
// TaintNodesByCondition admission adds node.kubernetes.io/not-ready besides it).
func hasTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t == Taint() {
			return true
		}
	}
	return false
}

// harness is one running virtual kubelet on its own node name and port.
type harness struct {
	e        *testenv.Env
	cs       kubernetes.Interface
	cache    cache.Cache
	vk       *VirtualKubelet
	cfg      Config
	owner    *rbacv1.ClusterRole
	url      string
	streamer *fakeStreamer
}

type harnessOptions struct {
	resolver func(h *harness) PodResolver
}

func newCache(t *testing.T, e *testenv.Env, ctx context.Context, nodeName string) cache.Cache {
	t.Helper()
	c, err := cache.New(e.Config, cache.Options{Scheme: e.Scheme, ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}: {Field: fields.OneTermEqualSelector("spec.nodeName", nodeName)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
	return c
}

func startVK(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	e := testenv.Require(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	nodeName := "vk-" + strings.ToLower(testenv.RandomHex(4))
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: nodeName + "-owner"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{e: e, cs: cs, owner: owner, streamer: &fakeStreamer{lines: []string{"[log] hello", "[fetch] GET https://api.example.com/ -> 200 ok"}}}
	h.cache = newCache(t, e, ctx, nodeName)
	h.cfg = Config{NodeName: nodeName, Address: "127.0.0.1", ListenAddr: l.Addr().String(), OwnerClusterRole: owner.Name,
		TLS: TLSConfig{Mode: TLSModeSelfSigned}}
	var resolver PodResolver = fakeResolver{targets: map[string]workerlogs.Target{"ns/api-worker/worker": apiTarget}}
	if o.resolver != nil {
		resolver = o.resolver(h)
	}
	h.vk, err = New(h.cfg, Deps{
		Clientset: cs, Cache: h.cache, StatusMapper: fakeMapper{}, Resolver: resolver, Streamer: h.streamer,
		Cert: NewSelfSignedCert(nodeName, "127.0.0.1"), Listener: l, Log: testr.New(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = h.vk.cfg
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = h.vk.Start(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("virtual kubelet exited with %v", runErr)
			}
		case <-time.After(30 * time.Second):
			t.Error("virtual kubelet did not stop within 30s")
		}
	})
	select {
	case <-h.vk.Ready():
	case <-done:
		t.Fatalf("virtual kubelet exited before ready: %v", runErr)
	case <-time.After(60 * time.Second):
		t.Fatal("virtual kubelet not ready within 60s")
	}
	h.url = "https://" + l.Addr().String()
	return h
}

func ctxT(t *testing.T) context.Context { return testenv.Context(t, 2*time.Minute) }

// B2: the Node is registered with the taint, labels, address, kubelet port and the owner
// reference to the ClusterRole, turns Ready, and heartbeats through its Lease.
func TestNodeRegistrationAndLease(t *testing.T) {
	h := startVK(t, harnessOptions{})
	ctx := ctxT(t)
	var n *corev1.Node
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var err error
		n, err = h.cs.CoreV1().Nodes().Get(ctx, h.cfg.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}
		r := readyCondition(n)
		return r.Status == corev1.ConditionTrue, "Ready " + string(r.Status) + " " + r.Reason
	})
	if !hasTaint(n) {
		t.Errorf("taints %v", n.Spec.Taints)
	}
	if n.Labels[LabelNodeType] != LabelNodeTypeValue || n.Labels[LabelNodeRole] != LabelNodeRoleValue || n.Labels[corev1.LabelOSStable] != "" {
		t.Errorf("labels %v", n.Labels)
	}
	if len(n.OwnerReferences) != 1 || n.OwnerReferences[0].UID != h.owner.UID || n.OwnerReferences[0].Kind != "ClusterRole" {
		t.Errorf("owner references %v", n.OwnerReferences)
	}
	port, _ := h.cfg.ListenPort()
	if n.Status.DaemonEndpoints.KubeletEndpoint.Port != port {
		t.Errorf("kubelet port %d, want %d", n.Status.DaemonEndpoints.KubeletEndpoint.Port, port)
	}
	var internal string
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			internal = a.Address
		}
	}
	if internal != "127.0.0.1" {
		t.Errorf("addresses %v", n.Status.Addresses)
	}
	var lease *coordinationv1.Lease
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var err error
		lease, err = h.cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, h.cfg.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}
		return lease.Spec.RenewTime != nil, "no renew time"
	})
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != h.cfg.NodeName || len(lease.OwnerReferences) != 1 || lease.OwnerReferences[0].UID != n.UID {
		t.Errorf("lease holder %v owners %v", lease.Spec.HolderIdentity, lease.OwnerReferences)
	}
}

// B2: PrepareNode refuses a Node it does not own, replaces one left by an earlier release of the
// same name, and restores its own Node's taint and labels.
func TestPrepareNode(t *testing.T) {
	e := testenv.Require(t, env)
	ctx := ctxT(t)
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	name := "vk-" + strings.ToLower(testenv.RandomHex(4))
	cr, err := cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name + "-owner"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{NodeName: name, Address: "127.0.0.1", OwnerClusterRole: cr.Name}.WithDefaults()
	mkNode := func(owners ...metav1.OwnerReference) {
		t.Helper()
		_ = cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		if err := waitNodeGone(ctx, cs, name, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// A real (or hand-made) node, and another release's node: refused, untouched.
	mkNode()
	if _, err := PrepareNode(ctx, cs, cfg); !errors.Is(err, ErrForeignNode) {
		t.Errorf("unowned node: %v, want ErrForeignNode", err)
	}
	mkNode(metav1.OwnerReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "other-release-workers-vk", UID: "x"})
	if _, err := PrepareNode(ctx, cs, cfg); !errors.Is(err, ErrForeignNode) {
		t.Errorf("another release's node: %v, want ErrForeignNode", err)
	}
	if _, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("foreign node touched: %v", err)
	}

	// Left by an earlier ClusterRole of the same name: deleted, so the NodeController creates it.
	mkNode(metav1.OwnerReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: cr.Name, UID: "earlier-install"})
	if _, err := PrepareNode(ctx, cs, cfg); err != nil {
		t.Fatalf("stale node: %v", err)
	}
	if _, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("stale node not deleted: %v", err)
	}

	// Ours, with the taint and a label removed by hand: restored.
	mkNode(OwnerReference(cr))
	if _, err := PrepareNode(ctx, cs, cfg); err != nil {
		t.Fatal(err)
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTaint(n) || n.Labels[LabelNodeType] != LabelNodeTypeValue {
		t.Errorf("own node not restored: taints %v labels %v", n.Spec.Taints, n.Labels)
	}
}

func createNamespace(t *testing.T, h *harness) string {
	t.Helper()
	ns := h.e.Namespace(t)
	// envtest runs no controller-manager: the ServiceAccount admission plugin needs "default".
	if _, err := h.cs.CoreV1().ServiceAccounts(ns).Create(ctxT(t), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func createWorkerScript(t *testing.T, h *harness, ns, name, account string, ready bool) *workersv1alpha1.WorkerScript {
	t.Helper()
	ctx := ctxT(t)
	ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	ws.Spec.AccountRef.Name = account
	// Observe-only (no forProvider needed); stand-in Pods serve those too.
	ws.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	if err := h.e.Client.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	setReady(t, h, ws, ready)
	return ws
}

func setReady(t *testing.T, h *harness, ws *workersv1alpha1.WorkerScript, ready bool) {
	t.Helper()
	st := metav1.ConditionFalse
	if ready {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{Type: commonv1alpha1.ConditionReady, Status: st, Reason: "Test"})
	if err := h.e.Client.Status().Update(ctxT(t), ws); err != nil {
		t.Fatal(err)
	}
}

func createPod(t *testing.T, h *harness, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	pod = pod.DeepCopy()
	pod.UID = ""
	created, err := h.cs.CoreV1().Pods(pod.Namespace).Create(ctxT(t), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func waitPodPhase(t *testing.T, h *harness, ns, name string, phase corev1.PodPhase, reason string) *corev1.Pod {
	t.Helper()
	var p *corev1.Pod
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		var err error
		p, err = h.cs.CoreV1().Pods(ns).Get(ctxT(t), name, metav1.GetOptions{})
		if err != nil {
			return false, err.Error()
		}
		return p.Status.Phase == phase && p.Status.Reason == reason, "phase " + string(p.Status.Phase) + " reason " + p.Status.Reason
	})
	return p
}

// B3: the PodController writes the StatusMapper's status, follows WorkerScript changes, fails a
// foreign Pod, and a stand-in Pod (spec grace 0) is deleted at once.
func TestPodStatusFlow(t *testing.T) {
	h := startVK(t, harnessOptions{})
	ctx := ctxT(t)
	ns := createNamespace(t, h)
	ws := createWorkerScript(t, h, ns, "api", "acct", false)
	pod := createPod(t, h, standInPod(ws, h.cfg.NodeName))

	waitPodPhase(t, h, ns, pod.Name, corev1.PodPending, "")
	setReady(t, h, ws, true)
	p := waitPodPhase(t, h, ns, pod.Name, corev1.PodRunning, "")
	if len(p.Status.ContainerStatuses) != 1 || !p.Status.ContainerStatuses[0].Ready {
		t.Errorf("container statuses %+v", p.Status.ContainerStatuses)
	}

	foreign := createPod(t, h, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "daemon-x"},
		Spec:       corev1.PodSpec{NodeName: h.cfg.NodeName, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	})
	waitPodPhase(t, h, ns, foreign.Name, corev1.PodFailed, standin.ForeignReason)

	if err := h.cs.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("stand-in pod with terminationGracePeriodSeconds 0 not deleted at once: %v", err)
	}
}

// httpsClient trusts any server certificate (the harness serves a self-signed one).
func httpsClient(certs ...tls.Certificate) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test server with a self-signed certificate
		Certificates:       certs,
	}}}
}

func get(t *testing.T, c *http.Client, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func serviceAccountToken(t *testing.T, h *harness, ns, name string) string {
	t.Helper()
	ctx := ctxT(t)
	if _, err := h.cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tr, err := h.cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To[int64](600)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tr.Status.Token
}

// unknownCAClientCert is a client certificate for "system:masters" signed by a CA the cluster
// does not know.
func unknownCAClientCert(t *testing.T) tls.Certificate {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "rogue-ca"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "admin", Organization: []string{"system:masters"}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// B1: the kubelet API authenticates with the cluster's client CA or a TokenReview and
// authorizes nodes/proxy on the node with a SubjectAccessReview.
func TestKubeletAPIAuth(t *testing.T) {
	h := startVK(t, harnessOptions{})
	ctx := ctxT(t)
	logsURL := h.url + "/containerLogs/ns/api-worker/worker"

	if code, body := get(t, httpsClient(), logsURL, ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d %q, want 401", code, body)
	}
	if code, body := get(t, httpsClient(unknownCAClientCert(t)), logsURL, ""); code != http.StatusUnauthorized {
		t.Errorf("client certificate from an unknown CA: %d %q, want 401", code, body)
	}
	if code, body := get(t, httpsClient(), logsURL, "not-a-token"); code != http.StatusUnauthorized {
		t.Errorf("invalid token: %d %q, want 401", code, body)
	}

	ns := createNamespace(t, h)
	denied := serviceAccountToken(t, h, ns, "viewer")
	if code, body := get(t, httpsClient(), logsURL, denied); code != http.StatusForbidden {
		t.Errorf("token without nodes/proxy: %d %q, want 403", code, body)
	}

	allowed := serviceAccountToken(t, h, ns, "kubelet-api")
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.NodeName + "-proxy"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, ResourceNames: []string{h.cfg.NodeName}, Verbs: []string{"get", "create"},
	}}}
	if _, err := h.cs.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: "kubelet-api"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		code, body := get(t, httpsClient(), logsURL, allowed)
		return code == http.StatusOK && strings.Contains(body, "[log] hello"), body
	})
	// Every other path needs nodes/proxy as well.
	if code, _ := get(t, httpsClient(), h.url+"/pods", denied); code != http.StatusForbidden {
		t.Errorf("/pods without nodes/proxy: %d", code)
	}

	// kube-apiserver's way in: a client certificate from the cluster's client CA (envtest's
	// admin user, in system:masters).
	if len(h.e.Config.CertData) == 0 {
		t.Skip("envtest admin has no client certificate")
	}
	admin, err := tls.X509KeyPair(h.e.Config.CertData, h.e.Config.KeyData)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, httpsClient(admin), logsURL+"?tailLines=1", ""); code != http.StatusOK || !strings.Contains(body, "hello") {
		t.Errorf("cluster client certificate: %d %q, want 200", code, body)
	}
	if _, o := h.streamer.last(); o.TailLines == nil || *o.TailLines != 1 {
		t.Errorf("tailLines not passed: %v", describe(o))
	}
}

// B4: /containerLogs end to end through authentication, the real resolver (Pod → WorkerScript →
// CloudflareAccount → token) and a fake log source; exec answers 501 with the message.
func TestContainerLogsEndToEnd(t *testing.T) {
	e := testenv.Require(t, env)
	var accounts *reconcile.Accounts
	h := startVK(t, harnessOptions{resolver: func(h *harness) PodResolver {
		reader, err := client.New(e.Config, client.Options{Scheme: e.Scheme, Cache: &client.CacheOptions{
			Reader: h.cache, DisableFor: []client.Object{&corev1.Secret{}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		accounts = reconcile.NewAccounts(ReadOnly(reader), reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true}),
			reconcile.WithClientFactory(BudgetClientFactory(DefaultAPIBudget, nil)))
		return NewResolver(reader, accounts, h.cfg.NodeName)
	}})
	ctx := ctxT(t)
	ns := createNamespace(t, h)
	e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{ns}})
	acct := e.CreateReadyAccount(t, ns, "acct")
	ws := createWorkerScript(t, h, ns, "api", "acct", true)
	pod := createPod(t, h, standInPod(ws, h.cfg.NodeName))
	waitPodPhase(t, h, ns, pod.Name, corev1.PodRunning, "")

	admin, err := tls.X509KeyPair(e.Config.CertData, e.Config.KeyData)
	if err != nil {
		t.Skipf("envtest admin has no client certificate: %v", err)
	}
	c := httpsClient(admin)
	code, body := get(t, c, h.url+"/containerLogs/"+ns+"/"+pod.Name+"/worker?timestamps=true", "")
	if code != http.StatusOK || body != "[log] hello\n[fetch] GET https://api.example.com/ -> 200 ok\n" {
		t.Fatalf("logs: %d %q", code, body)
	}
	target, opts := h.streamer.last()
	if target.AccountID != acct.AccountID || target.Script != "api" || target.WorkerScript.Name != "api" || target.Client == nil ||
		!target.AllowInsecureTail || !opts.Timestamps {
		t.Errorf("target %+v options %v", target, describe(opts))
	}
	// The resolver never writes: the WorkerScript has no account label from it.
	var cur workersv1alpha1.WorkerScript
	if err := e.Client.Get(ctx, client.ObjectKeyFromObject(ws), &cur); err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Labels[reconcile.AccountLabel]; ok {
		t.Errorf("the virtual kubelet labelled the WorkerScript: %v", cur.Labels)
	}

	for path, want := range map[string]int{
		"/containerLogs/" + ns + "/" + pod.Name + "/sidecar":              http.StatusNotFound,
		"/containerLogs/" + ns + "/nope/worker":                           http.StatusNotFound,
		"/containerLogs/" + ns + "/" + pod.Name + "/worker?previous=true": http.StatusBadRequest,
	} {
		if code, body := get(t, c, h.url+path, ""); code != want {
			t.Errorf("%s: %d %q, want %d", path, code, body, want)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, h.url+"/exec/"+ns+"/"+pod.Name+"/worker?command=sh&stdin=true&stdout=true", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(b), "only `kubectl logs` is available") {
		t.Errorf("exec: %d %q", resp.StatusCode, b)
	}

	// A Pod of another WorkerScript's UID in the namespace is not served.
	impostor := standInPod(ws, h.cfg.NodeName)
	impostor.Name = "impostor"
	impostor.Labels[workersv1alpha1.StandInLabelWorkerScriptUID] = "00000000-0000-0000-0000-000000000000"
	createPod(t, h, impostor)
	testenv.Eventually(t, 10*time.Second, func() (bool, string) {
		code, body := get(t, c, h.url+"/containerLogs/"+ns+"/impostor/worker", "")
		return code == http.StatusNotFound, body
	})
}

// testSigner signs approved CSRs of node with its CA, as kube-controller-manager would (envtest
// has none).
type testSigner struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate

	mu     sync.Mutex
	signed []certificatesv1.CertificateSigningRequest // as they were when signed
}

func (s *testSigner) signedCSRs() []certificatesv1.CertificateSigningRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]certificatesv1.CertificateSigningRequest(nil), s.signed...)
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-kubelet-ca"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	return &testSigner{key: key, cert: cert}
}

func (s *testSigner) sign(t *testing.T, csr *certificatesv1.CertificateSigningRequest) []byte {
	t.Helper()
	block, _ := pem.Decode(csr.Spec.Request)
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	life := time.Duration(*csr.Spec.ExpirationSeconds) * time.Second
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: req.Subject, DNSNames: req.DNSNames, IPAddresses: req.IPAddresses,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(life)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.cert, req.PublicKey, s.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// run signs every approved, unsigned CSR labelled for node until ctx ends.
func (s *testSigner) run(ctx context.Context, t *testing.T, cs kubernetes.Interface, node string) {
	for ctx.Err() == nil {
		list, err := cs.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{LabelSelector: CSRLabelNode + "=" + node})
		if err == nil {
			for i := range list.Items {
				csr := &list.Items[i]
				if !hasCondition(csr, certificatesv1.CertificateApproved) || len(csr.Status.Certificate) > 0 {
					continue
				}
				s.mu.Lock()
				s.signed = append(s.signed, *csr.DeepCopy())
				s.mu.Unlock()
				csr.Status.Certificate = s.sign(t, csr)
				_, _ = cs.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, csr, metav1.UpdateOptions{})
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TLS mode csr: the virtual kubelet requests a kubelet-serving certificate, approves only its
// own CSR, installs the signed certificate, and renews it when the address changes.
func TestCSRServingCert(t *testing.T) {
	e := testenv.Require(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	node := "vk-" + strings.ToLower(testenv.RandomHex(4))
	csrs := cs.CertificatesV1().CertificateSigningRequests()

	// A CSR for the same node that the virtual kubelet did not create (same requester here: the
	// test runs as one user), labelled like ours so the test signer would sign it if approved.
	foreignKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	foreignDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "system:node:" + node, Organization: []string{"system:nodes"}},
		DNSNames: []string{node}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}, foreignKey)
	foreign, err := csrs.Create(ctx, &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "foreign-", Labels: map[string]string{CSRLabelNode: node}},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: foreignDER}),
			SignerName: certificatesv1.KubeletServingSignerName,
			Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageServerAuth},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	signer := newTestSigner(t)
	go signer.run(ctx, t, cs, node)
	c := NewCSRCert(cs, node, "127.0.0.1", true, time.Hour, testr.New(t))
	c.PollInterval = 100 * time.Millisecond
	c.OwnerClusterRole = "rel-workers-vk"
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	select {
	case <-c.Ready():
	case err := <-runDone:
		t.Fatalf("Run returned %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("no certificate within 60s")
	}
	cert, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.Leaf.CheckSignatureFrom(signer.cert); err != nil {
		t.Errorf("served certificate not the signed one: %v", err)
	}
	if cert.Leaf.Subject.CommonName != "system:node:"+node || cert.Leaf.VerifyHostname("127.0.0.1") != nil {
		t.Errorf("leaf %v %v", cert.Leaf.Subject, cert.Leaf.IPAddresses)
	}

	signed := signer.signedCSRs()
	if len(signed) != 1 {
		t.Fatalf("%d CSRs signed, want 1", len(signed))
	}
	for _, csr := range signed {
		if csr.UID == foreign.UID {
			t.Errorf("the virtual kubelet approved a CSR it did not create: %+v", csr.Status.Conditions)
			continue
		}
		if csr.Spec.SignerName != certificatesv1.KubeletServingSignerName || csr.Spec.ExpirationSeconds == nil || *csr.Spec.ExpirationSeconds != 3600 ||
			csr.Spec.Username == "" || !hasCondition(&csr, certificatesv1.CertificateApproved) ||
			csr.Labels["app.kubernetes.io/managed-by"] != CSRManagedBy || csr.Labels[CSRLabelOwner] != "rel-workers-vk" {
			t.Errorf("our CSR %s: signer %s expiration %v username %q conditions %v labels %v", csr.Name, csr.Spec.SignerName,
				csr.Spec.ExpirationSeconds, csr.Spec.Username, csr.Status.Conditions, csr.Labels)
		}
		for _, cond := range csr.Status.Conditions {
			if cond.Type == certificatesv1.CertificateApproved && cond.Reason != CSRApprovalReason {
				t.Errorf("approval reason %q", cond.Reason)
			}
		}
	}
	// Once the certificate is installed our CSR is deleted; the foreign one stays, unapproved.
	testenv.Eventually(t, 10*time.Second, func() (bool, string) {
		list, err := csrs.List(ctx, metav1.ListOptions{LabelSelector: CSRLabelNode + "=" + node})
		if err != nil {
			return false, err.Error()
		}
		if len(list.Items) != 1 || list.Items[0].UID != foreign.UID {
			return false, fmt.Sprintf("%d CSRs left", len(list.Items))
		}
		return true, ""
	})

	// An address change renews the certificate for the new address.
	c.SetAddress("127.0.0.2")
	testenv.Eventually(t, 60*time.Second, func() (bool, string) {
		cur, err := c.GetCertificate(nil)
		if err != nil {
			return false, err.Error()
		}
		return cur.Leaf.VerifyHostname("127.0.0.2") == nil, "still " + cur.Leaf.IPAddresses[0].String()
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Errorf("Run: %v", err)
	}
	if cur, err := csrs.Get(context.Background(), foreign.Name, metav1.GetOptions{}); err != nil || hasCondition(cur, certificatesv1.CertificateApproved) {
		t.Errorf("foreign CSR approved (or gone): %v %v", err, cur.Status.Conditions)
	}
}

// approveOwn refuses a CSR whose UID or requester is not the one recorded at creation (deleted
// and recreated under the same name in between).
func TestCSRApproveOwnRefusesReplacedCSR(t *testing.T) {
	e := testenv.Require(t, env)
	ctx := ctxT(t)
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	node := "vk-" + strings.ToLower(testenv.RandomHex(4))
	c := NewCSRCert(cs, node, "127.0.0.1", true, time.Hour, testr.New(t))
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl, _ := c.expectedRequest("127.0.0.1")
	der, _ := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	reqPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	csr, err := cs.CertificatesV1().CertificateSigningRequests().Create(ctx, &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "replaced-"},
		Spec: certificatesv1.CertificateSigningRequestSpec{Request: reqPEM, SignerName: certificatesv1.KubeletServingSignerName,
			Usages: []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageServerAuth}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replaced := csr.DeepCopy()
	replaced.UID = "the-one-we-created"
	if err := c.approveOwn(ctx, replaced, reqPEM, key.Public(), "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("approveOwn of a replaced CSR: %v", err)
	}
	other := csr.DeepCopy()
	other.Spec.Username = "system:serviceaccount:flare:workers-vk"
	if err := c.approveOwn(ctx, other, reqPEM, key.Public(), "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "requester") {
		t.Errorf("approveOwn with another requester: %v", err)
	}
	cur, err := cs.CertificatesV1().CertificateSigningRequests().Get(ctx, csr.Name, metav1.GetOptions{})
	if err != nil || hasCondition(cur, certificatesv1.CertificateApproved) {
		t.Errorf("CSR approved: %v %v", err, cur.Status.Conditions)
	}
	// The genuine one is approved.
	if err := c.approveOwn(ctx, csr, reqPEM, key.Public(), "127.0.0.1"); err != nil {
		t.Errorf("approveOwn of our CSR: %v", err)
	}
}

// A CSR that is denied is deleted (only ours, by UID) before the next attempt.
func TestCSRDeniedIsDeleted(t *testing.T) {
	e := testenv.Require(t, env)
	ctx := ctxT(t)
	cs, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	node := "vk-" + strings.ToLower(testenv.RandomHex(4))
	c := NewCSRCert(cs, node, "127.0.0.1", false, time.Hour, testr.New(t))
	c.PollInterval = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := c.obtain(ctx, "127.0.0.1")
		done <- err
	}()
	csrs := cs.CertificatesV1().CertificateSigningRequests()
	sel := metav1.ListOptions{LabelSelector: CSRLabelNode + "=" + node}
	var ours *certificatesv1.CertificateSigningRequest
	testenv.Eventually(t, 10*time.Second, func() (bool, string) {
		list, err := csrs.List(ctx, sel)
		if err != nil || len(list.Items) != 1 {
			return false, fmt.Sprint("CSRs: ", err)
		}
		ours = &list.Items[0]
		return true, ""
	})
	ours.Status.Conditions = append(ours.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type: certificatesv1.CertificateDenied, Status: corev1.ConditionTrue, Reason: "Test"})
	if _, err := csrs.UpdateApproval(ctx, ours.Name, ours, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errCSRRefused) {
		t.Fatalf("obtain: %v, want refused", err)
	}
	if _, err := csrs.Get(ctx, ours.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("denied CSR not deleted: %v", err)
	}
}
