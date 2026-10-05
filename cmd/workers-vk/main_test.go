package main

import (
	"flag"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/chenhunghan/flare-operator/internal/vk"
)

func parse(t *testing.T, env map[string]string, args ...string) (Options, error) {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	var o Options
	fs := flag.NewFlagSet("workers-vk", flag.ContinueOnError)
	o.BindFlags(fs, getenv)
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	return o, o.Finish(getenv)
}

func TestFlagsDefaultsFromDownwardAPI(t *testing.T) {
	env := map[string]string{EnvPodIP: "10.42.0.9", EnvHostIP: "192.168.5.15", EnvPodNamespace: "flare-system"}
	o, err := parse(t, env, "--owner-cluster-role=rel-workers-vk", "--pod-image=ghcr.io/x/flare-operator:1", "--pod-label=team=a", "--pod-label=tier=b")
	if err != nil {
		t.Fatal(err)
	}
	c := o.VK
	if c.NodeName != vk.DefaultNodeName || c.Address != "10.42.0.9" || c.ListenAddr != ":10250" || c.TLS.Mode != vk.TLSModeCSR ||
		!c.TLS.Approve || c.TLS.SecretNamespace != "flare-system" || c.APIBudget != vk.DefaultAPIBudget || c.Logs.MaxEvents != 10000 {
		t.Errorf("config %+v", c)
	}
	if l := o.Server; l.MaxFollowersPerScript != 10 || l.MaxFollowersPerNamespace != 25 || l.MaxConcurrentRequests != 32 ||
		l.MaxConnections != 1000 || l.WriteTimeout != time.Minute || l.MaxStreamDuration != 4*time.Hour {
		t.Errorf("server limits %+v", l)
	}
	pc := o.PodConfig()
	if pc.NodeName != vk.DefaultNodeName || pc.Image != "ghcr.io/x/flare-operator:1" || pc.ExtraLabels["team"] != "a" || pc.ExtraLabels["tier"] != "b" ||
		pc.Requests.Cpu().String() != "1m" || pc.Requests.Memory().String() != "1Mi" {
		t.Errorf("pod config %+v", pc)
	}

	o, err = parse(t, env, "--owner-cluster-role=r", "--pod-image=i", "--address-mode=hostIP")
	if err != nil {
		t.Fatal(err)
	}
	if o.VK.Address != "192.168.5.15" || o.VK.ListenAddr != ":10260" {
		t.Errorf("hostIP mode: address %q listen %q", o.VK.Address, o.VK.ListenAddr)
	}
}

func TestFlagsRejected(t *testing.T) {
	env := map[string]string{EnvPodIP: "10.42.0.9"}
	for name, args := range map[string][]string{
		"no owner":       {"--pod-image=i"},
		"no address":     {"--owner-cluster-role=r", "--pod-image=i", "--address-mode=hostIP"},
		"bad tls mode":   {"--owner-cluster-role=r", "--pod-image=i", "--tls-mode=acme"},
		"bad label":      {"--owner-cluster-role=r", "--pod-image=i", "--pod-label=noequals"},
		"bad cpu":        {"--owner-cluster-role=r", "--pod-image=i", "--pod-cpu=lots"},
		"zero budget":    {"--owner-cluster-role=r", "--pod-image=i", "--api-budget=-5"},
		"bad selector":   {"--owner-cluster-role=r", "--pod-image=i", "--namespace-selector=a in ("},
		"secret no name": {"--owner-cluster-role=r", "--pod-image=i", "--tls-mode=secret"},
		"zero followers": {"--owner-cluster-role=r", "--logs-max-followers-per-script=0"},
		"no stream time": {"--owner-cluster-role=r", "--logs-max-stream-duration=0s"},
	} {
		if _, err := parse(t, env, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The cache holds only the node's Pods and never Secrets (token Secrets are read with get, so
// the virtual kubelet needs no list or watch on Secrets).
func TestManagerOptions(t *testing.T) {
	o := Options{VK: vk.Config{NodeName: "cf-workers"}, LeaderElect: true}
	mo := managerOptions(o, nil)
	if mo.LeaderElectionID != vk.LeaderElectionID || !mo.LeaderElection || !mo.LeaderElectionReleaseOnCancel {
		t.Errorf("leader election %v %q", mo.LeaderElection, mo.LeaderElectionID)
	}
	var podField string
	for obj, bo := range mo.Cache.ByObject {
		if _, ok := obj.(*corev1.Pod); ok && bo.Field != nil {
			podField = bo.Field.String()
		}
	}
	if podField != "spec.nodeName=cf-workers" {
		t.Errorf("pod cache field selector %q", podField)
	}
	if mo.Client.Cache == nil || len(mo.Client.Cache.DisableFor) != 1 {
		t.Fatalf("client cache options %+v", mo.Client.Cache)
	}
	if _, ok := mo.Client.Cache.DisableFor[0].(*corev1.Secret); !ok {
		t.Errorf("Secrets are cached: DisableFor %T", mo.Client.Cache.DisableFor[0])
	}
	var _ client.Object = mo.Client.Cache.DisableFor[0]
	if mo.Cache.DefaultTransform == nil {
		t.Error("no cache transform")
	}
}

func TestScheme(t *testing.T) {
	s, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"WorkerScript", "CloudflareAccount", "Pod"} {
		found := false
		for gvk := range s.AllKnownTypes() {
			if gvk.Kind == k {
				found = true
			}
		}
		if !found {
			t.Errorf("scheme lacks %s", k)
		}
	}
	if !strings.Contains(vk.LeaderElectionID, "workers-vk") {
		t.Error(vk.LeaderElectionID)
	}
}
