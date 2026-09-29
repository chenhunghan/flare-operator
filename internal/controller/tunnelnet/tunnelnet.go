// Package tunnelnet holds what the Tunnel and VPCService controllers share about the cluster
// network: where cluster DNS lives, how an in-cluster Service FQDN maps to a Service, and the
// VPCService index keys both controllers use for their watches.
package tunnelnet

import (
	"context"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
)

// ClusterDNS locates the cluster DNS Service and names the cluster domain.
type ClusterDNS struct {
	// Namespace and Name of the DNS Service (default kube-system/kube-dns).
	Namespace, Name string
	// Domain is the cluster domain (default cluster.local).
	Domain string
}

// WithDefaults fills unset fields.
func (d ClusterDNS) WithDefaults() ClusterDNS {
	if d.Namespace == "" {
		d.Namespace = "kube-system"
	}
	if d.Name == "" {
		d.Name = "kube-dns"
	}
	if d.Domain == "" {
		d.Domain = "cluster.local"
	}
	return d
}

// Service returns the cluster DNS Service, or nil when it does not exist.
func (d ClusterDNS) Service(ctx context.Context, c client.Reader) (*corev1.Service, error) {
	d = d.WithDefaults()
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: d.Name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &svc, nil
}

// ResolverIP returns the ClusterIP of the cluster DNS Service ("" when it is missing or headless).
func (d ClusterDNS) ResolverIP(ctx context.Context, c client.Reader) (string, error) {
	svc, err := d.Service(ctx, c)
	if err != nil || svc == nil {
		return "", err
	}
	ip := svc.Spec.ClusterIP
	if ip == corev1.ClusterIPNone {
		return "", nil
	}
	return ip, nil
}

// ParseServiceFQDN maps an in-cluster DNS name to its Service:
// <service>.<namespace>.svc.<domain> or <pod-hostname>.<service>.<namespace>.svc.<domain>
// (a trailing dot is allowed). ok is false for any other name.
func ParseServiceFQDN(host, domain string) (namespace, service string, ok bool) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	suffix := ".svc." + strings.Trim(strings.ToLower(domain), ".")
	if !strings.HasSuffix(h, suffix) {
		return "", "", false
	}
	labels := strings.Split(strings.TrimSuffix(h, suffix), ".")
	switch len(labels) {
	case 2:
		return labels[1], labels[0], labels[0] != "" && labels[1] != ""
	case 3:
		return labels[2], labels[1], labels[1] != "" && labels[2] != ""
	}
	return "", "", false
}

// Index names and key helpers. Each controller registers its own index (distinct names, so
// both can run in one manager) with the functions below.
const (
	// IndexTunnelRef indexes VPCServices by spec.forProvider.tunnelRef.name.
	IndexTunnelRef = "tunnelnet.vpcservice.tunnelRef"
	// IndexBackend indexes VPCServices by BackendKeys.
	IndexBackend = "tunnelnet.vpcservice.backend"
	// IndexServiceClusterIP indexes Services by spec.clusterIPs.
	IndexServiceClusterIP = "tunnelnet.service.clusterIP"
)

// TunnelRefKeys is the IndexTunnelRef extractor.
func TunnelRefKeys(o client.Object) []string {
	vs, ok := o.(*workersvpcv1alpha1.VPCService)
	if !ok || vs.TunnelRefName() == "" {
		return nil
	}
	return []string{vs.TunnelRefName()}
}

// ServiceKey is the backend key of an in-cluster Service.
func ServiceKey(namespace, name string) string { return "svc:" + namespace + "/" + name }

// IPKey is the backend key of an IP address (host.ipv4/ipv6 or a resolver IP).
func IPKey(ip string) string { return "ip:" + ip }

// BackendKeys returns the backend keys of a VPCService: the Service its hostname names (for
// the cluster domain) and every IP it uses (host addresses and resolver IPs).
func BackendKeys(domain string) func(client.Object) []string {
	return func(o client.Object) []string {
		vs, ok := o.(*workersvpcv1alpha1.VPCService)
		if !ok || vs.Spec.ForProvider == nil {
			return nil
		}
		h := vs.Spec.ForProvider.Host
		var keys []string
		if h.Hostname != nil {
			if ns, name, ok := ParseServiceFQDN(*h.Hostname, domain); ok {
				keys = append(keys, ServiceKey(ns, name))
			}
		}
		for _, ip := range []*string{h.IPv4, h.IPv6} {
			if ip != nil {
				keys = append(keys, IPKey(*ip))
			}
		}
		if h.ResolverNetwork != nil {
			for _, ip := range h.ResolverNetwork.ResolverIPs {
				keys = append(keys, IPKey(ip))
			}
		}
		return keys
	}
}

// ServiceClusterIPs is the IndexServiceClusterIP extractor.
func ServiceClusterIPs(o client.Object) []string {
	svc, ok := o.(*corev1.Service)
	if !ok {
		return nil
	}
	var out []string
	for _, ip := range svc.Spec.ClusterIPs {
		if ip != "" && ip != corev1.ClusterIPNone {
			out = append(out, ip)
		}
	}
	return out
}

var (
	indexMu sync.Mutex
	indexed = map[client.FieldIndexer]bool{}
)

// RegisterIndexes registers IndexTunnelRef, IndexBackend and IndexServiceClusterIP with the
// indexer once (both controllers call it; a second registration of the same field would fail).
func RegisterIndexes(ctx context.Context, fi client.FieldIndexer, domain string) error {
	indexMu.Lock()
	defer indexMu.Unlock()
	if indexed[fi] {
		return nil
	}
	if err := fi.IndexField(ctx, &workersvpcv1alpha1.VPCService{}, IndexTunnelRef, TunnelRefKeys); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, &workersvpcv1alpha1.VPCService{}, IndexBackend, BackendKeys(domain)); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, &corev1.Service{}, IndexServiceClusterIP, ServiceClusterIPs); err != nil {
		return err
	}
	indexed[fi] = true
	return nil
}
