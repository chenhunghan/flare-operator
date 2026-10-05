package vk

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

func logrDiscard() logr.Logger { return logr.Discard() }

func toolscacheTombstone(key string, obj any) any {
	return toolscache.DeletedFinalStateUnknown{Key: key, Obj: obj}
}

func validConfig() Config {
	return Config{Address: "10.0.0.7", OwnerClusterRole: "rel-workers-vk"}.WithDefaults()
}

func TestConfigDefaultsAndValidate(t *testing.T) {
	c := validConfig()
	if c.NodeName != DefaultNodeName || c.ListenAddr != ":10250" || c.TLS.Mode != TLSModeCSR || c.TLS.Lifetime != DefaultCSRLifetime ||
		c.APIBudget != DefaultAPIBudget || c.Logs.MaxEvents == 0 || c.Logs.PageSize != 2000 {
		t.Errorf("defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	if h := (Config{AddressMode: AddressHostIP}).WithDefaults(); h.ListenAddr != ":10260" {
		t.Errorf("hostIP listen address %q", h.ListenAddr)
	}
	for name, mut := range map[string]func(*Config){
		"no address":       func(c *Config) { c.Address = "" },
		"hostname address": func(c *Config) { c.Address = "node-1" },
		"bad mode":         func(c *Config) { c.AddressMode = "nodeIP" },
		"bad node name":    func(c *Config) { c.NodeName = "CF_Workers" },
		"bad port":         func(c *Config) { c.ListenAddr = ":0" },
		"bad tls mode":     func(c *Config) { c.TLS.Mode = "acme" },
		"short lifetime":   func(c *Config) { c.TLS.Lifetime = time.Minute },
		"secret unnamed":   func(c *Config) { c.TLS.Mode = TLSModeSecret },
		"no owner":         func(c *Config) { c.OwnerClusterRole = "" },
		"bad selector":     func(c *Config) { c.NamespaceSelector = "a in (" },
		"negative budget":  func(c *Config) { c.APIBudget = -1 },
		"page too big":     func(c *Config) { c.Logs.PageSize = 5000 },
	} {
		c := validConfig()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDesiredNode(t *testing.T) {
	c := validConfig()
	owner := OwnerReference(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "rel-workers-vk", UID: "cr-uid"}})
	n, err := DesiredNode(c, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "cf-workers" || len(n.Spec.Taints) != 1 || n.Spec.Taints[0] != Taint() {
		t.Errorf("name/taints: %s %v", n.Name, n.Spec.Taints)
	}
	if n.Labels[LabelNodeType] != LabelNodeTypeValue || n.Labels[LabelNodeRole] != LabelNodeRoleValue {
		t.Errorf("labels %v", n.Labels)
	}
	for _, k := range []string{corev1.LabelOSStable, corev1.LabelArchStable} {
		if _, ok := n.Labels[k]; ok {
			t.Errorf("label %s set: DaemonSets selecting it would target the node", k)
		}
	}
	if len(n.OwnerReferences) != 1 || n.OwnerReferences[0].Kind != "ClusterRole" || n.OwnerReferences[0].UID != "cr-uid" {
		t.Errorf("owner %v", n.OwnerReferences)
	}
	if n.Status.DaemonEndpoints.KubeletEndpoint.Port != 10250 ||
		n.Status.Addresses[0] != (corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.7"}) {
		t.Errorf("endpoint/addresses %v %v", n.Status.DaemonEndpoints, n.Status.Addresses)
	}
	if n.Status.Capacity.Pods().Value() != 10000 {
		t.Errorf("pods capacity %v", n.Status.Capacity.Pods())
	}
	if r := readyCondition(n); r.Status != corev1.ConditionFalse || r.Reason != NodeCertPendingReason {
		t.Errorf("not-ready condition %+v", r)
	}
	setNodeConditions(n, true, metav1.Now())
	if r := readyCondition(n); r.Status != corev1.ConditionTrue || r.Reason != NodeReadyReason {
		t.Errorf("ready condition %+v", r)
	}
}

func readyCondition(n *corev1.Node) corev1.NodeCondition {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c
		}
	}
	return corev1.NodeCondition{}
}

func TestClassifyNode(t *testing.T) {
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "rel-workers-vk", UID: "now"}}
	ref := func(name, uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: name, UID: types.UID(uid)}
	}
	for _, tc := range []struct {
		owners []metav1.OwnerReference
		want   nodeOwnership
	}{
		{[]metav1.OwnerReference{ref("rel-workers-vk", "now")}, nodeOurs},
		{[]metav1.OwnerReference{ref("rel-workers-vk", "before")}, nodeStale},
		{[]metav1.OwnerReference{ref("other-workers-vk", "x")}, nodeForeign},
		{nil, nodeForeign},
		{[]metav1.OwnerReference{ref("rel-workers-vk", "before"), ref("other", "y")}, nodeForeign},
	} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{OwnerReferences: tc.owners}}
		if got := classifyNode(n, cr); got != tc.want {
			t.Errorf("owners %v: %v, want %v", tc.owners, got, tc.want)
		}
	}
}

func TestBudgetClientFactory(t *testing.T) {
	var got cfclient.Options
	f := BudgetClientFactory(120, func(o cfclient.Options) (cfclient.Client, error) { got = o; return nil, nil })
	if _, err := f(cfclient.Options{Token: "t", RPS: 3.6, Burst: 50, MaxRetries: 2}); err != nil {
		t.Fatal(err)
	}
	if got.RPS != 0.4 || got.Burst != 20 || got.MaxRetries != 2 || got.Token != "t" {
		t.Errorf("options %+v: want 120/300 rps, burst 20, retries kept", got)
	}
	_, _ = BudgetClientFactory(5, func(o cfclient.Options) (cfclient.Client, error) { got = o; return nil, nil })(cfclient.Options{})
	if got.Burst != 5 {
		t.Errorf("burst %d above a budget of 5", got.Burst)
	}
}

func TestCSRCheckRequest(t *testing.T) {
	c := NewCSRCert(nil, "cf-workers", "10.0.0.7", true, time.Hour, logr.Discard())
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mk := func(cn string, org []string, dns []string, ip string) []byte {
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: cn, Organization: org}, DNSNames: dns, IPAddresses: []net.IP{net.ParseIP(ip)},
		}, key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	}
	good := mk("system:node:cf-workers", []string{"system:nodes"}, []string{"cf-workers"}, "10.0.0.7")
	if err := c.checkRequest(good, key.Public(), "10.0.0.7"); err != nil {
		t.Errorf("our request refused: %v", err)
	}
	for name, tc := range map[string]struct {
		req  []byte
		pub  any
		addr string
	}{
		"other node":    {mk("system:node:worker-1", []string{"system:nodes"}, []string{"cf-workers"}, "10.0.0.7"), key.Public(), "10.0.0.7"},
		"extra org":     {mk("system:node:cf-workers", []string{"system:nodes", "system:masters"}, []string{"cf-workers"}, "10.0.0.7"), key.Public(), "10.0.0.7"},
		"other SAN":     {mk("system:node:cf-workers", []string{"system:nodes"}, []string{"kubernetes.default"}, "10.0.0.7"), key.Public(), "10.0.0.7"},
		"other address": {good, key.Public(), "10.0.0.8"},
		"other key":     {good, other.Public(), "10.0.0.7"},
		"not PEM":       {[]byte("x"), key.Public(), "10.0.0.7"},
	} {
		if err := c.checkRequest(tc.req, tc.pub, tc.addr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKeyPairAndRenewal(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now, NotAfter: now.Add(10 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	p := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	c, err := keyPair(p, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := renewAt(c.Leaf); !got.Equal(now.Add(8*time.Hour).Truncate(time.Second)) && got.Sub(now.Add(8*time.Hour)).Abs() > time.Second {
		t.Errorf("renewAt %v, want 80%% of the lifetime", got)
	}
	if _, err := keyPair(p, other); err == nil || !strings.Contains(err.Error(), "not for our key") {
		t.Errorf("a certificate for another key was accepted: %v", err)
	}
	if _, err := keyPair([]byte("junk"), key); err == nil {
		t.Error("junk accepted")
	}
}

func TestSelfSigned(t *testing.T) {
	c, err := selfSigned("cf-workers", "10.0.0.7", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Leaf.VerifyHostname("10.0.0.7"); err != nil {
		t.Error(err)
	}
	if err := c.Leaf.VerifyHostname("cf-workers"); err != nil {
		t.Error(err)
	}
}
