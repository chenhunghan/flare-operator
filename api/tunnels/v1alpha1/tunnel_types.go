package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
)

// DefaultCloudflaredImage is the connector image used when spec.connector.image is empty. The
// tag is the cloudflared version that ran in the in-cluster spike (recordings 0175 and 0188:
// client_version 2026.9.3, linux_arm64).
const DefaultCloudflaredImage = "cloudflare/cloudflared:2026.9.3"

// DefaultConnectorReplicas is the number of cloudflared replicas when spec.connector.replicas
// is unset.
const DefaultConnectorReplicas int32 = 2

// TunnelParameters are the Cloudflare fields of a Tunnel: the request body of
// POST /accounts/{account_id}/cfd_tunnel. config_src is always "cloudflare" (remotely managed):
// cloudflared runs with only a token and no local configuration.
type TunnelParameters struct {
	// A user-friendly name for the tunnel. Defaults to metadata.name. It is used to adopt an
	// existing (not deleted) tunnel of the same name when no external-id annotation is set.
	// Renaming is not supported yet: a change is reported as Synced=False, reason Immutable.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Name string `json:"name,omitempty"`
}

// ConnectorSpec configures the cloudflared Deployment that connects the tunnel.
type ConnectorSpec struct {
	// Image of cloudflared. Defaults to DefaultCloudflaredImage.
	// +optional
	Image string `json:"image,omitempty"`
	// ImagePullPolicy of the cloudflared container.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// Replicas of cloudflared (each opens 4 connections to the edge). Defaults to 2.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// Resources of the cloudflared container. Defaults to requests cpu=10m, memory=32Mi and a
	// memory limit of 256Mi (the spike measured 4–11m CPU and about 19Mi).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// NodeSelector of the cloudflared pods.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations of the cloudflared pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// TunnelNetworkPolicy configures the egress NetworkPolicy of the cloudflared pods.
type TunnelNetworkPolicy struct {
	// Disabled turns off the generated NetworkPolicy. Without it cloudflared can reach every
	// address the pod can reach: tunnel ingress rules and warp-routing do not limit Workers VPC
	// traffic (docs/spike-results-2026-09-29.md §2).
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// ExcludeCIDRs are removed from the 0.0.0.0/0 rules (the Cloudflare edge on port 7844 and
	// external backends). Set them to the cluster's pod and service CIDRs so those rules do not
	// open in-cluster destinations.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=18
	// +kubebuilder:validation:items:XValidation:rule="isCIDR(self) && cidr(self).ip().family() == 4",message="must be an IPv4 CIDR"
	ExcludeCIDRs []string `json:"excludeCIDRs,omitempty"`
	// AllowHTTPS also allows egress on TCP 443 to 0.0.0.0/0. Not needed: cloudflared's
	// api.cloudflare.com pre-check fails softly and the tunnel uses port 7844 (spike §2.1).
	// +optional
	AllowHTTPS bool `json:"allowHTTPS,omitempty"`
}

// TunnelSpec defines the desired state of a Tunnel.
type TunnelSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	// ForProvider holds the Cloudflare API fields, named exactly as in the API.
	// +optional
	ForProvider TunnelParameters `json:"forProvider,omitempty"`
	// Connector configures the cloudflared Deployment. It is not managed when
	// managementPolicies is ["Observe"].
	// +optional
	Connector ConnectorSpec `json:"connector,omitempty"`
	// NetworkPolicy configures the generated egress NetworkPolicy of the cloudflared pods.
	// +optional
	NetworkPolicy TunnelNetworkPolicy `json:"networkPolicy,omitempty"`
}

// TunnelObservation is the tunnel as returned by GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}.
type TunnelObservation struct {
	// UUID of the tunnel.
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// Status: inactive (never connected), healthy, degraded or down.
	// +optional
	Status string `json:"status,omitempty"`
	// "cloudflare" for remotely managed tunnels, "local" otherwise.
	// +optional
	ConfigSrc string `json:"config_src,omitempty"`
	// +optional
	RemoteConfig *bool `json:"remote_config,omitempty"`
	// +optional
	TunType string `json:"tun_type,omitempty"`
	// +optional
	CreatedAt string `json:"created_at,omitempty"`
	// Set once the tunnel has been (soft-)deleted.
	// +optional
	DeletedAt *string `json:"deleted_at,omitempty"`
	// +optional
	ConnsActiveAt *string `json:"conns_active_at,omitempty"`
	// +optional
	ConnsInactiveAt *string `json:"conns_inactive_at,omitempty"`
}

// ConnectorStatus reports the cloudflared Deployment.
type ConnectorStatus struct {
	// DeploymentName is the owned cloudflared Deployment.
	// +optional
	DeploymentName string `json:"deploymentName,omitempty"`
	// TokenSecretName is the owned Secret holding the tunnel token (key "token").
	// +optional
	TokenSecretName string `json:"tokenSecretName,omitempty"`
	// Replicas desired.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// ReadyReplicas as reported by the Deployment.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
}

// NetworkPolicyStatus reports the generated NetworkPolicy.
type NetworkPolicyStatus struct {
	// Name of the owned NetworkPolicy.
	// +optional
	Name string `json:"name,omitempty"`
	// Backends lists, per referencing VPCService, what the policy allows, e.g.
	// "web: pods of Service ns/marker TCP/8080" or "db: external 0.0.0.0/0 TCP/5432".
	// +optional
	Backends []string `json:"backends,omitempty"`
}

// TunnelStatus defines the observed state of a Tunnel.
type TunnelStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	// AtProvider is the tunnel as last read from the Cloudflare API.
	// +optional
	AtProvider TunnelObservation `json:"atProvider,omitempty"`
	// Connector reports the cloudflared Deployment.
	// +optional
	Connector ConnectorStatus `json:"connector,omitempty"`
	// NetworkPolicy reports the generated NetworkPolicy.
	// +optional
	NetworkPolicy NetworkPolicyStatus `json:"networkPolicy,omitempty"`
}

// Tunnel is a remotely managed Cloudflare Tunnel (x-fern-sdk-group-name "tunnels") with its
// cloudflared connector.
//
// Created at /accounts/{account_id}/cfd_tunnel, managed at /accounts/{account_id}/cfd_tunnel/{id}
// (account scope). Default deletion policy: Delete. Deleting the Cloudflare tunnel is a soft
// delete (deleted_at is set, GET still answers 200); it is refused (400/1022) while cloudflared
// is connected, so the controller first scales the connector to zero.
//
// The name is limited to 63 characters because it is a label value on the owned objects.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={cloudflare,tunnels}
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="metadata.name must be at most 63 characters"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id"
// +kubebuilder:printcolumn:name="STATUS",type="string",JSONPath=".status.atProvider.status"
// +kubebuilder:printcolumn:name="CONNECTORS",type="integer",JSONPath=".status.connector.readyReplicas"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type Tunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TunnelSpec `json:"spec"`
	// +optional
	Status TunnelStatus `json:"status,omitempty"`
}

// GetResourceSpec implements commonv1alpha1.Managed.
func (o *Tunnel) GetResourceSpec() *commonv1alpha1.ResourceSpec { return &o.Spec.ResourceSpec }

// GetResourceStatus implements commonv1alpha1.Managed.
func (o *Tunnel) GetResourceStatus() *commonv1alpha1.ResourceStatus { return &o.Status.ResourceStatus }

// TunnelName returns the Cloudflare name of the tunnel: spec.forProvider.name or metadata.name.
func (o *Tunnel) TunnelName() string {
	if o.Spec.ForProvider.Name != "" {
		return o.Spec.ForProvider.Name
	}
	return o.Name
}

// TunnelList is a list of Tunnel.
//
// +kubebuilder:object:root=true
type TunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tunnel `json:"items"`
}

var _ commonv1alpha1.Managed = &Tunnel{}

func init() {
	SchemeBuilder.Register(&Tunnel{}, &TunnelList{})
}
