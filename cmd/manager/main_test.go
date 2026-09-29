package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagerOptionsControllerDefaults(t *testing.T) {
	opts := managerOptions(Options{MaxConcurrentReconciles: 4, ReconcileTimeout: 2 * time.Minute}, nil)
	if opts.Controller.MaxConcurrentReconciles != 4 || opts.Controller.ReconciliationTimeout != 2*time.Minute {
		t.Errorf("Controller = %+v, want 4 workers and a 2m reconcile timeout", opts.Controller)
	}
	if opts := managerOptions(Options{}, nil); opts.Controller.MaxConcurrentReconciles != 1 {
		t.Errorf("zero MaxConcurrentReconciles gave %d workers, want 1", opts.Controller.MaxConcurrentReconciles)
	}
}

func TestOptionsValidate(t *testing.T) {
	ok := Options{MaxConcurrentReconciles: 1, ReconcileTimeout: DefaultReconcileTimeout, CloudflareTimeout: DefaultCloudflareTimeout}
	if err := ok.Validate(); err != nil {
		t.Errorf("defaults: %v", err)
	}
	for name, mut := range map[string]func(*Options){
		"negative poll":              func(o *Options) { o.PollInterval = -time.Second },
		"poll below the minimum":     func(o *Options) { o.PollInterval = time.Second },
		"zero workers":               func(o *Options) { o.MaxConcurrentReconciles = 0 },
		"negative reconcile timeout": func(o *Options) { o.ReconcileTimeout = -1 },
		"negative request timeout":   func(o *Options) { o.CloudflareTimeout = -1 },
	} {
		o := ok
		mut(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c := (Options{}).HTTPClient(); c.Timeout != DefaultCloudflareTimeout {
		t.Errorf("default HTTP timeout %v", c.Timeout)
	}
}

// The chart passes every resilience flag from its reconcile values.
func TestChartPassesReconcileFlags(t *testing.T) {
	b, err := os.ReadFile("../../charts/flare-operator/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := os.ReadFile("../../charts/flare-operator/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for flag, key := range map[string]string{"--poll-interval=": "pollInterval:", "--max-concurrent-reconciles=": "maxConcurrentReconciles:",
		"--reconcile-timeout=": "timeout:", "--cloudflare-request-timeout=": "cloudflareRequestTimeout:"} {
		if !strings.Contains(string(b), flag) {
			t.Errorf("deployment.yaml does not pass %s", flag)
		}
		if !strings.Contains(string(v), key) {
			t.Errorf("values.yaml has no reconcile.%s", key)
		}
	}
}

func TestManagerOptionsReleaseLease(t *testing.T) {
	opts := managerOptions(Options{LeaderElect: true, LeaderElectNS: "flare-system"}, nil)
	if !opts.LeaderElection || !opts.LeaderElectionReleaseOnCancel {
		t.Errorf("LeaderElection=%v LeaderElectionReleaseOnCancel=%v, want both true", opts.LeaderElection, opts.LeaderElectionReleaseOnCancel)
	}
	if opts.GracefulShutdownTimeout == nil || *opts.GracefulShutdownTimeout != GracefulShutdownTimeout {
		t.Errorf("GracefulShutdownTimeout = %v, want %v", opts.GracefulShutdownTimeout, GracefulShutdownTimeout)
	}
}

// The chart's pod must outlive the manager's graceful shutdown, or it is killed before it
// releases the leader lease.
func TestChartGracePeriodExceedsShutdownTimeout(t *testing.T) {
	b, err := os.ReadFile("../../charts/flare-operator/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*terminationGracePeriodSeconds:\s*(\d+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no literal terminationGracePeriodSeconds in the chart's Deployment")
	}
	n, _ := strconv.Atoi(string(m[1]))
	if grace := time.Duration(n) * time.Second; grace <= GracefulShutdownTimeout+2*time.Second {
		t.Errorf("chart terminationGracePeriodSeconds %v leaves too little time after the %v graceful shutdown to release the lease", grace, GracefulShutdownTimeout)
	}
}
