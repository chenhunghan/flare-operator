package main

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

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
