package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "github.com/chenhunghan/flare-operator/api/workersvpc/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller/tunnelnet"
)

// The egress NetworkPolicy of the cloudflared pods (spike §2.1: a policy on the cloudflared pod
// is a real isolation boundary; tunnel ingress rules and warp-routing are not). It allows:
//
//  1. DNS (UDP and TCP) to the cluster DNS pods, selected by the kube-dns Service's selector.
//     cloudflared needs DNS itself (edge discovery is an SRV lookup; without DNS it
//     crash-loops) and resolves VPC service hostnames.
//  2. The Cloudflare edge: 0.0.0.0/0 minus spec.networkPolicy.excludeCIDRs on TCP and UDP 7844
//     (plus TCP 443 when allowHTTPS; the spike found 443 unnecessary).
//  3. The backend of every VPCService whose tunnelRef names this Tunnel:
//     - a hostname <svc>.<ns>.svc.<cluster-domain> (or <pod>.<svc>.<ns>.svc.…) of a Service
//       with a selector → that Service's pods (namespace + pod selector) on the Service's
//       target ports for the VPC service's ports. Selectors are used instead of ClusterIP
//       ipBlocks because the policy is evaluated after the Service address has been
//       translated to a pod address (kube-router, spike §2.1).
//     - an IP that is a Service ClusterIP → that Service's pods, likewise; any other IP → an
//       ipBlock /32 (/128) on the service ports (pod IPs work, spike §2.1).
//     - any other hostname (outside the cluster) → 0.0.0.0/0 minus excludeCIDRs on the
//       service ports; the policy cannot name a DNS name. Without a known port (type tcp with
//       neither tcp_port nor a known app_protocol) nothing is allowed: the policy never opens
//       every port to 0.0.0.0/0.
//     - an in-cluster name whose Service does not exist yet, has no selector or is an
//       ExternalName → nothing (reported as pending/unsupported in status); the Tunnel is
//       re-reconciled when the Service changes.
//     - resolver_ips other than the cluster DNS ClusterIP → port 53 (UDP/TCP) to that
//       resolver, resolved like an IP host.
//
// Service ports: http_port / https_port (80 and 443 when neither is set), tcp_port (5432 for
// app_protocol postgresql and 3306 for mysql when unset). A tcp service with no known port
// allows every TCP port of the backing Service (its target ports), every port of a single
// address, and nothing for an external hostname; the non-Service cases add a warning.
//
// status.networkPolicy.warnings also reports an empty excludeCIDRs: the controller does not
// discover the cluster's pod and Service CIDRs, so the 0.0.0.0/0 rules then reach in-cluster
// pods on those ports too (the spike's policy excluded them, §2.1).

func npPort(proto corev1.Protocol, port intstr.IntOrString) networkingv1.NetworkPolicyPort {
	p := proto
	pt := port
	return networkingv1.NetworkPolicyPort{Protocol: &p, Port: &pt}
}

func namespacePeer(namespace string, podSelector map[string]string) networkingv1.NetworkPolicyPeer {
	sel := map[string]string{}
	for k, v := range podSelector {
		sel[k] = v
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: sel},
	}
}

func ipv4Everywhere(exclude []string) networkingv1.NetworkPolicyPeer {
	var except []string
	for _, c := range exclude {
		if ip, _, err := net.ParseCIDR(c); err == nil && ip.To4() != nil {
			except = append(except, c)
		}
	}
	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: except}}
}

func hostCIDR(ip string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		return ip + "/128"
	}
	return ip + "/32"
}

// targetPort is a Service port's target (named target ports stay named; NetworkPolicy resolves
// them on the pod), defaulting to the port itself.
func targetPort(sp corev1.ServicePort) intstr.IntOrString {
	switch {
	case sp.TargetPort.Type == intstr.String && sp.TargetPort.StrVal != "":
		return sp.TargetPort
	case sp.TargetPort.Type == intstr.Int && sp.TargetPort.IntVal != 0:
		return intstr.FromInt32(sp.TargetPort.IntVal)
	}
	return intstr.FromInt32(sp.Port)
}

func protocolOf(sp corev1.ServicePort) corev1.Protocol {
	if sp.Protocol == "" {
		return corev1.ProtocolTCP
	}
	return sp.Protocol
}

// servicePorts maps wanted service ports to the Service's target ports. A wanted port the
// Service does not expose is used as is. No wanted ports means every port of the Service for
// proto (nil, i.e. all ports, only when the Service has none).
func servicePorts(svc *corev1.Service, proto corev1.Protocol, wanted []int32) []networkingv1.NetworkPolicyPort {
	var out []networkingv1.NetworkPolicyPort
	if len(wanted) == 0 {
		seen := map[string]bool{}
		for _, sp := range svc.Spec.Ports {
			if protocolOf(sp) != proto {
				continue
			}
			tp := targetPort(sp)
			if !seen[tp.String()] {
				seen[tp.String()] = true
				out = append(out, npPort(proto, tp))
			}
		}
		return out
	}
	for _, w := range wanted {
		target := intstr.FromInt32(w)
		for _, sp := range svc.Spec.Ports {
			if sp.Port == w && protocolOf(sp) == proto {
				target = targetPort(sp)
				break
			}
		}
		out = append(out, npPort(proto, target))
	}
	return out
}

func plainPorts(proto corev1.Protocol, wanted []int32) []networkingv1.NetworkPolicyPort {
	var out []networkingv1.NetworkPolicyPort
	for _, w := range wanted {
		out = append(out, npPort(proto, intstr.FromInt32(w)))
	}
	return out
}

// wantedPorts are the backend ports of a VPC service (see the package comment).
func wantedPorts(fp *workersvpcv1alpha1.VPCServiceParameters) []int32 {
	var out []int32
	switch fp.Type {
	case "tcp":
		switch {
		case fp.TCPPort != nil:
			out = append(out, *fp.TCPPort)
		case fp.AppProtocol != nil && *fp.AppProtocol == "postgresql":
			out = append(out, 5432)
		case fp.AppProtocol != nil && *fp.AppProtocol == "mysql":
			out = append(out, 3306)
		}
	default:
		if fp.HTTPPort != nil {
			out = append(out, *fp.HTTPPort)
		}
		if fp.HTTPSPort != nil {
			out = append(out, *fp.HTTPSPort)
		}
		if len(out) == 0 {
			out = []int32{80, 443}
		}
	}
	return out
}

func describePorts(proto corev1.Protocol, ports []networkingv1.NetworkPolicyPort) string {
	if len(ports) == 0 {
		return "all ports"
	}
	var s []string
	for _, p := range ports {
		s = append(s, fmt.Sprintf("%s/%s", proto, p.Port.String()))
	}
	return strings.Join(s, ",")
}

// npBuilder accumulates egress rules (deduplicated, in insertion order).
type npBuilder struct {
	rules []networkingv1.NetworkPolicyEgressRule
	seen  map[string]bool
}

func (b *npBuilder) add(r networkingv1.NetworkPolicyEgressRule) {
	key, _ := json.Marshal(r)
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	if b.seen[string(key)] {
		return
	}
	b.seen[string(key)] = true
	b.rules = append(b.rules, r)
}

// serviceForIP returns the Service whose ClusterIP is ip, or nil.
func (r *Reconciler) serviceForIP(ctx context.Context, ip string) (*corev1.Service, error) {
	var list corev1.ServiceList
	if err := r.List(ctx, &list, client.MatchingFields{tunnelnet.IndexServiceClusterIP: ip}); err != nil {
		return nil, err
	}
	for i := range list.Items {
		return &list.Items[i], nil
	}
	return nil, nil
}

// selectable reports whether a Service's pods can be named by a selector.
func selectable(svc *corev1.Service) bool {
	return svc != nil && svc.Spec.Type != corev1.ServiceTypeExternalName && len(svc.Spec.Selector) > 0
}

// ipRule allows ports on ip: the pods behind it when it is a Service ClusterIP, else the address.
// No wanted ports allows every port (see servicePorts; for a plain address, every port).
func (r *Reconciler) ipRule(ctx context.Context, ip string, proto corev1.Protocol, wanted []int32) (networkingv1.NetworkPolicyEgressRule, string, error) {
	svc, err := r.serviceForIP(ctx, ip)
	if err != nil {
		return networkingv1.NetworkPolicyEgressRule{}, "", err
	}
	if selectable(svc) {
		ports := servicePorts(svc, proto, wanted)
		return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{namespacePeer(svc.Namespace, svc.Spec.Selector)}, Ports: ports},
			fmt.Sprintf("pods of Service %s/%s (ClusterIP %s) %s", svc.Namespace, svc.Name, ip, describePorts(proto, ports)), nil
	}
	ports := plainPorts(proto, wanted)
	return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: hostCIDR(ip)}}}, Ports: ports},
		fmt.Sprintf("address %s %s", hostCIDR(ip), describePorts(proto, ports)), nil
}

// networkPolicySpec builds the policy for t from the VPCServices that reference it and
// returns it with one status line per backend and the warnings (see the package comment).
func (r *Reconciler) networkPolicySpec(ctx context.Context, t *tunnelsv1alpha1.Tunnel, vpcs []workersvpcv1alpha1.VPCService) (networkingv1.NetworkPolicySpec, []string, []string, error) {
	dns := r.DNS.WithDefaults()
	var b npBuilder
	var backends, warnings []string
	fail := func(err error) (networkingv1.NetworkPolicySpec, []string, []string, error) {
		return networkingv1.NetworkPolicySpec{}, nil, nil, err
	}
	if len(t.Spec.NetworkPolicy.ExcludeCIDRs) == 0 {
		warnings = append(warnings, "spec.networkPolicy.excludeCIDRs is empty: the 0.0.0.0/0 rules (Cloudflare edge on port 7844"+
			" and external backends) also allow in-cluster pods and Services on those ports; set it to the cluster's pod and Service CIDRs")
	}

	// 1. DNS.
	dnsSvc, err := dns.Service(ctx, r.Client)
	if err != nil {
		return fail(err)
	}
	dnsIP := ""
	if selectable(dnsSvc) {
		dnsIP = dnsSvc.Spec.ClusterIP
		var ports []networkingv1.NetworkPolicyPort
		for _, proto := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
			ports = append(ports, servicePorts(dnsSvc, proto, []int32{53})...)
		}
		b.add(networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{namespacePeer(dnsSvc.Namespace, dnsSvc.Spec.Selector)}, Ports: ports})
	} else {
		if dnsSvc != nil {
			dnsIP = dnsSvc.Spec.ClusterIP
		}
		b.add(networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{namespacePeer(dns.Namespace, map[string]string{"k8s-app": "kube-dns"})},
			Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolUDP, intstr.FromInt32(53)), npPort(corev1.ProtocolTCP, intstr.FromInt32(53))},
		})
	}

	// 2. Cloudflare edge.
	edge := []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, intstr.FromInt32(7844)), npPort(corev1.ProtocolUDP, intstr.FromInt32(7844))}
	if t.Spec.NetworkPolicy.AllowHTTPS {
		edge = append(edge, npPort(corev1.ProtocolTCP, intstr.FromInt32(443)))
	}
	b.add(networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{ipv4Everywhere(t.Spec.NetworkPolicy.ExcludeCIDRs)}, Ports: edge})

	// 3. Backends.
	sort.Slice(vpcs, func(i, j int) bool { return vpcs[i].Name < vpcs[j].Name })
	for i := range vpcs {
		vs := &vpcs[i]
		fp := vs.Spec.ForProvider
		if fp == nil {
			continue
		}
		wanted := wantedPorts(fp)
		h := fp.Host
		if h.ResolverNetwork != nil {
			for _, ip := range h.ResolverNetwork.ResolverIPs {
				if ip == dnsIP {
					continue
				}
				var ports []int32 = []int32{53}
				for _, proto := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
					rule, desc, err := r.ipRule(ctx, ip, proto, ports)
					if err != nil {
						return fail(err)
					}
					b.add(rule)
					if proto == corev1.ProtocolUDP {
						backends = append(backends, fmt.Sprintf("%s: resolver %s", vs.Name, desc))
					}
				}
			}
		}
		switch {
		case h.Hostname != nil:
			host := *h.Hostname
			ns, name, inCluster := tunnelnet.ParseServiceFQDN(host, dns.Domain)
			if !inCluster {
				if len(wanted) == 0 {
					msg := fmt.Sprintf("%s: not allowed, external host %s has no known port (set tcp_port); every port to 0.0.0.0/0 is never opened", vs.Name, host)
					backends = append(backends, msg)
					warnings = append(warnings, msg)
					continue
				}
				ports := plainPorts(corev1.ProtocolTCP, wanted)
				b.add(networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{ipv4Everywhere(t.Spec.NetworkPolicy.ExcludeCIDRs)}, Ports: ports})
				backends = append(backends, fmt.Sprintf("%s: external host %s via 0.0.0.0/0 %s", vs.Name, host, describePorts(corev1.ProtocolTCP, ports)))
				continue
			}
			var svc corev1.Service
			if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
				if !apierrors.IsNotFound(err) {
					return fail(err)
				}
				backends = append(backends, fmt.Sprintf("%s: pending, Service %s/%s not found", vs.Name, ns, name))
				continue
			}
			if !selectable(&svc) {
				backends = append(backends, fmt.Sprintf("%s: not allowed, Service %s/%s has no pod selector", vs.Name, ns, name))
				continue
			}
			ports := servicePorts(&svc, corev1.ProtocolTCP, wanted)
			b.add(networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{namespacePeer(svc.Namespace, svc.Spec.Selector)}, Ports: ports})
			backends = append(backends, fmt.Sprintf("%s: pods of Service %s/%s %s", vs.Name, ns, name, describePorts(corev1.ProtocolTCP, ports)))
		default:
			for _, ip := range []*string{h.IPv4, h.IPv6} {
				if ip == nil {
					continue
				}
				rule, desc, err := r.ipRule(ctx, *ip, corev1.ProtocolTCP, wanted)
				if err != nil {
					return fail(err)
				}
				b.add(rule)
				backends = append(backends, vs.Name+": "+desc)
				if len(rule.Ports) == 0 {
					warnings = append(warnings, fmt.Sprintf("%s: no known port (set tcp_port), so every port of %s is allowed", vs.Name, *ip))
				}
			}
		}
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: selectorLabels(t)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress:      b.rules,
	}, backends, warnings, nil
}
