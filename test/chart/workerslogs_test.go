package chart

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const vkName = "flare-operator-workers-vk"

// named decodes the document of kind with metadata.name name (and namespace ns when not "").
func named[T any](t *testing.T, docs map[string][][]byte, kind, ns, name string) *T {
	t.Helper()
	for _, d := range docs[kind] {
		var u unstructured.Unstructured
		if err := yaml.Unmarshal(d, &u.Object); err != nil {
			t.Fatal(err)
		}
		if u.GetName() == name && (ns == "" || u.GetNamespace() == ns) {
			var v T
			if err := yaml.UnmarshalStrict(d, &v); err != nil {
				t.Fatalf("decode %s %s: %v", kind, name, err)
			}
			return &v
		}
	}
	return nil
}

func mustNamed[T any](t *testing.T, docs map[string][][]byte, kind, ns, name string) *T {
	t.Helper()
	v := named[T](t, docs, kind, ns, name)
	if v == nil {
		t.Fatalf("no %s %s/%s rendered", kind, ns, name)
	}
	return v
}

func workersVK(t *testing.T, docs map[string][][]byte) *appsv1.Deployment {
	t.Helper()
	return mustNamed[appsv1.Deployment](t, docs, "Deployment", "flare-system", vkName)
}

// TestWorkersLogsOff: the feature is off by default and then renders nothing of its own.
func TestWorkersLogsOff(t *testing.T) {
	for _, sets := range [][]string{nil, {"networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}", "flarefake.enabled=true"}} {
		docs := mustRender(t, sets...)
		for kind, ds := range docs {
			for _, d := range ds {
				if bytes.Contains(d, []byte("workers-vk")) || bytes.Contains(d, []byte("--node-name")) {
					t.Errorf("--set %v: %s mentions the workers virtual kubelet with workersLogs off:\n%s", sets, kind, d)
				}
			}
		}
	}
}

func TestWorkersLogsDeployment(t *testing.T) {
	docs := mustRender(t, "workersLogs.enabled=true", "image.tag=1.2.3", "flarefake.enabled=true")
	d := workersVK(t, docs)
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %d, strategy %s: want 1 and Recreate", *d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	if d.Spec.Selector.MatchLabels["app.kubernetes.io/component"] != "workers-vk" ||
		d.Spec.Template.Labels["app.kubernetes.io/component"] != "workers-vk" {
		t.Errorf("selector %v, labels %v", d.Spec.Selector.MatchLabels, d.Spec.Template.Labels)
	}
	// The manager Deployment's selector must not match the virtual kubelet's Pods.
	m := manager(t, docs)
	for k, v := range m.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			return
		}
	}
	t.Errorf("manager selector %v matches the virtual kubelet's pods %v", m.Spec.Selector.MatchLabels, d.Spec.Template.Labels)
}

func TestWorkersLogsPod(t *testing.T) {
	docs := mustRender(t, "workersLogs.enabled=true", "image.tag=1.2.3", "flarefake.enabled=true")
	ps := workersVK(t, docs).Spec.Template.Spec
	if ps.ServiceAccountName != vkName || ps.HostNetwork {
		t.Errorf("serviceAccountName %q, hostNetwork %v", ps.ServiceAccountName, ps.HostNetwork)
	}
	mustNamed[corev1.ServiceAccount](t, docs, "ServiceAccount", "flare-system", vkName)
	if len(ps.Containers) != 1 {
		t.Fatalf("containers = %v", ps.Containers)
	}
	c := ps.Containers[0]
	if c.Image != "ghcr.io/chenhunghan/flare-operator:1.2.3" || !slices.Equal(c.Command, []string{"/workers-vk"}) {
		t.Errorf("image %q command %v: want the operator image running /workers-vk", c.Image, c.Command)
	}
	for _, want := range []string{
		"--node-name=cf-workers", "--address-mode=podIP", "--listen-address=:10250", "--tls-mode=csr",
		"--tls-approve-csr=true", "--tls-lifetime=24h", "--owner-cluster-role=" + vkName,
		"--pod-image=registry.k8s.io/pause:3.10", "--pod-cpu=1m", "--pod-memory=1Mi",
		"--api-budget=120", "--logs-default-window=72h", "--logs-max-events=10000", "--logs-max-followers=100",
		"--leader-elect=true", "--leader-election-namespace=flare-system", "--health-probe-bind-address=:8081",
		"--allowed-base-url=http://flare-operator-flarefake.flare-system.svc:8787/client/v4",
	} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("args %v lack %s", c.Args, want)
		}
	}
	for _, a := range c.Args {
		if strings.HasPrefix(a, "--namespace-selector") || strings.HasPrefix(a, "--pod-label") || strings.HasPrefix(a, "--tls-secret") {
			t.Errorf("unexpected default arg %s", a)
		}
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			env[e.Name] = e.ValueFrom.FieldRef.FieldPath
		}
	}
	if env["POD_IP"] != "status.podIP" || env["HOST_IP"] != "status.hostIP" || env["POD_NAMESPACE"] != "metadata.namespace" {
		t.Errorf("downward API env = %v", env)
	}
	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports["kubelet-api"] != 10250 || ports["health"] != 8081 {
		t.Errorf("ports = %v", ports)
	}
	// Restricted Pod Security, as the manager.
	if ps.SecurityContext == nil || !*ps.SecurityContext.RunAsNonRoot || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v", ps.SecurityContext)
	}
	sc := c.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container securityContext = %+v", sc)
	}
	if c.Resources.Limits.Memory().IsZero() || c.LivenessProbe == nil || c.ReadinessProbe == nil {
		t.Errorf("resources %+v / probes missing", c.Resources)
	}

	// Options.
	docs = mustRender(t, "workersLogs.enabled=true", "workersLogs.hostNetwork=true", "workersLogs.podImage=registry.local/pause:1",
		"workersLogs.podLabels.team=a", "workersLogs.namespaceSelector.matchLabels.logs=on",
		"workersLogs.namespaceSelector.matchExpressions[0].key=tier", "workersLogs.namespaceSelector.matchExpressions[0].operator=NotIn",
		"workersLogs.namespaceSelector.matchExpressions[0].values={dev,test}",
		"workersLogs.namespaceSelector.matchExpressions[1].key=private", "workersLogs.namespaceSelector.matchExpressions[1].operator=DoesNotExist",
		"workersLogs.nodeName=cf-workers-eu", "workersLogs.tls.mode=secret", "workersLogs.tls.secretName=vk-tls",
		"workersLogs.extraArgs={--v=2}")
	ps = workersVK(t, docs).Spec.Template.Spec
	c = ps.Containers[0]
	if !ps.HostNetwork || ps.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("hostNetwork %v dnsPolicy %s", ps.HostNetwork, ps.DNSPolicy)
	}
	for _, want := range []string{"--address-mode=hostIP", "--listen-address=:10260", "--health-probe-bind-address=:10261",
		"--pod-image=registry.local/pause:1", "--pod-label=team=a", "--namespace-selector=logs=on,tier notin (dev,test),!private",
		"--node-name=cf-workers-eu", "--tls-mode=secret", "--tls-secret-namespace=flare-system", "--tls-secret-name=vk-tls", "--v=2"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("args %v lack %s", c.Args, want)
		}
	}
	if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--tls-approve-csr") }) {
		t.Errorf("csr args with tls.mode=secret: %v", c.Args)
	}
	// A port other than the real kubelet's is kept on the host network.
	c = workersVK(t, mustRender(t, "workersLogs.enabled=true", "workersLogs.hostNetwork=true", "workersLogs.port=10300")).Spec.Template.Spec.Containers[0]
	if !slices.Contains(c.Args, "--listen-address=:10300") {
		t.Errorf("args %v lack --listen-address=:10300", c.Args)
	}
	if _, stderr, err := render(t, "workersLogs.enabled=true", "workersLogs.tls.mode=secret"); err == nil || !strings.Contains(stderr, "secretName") {
		t.Errorf("tls.mode=secret without secretName: err %v, %s", err, stderr)
	}
}

func rule(groups, resources, verbs []string, names ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs, ResourceNames: names}
}

var (
	core = []string{""}
	// vkBaseRules are the ClusterRole rules in every TLS mode (docs/workers-logs-design.md §4.3).
	vkBaseRules = []rbacv1.PolicyRule{
		rule(core, []string{"nodes"}, []string{"create", "list", "watch"}),
		rule(core, []string{"nodes"}, []string{"get", "update", "patch", "delete"}, "cf-workers"),
		rule(core, []string{"nodes/status"}, []string{"update", "patch"}, "cf-workers"),
		rule(core, []string{"pods"}, []string{"get", "list", "watch", "create", "patch", "delete"}),
		rule(core, []string{"pods/status"}, []string{"update", "patch"}),
		rule([]string{"", "events.k8s.io"}, []string{"events"}, []string{"create", "patch"}),
		rule([]string{"flare.dev"}, []string{"workerscripts", "cloudflareaccounts"}, []string{"get", "list", "watch"}),
		rule(core, []string{"secrets"}, []string{"get"}),
		rule(core, []string{"namespaces"}, []string{"get", "list", "watch"}),
		rule([]string{"rbac.authorization.k8s.io"}, []string{"clusterroles"}, []string{"get"}, vkName),
	}
	vkCSRRule     = rule([]string{"certificates.k8s.io"}, []string{"certificatesigningrequests"}, []string{"create", "get", "list", "watch", "delete"})
	vkApproveRule = []rbacv1.PolicyRule{
		rule([]string{"certificates.k8s.io"}, []string{"certificatesigningrequests/approval"}, []string{"update"}),
		rule([]string{"certificates.k8s.io"}, []string{"signers"}, []string{"approve"}, "kubernetes.io/kubelet-serving"),
	}
	vkAuthRules = []rbacv1.PolicyRule{
		rule([]string{"authentication.k8s.io"}, []string{"tokenreviews"}, []string{"create"}),
		rule([]string{"authorization.k8s.io"}, []string{"subjectaccessreviews"}, []string{"create"}),
	}
)

func concat(parts ...[]rbacv1.PolicyRule) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestWorkersLogsRBAC checks the exact RBAC of the virtual kubelet in each TLS mode.
func TestWorkersLogsRBAC(t *testing.T) {
	for _, tc := range []struct {
		sets []string
		want []rbacv1.PolicyRule
	}{
		{nil, concat(vkBaseRules, []rbacv1.PolicyRule{vkCSRRule}, vkApproveRule, vkAuthRules)},
		{[]string{"workersLogs.tls.csr.approve=false"}, concat(vkBaseRules, []rbacv1.PolicyRule{vkCSRRule}, vkAuthRules)},
		{[]string{"workersLogs.tls.mode=selfSigned"}, concat(vkBaseRules, vkAuthRules)},
		{[]string{"workersLogs.tls.mode=secret", "workersLogs.tls.secretName=vk-tls"}, concat(vkBaseRules, vkAuthRules)},
	} {
		docs := mustRender(t, append([]string{"workersLogs.enabled=true"}, tc.sets...)...)
		cr := mustNamed[rbacv1.ClusterRole](t, docs, "ClusterRole", "", vkName)
		if !reflect.DeepEqual(cr.Rules, tc.want) {
			got, _ := yaml.Marshal(cr.Rules)
			want, _ := yaml.Marshal(tc.want)
			t.Errorf("--set %v: ClusterRole rules\n%s\nwant\n%s", tc.sets, got, want)
		}
		sa := rbacv1.Subject{Kind: "ServiceAccount", Name: vkName, Namespace: "flare-system"}
		crb := mustNamed[rbacv1.ClusterRoleBinding](t, docs, "ClusterRoleBinding", "", vkName)
		if crb.RoleRef.Name != vkName || !slices.Equal(crb.Subjects, []rbacv1.Subject{sa}) {
			t.Errorf("ClusterRoleBinding = %+v", crb)
		}

		le := mustNamed[rbacv1.Role](t, docs, "Role", "flare-system", vkName+"-leader-election")
		wantLE := []rbacv1.PolicyRule{
			rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
			rule(core, []string{"events"}, []string{"create", "patch"}),
		}
		if slices.Contains(tc.sets, "workersLogs.tls.mode=secret") {
			wantLE = append(wantLE, rule(core, []string{"secrets"}, []string{"get", "list", "watch"}, "vk-tls"))
		}
		if !reflect.DeepEqual(le.Rules, wantLE) {
			t.Errorf("--set %v: leader-election Role rules = %+v", tc.sets, le.Rules)
		}
		nl := mustNamed[rbacv1.Role](t, docs, "Role", "kube-node-lease", vkName+"-node-lease")
		if !reflect.DeepEqual(nl.Rules, []rbacv1.PolicyRule{
			rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create"}),
			rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"get", "update"}, "cf-workers"),
		}) {
			t.Errorf("node-lease Role rules = %+v", nl.Rules)
		}
		for _, rb := range []struct{ ns, name, role string }{
			{"flare-system", vkName + "-leader-election", vkName + "-leader-election"},
			{"kube-node-lease", vkName + "-node-lease", vkName + "-node-lease"},
			{"kube-system", vkName + "-authentication-reader", "extension-apiserver-authentication-reader"},
		} {
			b := mustNamed[rbacv1.RoleBinding](t, docs, "RoleBinding", rb.ns, rb.name)
			if b.RoleRef.Kind != "Role" || b.RoleRef.Name != rb.role || !slices.Equal(b.Subjects, []rbacv1.Subject{sa}) {
				t.Errorf("RoleBinding %s/%s = %+v", rb.ns, rb.name, b)
			}
		}
	}
	// The manager's RBAC does not change.
	off, on := mustRender(t), mustRender(t, "workersLogs.enabled=true")
	for _, name := range []string{"flare-operator-manager"} {
		a := mustNamed[rbacv1.ClusterRole](t, off, "ClusterRole", "", name)
		b := mustNamed[rbacv1.ClusterRole](t, on, "ClusterRole", "", name)
		if !reflect.DeepEqual(a.Rules, b.Rules) {
			t.Errorf("ClusterRole %s changes with workersLogs.enabled", name)
		}
	}
	if !reflect.DeepEqual(manager(t, off).Spec, manager(t, on).Spec) {
		t.Error("the manager Deployment changes with workersLogs.enabled")
	}
	// rbac.create=false: no RBAC objects (an administrator provides them), the rest stays.
	docs := mustRender(t, "workersLogs.enabled=true", "rbac.create=false")
	if named[rbacv1.ClusterRole](t, docs, "ClusterRole", "", vkName) != nil || named[rbacv1.Role](t, docs, "Role", "kube-node-lease", vkName+"-node-lease") != nil {
		t.Error("rbac.create=false still renders the virtual kubelet's RBAC")
	}
	workersVK(t, docs)
}

func TestWorkersLogsNetworkPolicy(t *testing.T) {
	docs := mustRender(t, "workersLogs.enabled=true")
	if named[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", "flare-system", vkName) != nil {
		t.Error("a virtual kubelet NetworkPolicy without networkPolicy.enabled")
	}
	docs = mustRender(t, "workersLogs.enabled=true", "networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}",
		"flarefake.enabled=true", "workersLogs.networkPolicy.kubeletAPIFrom[0].podSelector.matchLabels.k8s-app=konnectivity-agent")
	np := mustNamed[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", "flare-system", vkName)
	if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "workers-vk" {
		t.Errorf("podSelector = %v", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 || np.Spec.Ingress[0].Ports[0].Port.IntValue() != 10250 {
		t.Fatalf("ingress = %+v, want the kubelet port only", np.Spec.Ingress)
	}
	from := np.Spec.Ingress[0].From
	if len(from) != 2 || from[0].IPBlock == nil || from[0].IPBlock.CIDR != "10.0.0.1/32" ||
		from[1].PodSelector == nil || from[1].PodSelector.MatchLabels["k8s-app"] != "konnectivity-agent" {
		t.Errorf("ingress from = %+v, want the API server CIDR and kubeletAPIFrom", from)
	}
	ports := egressPorts(np)
	if !slices.Contains(ports[53], "pods:kube-dns") || !slices.Equal(ports[6443], []string{"10.0.0.1/32"}) ||
		!slices.Contains(ports[443], "104.16.0.0/13") || !slices.Equal(ports[8787], []string{"pods:flarefake"}) {
		t.Errorf("egress = %v", ports)
	}
	// The manager's NetworkPolicy is unchanged and does not select the virtual kubelet.
	mnp := mustNamed[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", "flare-system", "flare-operator")
	if mnp.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "manager" {
		t.Errorf("manager NetworkPolicy selector = %v", mnp.Spec.PodSelector.MatchLabels)
	}
	// hostNetwork: the policy follows the port.
	docs = mustRender(t, "workersLogs.enabled=true", "workersLogs.hostNetwork=true", "networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}")
	np = mustNamed[networkingv1.NetworkPolicy](t, docs, "NetworkPolicy", "flare-system", vkName)
	if np.Spec.Ingress[0].Ports[0].Port.IntValue() != 10260 {
		t.Errorf("hostNetwork ingress port = %v", np.Spec.Ingress[0].Ports[0].Port)
	}
}

func TestWorkersLogsSchemaRejects(t *testing.T) {
	for _, set := range []string{
		"workersLogs.enable=true",
		"workersLogs.enabled=yes",
		"workersLogs.tls.mode=acme",
		"workersLogs.tls.csr.lifetime=1day",
		"workersLogs.apiBudget=0",
		"workersLogs.apiBudget=5000",
		"workersLogs.nodeName=CF_Workers",
		"workersLogs.port=0",
		"workersLogs.podResources.cpu=lots",
		"workersLogs.podResources.gpu=1",
		"workersLogs.logs.maxEvents=0",
		"workersLogs.namespaceSelector.matchNames={a}",
		"workersLogs.namespaceSelector.matchExpressions[0].key=a",
		"workersLogs.namespaceSelector.matchExpressions[0].operator=Equals",
	} {
		if _, stderr, err := render(t, set); err == nil {
			t.Errorf("--set %s rendered; want a schema error", set)
		} else if !strings.Contains(stderr, "schema") {
			t.Errorf("--set %s: error does not mention the schema: %s", set, stderr)
		}
	}
}

// TestWorkersLogsReuseValues: `helm upgrade --reuse-values --set workersLogs.enabled=true` from
// a chart version without workersLogs (the values of 0.1.0 plus that one key) renders the same
// virtual kubelet as a fresh install, so the template defaults equal values.yaml.
func TestWorkersLogsReuseValues(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "flare-operator")
	copyChart(t, chartDir(t), dir)
	old, err := os.ReadFile(filepath.Join("testdata", "values-0.1.0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	reused, stderr, err := renderDir(t, dir, "image.repository=r.example/flare-operator", "image.tag=0.2.0", "clusterName=prod", "workersLogs.enabled=true")
	if err != nil {
		t.Fatalf("render with 0.1.0 values: %v\n%s", err, stderr)
	}
	fresh := mustRender(t, "image.repository=r.example/flare-operator", "image.tag=0.2.0", "clusterName=prod", "workersLogs.enabled=true")
	a, b := workersVK(t, reused), workersVK(t, fresh)
	if !reflect.DeepEqual(a.Spec.Template.Spec, b.Spec.Template.Spec) {
		ay, _ := yaml.Marshal(a.Spec.Template.Spec)
		by, _ := yaml.Marshal(b.Spec.Template.Spec)
		t.Errorf("reused values render another virtual kubelet pod:\n%s\nfresh install:\n%s", ay, by)
	}
	ra := mustNamed[rbacv1.ClusterRole](t, reused, "ClusterRole", "", vkName)
	rb := mustNamed[rbacv1.ClusterRole](t, fresh, "ClusterRole", "", vkName)
	if !reflect.DeepEqual(ra.Rules, rb.Rules) {
		t.Error("reused values render other ClusterRole rules")
	}
	// And a false that overrides a true default survives the defaulting.
	docs := mustRender(t, "workersLogs.enabled=true", "workersLogs.tls.csr.approve=false")
	if !slices.Contains(workersVK(t, docs).Spec.Template.Spec.Containers[0].Args, "--tls-approve-csr=false") {
		t.Error("tls.csr.approve=false did not reach the args")
	}
}

func copyChart(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
