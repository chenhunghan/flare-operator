package vk

import (
	"context"
	"crypto/tls"
	"errors"
	"time"

	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// Node identity. Exactly one node per cluster (per release): stand-in Pods of every namespace
// and account are bound to it, and credentials are resolved per Pod.
const (
	// DefaultNodeName is the virtual node's name (flag --node-name, chart workersLogs.nodeName).
	DefaultNodeName = "cf-workers"
	// TaintKey and TaintValue form the node's NoSchedule taint
	// virtual-kubelet.io/provider=cloudflare (docs/virtual-kubelet-design.md §3). Stand-in Pods
	// tolerate it; nothing else lands on the node by accident.
	TaintKey   = "virtual-kubelet.io/provider"
	TaintValue = "cloudflare"
	// LabelNodeType is the conventional virtual-kubelet node label (type=virtual-kubelet).
	// The node deliberately has no kubernetes.io/os or kubernetes.io/arch label, so DaemonSets
	// that select kubernetes.io/os=linux and tolerate every taint (k0s kube-proxy, kube-router)
	// do not target it.
	LabelNodeType      = "type"
	LabelNodeTypeValue = "virtual-kubelet"
	// LabelNodeRole marks the node as the Workers virtual node of flare-operator.
	LabelNodeRole      = "flare.dev/virtual-node"
	LabelNodeRoleValue = "workers"
	// DefaultListenPort is the kubelet API port (status.daemonEndpoints.kubeletEndpoint.Port).
	// With hostNetwork the chart uses DefaultHostNetworkPort so as not to clash with the real
	// kubelet.
	DefaultListenPort      = 10250
	DefaultHostNetworkPort = 10260
	// LeaderElectionID is the Lease (release namespace) that elects the one active replica.
	LeaderElectionID = "flare-operator-workers-vk.flare.dev"
	// DefaultAPIBudget is the virtual kubelet's own Cloudflare request budget per token per 5
	// minutes, separate from the manager's limiter (it is another process): the manager's
	// default of 1080 leaves 120 of Cloudflare's 1200.
	DefaultAPIBudget = 120
)

// TLSMode chooses how the kubelet API gets its serving certificate.
type TLSMode string

// TLS modes.
const (
	// TLSModeCSR (default) requests a kubernetes.io/kubelet-serving certificate for
	// CN=system:node:<node>, O=system:nodes and SANs <node> and the node's InternalIP, approves
	// its own CSR when TLSConfig.Approve is set, and renews at 80% of its lifetime or when the
	// address changes. Works where kube-apiserver verifies kubelet certificates
	// (--kubelet-certificate-authority, as k0s does) and where it does not.
	TLSModeCSR TLSMode = "csr"
	// TLSModeSelfSigned generates a self-signed certificate at start. Only for clusters whose
	// kube-apiserver does not verify kubelet serving certificates; metrics-server will refuse
	// to scrape the node unless it runs with --kubelet-insecure-tls.
	TLSModeSelfSigned TLSMode = "selfSigned"
	// TLSModeSecret loads tls.crt/tls.key from a kubernetes.io/tls Secret (e.g. issued by
	// cert-manager) and reloads it when it changes. The SANs must cover the node address.
	TLSModeSecret TLSMode = "secret"
)

// TLSConfig configures the serving certificate.
type TLSConfig struct {
	Mode TLSMode
	// Approve lets the virtual kubelet approve its own CSR (TLSModeCSR). Needs RBAC approve on
	// signers/kubernetes.io/kubelet-serving. Without it an administrator (or an approver that
	// accepts non-node requesters) must approve each CSR; k0s's approver does not (the CSR's
	// requesting user must equal its CN).
	Approve bool
	// Lifetime is the requested certificate lifetime (spec.expirationSeconds; default 24h).
	Lifetime time.Duration
	// SecretNamespace and SecretName name the Secret of TLSModeSecret.
	SecretNamespace string
	SecretName      string
}

// AddressMode chooses the node's InternalIP.
type AddressMode string

// Address modes.
const (
	// AddressPodIP (default) advertises the virtual kubelet Pod's IP (status.podIP).
	// kube-apiserver must be able to reach Pod IPs: true on k0s and kubeadm control planes
	// that run the CNI, and where kube-apiserver reaches nodes through konnectivity.
	AddressPodIP AddressMode = "podIP"
	// AddressHostIP advertises the host's IP (hostNetwork Pods; DefaultHostNetworkPort), for
	// control planes that reach node IPs but not Pod IPs (managed control planes with an
	// overlay CNI).
	AddressHostIP AddressMode = "hostIP"
)

// Config is the virtual kubelet's runtime configuration (cmd/workers-vk flags).
type Config struct {
	// NodeName is the virtual node's name (DefaultNodeName).
	NodeName string
	// Address is the node's InternalIP, from the downward API (POD_IP or HOST_IP per
	// AddressMode).
	Address     string
	AddressMode AddressMode
	// ListenAddr is the kubelet API listen address (":10250").
	ListenAddr string
	TLS        TLSConfig
	// OwnerClusterRole names the release's ClusterRole that owns the Node (garbage collection
	// of the Node when the release goes away). The virtual kubelet refuses to start when the
	// Node exists with a different owner (another release's node of the same name).
	OwnerClusterRole string
	// PodImage is the never-pulled placeholder image of stand-in Pods (default
	// standin.DefaultImage, fixed across releases).
	PodImage string
	// NamespaceSelector (label selector syntax) limits the namespaces that get stand-in Pods
	// ("" = all).
	NamespaceSelector string
	// APIBudget is the Cloudflare request budget per token per 5 minutes (DefaultAPIBudget).
	APIBudget int
	// Logs bounds log requests.
	Logs workerlogs.Limits
}

// PodResolver maps a stand-in Pod to the Worker whose logs it shows. Workstream B implements it
// over the controller-runtime cache (Pods, WorkerScripts, CloudflareAccounts) and a read-only
// reconcile.Accounts (wrapped so that it cannot write the account label; Secrets are read
// uncached with get).
type PodResolver interface {
	// Resolve returns the Target of container in Pod namespace/name. The Pod must be bound to
	// this node, carry the stand-in labels, and be controlled by a WorkerScript of the same
	// namespace whose UID matches its StandInLabelWorkerScriptUID; the script is that
	// WorkerScript's ScriptName() and the credentials those of its accountRef (a Ready
	// CloudflareAccount in the namespace). Errors: ErrPodNotFound, ErrNotStandIn,
	// ErrContainerNotFound, or a *reconcile.AccountError when the account cannot be used.
	Resolve(ctx context.Context, namespace, name, container string) (workerlogs.Target, error)
}

// ServingCert provides the kubelet API's certificate (one implementation per TLSMode).
type ServingCert interface {
	// GetCertificate is tls.Config.GetCertificate; it returns the current certificate.
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	// Run obtains the first certificate and keeps it fresh until ctx ends.
	Run(ctx context.Context) error
	// Ready is closed once a certificate is available; the node reports Ready only after it.
	Ready() <-chan struct{}
}

// Errors of the kubelet API (mapped to HTTP statuses through virtual-kubelet's errdefs).
var (
	// ErrUnsupported: exec, attach, port-forward and run on a stand-in Pod (501).
	ErrUnsupported = errors.New("not supported on Cloudflare Workers stand-in pods: only `kubectl logs` is available")
	// ErrPodNotFound: no such Pod on this node (404).
	ErrPodNotFound = errors.New("pod not found on the Cloudflare Workers virtual node")
	// ErrNotStandIn: the Pod is bound to the node but is not a WorkerScript stand-in (404).
	ErrNotStandIn = errors.New("pod is not a WorkerScript stand-in pod; the Cloudflare Workers virtual node runs nothing else")
	// ErrContainerNotFound: a container other than StandInContainerName (404).
	ErrContainerNotFound = errors.New(`stand-in pods have one container, "worker"`)
)
