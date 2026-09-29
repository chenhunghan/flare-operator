package testenv

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
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
}

// Manager is a running controller manager.
type Manager struct {
	ctrl.Manager
	Deps controller.Deps
	// Client is the manager's cached client.
	Client client.Client
}

// StartManager starts a manager against the envtest API server; it is stopped at the end of t.
// Several managers may run in one test binary (controller name validation is off), but two
// managers running the same controllers race each other, so start one per test.
func (e *Env) StartManager(t testing.TB, o ManagerOptions) *Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Scheme:                 e.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
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
	deps := controller.Deps{
		Accounts:    reconcile.NewAccounts(mgr.GetClient(), reconcile.WithUserAgent("flare-operator-testenv"), reconcile.WithBaseURLPolicy(policy)),
		Tagger:      o.Tagger,
		ClusterName: o.ClusterName,
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
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("manager: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("manager did not stop within 30s")
		}
	})
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatalf("manager cache did not sync")
	}
	return &Manager{Manager: mgr, Deps: deps, Client: mgr.GetClient()}
}
