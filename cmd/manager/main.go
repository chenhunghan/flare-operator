// Command manager runs the flare-operator controllers.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/version"
)

// Options are the manager's command-line settings.
type Options struct {
	MetricsAddr     string
	ProbeAddr       string
	LeaderElect     bool
	LeaderElectNS   string
	ClusterName     string
	OwnershipTags   bool
	UserAgent       string
	OnlyControllers stringList
	// AllowBaseURLOverride honours any CloudflareAccount spec.baseURL; AllowedBaseURLs only
	// those listed. Both default to off: an override sends the account's token elsewhere.
	AllowBaseURLOverride bool
	AllowedBaseURLs      stringList

	// Resilience and scale (docs/resilience.md).
	//
	// PollInterval is the drift-poll interval of every managed kind (0: each controller's
	// default, 5m for generated kinds, 10m for Tunnel, VPCService and WorkerScript).
	PollInterval time.Duration
	// MaxConcurrentReconciles is the number of parallel workers per controller.
	MaxConcurrentReconciles int
	// ReconcileTimeout bounds one reconcile (its context deadline); 0 disables it.
	ReconcileTimeout time.Duration
	// CloudflareTimeout bounds one Cloudflare HTTP request (connect to last body byte).
	CloudflareTimeout time.Duration
}

// Defaults of the resilience flags.
const (
	DefaultMaxConcurrentReconciles = 1
	DefaultReconcileTimeout        = 5 * time.Minute
	DefaultCloudflareTimeout       = 60 * time.Second
	// MinPollInterval bounds --poll-interval from below: each poll costs Cloudflare API calls.
	MinPollInterval = 10 * time.Second
)

// Validate rejects flag values the manager cannot run with.
func (o Options) Validate() error {
	switch {
	case o.PollInterval < 0:
		return fmt.Errorf("poll-interval must not be negative")
	case o.PollInterval > 0 && o.PollInterval < MinPollInterval:
		return fmt.Errorf("poll-interval %s is below the %s minimum (each poll costs Cloudflare API calls)", o.PollInterval, MinPollInterval)
	case o.MaxConcurrentReconciles < 1:
		return fmt.Errorf("max-concurrent-reconciles must be at least 1")
	case o.ReconcileTimeout < 0 || o.CloudflareTimeout < 0:
		return fmt.Errorf("timeouts must not be negative")
	}
	return nil
}

// HTTPClient is the HTTP client of every Cloudflare API client (CloudflareTimeout per request).
func (o Options) HTTPClient() *http.Client {
	t := o.CloudflareTimeout
	if t == 0 {
		t = DefaultCloudflareTimeout
	}
	return &http.Client{Timeout: t}
}

// BaseURLPolicy returns the spec.baseURL policy selected by the flags.
func (o Options) BaseURLPolicy() reconcile.BaseURLPolicy {
	return reconcile.BaseURLPolicy{AllowAny: o.AllowBaseURLOverride, Allowed: o.AllowedBaseURLs}
}

type stringList []string

func (s *stringList) String() string { return fmt.Sprint(*s) }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// GracefulShutdownTimeout bounds how long the manager waits for running reconciles after
// SIGTERM before it releases the leader lease and exits. It must stay below the chart's
// terminationGracePeriodSeconds (charts/flare-operator/templates/deployment.yaml), or the
// kubelet kills the pod before the lease is released and the next manager waits for it to
// expire (LeaseDuration, 15 s).
const GracefulShutdownTimeout = 5 * time.Second

// managerOptions are the controller-runtime manager options for o.
//
// LeaderElectionReleaseOnCancel gives up the lease as soon as the manager stops, so a
// restarted or rolled manager takes over at once instead of after the lease expires. That is
// safe only because the process exits right after mgr.Start returns (run → main), so nothing
// keeps acting as leader once the lease is released.
func managerOptions(o Options, scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress:        o.ProbeAddr,
		LeaderElection:                o.LeaderElect,
		LeaderElectionID:              "flare-operator.cloudflare.flare.dev",
		LeaderElectionNamespace:       o.LeaderElectNS,
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptr.To(GracefulShutdownTimeout),
		// The cache holds Secrets, ConfigMaps, Deployments and Services cluster-wide: keep out
		// what the operator never reads (managedFields, Helm release data).
		Cache: cache.Options{DefaultTransform: controller.CacheTransform},
		// Applied to every controller (builder defaults): parallel workers and a context
		// deadline per reconcile, so a hung Cloudflare call cannot hold a worker forever.
		Controller: config.Controller{
			MaxConcurrentReconciles: max(o.MaxConcurrentReconciles, 1),
			ReconciliationTimeout:   o.ReconcileTimeout,
		},
	}
}

// Scheme returns a scheme with client-go types and every registered API group.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := controller.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Tagger returns the ownership tagger selected by the flags.
func (o Options) Tagger() reconcile.Tagger {
	if !o.OwnershipTags {
		return reconcile.NoopTagger{}
	}
	return reconcile.ResourceTagger{}
}

func main() {
	var o Options
	flag.StringVar(&o.MetricsAddr, "metrics-bind-address", ":8080", `metrics endpoint address ("0" disables)`)
	flag.StringVar(&o.ProbeAddr, "health-probe-bind-address", ":8081", "health (/healthz) and readiness (/readyz) probe address")
	flag.BoolVar(&o.LeaderElect, "leader-elect", false, "enable leader election (required with more than one replica)")
	flag.StringVar(&o.LeaderElectNS, "leader-election-namespace", "", "namespace of the leader-election lease (default: in-cluster namespace)")
	flag.StringVar(&o.ClusterName, "cluster-name", "default", "cluster identity used in ownership tags (flare.dev/owner=<cluster>/<ns>/<name>)")
	flag.BoolVar(&o.OwnershipTags, "ownership-tags", true, "tag managed Cloudflare resources with flare.dev/owner through Resource Tagging")
	flag.StringVar(&o.UserAgent, "user-agent", "flare-operator", "User-Agent for Cloudflare API calls")
	flag.Var(&o.OnlyControllers, "controller", "run only this controller (repeatable; default: all registered)")
	flag.BoolVar(&o.AllowBaseURLOverride, "allow-base-url-override", false,
		"honour any CloudflareAccount spec.baseURL (e.g. flarefake in tests); off by default because an override sends the account's API token to that URL")
	flag.Var(&o.AllowedBaseURLs, "allowed-base-url", "a CloudflareAccount spec.baseURL to honour (repeatable; exact match, trailing slash ignored)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.DurationVar(&o.PollInterval, "poll-interval", 0,
		"drift-poll interval of managed objects (0: controller defaults, 5m generated kinds, 10m Tunnel/VPCService/WorkerScript; minimum 10s); each poll costs about 2 Cloudflare API calls per object")
	flag.IntVar(&o.MaxConcurrentReconciles, "max-concurrent-reconciles", DefaultMaxConcurrentReconciles,
		"parallel reconciles per controller (Cloudflare calls still share each token's rate limit)")
	flag.DurationVar(&o.ReconcileTimeout, "reconcile-timeout", DefaultReconcileTimeout, "context deadline of one reconcile (0 disables)")
	flag.DurationVar(&o.CloudflareTimeout, "cloudflare-request-timeout", DefaultCloudflareTimeout, "timeout of one Cloudflare API HTTP request")
	zo := zap.Options{Development: false}
	zo.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get().String("flare-operator"))
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zo)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("flare-operator", version.Get().KeysAndValues()...)
	if err := o.Validate(); err != nil {
		setupLog.Error(err, "invalid flags")
		os.Exit(2)
	}

	if err := run(o); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

func run(o Options) error {
	scheme, err := Scheme()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions(o, scheme))
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	deps := controller.Deps{
		Accounts: reconcile.NewAccounts(mgr.GetClient(), reconcile.WithUserAgent(o.UserAgent), reconcile.WithBaseURLPolicy(o.BaseURLPolicy()),
			reconcile.WithHTTPClient(o.HTTPClient())),
		Tagger:       o.Tagger(),
		ClusterName:  o.ClusterName,
		PollInterval: o.PollInterval,
	}
	if err := controller.SetupAll(mgr, deps, o.OnlyControllers...); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting manager", "version", version.Version, "controllers", len(controller.Registrations()), "cluster", o.ClusterName)
	return mgr.Start(ctrl.SetupSignalHandler())
}
