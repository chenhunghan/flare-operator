package testenv

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/account"
	"flare.dev/operator/internal/reconcile"
)

// ManagerOptions configures StartManager.
type ManagerOptions struct {
	// Controllers are registered controller names to run besides the CloudflareAccount
	// controller (which always runs).
	Controllers []string
	// Setup adds unregistered controllers.
	Setup []func(ctrl.Manager, controller.Deps) error
	// Tagger defaults to reconcile.ResourceTagger{} (the fake emulates Resource Tagging).
	Tagger reconcile.Tagger
	// ClusterName defaults to "testenv".
	ClusterName string
	// AccountVerifyInterval overrides the CloudflareAccount re-verify interval.
	AccountVerifyInterval time.Duration
	// AccountDependencyRequeue overrides how often a CloudflareAccount deletion blocked by
	// managed objects is re-checked (default 1s in tests).
	AccountDependencyRequeue time.Duration
	// BaseURLPolicy defaults to allowing every spec.baseURL (accounts point at flarefake).
	BaseURLPolicy *reconcile.BaseURLPolicy
	// AccountsOptions are appended to the options of the shared reconcile.Accounts, e.g.
	// reconcile.WithHTTPClient to observe or record every Cloudflare request (test/live).
	AccountsOptions []reconcile.AccountsOption
	// MetricsBindAddress serves the manager's Prometheus metrics (default "0": off).
	MetricsBindAddress string
	// MaxConcurrentReconciles and ReconcileTimeout are the manager-wide controller defaults
	// (cmd/manager --max-concurrent-reconciles, --reconcile-timeout); zero keeps
	// controller-runtime's (1 worker, no deadline).
	MaxConcurrentReconciles int
	ReconcileTimeout        time.Duration
	// PollInterval is Deps.PollInterval (cmd/manager --poll-interval) for registered controllers.
	PollInterval time.Duration
	// Namespaces restricts the manager's cache (and so its controllers) to these existing
	// namespaces, so tests with their own managers can run in parallel without reconciling
	// each other's objects. Empty watches every namespace.
	Namespaces []string
}

// Manager is a running controller manager.
type Manager struct {
	ctrl.Manager
	Deps controller.Deps
	// Client is the manager's cached client.
	Client client.Client

	stopOnce sync.Once
	stop     func() error
}

// Stop stops the manager and waits for it to exit (at most 30s), as a crash or restart would;
// the cleanup at the end of the test then does nothing. Crash-consistency tests stop one
// manager and start another on the same objects.
func (m *Manager) Stop(t testing.TB) {
	t.Helper()
	var err error
	m.stopOnce.Do(func() { err = m.stop() })
	if err != nil {
		t.Errorf("manager: %v", err)
	}
}

// StartManager starts a manager against the envtest API server; it is stopped at the end of t.
// Several managers may run in one test binary (controller name validation is off), but two
// managers running the same controllers race each other, so start one per test.
func (e *Env) StartManager(t testing.TB, o ManagerOptions) *Manager {
	t.Helper()
	metricsAddr := o.MetricsBindAddress
	if metricsAddr == "" {
		metricsAddr = "0"
	}
	var cacheOpts cache.Options
	if len(o.Namespaces) > 0 {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range o.Namespaces {
			cacheOpts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Cache:                  cacheOpts,
		Scheme:                 e.Scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: "0",
		Controller: config.Controller{SkipNameValidation: ptr.To(true), MaxConcurrentReconciles: o.MaxConcurrentReconciles,
			ReconciliationTimeout: o.ReconcileTimeout},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if o.Tagger == nil {
		o.Tagger = reconcile.ResourceTagger{}
	}
	if o.ClusterName == "" {
		o.ClusterName = "testenv"
	}
	if o.AccountDependencyRequeue == 0 {
		o.AccountDependencyRequeue = time.Second
	}
	// CloudflareAccounts point at the in-process flarefake: the equivalent of the manager's
	// --allow-base-url-override.
	policy := reconcile.BaseURLPolicy{AllowAny: true}
	if o.BaseURLPolicy != nil {
		policy = *o.BaseURLPolicy
	}
	aopts := append([]reconcile.AccountsOption{reconcile.WithUserAgent("flare-operator-testenv"), reconcile.WithBaseURLPolicy(policy)},
		o.AccountsOptions...)
	deps := controller.Deps{
		Accounts:     reconcile.NewAccounts(mgr.GetClient(), aopts...),
		Tagger:       o.Tagger,
		ClusterName:  o.ClusterName,
		PollInterval: o.PollInterval,
	}
	ar := &account.Reconciler{Client: mgr.GetClient(), Accounts: deps.Accounts, VerifyInterval: o.AccountVerifyInterval,
		DependencyRequeue: o.AccountDependencyRequeue}
	if err := ar.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup account controller: %v", err)
	}
	var names []string
	for _, n := range o.Controllers {
		if n != account.Name {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		if err := controller.SetupAll(mgr, deps, names...); err != nil {
			t.Fatalf("setup controllers: %v", err)
		}
	}
	for _, s := range o.Setup {
		if err := s(mgr, deps); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	m := &Manager{Manager: mgr, Deps: deps, Client: mgr.GetClient()}
	m.stop = func() error {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		case <-time.After(30 * time.Second):
			return errors.New("manager did not stop within 30s")
		}
	}
	t.Cleanup(func() { m.Stop(t) })
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatalf("manager cache did not sync")
	}
	return m
}
