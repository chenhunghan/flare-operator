package chart

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorkersLogsPodImagePinned: the stand-in placeholder image does not follow image.tag, so
// an operator upgrade does not change (or recreate) the stand-in Pods.
func TestWorkersLogsPodImagePinned(t *testing.T) {
	podImage := func(tag string) string {
		for _, a := range workersVK(t, mustRender(t, "workersLogs.enabled=true", "image.tag="+tag)).Spec.Template.Spec.Containers[0].Args {
			if v, ok := strings.CutPrefix(a, "--pod-image="); ok {
				return v
			}
		}
		t.Fatal("no --pod-image")
		return ""
	}
	if a, b := podImage("1.0.0"), podImage("2.0.0"); a != b || a != "registry.k8s.io/pause:3.10" {
		t.Errorf("--pod-image %q / %q across operator versions, want the pinned placeholder", a, b)
	}
}

// TestWorkersLogsSelectorFailsClosed: a namespaceSelector the virtual kubelet could not parse
// fails the render even without the schema.
func TestWorkersLogsSelectorFailsClosed(t *testing.T) {
	for _, sets := range [][]string{
		{"workersLogs.namespaceSelector.matchExpressions[0].key=a", "workersLogs.namespaceSelector.matchExpressions[0].operator=Equals"},
		{"workersLogs.namespaceSelector.matchExpressions[0].key=a", "workersLogs.namespaceSelector.matchExpressions[0].operator=In"},
	} {
		args := append([]string{"flag:--skip-schema-validation", "workersLogs.enabled=true"}, sets...)
		if _, stderr, err := render(t, args...); err == nil || !strings.Contains(stderr, "namespaceSelector") {
			t.Errorf("--set %v rendered without the schema (err %v, %s); want the template to fail", sets, err, stderr)
		}
	}
}

// installNotes renders the install NOTES (helm install --dry-run=client).
func installNotes(t *testing.T, sets ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH")
	}
	args := []string{"install", "flare-operator", chartDir(t), "-n", "flare-system", "--dry-run=client", "--set", "clusterName=test"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm %v: %v\n%s", args, err, out)
	}
	_, notes, _ := strings.Cut(string(out), "NOTES:")
	return notes
}

func TestWorkersLogsNotes(t *testing.T) {
	np := []string{"workersLogs.enabled=true", "networkPolicy.enabled=true", "networkPolicy.apiServer.cidrs={10.0.0.1/32}"}
	if n := installNotes(t, np...); strings.Contains(n, "cloudflareAPI.cidrs is empty") || strings.Contains(n, "hostNetwork is on") {
		t.Errorf("default notes warn:\n%s", n)
	}
	if n := installNotes(t, append(np, "networkPolicy.cloudflareAPI.cidrs=null")...); !strings.Contains(n, "WARNING: networkPolicy.cloudflareAPI.cidrs is empty") {
		t.Errorf("no warning for empty cloudflareAPI.cidrs:\n%s", n)
	}
	if n := installNotes(t, append(np, "workersLogs.hostNetwork=true")...); !strings.Contains(n, "do not enforce NetworkPolicy on host-network Pods") {
		t.Errorf("no hostNetwork NetworkPolicy note:\n%s", n)
	}
	if n := installNotes(t, "workersLogs.enabled=true", "rbac.create=false"); !strings.Contains(n, "kubectl delete node cf-workers") {
		t.Errorf("no rbac.create=false note:\n%s", n)
	}
	if n := installNotes(t, "rbac.create=false"); strings.Contains(n, "cf-workers") {
		t.Errorf("workersLogs notes with the feature off:\n%s", n)
	}
}
