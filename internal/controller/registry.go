// Package controller is the registry through which controllers and API schemes join the
// manager (cmd/manager) and the test harness (internal/testenv).
//
// To add a controller (later workstreams), register it from an init function in the
// controller's package:
//
//	func init() {
//		controller.Register(controller.Registration{
//			Name:        "kvnamespace",
//			AddToScheme: kvv1alpha1.AddToScheme,
//			Setup: func(mgr ctrl.Manager, d controller.Deps) error {
//				return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger}).SetupWithManager(mgr)
//			},
//		})
//	}
//
// then blank-import the package in cmd/manager/controllers.go. Tests import the package
// (registering it) and run it with testenv's Env.StartManager(t, testenv.ManagerOptions{
// Controllers: []string{"kvnamespace"}}); testenv.Start picks up every registered scheme.
//
// Put kubebuilder RBAC markers on the reconciler; `make manifests` collects them from
// ./internal/controller/... into config/rbac. A Registration may also carry only a scheme
// (Setup nil), e.g. for API groups whose controllers are registered elsewhere.
//
// Deps carries the shared runtime: the Accounts cache/resolver, the ownership Tagger (a
// NoopTagger when --ownership-tags=false) and the cluster name used in owner tags.
//
// Leader election needs leases:
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
package controller

import (
	"fmt"
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	"flare.dev/operator/internal/reconcile"
)

// Deps is the shared runtime handed to every controller's Setup.
type Deps struct {
	Accounts    *reconcile.Accounts
	Tagger      reconcile.Tagger
	ClusterName string
}

// Registration describes one controller (and/or API scheme).
type Registration struct {
	// Name is unique; it orders setup and names the controller in logs.
	Name string
	// AddToScheme registers the controller's API types (optional).
	AddToScheme func(*runtime.Scheme) error
	// Setup adds the controller to the manager (optional).
	Setup func(ctrl.Manager, Deps) error
}

var (
	mu   sync.Mutex
	regs = map[string]Registration{}
)

// Register adds r. It panics on a duplicate name (a programming error caught at start-up).
func Register(r Registration) {
	mu.Lock()
	defer mu.Unlock()
	if r.Name == "" {
		panic("controller.Register: empty name")
	}
	if _, dup := regs[r.Name]; dup {
		panic(fmt.Sprintf("controller.Register: duplicate registration %q", r.Name))
	}
	regs[r.Name] = r
}

// Registrations returns all registrations sorted by name.
func Registrations() []Registration {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Registration, 0, len(regs))
	for _, r := range regs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddToScheme registers every registered API group with s.
func AddToScheme(s *runtime.Scheme) error {
	for _, r := range Registrations() {
		if r.AddToScheme == nil {
			continue
		}
		if err := r.AddToScheme(s); err != nil {
			return fmt.Errorf("%s: add to scheme: %w", r.Name, err)
		}
	}
	return nil
}

// SetupAll adds every registered controller to mgr. If only is non-empty, just those names
// are set up (tests use this to run a subset).
func SetupAll(mgr ctrl.Manager, d Deps, only ...string) error {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	subset := len(want) > 0 // want shrinks below: decide once, or the rest would all pass
	for _, r := range Registrations() {
		if r.Setup == nil || (subset && !want[r.Name]) {
			continue
		}
		if err := r.Setup(mgr, d); err != nil {
			return fmt.Errorf("%s: setup: %w", r.Name, err)
		}
		delete(want, r.Name)
	}
	for n := range want {
		return fmt.Errorf("controller %q is not registered", n)
	}
	return nil
}
