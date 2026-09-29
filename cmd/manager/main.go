// Command manager runs the flare-operator controllers.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/reconcile"
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
	zo := zap.Options{Development: false}
	zo.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zo)))
	setupLog := ctrl.Log.WithName("setup")

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
		Accounts:    reconcile.NewAccounts(mgr.GetClient(), reconcile.WithUserAgent(o.UserAgent), reconcile.WithBaseURLPolicy(o.BaseURLPolicy())),
		Tagger:      o.Tagger(),
		ClusterName: o.ClusterName,
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
	ctrl.Log.WithName("setup").Info("starting manager", "controllers", len(controller.Registrations()), "cluster", o.ClusterName)
	return mgr.Start(ctrl.SetupSignalHandler())
}
