// Package chart tests charts/flare-operator by rendering it with `helm template` and checking
// the objects: optional resources (PDB, NetworkPolicy, ServiceMonitor), values.schema.json
// rejections, and the manager's restricted-PSS security settings. The tests skip when helm is
// not on PATH (make helm-lint needs it too).
package chart

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func chartDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "charts", "flare-operator")
}

// render runs helm template with --set args and returns the documents by kind, or the error
// output when helm fails.
func render(t *testing.T, sets ...string) (map[string][][]byte, string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH")
	}
	args := []string{"template", "flare-operator", chartDir(t), "-n", "flare-system"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command(helm, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, stderr.String(), err
	}
	docs := map[string][][]byte{}
	for _, d := range strings.Split(out.String(), "\n---") {
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(d), &u.Object); err != nil {
			t.Fatalf("decode rendered document: %v\n%s", err, d)
		}
		if u.GetKind() == "" {
			continue
		}
		docs[u.GetKind()] = append(docs[u.GetKind()], []byte(d))
	}
	return docs, "", nil
}

func mustRender(t *testing.T, sets ...string) map[string][][]byte {
	t.Helper()
	docs, stderr, err := render(t, sets...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", sets, err, stderr)
	}
	return docs
}

func decode[T any](t *testing.T, docs map[string][][]byte, kind string, i int) *T {
	t.Helper()
	if len(docs[kind]) <= i {
		t.Fatalf("no %s #%d rendered (have %d)", kind, i, len(docs[kind]))
	}
	var v T
	if err := yaml.UnmarshalStrict(docs[kind][i], &v); err != nil {
		t.Fatalf("decode %s: %v", kind, err)
	}
	return &v
}

func manager(t *testing.T, docs map[string][][]byte) *appsv1.Deployment {
	t.Helper()
	for i := range docs["Deployment"] {
		d := decode[appsv1.Deployment](t, docs, "Deployment", i)
		if d.Spec.Template.Labels["app.kubernetes.io/component"] == "manager" {
			return d
		}
	}
	t.Fatal("no manager Deployment rendered")
	return nil
}

func TestDefaults(t *testing.T) {
	docs := mustRender(t)
	for _, k := range []string{"PodDisruptionBudget", "NetworkPolicy", "ServiceMonitor"} {
		if len(docs[k]) != 0 {
			t.Errorf("default render has a %s", k)
		}
	}
	d := manager(t, docs)
	if *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %d", *d.Spec.Replicas)
	}
	ps := d.Spec.Template.Spec
	// Restricted Pod Security Standard (https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted).
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot {
		t.Error("pod runAsNonRoot is not true")
	}
	if ps.SecurityContext.SeccompProfile == nil || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod seccompProfile is not RuntimeDefault")
	}
	c := ps.Containers[0]
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation is not false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem is not true")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("privileged is not false")
	}
	if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 {
		t.Errorf("capabilities = %+v, want drop [ALL]", sc.Capabilities)
	}
	if c.Resources.Requests.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Errorf("resources = %+v, want cpu/memory requests and a memory limit", c.Resources)
	}
	if c.LivenessProbe == nil || c.ReadinessProbe == nil {
		t.Error("probes missing")
	}
	if !slices.Contains(c.Args, "--leader-elect=true") {
		t.Errorf("args %v lack --leader-elect=true", c.Args)
	}
	if ps.TopologySpreadConstraints != nil || ps.PriorityClassName != "" {
		t.Error("topologySpreadConstraints / priorityClassName set by default")
	}
}

func TestPodDisruptionBudget(t *testing.T) {
	if docs := mustRender(t, "replicas=1", "podDisruptionBudget.enabled=true"); len(docs["PodDisruptionBudget"]) != 0 {
		t.Error("a PDB was rendered for one replica")
	}
	docs := mustRender(t, "replicas=3")
	pdb := decode[policyv1.PodDisruptionBudget](t, docs, "PodDisruptionBudget", 0)
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 || pdb.Spec.MaxUnavailable != nil {
		t.Errorf("pdb spec = %+v, want minAvailable 1", pdb.Spec)
	}
	if pdb.Spec.Selector.MatchLabels["app.kubernetes.io/component"] != "manager" {
		t.Errorf("pdb selector = %v", pdb.Spec.Selector.MatchLabels)
	}
	docs = mustRender(t, "replicas=3", "podDisruptionBudget.maxUnavailable=34%")
	pdb = decode[policyv1.PodDisruptionBudget](t, docs, "PodDisruptionBudget", 0)
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.String() != "34%" || pdb.Spec.MinAvailable != nil {
		t.Errorf("pdb spec = %+v, want maxUnavailable 34%%", pdb.Spec)
	}
	if docs := mustRender(t, "replicas=3", "podDisruptionBudget.enabled=false"); len(docs["PodDisruptionBudget"]) != 0 {
		t.Error("podDisruptionBudget.enabled=false still renders a PDB")
	}
	if _, stderr, err := render(t, "replicas=2", "leaderElection.enabled=false"); err == nil || !strings.Contains(stderr, "requires leaderElection") {
		t.Errorf("replicas=2 without leader election: err %v, %s", err, stderr)
	}
}

func egressPorts(np *networkingv1.NetworkPolicy) map[int32][]string {
	out := map[int32][]string{}
	for _, r := range np.Spec.Egress {
		var peers []string
		for _, p := range r.To {
			switch {
			case p.IPBlock != nil:
				peers = append(peers, p.IPBlock.CIDR)
			case p.PodSelector != nil:
				peers = append(peers, "pods:"+p.PodSelector.MatchLabels["app.kubernetes.io/component"]+p.PodSelector.MatchLabels["k8s-app"])
			}
		}
		for _, port := range r.Ports {
			out[port.Port.IntVal] = append(out[port.Port.IntVal], peers...)
		}
	}
	return out
}

func TestNetworkPolicy(t *testing.T) {
	if _, stderr, err := render(t, "networkPolicy.enabled=true"); err == nil || !strings.Contains(stderr, "networkPolicy.apiServer.cidrs") {
		t.Fatalf("networkPolicy without apiServer.cidrs: err %v, %s", err, stderr)
	}
	docs := mustRender(t, "networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}")
	np := decode[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", 0)
	if !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) {
		t.Errorf("policyTypes = %v", np.Spec.PolicyTypes)
	}
	if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "manager" {
		t.Errorf("podSelector = %v", np.Spec.PodSelector.MatchLabels)
	}
	ports := egressPorts(np)
	if !slices.Contains(ports[53], "pods:kube-dns") {
		t.Errorf("no DNS egress: %v", ports)
	}
	if !slices.Equal(ports[6443], []string{"10.0.0.1/32"}) {
		t.Errorf("API server egress = %v", ports[6443])
	}
	// api.cloudflare.com resolves into 104.16.0.0/13 (DOCS: https://www.cloudflare.com/ips/).
	if !slices.Contains(ports[443], "104.16.0.0/13") || !slices.Contains(ports[443], "2606:4700::/32") {
		t.Errorf("Cloudflare egress = %v", ports[443])
	}
	if _, ok := ports[8787]; ok {
		t.Error("flarefake egress without flarefake.enabled")
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 0 || np.Spec.Ingress[0].Ports[0].Port.StrVal != "metrics" {
		t.Errorf("ingress = %+v, want the metrics port from anywhere", np.Spec.Ingress)
	}

	docs = mustRender(t, "networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}", "flarefake.enabled=true",
		"networkPolicy.cloudflareAPI.cidrs=null", "metrics.enabled=false")
	np = decode[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", 0)
	ports = egressPorts(np)
	if !slices.Equal(ports[8787], []string{"pods:flarefake"}) {
		t.Errorf("flarefake egress = %v", ports[8787])
	}
	if _, ok := ports[443]; ok {
		t.Errorf("Cloudflare egress with cloudflareAPI.cidrs empty: %v", ports[443])
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("ingress with metrics disabled = %+v", np.Spec.Ingress)
	}
}

func TestServiceMonitor(t *testing.T) {
	docs := mustRender(t, "metrics.serviceMonitor.enabled=true", "metrics.serviceMonitor.interval=30s", "metrics.serviceMonitor.labels.release=kps")
	var sm unstructured.Unstructured
	if err := yaml.Unmarshal(docs["ServiceMonitor"][0], &sm.Object); err != nil {
		t.Fatal(err)
	}
	if sm.GetAPIVersion() != "monitoring.coreos.com/v1" || sm.GetLabels()["release"] != "kps" {
		t.Errorf("servicemonitor = %v", sm.Object)
	}
	eps, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if len(eps) != 1 || eps[0].(map[string]any)["port"] != "metrics" || eps[0].(map[string]any)["interval"] != "30s" {
		t.Errorf("endpoints = %v", eps)
	}
	// The ServiceMonitor selects the metrics Service by the manager selector labels.
	svc := decode[corev1.Service](t, docs, "Service", 0)
	sel, _, _ := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	for k, v := range sel {
		if svc.Labels[k] != v {
			t.Errorf("metrics Service lacks label %s=%s (has %v)", k, v, svc.Labels)
		}
	}
	if _, stderr, err := render(t, "metrics.serviceMonitor.enabled=true", "metrics.service.enabled=false"); err == nil || !strings.Contains(stderr, "requires metrics.enabled") {
		t.Errorf("serviceMonitor without the metrics Service: err %v, %s", err, stderr)
	}
}

func TestTopologyAndPriority(t *testing.T) {
	docs := mustRender(t, "priorityClassName=system-cluster-critical",
		"topologySpreadConstraints[0].maxSkew=1", "topologySpreadConstraints[0].topologyKey=kubernetes.io/hostname",
		"topologySpreadConstraints[0].whenUnsatisfiable=DoNotSchedule")
	ps := manager(t, docs).Spec.Template.Spec
	if ps.PriorityClassName != "system-cluster-critical" {
		t.Errorf("priorityClassName = %q", ps.PriorityClassName)
	}
	if len(ps.TopologySpreadConstraints) != 1 {
		t.Fatalf("topologySpreadConstraints = %+v", ps.TopologySpreadConstraints)
	}
	tsc := ps.TopologySpreadConstraints[0]
	if tsc.LabelSelector == nil || tsc.LabelSelector.MatchLabels["app.kubernetes.io/component"] != "manager" || tsc.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("constraint = %+v, want the manager selector filled in", tsc)
	}
}

// TestSchemaRejects checks that values.schema.json turns typos and wrong types into errors.
func TestSchemaRejects(t *testing.T) {
	for _, set := range []string{
		"replica=2",                  // unknown top-level key (typo of replicas)
		"replicas=two",               // wrong type
		"image.pullpolicy=Always",    // unknown nested key
		"image.pullPolicy=Sometimes", // not in the enum
		"clusterName=a/b",            // '/' separates the owner tag's parts
		"logging.encoder=xml",        // not json or console
		"metrics.port=70000",         // out of range
		"networkPolicy.enable=true",  // typo of enabled
		"podDisruptionBudget.minAvailable=-1",
		"podDisruptionBudget.minAvailable=", // empty would render a PDB with neither field
		"baseURLOverride.allowed={ftp://x}", // not http(s)
	} {
		if _, stderr, err := render(t, set); err == nil {
			t.Errorf("--set %s rendered; want a schema error", set)
		} else if !strings.Contains(stderr, "schema") {
			t.Errorf("--set %s: error does not mention the schema: %s", set, stderr)
		}
	}
	// Values the schema must keep accepting.
	mustRender(t, "clusterName=Prod_eu-1.example", "podDisruptionBudget.maxUnavailable=1", "global.foo=bar", "logging.level=2")
}
