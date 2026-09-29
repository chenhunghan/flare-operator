package tunnel_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/testenv"
)

func hostnameParams(host string, port int32, tunnel string) *workersvpcv1alpha1.VPCServiceParameters {
	return &workersvpcv1alpha1.VPCServiceParameters{
		Type:      "http",
		Host:      workersvpcv1alpha1.VPCServiceHost{Hostname: str(host)},
		HTTPPort:  i32(port),
		TunnelRef: &commonv1alpha1.LocalRef{Name: tunnel},
	}
}

// TestVPCServiceAndDeleteOrder: create (tunnel_id from tunnelRef, resolver_ips defaulted to
// kube-dns), idempotency, update (full PUT), the NetworkPolicy following the VPCService, and
// the Tunnel's deletion waiting for the VPCService.
func TestVPCServiceAndDeleteOrder(t *testing.T) {
	h := start(t)
	dnsIP := h.ensureKubeDNS()
	h.service("marker", map[string]string{"app": "marker"},
		corev1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)})
	host := "marker." + h.ns + ".svc.cluster.local"

	// Both at once: the VPCService waits (DependencyNotReady) until the Tunnel has an ID.
	m := h.mark()
	h.newVPC("web", hostnameParams(host, 80, "tun"), nil)
	h.newTunnel("tun", nil)
	h.setDeploymentStatus("tun", 2, 2)
	tun := h.waitTunnel("tun", tunnelReady)
	vs := h.waitVPC("web", vpcReady)

	obs := vs.Status.AtProvider
	if obs.ServiceID == "" || vs.Status.ID != obs.ServiceID || vs.Annotations[commonv1alpha1.AnnotationExternalID] != obs.ServiceID {
		t.Errorf("ids: status %q atProvider %q annotation %q", vs.Status.ID, obs.ServiceID, vs.Annotations[commonv1alpha1.AnnotationExternalID])
	}
	if obs.Name != "web" || obs.Type != "http" || obs.HTTPPort == nil || *obs.HTTPPort != 80 || obs.Host == nil || obs.Host.Hostname == nil || *obs.Host.Hostname != host {
		t.Errorf("atProvider %+v", obs)
	}
	rn := obs.Host.ResolverNetwork
	if rn == nil || rn.TunnelID != tun.Status.ID || !reflect.DeepEqual(rn.ResolverIPs, []string{dnsIP}) {
		t.Errorf("resolver_network %+v, want tunnel %s and resolver_ips [%s]", rn, tun.Status.ID, dnsIP)
	}
	if obs.TLSSettings != nil {
		t.Errorf("tls_settings echoed although unset: %+v", obs.TLSSettings)
	}
	if n := testenv.Count(h.since(m), http.MethodPost, "/connectivity/directory/services"); n != 1 {
		t.Errorf("POST services count %d", n)
	}

	// The NetworkPolicy allows the Service's pods on the target port.
	testenv.Eventually(t, 30e9, func() (bool, string) {
		tun := h.waitTunnel("tun", func(*tunnelsv1alpha1.Tunnel) bool { return true })
		return contains(tun.Status.NetworkPolicy.Backends, "web: pods of Service "+h.ns+"/marker TCP/8080"), strings.Join(tun.Status.NetworkPolicy.Backends, "; ")
	})
	np := h.networkPolicy("tun")
	if !hasRule(np, peerNS(h.ns, map[string]string{"app": "marker"}), tcp(intstr.FromInt32(8080))) {
		t.Errorf("no backend rule:\n%s", dumpNP(np))
	}

	// Idempotency across both kinds.
	h.assertNoWritesAfterReconcile([]client.Object{tun, vs}, []string{"/cfd_tunnel/" + tun.Status.ID, "/connectivity/directory/services/" + vs.Status.ID})

	// Update: one full PUT; the policy follows the new port.
	m = h.mark()
	h.updateVPC("web", func(vs *workersvpcv1alpha1.VPCService) {
		vs.Spec.ForProvider.HTTPPort = i32(8081)
		vs.Spec.ForProvider.TLSSettings = &workersvpcv1alpha1.VPCServiceTLSSettings{CertVerificationMode: "disabled"}
	})
	vs = h.waitVPC("web", func(v *workersvpcv1alpha1.VPCService) bool {
		return vpcReady(v) && v.Status.AtProvider.HTTPPort != nil && *v.Status.AtProvider.HTTPPort == 8081
	})
	if vs.Status.AtProvider.TLSSettings == nil || vs.Status.AtProvider.TLSSettings.CertVerificationMode != "disabled" {
		t.Errorf("tls_settings %+v", vs.Status.AtProvider.TLSSettings)
	}
	if w := testenv.Writes(h.since(m)); len(w) != 1 || w[0].Method != http.MethodPut {
		t.Errorf("update writes:\n%s", testenv.Summary(w))
	}
	testenv.Eventually(t, 30e9, func() (bool, string) {
		np := h.networkPolicy("tun")
		return hasRule(np, peerNS(h.ns, map[string]string{"app": "marker"}), tcp(intstr.FromInt32(8081))), dumpNP(np)
	})

	// Deleting the Tunnel waits for the VPCService (Cloudflare would allow it, 0099).
	m = h.mark()
	if err := h.e.Client.Delete(h.ctx(), tun); err != nil {
		t.Fatal(err)
	}
	h.waitTunnel("tun", func(t *tunnelsv1alpha1.Tunnel) bool {
		return hasCond(t.Status.Conditions, t.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonDependency)
	})
	if got := h.cfTunnel(tun.Status.ID); got.DeletedAt != nil {
		t.Fatal("tunnel deleted while a VPCService references it")
	}
	if n := testenv.Count(h.since(m), http.MethodDelete, ""); n != 0 {
		t.Fatalf("DELETE while blocked:\n%s", testenv.Summary(h.since(m)))
	}
	// The VPCService does not re-create against a Tunnel being deleted, but stays deletable.
	h.waitVPC("web", func(v *workersvpcv1alpha1.VPCService) bool {
		return hasCond(v.Status.Conditions, v.Generation, "Ready", metav1.ConditionFalse, commonv1alpha1.ReasonDependency)
	})
	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	h.waitGone(vs)
	var gone map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+vs.Status.ID, &gone); !cfclient.IsNotFound(err) {
		t.Errorf("VPC service still exists: %v %v", gone, err)
	}
	// Now the Tunnel proceeds: scale to zero, then delete.
	testenv.Eventually(t, 30e9, func() (bool, string) { return *h.deployment("tun").Spec.Replicas == 0, "not scaled down" })
	h.setDeploymentStatus("tun", 0, 0)
	h.waitGone(tun)
	if got := h.cfTunnel(tun.Status.ID); got.DeletedAt == nil {
		t.Error("tunnel not deleted")
	}
	j := h.since(m)
	var order []string
	for _, e := range testenv.Writes(j) {
		if e.Method == http.MethodDelete {
			order = append(order, e.Path)
		}
	}
	if len(order) != 2 || !strings.Contains(order[0], "/connectivity/directory/services/") || !strings.Contains(order[1], "/cfd_tunnel/") {
		t.Errorf("delete order %v", order)
	}
}

func TestNetworkPolicyBackends(t *testing.T) {
	h := start(t)
	db := h.service("db", map[string]string{"app": "db"},
		corev1.ServicePort{Name: "pg", Port: 5432, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("postgres")})
	if err := h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(db), db); err != nil {
		t.Fatal(err)
	}
	h.newTunnel("np", func(tun *tunnelsv1alpha1.Tunnel) {
		tun.Spec.NetworkPolicy.ExcludeCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12"}
	})
	ref := &commonv1alpha1.LocalRef{Name: "np"}
	h.newVPC("a-db", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432), AppProtocol: str("postgresql"),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str(db.Spec.ClusterIP)}, TunnelRef: ref}, nil)
	h.newVPC("b-ext", &workersvpcv1alpha1.VPCServiceParameters{Type: "http", HTTPSPort: i32(443),
		Host:      workersvpcv1alpha1.VPCServiceHost{Hostname: str("api.example.com"), ResolverNetwork: &workersvpcv1alpha1.VPCServiceResolverNetwork{ResolverIPs: []string{"192.0.2.53"}}},
		TunnelRef: ref}, nil)
	h.newVPC("c-podip", &workersvpcv1alpha1.VPCServiceParameters{Type: "http",
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.244.1.7")}, TunnelRef: ref}, nil)
	h.newVPC("d-later", hostnameParams("later."+h.ns+".svc.cluster.local", 80, "np"), nil)

	tun := h.waitTunnel("np", func(t *tunnelsv1alpha1.Tunnel) bool { return len(t.Status.NetworkPolicy.Backends) == 5 })
	want := []string{
		"a-db: pods of Service " + h.ns + "/db (ClusterIP " + db.Spec.ClusterIP + ") TCP/postgres",
		"b-ext: resolver address 192.0.2.53/32 UDP/53",
		"b-ext: external host api.example.com via 0.0.0.0/0 TCP/443",
		"c-podip: address 10.244.1.7/32 TCP/80,TCP/443",
		"d-later: pending, Service " + h.ns + "/later not found",
	}
	if !reflect.DeepEqual(tun.Status.NetworkPolicy.Backends, want) {
		t.Errorf("backends\n got %q\nwant %q", tun.Status.NetworkPolicy.Backends, want)
	}
	np := h.networkPolicy("np")
	if !metav1.IsControlledBy(np, tun) || !reflect.DeepEqual(np.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) ||
		!reflect.DeepEqual(np.Spec.PodSelector.MatchLabels, map[string]string{"app.kubernetes.io/name": "cloudflared", "cloudflare.flare.dev/tunnel": "np"}) {
		t.Errorf("policy meta/selector:\n%s", dumpNP(np))
	}
	world := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8", "172.16.0.0/12"}}}
	checks := []struct {
		name  string
		peer  networkingv1.NetworkPolicyPeer
		ports []networkingv1.NetworkPolicyPort
	}{
		{"dns", peerNS("kube-system", map[string]string{"k8s-app": "kube-dns"}), []networkingv1.NetworkPolicyPort{udp(intstr.FromInt32(53)), tcp(intstr.FromInt32(53))}},
		{"edge", world, []networkingv1.NetworkPolicyPort{tcp(intstr.FromInt32(7844)), udp(intstr.FromInt32(7844))}},
		{"db via ClusterIP → pods, named target port", peerNS(h.ns, map[string]string{"app": "db"}), []networkingv1.NetworkPolicyPort{tcp(intstr.FromString("postgres"))}},
		{"external hostname", world, []networkingv1.NetworkPolicyPort{tcp(intstr.FromInt32(443))}},
		{"custom resolver", ipPeer("192.0.2.53/32"), []networkingv1.NetworkPolicyPort{udp(intstr.FromInt32(53))}},
		{"pod IP", ipPeer("10.244.1.7/32"), []networkingv1.NetworkPolicyPort{tcp(intstr.FromInt32(80)), tcp(intstr.FromInt32(443))}},
	}
	for _, c := range checks {
		if !hasRule(np, c.peer, c.ports...) {
			t.Errorf("missing %s rule:\n%s", c.name, dumpNP(np))
		}
	}
	if len(np.Spec.Egress) != 7 {
		t.Errorf("%d egress rules, want 7:\n%s", len(np.Spec.Egress), dumpNP(np))
	}

	// The Service appearing later is picked up through the Service watch.
	h.service("later", map[string]string{"app": "later"}, corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(9090)})
	testenv.Eventually(t, 30e9, func() (bool, string) {
		np := h.networkPolicy("np")
		return hasRule(np, peerNS(h.ns, map[string]string{"app": "later"}), tcp(intstr.FromInt32(9090))), dumpNP(np)
	})
	// A selector change on a backend Service regenerates the policy.
	var later corev1.Service
	if err := h.e.Client.Get(h.ctx(), client.ObjectKey{Namespace: h.ns, Name: "later"}, &later); err != nil {
		t.Fatal(err)
	}
	later.Spec.Selector = map[string]string{"app": "later-v2"}
	if err := h.e.Client.Update(h.ctx(), &later); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 30e9, func() (bool, string) {
		np := h.networkPolicy("np")
		return hasRule(np, peerNS(h.ns, map[string]string{"app": "later-v2"}), tcp(intstr.FromInt32(9090))), dumpNP(np)
	})
	// allowHTTPS adds 443 to the edge rule; disabling removes the policy.
	h.updateTunnel("np", func(t *tunnelsv1alpha1.Tunnel) { t.Spec.NetworkPolicy.AllowHTTPS = true })
	testenv.Eventually(t, 30e9, func() (bool, string) {
		np := h.networkPolicy("np")
		return hasRule(np, world, tcp(intstr.FromInt32(7844)), udp(intstr.FromInt32(7844)), tcp(intstr.FromInt32(443))), dumpNP(np)
	})
	h.updateTunnel("np", func(t *tunnelsv1alpha1.Tunnel) { t.Spec.NetworkPolicy.Disabled = true })
	h.waitGone(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: "np-cloudflared"}})
}

func TestVPCServiceObserveOnlyAndOrphan(t *testing.T) {
	h := start(t)
	id, _ := h.apiCreateTunnel("raw")
	// A service created outside the operator, referenced by raw tunnel_id.
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: http.MethodPost, Path: "/accounts/" + h.acct.AccountID + "/connectivity/directory/services",
		Body: map[string]any{"name": "outside", "type": "tcp", "tcp_port": 5432, "host": map[string]any{"ipv4": "10.0.0.5", "network": map[string]string{"tunnel_id": id}}}})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ServiceID string `json:"service_id"`
	}
	if err := json.Unmarshal(resp.Result, &created); err != nil {
		t.Fatal(err)
	}

	// Observe-only, no forProvider, pinned by annotation: read-only.
	m := h.mark()
	h.newVPC("observer", nil, func(vs *workersvpcv1alpha1.VPCService) {
		vs.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
		vs.Spec.DeletionPolicy = commonv1alpha1.DeletionDelete
		vs.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: created.ServiceID}
	})
	vs := h.waitVPC("observer", vpcReady)
	if vs.Status.AtProvider.Name != "outside" || vs.Status.AtProvider.Host == nil || vs.Status.AtProvider.Host.Network == nil || vs.Status.AtProvider.Host.Network.TunnelID != id {
		t.Errorf("atProvider %+v", vs.Status.AtProvider)
	}
	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	h.waitGone(vs)
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("observe-only wrote:\n%s", testenv.Summary(w))
	}

	// Adopt by name with a raw tunnel_id (no tunnelRef), Orphan on delete.
	h.newVPC("outside", &workersvpcv1alpha1.VPCServiceParameters{Type: "tcp", TCPPort: i32(5432),
		Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.5"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str(id)}}},
		func(vs *workersvpcv1alpha1.VPCService) { vs.Spec.DeletionPolicy = commonv1alpha1.DeletionOrphan })
	vs = h.waitVPC("outside", vpcReady)
	if vs.Status.ID != created.ServiceID || vs.Status.TunnelID != id {
		t.Errorf("adopt by name: id %q tunnel %q", vs.Status.ID, vs.Status.TunnelID)
	}
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Errorf("adopting an identical service wrote:\n%s", testenv.Summary(w))
	}
	if err := h.e.Client.Delete(h.ctx(), vs); err != nil {
		t.Fatal(err)
	}
	h.waitGone(vs)
	var still map[string]any
	if err := h.apiGet("/connectivity/directory/services/"+created.ServiceID, &still); err != nil {
		t.Errorf("orphaned service deleted: %v", err)
	}
}

func TestVPCServiceShortNameRejected(t *testing.T) {
	h := start(t)
	h.service("marker", map[string]string{"app": "marker"}, corev1.ServicePort{Port: 80})
	h.newTunnel("tun", nil)
	h.newVPC("short", hostnameParams("marker."+h.ns, 80, "tun"), nil)
	vs := h.waitVPC("short", func(v *workersvpcv1alpha1.VPCService) bool {
		return hasCond(v.Status.Conditions, v.Generation, "Synced", metav1.ConditionFalse, "InvalidHostname")
	})
	if vs.Status.ID != "" {
		t.Errorf("short name created a service")
	}
}

func TestVPCServiceValidation(t *testing.T) {
	h := start(t)
	ref := &commonv1alpha1.LocalRef{Name: "tun"}
	mk := func(name string, fp *workersvpcv1alpha1.VPCServiceParameters, policies ...commonv1alpha1.ManagementAction) error {
		vs := &workersvpcv1alpha1.VPCService{
			ObjectMeta: metav1.ObjectMeta{Namespace: h.ns, Name: name},
			Spec: workersvpcv1alpha1.VPCServiceSpec{
				ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, ManagementPolicies: policies},
				ForProvider:  fp,
			},
		}
		return h.e.Client.Create(h.ctx(), vs, client.DryRunAll)
	}
	host := func(h workersvpcv1alpha1.VPCServiceHost) *workersvpcv1alpha1.VPCServiceParameters {
		return &workersvpcv1alpha1.VPCServiceParameters{Type: "http", Host: h, TunnelRef: ref}
	}
	bad := map[string]*workersvpcv1alpha1.VPCServiceParameters{
		"svc-suffix":       host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("marker.ns.svc")}),
		"svc-cluster":      host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("marker.ns.svc.cluster")}),
		"single-label":     host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("marker")}),
		"upper-case":       host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("Marker.example.com")}),
		"hostname-and-ip":  host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("a.example.com"), IPv4: str("10.0.0.1")}),
		"no-host":          host(workersvpcv1alpha1.VPCServiceHost{}),
		"bad-ipv4":         host(workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0")}),
		"ipv6-in-ipv4":     host(workersvpcv1alpha1.VPCServiceHost{IPv4: str("fe80::1")}),
		"resolver-with-ip": host(workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1"), ResolverNetwork: &workersvpcv1alpha1.VPCServiceResolverNetwork{}}),
		"bad-resolver":     host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("a.example.com"), ResolverNetwork: &workersvpcv1alpha1.VPCServiceResolverNetwork{ResolverIPs: []string{"dns"}}}),
		"ref-and-id": {Type: "http", TunnelRef: ref,
			Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str("x")}}},
		"no-tunnel":      {Type: "http", Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1")}},
		"tcp-http-port":  {Type: "tcp", HTTPPort: i32(80), TunnelRef: ref, Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1")}},
		"http-tcp-port":  {Type: "http", TCPPort: i32(80), TunnelRef: ref, Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1")}},
		"no-forprovider": nil,
	}
	for name, fp := range bad {
		if err := mk(name, fp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := map[string]*workersvpcv1alpha1.VPCServiceParameters{
		"fqdn":         host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("marker.ns.svc.cluster.local")}),
		"fqdn-dot":     host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("marker.ns.svc.cluster.local.")}),
		"external":     host(workersvpcv1alpha1.VPCServiceHost{Hostname: str("api.example.com"), ResolverNetwork: &workersvpcv1alpha1.VPCServiceResolverNetwork{ResolverIPs: []string{"10.96.0.10"}}}),
		"dual-stack":   host(workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1"), IPv6: str("fe80::1")}),
		"raw-tunnelid": {Type: "tcp", TCPPort: i32(5432), Host: workersvpcv1alpha1.VPCServiceHost{IPv4: str("10.0.0.1"), Network: &workersvpcv1alpha1.VPCServiceNetwork{TunnelID: str("x")}}},
	}
	for name, fp := range good {
		if err := mk(name, fp); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	if err := mk("observe-no-forprovider", nil, commonv1alpha1.ManageObserve); err != nil {
		t.Errorf("observe-only without forProvider rejected: %v", err)
	}
}

// ---- NetworkPolicy matching helpers ----------------------------------------------------------

func peerNS(ns string, pods map[string]string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: pods},
	}
}

func ipPeer(cidr string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}
}

func port(proto corev1.Protocol, p intstr.IntOrString) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}
func tcp(p intstr.IntOrString) networkingv1.NetworkPolicyPort { return port(corev1.ProtocolTCP, p) }
func udp(p intstr.IntOrString) networkingv1.NetworkPolicyPort { return port(corev1.ProtocolUDP, p) }

// hasRule reports whether some egress rule has exactly peer as its only destination and
// exactly ports.
func hasRule(np *networkingv1.NetworkPolicy, peer networkingv1.NetworkPolicyPeer, ports ...networkingv1.NetworkPolicyPort) bool {
	for _, r := range np.Spec.Egress {
		if len(r.To) == 1 && reflect.DeepEqual(r.To[0], peer) && reflect.DeepEqual(r.Ports, ports) {
			return true
		}
	}
	return false
}

func dumpNP(np *networkingv1.NetworkPolicy) string {
	b, _ := json.MarshalIndent(np.Spec.Egress, "", " ")
	return string(b)
}
