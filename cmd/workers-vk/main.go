// Command workers-vk runs the Workers virtual kubelet: one virtual node (default cf-workers)
// whose kubelet API serves `kubectl logs` for the stand-in Pods of WorkerScripts, with the
// stand-in controller that keeps those Pods. Design: docs/workers-logs-design.md. It ships in
// the operator image beside /manager and runs as its own Deployment (chart value
// workersLogs.enabled).
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/version"
	"github.com/chenhunghan/flare-operator/internal/vk"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

// Environment variables the chart sets from the downward API.
const (
	EnvPodIP        = "POD_IP"
	EnvHostIP       = "HOST_IP"
	EnvPodNamespace = "POD_NAMESPACE"
)

// DefaultCloudflareTimeout bounds one Cloudflare API request (a log query; a follow's tail
// WebSocket is not bounded by it).
const DefaultCloudflareTimeout = 60 * time.Second

// AuthQPS and AuthBurst rate-limit the kubelet API's TokenReview and SubjectAccessReview client.
const (
	AuthQPS   = 10
	AuthBurst = 20
)

// GracefulShutdownTimeout bounds the wait for runnables after SIGTERM; it must stay below the
// chart's terminationGracePeriodSeconds.
const GracefulShutdownTimeout = 10 * time.Second

// Options are the command-line settings.
type Options struct {
	VK vk.Config

	// PodLabels, PodCPU and PodMemory shape stand-in Pods (standin.PodConfig).
	PodLabels stringMap
	PodCPU    string
	PodMemory string

	MetricsAddr   string
	ProbeAddr     string
	LeaderElect   bool
	LeaderElectNS string
	UserAgent     string
	// AllowBaseURLOverride and AllowedBaseURLs are the manager's spec.baseURL policy flags; the
	// virtual kubelet must refuse the same overrides the manager refuses.
	AllowBaseURLOverride bool
	AllowedBaseURLs      stringList
	CloudflareTimeout    time.Duration

	// Server bounds the kubelet API (connections, log requests, follow sessions, stalls).
	Server vk.ServerLimits
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// stringMap is a repeatable key=value flag.
type stringMap map[string]string

func (m *stringMap) String() string {
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k+"="+(*m)[k])
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (m *stringMap) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("%q: want key=value", v)
	}
	if errs := validation.IsQualifiedName(k); len(errs) > 0 {
		return fmt.Errorf("label key %q: %s", k, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(val); len(errs) > 0 {
		return fmt.Errorf("label value %q: %s", val, strings.Join(errs, "; "))
	}
	if *m == nil {
		*m = stringMap{}
	}
	(*m)[k] = val
	return nil
}

// BindFlags registers every flag on fs. getenv supplies the downward-API defaults.
func (o *Options) BindFlags(fs *flag.FlagSet, getenv func(string) string) {
	c := &o.VK
	fs.StringVar(&c.NodeName, "node-name", vk.DefaultNodeName, "name of the virtual node (one per release)")
	fs.StringVar((*string)(&c.AddressMode), "address-mode", string(vk.AddressPodIP),
		`node InternalIP: "podIP" (this Pod's IP, port 10250) or "hostIP" (hostNetwork, port 10260)`)
	fs.StringVar(&c.Address, "address", "", "node InternalIP (default: $"+EnvPodIP+", or $"+EnvHostIP+" with --address-mode=hostIP)")
	fs.StringVar(&c.ListenAddr, "listen-address", "", "kubelet API listen address (default :10250, or :10260 with --address-mode=hostIP)")
	fs.StringVar((*string)(&c.TLS.Mode), "tls-mode", string(vk.TLSModeCSR),
		`serving certificate: "csr" (kubelet-serving CSR), "selfSigned" or "secret"`)
	fs.BoolVar(&c.TLS.Approve, "tls-approve-csr", true, "approve our own kubelet-serving CSR (needs approve on signers/kubernetes.io/kubelet-serving)")
	fs.DurationVar(&c.TLS.Lifetime, "tls-lifetime", vk.DefaultCSRLifetime, "requested certificate lifetime (spec.expirationSeconds; at least 10m)")
	fs.StringVar(&c.TLS.SecretName, "tls-secret-name", "", "kubernetes.io/tls Secret with the serving certificate (--tls-mode=secret)")
	fs.StringVar(&c.TLS.SecretNamespace, "tls-secret-namespace", getenv(EnvPodNamespace), "namespace of --tls-secret-name (default: $"+EnvPodNamespace+")")
	fs.StringVar(&c.OwnerClusterRole, "owner-cluster-role", "", "the release's ClusterRole that owns the Node (required)")
	fs.StringVar(&c.PodImage, "pod-image", "", "placeholder image of stand-in Pods, never pulled (default standin.DefaultImage, registry.k8s.io/pause)")
	fs.StringVar(&c.NamespaceSelector, "namespace-selector", "", "label selector of the namespaces that get stand-in Pods (empty: all)")
	fs.IntVar(&c.APIBudget, "api-budget", vk.DefaultAPIBudget, "Cloudflare requests per 5 minutes per token for this process (the manager has its own)")
	fs.DurationVar(&c.Logs.DefaultWindow, "logs-default-window", 72*time.Hour, "history window of `kubectl logs` without --since")
	fs.IntVar(&c.Logs.MaxEvents, "logs-max-events", 10000, "most events one `kubectl logs` reads (2000 per Cloudflare request)")
	fs.IntVar(&c.Logs.MaxFollowers, "logs-max-followers", 100, "most concurrent `kubectl logs -f` sessions")
	fs.IntVar(&o.Server.MaxFollowersPerScript, "logs-max-followers-per-script", vk.DefaultMaxFollowersPerScript,
		"most concurrent `kubectl logs -f` sessions of one Worker")
	fs.IntVar(&o.Server.MaxFollowersPerNamespace, "logs-max-followers-per-namespace", vk.DefaultMaxFollowersPerNamespace,
		"most concurrent `kubectl logs -f` sessions in one namespace")
	fs.IntVar(&o.Server.MaxConcurrentRequests, "logs-max-concurrent-requests", vk.DefaultMaxConcurrentRequests,
		"most concurrent `kubectl logs` requests without -f")
	fs.DurationVar(&o.Server.WriteTimeout, "logs-write-timeout", vk.DefaultWriteTimeout,
		"end a log stream whose reader has not accepted data for this long")
	fs.DurationVar(&o.Server.MaxStreamDuration, "logs-max-stream-duration", vk.DefaultMaxStreamDuration,
		"end any log stream (kubectl logs -f) after this long")
	fs.IntVar(&o.Server.MaxConnections, "max-connections", vk.DefaultMaxConnections, "most open connections to the kubelet API")
	fs.Var(&o.PodLabels, "pod-label", "extra label key=value of stand-in Pods (repeatable)")
	fs.StringVar(&o.PodCPU, "pod-cpu", "1m", "CPU request and limit of a stand-in Pod")
	fs.StringVar(&o.PodMemory, "pod-memory", "1Mi", "memory request and limit of a stand-in Pod")
	// Off by default (the chart does not pass it): with hostNetwork a fixed port could clash
	// with the node's own services.
	fs.StringVar(&o.MetricsAddr, "metrics-bind-address", "0", `metrics endpoint address ("0" disables)`)
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", ":8081", "health (/healthz) and readiness (/readyz) probe address")
	fs.BoolVar(&o.LeaderElect, "leader-elect", true, "elect one active replica (Lease "+vk.LeaderElectionID+")")
	fs.StringVar(&o.LeaderElectNS, "leader-election-namespace", "", "namespace of the leader-election Lease (default: in-cluster namespace)")
	fs.StringVar(&o.UserAgent, "user-agent", "flare-operator-workers-vk", "User-Agent for Cloudflare API calls")
	fs.BoolVar(&o.AllowBaseURLOverride, "allow-base-url-override", false, "honour any CloudflareAccount spec.baseURL (flarefake); off by default")
	fs.Var(&o.AllowedBaseURLs, "allowed-base-url", "a CloudflareAccount spec.baseURL to honour (repeatable)")
	fs.DurationVar(&o.CloudflareTimeout, "cloudflare-request-timeout", DefaultCloudflareTimeout, "timeout of one Cloudflare API HTTP request")
}

// Finish fills the defaults that depend on other flags and the environment, then validates.
func (o *Options) Finish(getenv func(string) string) error {
	if o.VK.Address == "" {
		if o.VK.AddressMode == vk.AddressHostIP {
			o.VK.Address = getenv(EnvHostIP)
		} else {
			o.VK.Address = getenv(EnvPodIP)
		}
	}
	o.VK = o.VK.WithDefaults()
	var errs []error
	if err := o.VK.Validate(); err != nil {
		errs = append(errs, err)
	}
	if _, err := o.PodRequests(); err != nil {
		errs = append(errs, err)
	}
	if l := o.Server; l.MaxConnections < 1 || l.MaxConcurrentRequests < 1 || l.MaxFollowersPerScript < 1 ||
		l.MaxFollowersPerNamespace < 1 || l.WriteTimeout <= 0 || l.MaxStreamDuration <= 0 {
		errs = append(errs, errors.New("--max-connections and the --logs-max-* limits must be at least 1, --logs-write-timeout and --logs-max-stream-duration positive"))
	}
	if o.CloudflareTimeout <= 0 {
		errs = append(errs, errors.New("--cloudflare-request-timeout must be positive"))
	}
	return errors.Join(errs...)
}

// PodRequests are the stand-in container's requests (and limits).
func (o *Options) PodRequests() (corev1.ResourceList, error) {
	cpu, err := resource.ParseQuantity(o.PodCPU)
	if err != nil {
		return nil, fmt.Errorf("--pod-cpu: %w", err)
	}
	mem, err := resource.ParseQuantity(o.PodMemory)
	if err != nil {
		return nil, fmt.Errorf("--pod-memory: %w", err)
	}
	return corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem}, nil
}

// PodConfig is the stand-in Pods' shared configuration (an empty Image is defaulted by wire to
// standin.DefaultImage).
func (o *Options) PodConfig() standin.PodConfig {
	req, _ := o.PodRequests()
	return standin.PodConfig{NodeName: o.VK.NodeName, Image: o.VK.PodImage, Requests: req, ExtraLabels: o.PodLabels}
}

// BaseURLPolicy is the spec.baseURL policy of the flags.
func (o *Options) BaseURLPolicy() reconcile.BaseURLPolicy {
	return reconcile.BaseURLPolicy{AllowAny: o.AllowBaseURLOverride, Allowed: o.AllowedBaseURLs}
}

// Scheme has client-go's types, WorkerScripts and CloudflareAccounts.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, workersv1alpha1.AddToScheme, cloudflarev1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// managerOptions: the cache holds only the Pods bound to the node (the stand-in controller and
// the resolver need no others), and Secrets are never cached: token Secrets are read with get,
// so the virtual kubelet needs no list or watch on Secrets.
func managerOptions(o Options, scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress:        o.ProbeAddr,
		LeaderElection:                o.LeaderElect,
		LeaderElectionID:              vk.LeaderElectionID,
		LeaderElectionNamespace:       o.LeaderElectNS,
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptr.To(GracefulShutdownTimeout),
		Cache: cache.Options{
			DefaultTransform: controller.CacheTransform,
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {Field: fields.OneTermEqualSelector("spec.nodeName", o.VK.NodeName)},
			},
		},
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	}
}

func main() {
	var o Options
	o.BindFlags(flag.CommandLine, os.Getenv)
	showVersion := flag.Bool("version", false, "print the version and exit")
	zo := zap.Options{Development: false}
	zo.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get().String("flare-operator workers-vk"))
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zo)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("flare-operator workers-vk", version.Get().KeysAndValues()...)
	if err := o.Finish(os.Getenv); err != nil {
		setupLog.Error(err, "invalid flags")
		os.Exit(2)
	}
	if err := run(o); err != nil {
		setupLog.Error(err, "workers-vk exited")
		os.Exit(1)
	}
}

func run(o Options) error {
	scheme, err := Scheme()
	if err != nil {
		return err
	}
	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, managerOptions(o, scheme))
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	// TokenReviews and SubjectAccessReviews get their own client and rate limit, so callers that
	// force reviews cannot starve the node lease, Pod status and CSR calls.
	authCfg := rest.CopyConfig(restCfg)
	authCfg.QPS, authCfg.Burst = AuthQPS, AuthBurst
	authCS, err := kubernetes.NewForConfig(authCfg)
	if err != nil {
		return err
	}
	nsSel, err := standin.ParseNamespaceSelector(o.VK.NamespaceSelector)
	if err != nil {
		return err
	}
	impl, err := wire(o)
	if err != nil {
		return err
	}
	accounts := reconcile.NewAccounts(vk.ReadOnly(mgr.GetClient()),
		reconcile.WithUserAgent(o.UserAgent),
		reconcile.WithBaseURLPolicy(o.BaseURLPolicy()),
		reconcile.WithHTTPClient(&http.Client{Timeout: o.CloudflareTimeout}),
		reconcile.WithClientFactory(vk.BudgetClientFactory(o.VK.APIBudget, nil)))
	v, err := vk.New(o.VK, vk.Deps{
		Clientset:     cs,
		Cache:         mgr.GetCache(),
		StatusMapper:  impl.StatusMapper,
		Resolver:      vk.NewResolver(mgr.GetClient(), accounts, o.VK.NodeName).WithNamespaceSelector(nsSel),
		AuthClientset: authCS,
		Limits:        o.Server,
		Streamer:      impl.Streamer,
		ParseOptions:  impl.ParseOptions,
		Log:           ctrl.Log.WithName("vk"),
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(v); err != nil {
		return err
	}
	if err := impl.SetupStandIn(mgr); err != nil {
		return fmt.Errorf("stand-in controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting workers-vk", "version", version.Version, "node", o.VK.NodeName,
		"address", o.VK.Address, "tls", o.VK.TLS.Mode)
	return mgr.Start(ctrl.SetupSignalHandler())
}
