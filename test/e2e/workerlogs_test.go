//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
)

// TestWorkersLogs is the k0s e2e plan of docs/workers-logs-design.md §11: the chart installed
// with flarefake and workersLogs.enabled (charts/flare-operator/ci/workers-logs-values.yaml).
// It skips when the release has no workers-vk Deployment. It runs the real kubectl
// (E2E_KUBECTL, default kubectl) against the cluster of KUBECONFIG.
//
// The last step turns the feature off and on again with `helm upgrade --reuse-values` (helm on
// PATH, chart E2E_CHART, default ../../charts/flare-operator); E2E_WORKERS_LOGS_TOGGLE=0 skips it.
func TestWorkersLogs(t *testing.T) {
	s := newSuite(t)
	w := &workerLogsRun{s: s, node: getenv("E2E_WORKERS_NODE", "cf-workers"), vk: s.cfg.Release + "-workers-vk"}
	var d appsv1.Deployment
	if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.cfg.OperatorNS, Name: w.vk}, &d); apierrors.IsNotFound(err) {
		t.Skipf("workersLogs is not enabled in release %s/%s (no Deployment %s); install with -f charts/flare-operator/ci/workers-logs-values.yaml",
			s.cfg.OperatorNS, s.cfg.Release, w.vk)
	} else if err != nil {
		t.Fatal(err)
	}
	w.vkArgs = d.Spec.Template.Spec.Containers[0].Args
	kubectl, err := exec.LookPath(getenv("E2E_KUBECTL", "kubectl"))
	if err != nil {
		t.Fatalf("kubectl is required for the workers logs e2e (E2E_KUBECTL): %v", err)
	}
	w.kubectlPath = kubectl
	s.ns = "flare-e2e-logs-" + randomHex(3)
	w.otherNS = s.ns + "-other"
	t.Logf("namespace %s, account %s, node %s", s.ns, s.accountID, w.node)
	t.Cleanup(func() {
		s.t = t
		s.cleanup() // deletes s.ns and waits (bounded) for it
		// The other namespace holds only a RoleBinding; wait for it too, bounded.
		other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.otherNS}}
		if err := s.c.Delete(context.Background(), other); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace %s: %v", w.otherNS, err)
			return
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			err := s.c.Get(context.Background(), client.ObjectKeyFromObject(other), other)
			if apierrors.IsNotFound(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("namespace %s still exists 2m after its deletion (err %v)", w.otherNS, err)
				return
			}
			time.Sleep(2 * time.Second)
		}
	})

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"NodeReady", w.testNode},
		{"StandInPod", w.testStandInPod},
		{"Logs", w.testLogs},
		{"Follow", w.testFollow},
		{"ExecRefused", w.testExec},
		{"Authorization", w.testAuthorization},
		{"OptOut", w.testOptOut},
		{"DeleteWorkerScript", w.testDeleteWorkerScript},
		{"DisableFeature", w.testDisable},
	}
	for _, st := range steps {
		if !t.Run(st.name, func(t *testing.T) { s.t = t; st.run(t) }) {
			s.t = t
			t.Fatalf("step %s failed; stopping", st.name)
		}
	}
	s.t = t
}

type workerLogsRun struct {
	s           *suite
	node, vk    string
	vkArgs      []string
	kubectlPath string
	otherNS     string
	ws          *workersv1alpha1.WorkerScript
	pod         string
	msgs        []string // history messages, oldest first
}

// --- flarefake control endpoints (internal/fake/control.go of the workers-logs merge) --------

// workerInvocation is the body of POST /_fake/accounts/{account}/workers/{script}/logs
// (fake.WorkerInvocation): one invocation's events, queryable after ingestDelay and sent at once
// to the script's connected tails.
type workerInvocation struct {
	Timestamp   string            `json:"timestamp,omitempty"` // RFC 3339; empty = flarefake's now
	Level       string            `json:"level,omitempty"`
	Message     any               `json:"message,omitempty"`
	Request     map[string]any    `json:"request,omitempty"`
	Exception   map[string]string `json:"exception,omitempty"`
	IngestDelay string            `json:"ingestDelay,omitempty"`
}

// tailSession is one entry of GET /_fake/accounts/{account}/workers/{script}/tails
// (fake.TailSession).
type tailSession struct {
	ID          string `json:"id"`
	Deleted     bool   `json:"deleted"`
	Connections int    `json:"connections"`
}

func (w *workerLogsRun) inject(t *testing.T, inv workerInvocation) {
	t.Helper()
	if _, err := w.s.fake("POST", fmt.Sprintf("/_fake/accounts/%s/workers/%s/logs", w.s.accountID, w.ws.ScriptName()), inv); err != nil {
		t.Fatalf("inject worker logs: %v", err)
	}
}

func (w *workerLogsRun) tails(t *testing.T) []tailSession {
	t.Helper()
	b, err := w.s.fake("GET", fmt.Sprintf("/_fake/accounts/%s/workers/%s/tails", w.s.accountID, w.ws.ScriptName()), nil)
	if err != nil {
		t.Fatalf("list tail sessions: %v", err)
	}
	var out []tailSession
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode tail sessions: %v: %s", err, b)
	}
	return out
}

// --- kubectl ---------------------------------------------------------------------------------

// kubectl runs the real kubectl and returns stdout, stderr and the error.
func (w *workerLogsRun) kubectl(args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.kubectlPath, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (w *workerLogsRun) logs(t *testing.T, args ...string) []string {
	t.Helper()
	out, stderr, err := w.kubectl(append([]string{"logs", "-n", w.s.ns}, args...)...)
	if err != nil {
		t.Fatalf("kubectl logs %v: %v: %s", args, err, stderr)
	}
	return lines(out)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func indexOf(ls []string, sub string) int {
	return slices.IndexFunc(ls, func(l string) bool { return strings.Contains(l, sub) })
}

// --- steps -----------------------------------------------------------------------------------

// testNode: the virtual node is Ready with its labels, taint, owner and kubelet endpoint, and
// (tls.mode csr) the virtual kubelet deleted its own CSRs once the certificate was issued. That
// the certificate is signed by the cluster CA is proven by the Logs step: k0s's kube-apiserver
// verifies kubelet serving certificates (--kubelet-certificate-authority).
func (w *workerLogsRun) testNode(t *testing.T) {
	s := w.s
	var node corev1.Node
	eventually(t, 2*time.Minute, "node "+w.node+" Ready", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKey{Name: w.node}, &node); err != nil {
			return false, err.Error()
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c.Status == corev1.ConditionTrue, fmt.Sprintf("Ready=%s (%s: %s)", c.Status, c.Reason, c.Message)
			}
		}
		return false, "no Ready condition"
	})
	if node.Labels["type"] != "virtual-kubelet" || node.Labels["flare.dev/virtual-node"] != "workers" {
		t.Errorf("node labels = %v", node.Labels)
	}
	if _, ok := node.Labels["kubernetes.io/os"]; ok {
		t.Errorf("node has a kubernetes.io/os label (DaemonSets would target it): %v", node.Labels)
	}
	if !slices.ContainsFunc(node.Spec.Taints, func(tn corev1.Taint) bool {
		return tn.Key == "virtual-kubelet.io/provider" && tn.Value == "cloudflare" && tn.Effect == corev1.TaintEffectNoSchedule
	}) {
		t.Errorf("node taints = %v", node.Spec.Taints)
	}
	if !slices.ContainsFunc(node.OwnerReferences, func(r metav1.OwnerReference) bool { return r.Kind == "ClusterRole" && r.Name == w.vk }) {
		t.Errorf("node ownerReferences = %v, want the ClusterRole %s", node.OwnerReferences, w.vk)
	}
	if node.Status.DaemonEndpoints.KubeletEndpoint.Port == 0 {
		t.Error("node has no kubelet endpoint port")
	}
	if !slices.ContainsFunc(node.Status.Addresses, func(a corev1.NodeAddress) bool { return a.Type == corev1.NodeInternalIP && a.Address != "" }) {
		t.Errorf("node addresses = %v, want an InternalIP", node.Status.Addresses)
	}
	if slices.Contains(w.vkArgs, "--tls-mode=csr") {
		requester := fmt.Sprintf("system:serviceaccount:%s:%s", s.cfg.OperatorNS, w.vk)
		eventually(t, time.Minute, "no CSRs of the virtual kubelet left after issuance", func() (bool, string) {
			var l certificatesv1.CertificateSigningRequestList
			if err := s.c.List(s.ctx(), &l); err != nil {
				return false, err.Error()
			}
			var left []string
			for _, csr := range l.Items {
				if csr.Spec.SignerName == certificatesv1.KubeletServingSignerName && csr.Spec.Username == requester {
					left = append(left, csr.Name)
				}
			}
			return len(left) == 0, fmt.Sprintf("CSRs of %s: %v", requester, left)
		})
	}
}

// testStandInPod: a WorkerScript gets a Running, Ready stand-in Pod showing its version.
func (w *workerLogsRun) testStandInPod(t *testing.T) {
	s := w.s
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns, Labels: map[string]string{"flare.dev/e2e": "true"}}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "cf-token"},
		StringData: map[string]string{"token": "e2e-token-" + randomHex(8)}}
	s.create(secret)
	acct := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "acct"},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{AccountID: s.accountID, BaseURL: s.cfg.baseURL(),
			TokenSecretRef: cloudflarev1alpha1.SecretKeySelector{Name: secret.Name, Key: "token"}}}
	s.create(acct)
	w.ws = &workersv1alpha1.WorkerScript{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "api"},
		Spec: workersv1alpha1.WorkerScriptSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete},
			ForProvider: &workersv1alpha1.WorkerScriptParameters{
				ScriptName:        s.ns + "-api",
				MainModule:        "index.js",
				CompatibilityDate: "2026-09-01",
				Modules:           map[string]workersv1alpha1.WorkerModule{"index.js": {Type: "esm", Content: workerCode("v1")}},
				Observability:     &workersv1alpha1.WorkerObservability{Enabled: true},
			},
		},
	}
	s.create(w.ws)
	s.waitManaged(w.ws, s.cfg.Timeout)
	w.pod = w.ws.Name + "-worker"

	var pod corev1.Pod
	eventually(t, 2*time.Minute, "stand-in pod "+w.pod+" Running and Ready", func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: w.pod}, &pod); err != nil {
			return false, err.Error()
		}
		return pod.Status.Phase == corev1.PodRunning && podReady(&pod), fmt.Sprintf("phase %s, conditions %v", pod.Status.Phase, pod.Status.Conditions)
	})
	if pod.Spec.NodeName != w.node || !metav1.IsControlledBy(&pod, w.ws) || pod.Labels["flare.dev/stand-in"] != "worker" ||
		pod.Labels["flare.dev/workerscript"] != w.ws.Name {
		t.Errorf("stand-in pod = %+v", pod.ObjectMeta)
	}
	want := "cloudflare-workers://" + w.ws.ScriptName() + "@" + w.ws.Status.AtProvider.VersionID
	if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].ContainerID != want {
		t.Errorf("container statuses = %+v, want containerID %s", pod.Status.ContainerStatuses, want)
	}
}

var reTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z `)

// testLogs: history through the real kubectl logs and its options.
func (w *workerLogsRun) testLogs(t *testing.T) {
	tag := randomHex(4)
	old := "e2e-old-" + tag
	w.inject(t, workerInvocation{Timestamp: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339), Level: "log",
		Message: old, IngestDelay: "0s"})
	for i := 1; i <= 3; i++ {
		m := fmt.Sprintf("e2e-hist-%d-%s", i, tag)
		w.msgs = append(w.msgs, m)
		inv := workerInvocation{Level: "info", Message: []any{m, i}, IngestDelay: "0s",
			Request: map[string]any{"method": "POST", "url": "https://w.example.com/x?i=" + fmt.Sprint(i), "status": 201}}
		if i == 2 {
			inv.Exception = map[string]string{"name": "Error", "message": "boom-" + tag, "stack": "Error: boom\n    at f (index.js:1:1)"}
		}
		w.inject(t, inv)
		time.Sleep(50 * time.Millisecond) // distinct timestamps, so the order is defined
	}

	var all []string
	eventually(t, time.Minute, "kubectl logs to show the injected events", func() (bool, string) {
		out, stderr, err := w.kubectl("logs", "-n", w.s.ns, w.pod)
		if err != nil {
			return false, fmt.Sprintf("%v: %s", err, stderr)
		}
		all = lines(out)
		return indexOf(all, w.msgs[2]) >= 0, out
	})
	iOld, i1, i2, i3 := indexOf(all, old), indexOf(all, w.msgs[0]), indexOf(all, w.msgs[1]), indexOf(all, w.msgs[2])
	if !(iOld >= 0 && iOld < i1 && i1 < i2 && i2 < i3) {
		t.Errorf("history not oldest first (old %d, 1 %d, 2 %d, 3 %d):\n%s", iOld, i1, i2, i3, strings.Join(all, "\n"))
	}
	if indexOf(all, "boom-"+tag) < 0 || indexOf(all, "https://w.example.com/x?i=3") < 0 {
		t.Errorf("exception or request summary missing:\n%s", strings.Join(all, "\n"))
	}

	// --tail=2: the last two lines (the last invocation's console line and its summary).
	tail := w.logs(t, w.pod, "--tail=2")
	if len(tail) != 2 || indexOf(tail, w.msgs[2]) != 0 || indexOf(tail, w.msgs[1]) >= 0 {
		t.Errorf("--tail=2 = %q", tail)
	}
	// --tail=0: no history.
	if got := w.logs(t, w.pod, "--tail=0"); len(got) != 0 {
		t.Errorf("--tail=0 = %q, want nothing", got)
	}
	// --timestamps: the kubelet's fixed-width UTC prefix.
	for _, l := range w.logs(t, w.pod, "--timestamps", "--tail=4") {
		if !reTimestamp.MatchString(l) {
			t.Errorf("--timestamps line %q lacks the RFC 3339 nanosecond prefix", l)
		}
	}
	// --since=1m: the 10-minute-old event is out of the window.
	since := w.logs(t, w.pod, "--since=1m")
	if indexOf(since, old) >= 0 || indexOf(since, w.msgs[2]) < 0 {
		t.Errorf("--since=1m = %q, want the recent events only", since)
	}
	// --since-time: the same window as an absolute time (the old event is 10 minutes back; a few
	// seconds of clock skew between this machine and the cluster do not matter).
	sinceTime := w.logs(t, w.pod, "--since-time="+time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339))
	if indexOf(sinceTime, old) >= 0 || indexOf(sinceTime, w.msgs[0]) < 0 || indexOf(sinceTime, w.msgs[2]) < 0 {
		t.Errorf("--since-time=-2m = %q, want the recent events only", sinceTime)
	}
	// --limit-bytes: the stream stops after N bytes of the same output.
	full, _, err := w.kubectl("logs", "-n", w.s.ns, w.pod)
	if err != nil {
		t.Fatal(err)
	}
	limited, stderr, err := w.kubectl("logs", "-n", w.s.ns, w.pod, "--limit-bytes=40")
	if err != nil {
		t.Fatalf("--limit-bytes=40: %v: %s", err, stderr)
	}
	if len(limited) == 0 || len(limited) > 40 || !strings.HasPrefix(full, limited) {
		t.Errorf("--limit-bytes=40 = %q (%d bytes), want a prefix of at most 40 bytes of %q", limited, len(limited), full)
	}
	// --previous: refused with a clear message.
	if _, stderr, err := w.kubectl("logs", "-n", w.s.ns, w.pod, "--previous"); err == nil {
		t.Error("kubectl logs --previous succeeded")
	} else if !strings.Contains(stderr, "previous logs are not available") {
		t.Errorf("kubectl logs --previous error %q does not say previous logs are not available", stderr)
	}
	// -l: kubectl resolves the selector to the stand-in Pod.
	sel := w.logs(t, "-l", "flare.dev/workerscript="+w.ws.Name, "--tail=-1")
	if indexOf(sel, w.msgs[2]) < 0 {
		t.Errorf("-l flare.dev/workerscript=%s = %q", w.ws.Name, sel)
	}
	// -c with another container: refused.
	if _, stderr, err := w.kubectl("logs", "-n", w.s.ns, w.pod, "-c", "sidecar"); err == nil {
		t.Error("kubectl logs -c sidecar succeeded")
	} else {
		t.Logf("-c sidecar: %s", strings.TrimSpace(stderr))
	}
}

// testFollow: kubectl logs -f streams a live event from the tail; stopping it deletes the tail
// and closes its socket.
func (w *workerLogsRun) testFollow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, w.kubectlPath, "logs", "-f", "--tail=0", "-n", w.s.ns, w.pod)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			got <- sc.Text()
		}
		close(got)
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			_, _ = io.Copy(io.Discard, stdout)
			_ = cmd.Wait()
		}
	}
	defer stop()

	eventually(t, time.Minute, "a connected tail for the follow", func() (bool, string) {
		ts := w.tails(t)
		return slices.ContainsFunc(ts, func(s tailSession) bool { return !s.Deleted && s.Connections > 0 }), fmt.Sprintf("%+v; kubectl: %s", ts, stderr.String())
	})
	live := "e2e-live-" + randomHex(4)
	w.inject(t, workerInvocation{Level: "warn", Message: live, IngestDelay: "1h"}) // the tail only
	deadline := time.After(30 * time.Second)
	var seen []string
wait:
	for {
		select {
		case l, ok := <-got:
			if !ok {
				t.Fatalf("kubectl logs -f ended before %s arrived: %s (lines %q)", live, stderr.String(), seen)
			}
			seen = append(seen, l)
			if strings.Contains(l, live) {
				break wait
			}
		case <-deadline:
			t.Fatalf("no line with %s within 30s (lines %q, stderr %s)", live, seen, stderr.String())
		}
	}
	stop()
	eventually(t, time.Minute, "the tail deleted and its socket closed after kubectl logs -f stopped", func() (bool, string) {
		ts := w.tails(t)
		for _, s := range ts {
			if !s.Deleted || s.Connections > 0 {
				return false, fmt.Sprintf("%+v", ts)
			}
		}
		return len(ts) > 0, fmt.Sprintf("%+v", ts)
	})
}

// testExec: anything but logs is refused with a clear message.
func (w *workerLogsRun) testExec(t *testing.T) {
	_, stderr, err := w.kubectl("exec", "-n", w.s.ns, w.pod, "--", "sh", "-c", "true")
	if err == nil {
		t.Fatal("kubectl exec on a stand-in pod succeeded")
	}
	if !strings.Contains(stderr, "only `kubectl logs` is available") {
		t.Errorf("kubectl exec error %q does not say that only kubectl logs is available", stderr)
	}
}

// testAuthorization: pods/log in the WorkerScript's namespace is what grants a Worker's logs.
func (w *workerLogsRun) testAuthorization(t *testing.T) {
	s := w.s
	viewer, other := "flare-e2e-viewer-"+randomHex(3), "flare-e2e-other-"+randomHex(3)
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.otherNS, Labels: map[string]string{"flare.dev/e2e": "true"}}})
	bind := func(ns, user string) {
		s.create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: user},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}})
	}
	bind(s.ns, viewer)
	bind(w.otherNS, other)
	eventually(t, 30*time.Second, "a namespace viewer to read the Worker's logs", func() (bool, string) {
		out, stderr, err := w.kubectl("--as="+viewer, "logs", "-n", s.ns, w.pod)
		return err == nil && strings.Contains(out, w.msgs[2]), fmt.Sprintf("err %v: %s", err, stderr)
	})
	_, stderr, err := w.kubectl("--as="+other, "logs", "-n", s.ns, w.pod)
	if err == nil || !strings.Contains(strings.ToLower(stderr), "forbidden") {
		t.Errorf("a viewer of another namespace: err %v, stderr %q; want Forbidden", err, stderr)
	}
}

func (w *workerLogsRun) waitNoPod(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	w.s.waitGone(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, timeout)
	t.Logf("pod %s/%s gone after %s", ns, name, time.Since(start).Round(time.Second))
}

func (w *workerLogsRun) waitPod(t *testing.T, ns, name string) {
	t.Helper()
	eventually(t, time.Minute, "stand-in pod "+name, func() (bool, string) {
		var p corev1.Pod
		err := w.s.c.Get(w.s.ctx(), client.ObjectKey{Namespace: ns, Name: name}, &p)
		return err == nil && p.DeletionTimestamp == nil, fmt.Sprint(err)
	})
}

// testOptOut: the annotation flare.dev/stand-in-pod "false" removes the Pod; removing it brings
// the Pod back.
func (w *workerLogsRun) testOptOut(t *testing.T) {
	s := w.s
	s.patch(w.ws, func() {
		if w.ws.Annotations == nil {
			w.ws.Annotations = map[string]string{}
		}
		w.ws.Annotations[workersv1alpha1.AnnotationStandInPod] = "false"
	})
	w.waitNoPod(t, s.ns, w.pod, time.Minute)
	if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(w.ws), w.ws); err != nil {
		t.Fatal(err)
	}
	base := w.ws.DeepCopy()
	delete(w.ws.Annotations, workersv1alpha1.AnnotationStandInPod)
	if err := s.c.Patch(s.ctx(), w.ws, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	w.waitPod(t, s.ns, w.pod)
}

// testDeleteWorkerScript: deleting the WorkerScript removes its Pod (garbage collection).
func (w *workerLogsRun) testDeleteWorkerScript(t *testing.T) {
	s := w.s
	if err := s.c.Delete(s.ctx(), w.ws); err != nil {
		t.Fatal(err)
	}
	w.waitNoPod(t, s.ns, w.pod, 90*time.Second)
	s.waitGone(w.ws, s.cfg.Timeout)
}

// testDisable: helm upgrade with workersLogs.enabled=false removes the Node (owned by the
// ClusterRole Helm deletes) and the stand-in Pods bound to it; enabling it again restores both.
func (w *workerLogsRun) testDisable(t *testing.T) {
	if getenv("E2E_WORKERS_LOGS_TOGGLE", "1") == "0" {
		t.Skip("E2E_WORKERS_LOGS_TOGGLE=0")
	}
	helm, err := exec.LookPath(getenv("E2E_HELM", "helm"))
	if err != nil {
		t.Skip("helm not on PATH; cannot toggle workersLogs")
	}
	s := w.s
	ws := &workersv1alpha1.WorkerScript{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "toggle"},
		Spec: workersv1alpha1.WorkerScriptSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete},
			ForProvider: &workersv1alpha1.WorkerScriptParameters{ScriptName: s.ns + "-toggle", MainModule: "index.js", CompatibilityDate: "2026-09-01",
				Modules: map[string]workersv1alpha1.WorkerModule{"index.js": {Type: "esm", Content: workerCode("toggle")}}},
		},
	}
	s.create(ws)
	w.waitPod(t, s.ns, "toggle-worker")

	chart := getenv("E2E_CHART", "../../charts/flare-operator")
	upgrade := func(enabled bool) {
		t.Helper()
		args := []string{"upgrade", s.cfg.Release, chart, "-n", s.cfg.OperatorNS, "--reuse-values", "--wait", "--timeout", "5m",
			fmt.Sprintf("--set=workersLogs.enabled=%v", enabled)}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, helm, args...).CombinedOutput(); err != nil {
			t.Fatalf("helm %v: %v\n%s", args, err, out)
		}
	}
	// Turn it back on whatever happens, so the installation is left as found.
	defer func() {
		upgrade(true)
		eventually(t, 3*time.Minute, "node "+w.node+" back and the stand-in pod recreated", func() (bool, string) {
			var n corev1.Node
			if err := s.c.Get(s.ctx(), client.ObjectKey{Name: w.node}, &n); err != nil {
				return false, err.Error()
			}
			var p corev1.Pod
			err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: "toggle-worker"}, &p)
			return err == nil, fmt.Sprint(err)
		})
	}()

	start := time.Now()
	upgrade(false)
	s.waitGone(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: w.node}}, 90*time.Second)
	t.Logf("node gone %s after helm upgrade started", time.Since(start).Round(time.Second))
	w.waitNoPod(t, s.ns, "toggle-worker", 2*time.Minute)
	if d := time.Since(start); d > 2*time.Minute {
		t.Errorf("disabling took %s to remove the node and its pods", d)
	}
}
