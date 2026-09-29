package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
)

// VPCServiceNetwork is host.network of an IP host.
type VPCServiceNetwork struct {
	// UUID of the tunnel that reaches the host. Leave unset when forProvider.tunnelRef is set
	// (the controller fills it in from the Tunnel's status.id). A raw tunnel_id is not linked
	// to a Tunnel object even when it equals one's ID: that Tunnel's egress NetworkPolicy does
	// not allow this backend and its deletion does not wait for this service. Use tunnelRef
	// for tunnels managed in this cluster.
	// +optional
	TunnelID *string `json:"tunnel_id,omitempty"`
}

// VPCServiceResolverNetwork is host.resolver_network of a hostname host.
type VPCServiceResolverNetwork struct {
	// UUID of the tunnel whose cloudflared resolves and reaches the host. Leave unset when
	// forProvider.tunnelRef is set. As for network.tunnel_id, a raw ID is not linked to a
	// Tunnel object (no NetworkPolicy egress, no deletion ordering); prefer tunnelRef.
	// +optional
	TunnelID *string `json:"tunnel_id,omitempty"`
	// DNS servers cloudflared queries for the hostname. When unset the controller sends the
	// ClusterIP of the cluster DNS Service (kube-system/kube-dns), discovered at runtime; the
	// spike showed cloudflared would otherwise use its pod's /etc/resolv.conf (§2.1).
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=45
	// +kubebuilder:validation:items:XValidation:rule="isIP(self)",message="must be an IP address"
	ResolverIPs []string `json:"resolver_ips,omitempty"`
}

// VPCServiceHost is the backend: either a hostname (resolved on the cloudflared side) or an
// IPv4 and/or IPv6 address.
//
// +kubebuilder:validation:XValidation:rule="has(self.hostname) != (has(self.ipv4) || has(self.ipv6))",message="set either hostname or ipv4/ipv6"
// +kubebuilder:validation:XValidation:rule="!has(self.resolver_network) || has(self.hostname)",message="resolver_network is only valid with hostname"
// +kubebuilder:validation:XValidation:rule="!has(self.network) || !has(self.hostname)",message="network is only valid with ipv4/ipv6"
type VPCServiceHost struct {
	// Fully qualified, lower-case domain name of the backend. cloudflared never applies DNS
	// search domains, so short names such as "svc" or "svc.ns" fail with dns_error (spike §2.1);
	// use e.g. "svc.ns.svc.cluster.local". The API lower-cases hostnames (0066).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]*[a-z0-9])?\.?$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('\\\\.svc\\\\.?$') && !self.matches('\\\\.svc\\\\.cluster\\\\.?$')",message="must be fully qualified: search domains never apply (use <service>.<namespace>.svc.<cluster-domain>)"
	Hostname *string `json:"hostname,omitempty"`
	// IPv4 address of the backend.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:XValidation:rule="isIP(self) && ip(self).family() == 4",message="must be an IPv4 address"
	IPv4 *string `json:"ipv4,omitempty"`
	// IPv6 address of the backend.
	// +optional
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self) && ip(self).family() == 6",message="must be an IPv6 address"
	IPv6 *string `json:"ipv6,omitempty"`
	// Network of an IP host.
	// +optional
	Network *VPCServiceNetwork `json:"network,omitempty"`
	// ResolverNetwork of a hostname host.
	// +optional
	ResolverNetwork *VPCServiceResolverNetwork `json:"resolver_network,omitempty"`
}

// VPCServiceTLSSettings are the TLS settings for the connection to the origin.
type VPCServiceTLSSettings struct {
	// TLS certificate verification mode: verify_full (default: verify chain and hostname),
	// verify_ca (chain only) or disabled.
	// +kubebuilder:validation:Enum=verify_full;verify_ca;disabled
	CertVerificationMode string `json:"cert_verification_mode"`
}

// VPCServiceParameters are the configurable fields of a VPCService: the request bodies of
// POST /accounts/{account_id}/connectivity/directory/services and PUT …/services/{service_id}
// (a full replace), plus tunnelRef.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'tcp' || (!has(self.tcp_port) && !has(self.app_protocol))",message="tcp_port and app_protocol are only valid for type tcp"
// +kubebuilder:validation:XValidation:rule="self.type == 'http' || (!has(self.http_port) && !has(self.https_port))",message="http_port and https_port are only valid for type http"
// +kubebuilder:validation:XValidation:rule="has(self.tunnelRef) != ((has(self.host.network) && has(self.host.network.tunnel_id)) || (has(self.host.resolver_network) && has(self.host.resolver_network.tunnel_id)))",message="set exactly one of tunnelRef or the host's network/resolver_network tunnel_id"
type VPCServiceParameters struct {
	// Name of the service, unique in the account. Defaults to metadata.name. An existing
	// service of the same name is never adopted by a managing object (VPC services carry no
	// ownership tag, so two objects could otherwise manage, and delete, one service): the
	// object reports Synced=False, reason NameConflict. To adopt it, set the
	// cloudflare.flare.dev/external-id annotation to its service_id. Observe-only objects do
	// look services up by name.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name,omitempty"`
	// Type of the service.
	// +kubebuilder:validation:Enum=http;tcp
	Type string `json:"type"`
	// Host is the backend.
	Host VPCServiceHost `json:"host"`
	// HTTPPort of an http service.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	HTTPPort *int32 `json:"http_port,omitempty"`
	// HTTPSPort of an http service.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	HTTPSPort *int32 `json:"https_port,omitempty"`
	// TCPPort of a tcp service.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	TCPPort *int32 `json:"tcp_port,omitempty"`
	// AppProtocol of a tcp service.
	// +optional
	// +kubebuilder:validation:Enum=postgresql;mysql
	AppProtocol *string `json:"app_protocol,omitempty"`
	// TLSSettings for the connection to the origin; echoed by the API only when set (0193).
	// +optional
	TLSSettings *VPCServiceTLSSettings `json:"tls_settings,omitempty"`
	// TunnelRef names a Tunnel (same namespace) whose status.id becomes the host's tunnel_id.
	// The Tunnel cannot finish deleting while this VPCService exists (Cloudflare does not
	// check the dependency itself, spike §3).
	// +optional
	TunnelRef *commonv1alpha1.LocalRef `json:"tunnelRef,omitempty"`
}

// VPCServiceNetworkObservation is host.network / host.resolver_network as returned by the API.
type VPCServiceNetworkObservation struct {
	// +optional
	TunnelID string `json:"tunnel_id,omitempty"`
	// +optional
	ResolverIPs []string `json:"resolver_ips,omitempty"`
}

// VPCServiceHostObservation is the host as returned by the API.
type VPCServiceHostObservation struct {
	// +optional
	Hostname *string `json:"hostname,omitempty"`
	// +optional
	IPv4 *string `json:"ipv4,omitempty"`
	// +optional
	IPv6 *string `json:"ipv6,omitempty"`
	// +optional
	Network *VPCServiceNetworkObservation `json:"network,omitempty"`
	// +optional
	ResolverNetwork *VPCServiceNetworkObservation `json:"resolver_network,omitempty"`
}

// VPCServiceObservation is the service as returned by
// GET /accounts/{account_id}/connectivity/directory/services/{service_id}.
type VPCServiceObservation struct {
	// UUIDv7 of the service.
	// +optional
	ServiceID string `json:"service_id,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Host *VPCServiceHostObservation `json:"host,omitempty"`
	// +optional
	HTTPPort *int32 `json:"http_port,omitempty"`
	// +optional
	HTTPSPort *int32 `json:"https_port,omitempty"`
	// +optional
	TCPPort *int32 `json:"tcp_port,omitempty"`
	// +optional
	AppProtocol *string `json:"app_protocol,omitempty"`
	// +optional
	TLSSettings *VPCServiceTLSSettings `json:"tls_settings,omitempty"`
	// Reset by every PUT (full replace, 0066).
	// +optional
	CreatedAt string `json:"created_at,omitempty"`
	// +optional
	UpdatedAt string `json:"updated_at,omitempty"`
}

// VPCServiceSpec defines the desired state of a VPCService.
//
// +kubebuilder:validation:XValidation:rule="has(self.forProvider) || (has(self.managementPolicies) && self.managementPolicies == ['Observe'])",message="forProvider is required unless managementPolicies is [Observe]"
type VPCServiceSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	// ForProvider holds the Cloudflare API fields, named exactly as in the API. It may be
	// omitted for an observe-only object that adopts a service by external-id annotation.
	// +optional
	ForProvider *VPCServiceParameters `json:"forProvider,omitempty"`
}

// VPCServiceStatus defines the observed state of a VPCService.
type VPCServiceStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	// AtProvider is the service as last read from the Cloudflare API.
	// +optional
	AtProvider VPCServiceObservation `json:"atProvider,omitempty"`
	// TunnelID is the tunnel_id last sent to Cloudflare (resolved from tunnelRef).
	// +optional
	TunnelID string `json:"tunnelID,omitempty"`
}

// VPCService is a Workers VPC connectivity service (x-fern-sdk-group-name
// "workers-vpc.services").
//
// Created at /accounts/{account_id}/connectivity/directory/services, managed at
// …/services/{id} (account scope). Default deletion policy: Delete. Updates are full-replace
// PUTs (never partial).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={cloudflare,workersvpc}
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id"
// +kubebuilder:printcolumn:name="TUNNEL",type="string",JSONPath=".spec.forProvider.tunnelRef.name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type VPCService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VPCServiceSpec `json:"spec"`
	// +optional
	Status VPCServiceStatus `json:"status,omitempty"`
}

// GetResourceSpec implements commonv1alpha1.Managed.
func (o *VPCService) GetResourceSpec() *commonv1alpha1.ResourceSpec { return &o.Spec.ResourceSpec }

// GetResourceStatus implements commonv1alpha1.Managed.
func (o *VPCService) GetResourceStatus() *commonv1alpha1.ResourceStatus {
	return &o.Status.ResourceStatus
}

// ServiceName returns the Cloudflare name: spec.forProvider.name or metadata.name.
func (o *VPCService) ServiceName() string {
	if o.Spec.ForProvider != nil && o.Spec.ForProvider.Name != "" {
		return o.Spec.ForProvider.Name
	}
	return o.Name
}

// TunnelRefName returns spec.forProvider.tunnelRef.name, or "".
func (o *VPCService) TunnelRefName() string {
	if o.Spec.ForProvider == nil || o.Spec.ForProvider.TunnelRef == nil {
		return ""
	}
	return o.Spec.ForProvider.TunnelRef.Name
}

// VPCServiceList is a list of VPCService.
//
// +kubebuilder:object:root=true
type VPCServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VPCService `json:"items"`
}

var _ commonv1alpha1.Managed = &VPCService{}

func init() {
	SchemeBuilder.Register(&VPCService{}, &VPCServiceList{})
}
