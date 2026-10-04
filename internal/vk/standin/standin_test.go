package standin_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/vk"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

func workerScript(name string) *workersv1alpha1.WorkerScript {
	return &workersv1alpha1.WorkerScript{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, UID: types.UID("uid-" + name)},
	}
}

func TestPodName(t *testing.T) {
	if got := standin.PodName("api"); got != "api-worker" {
		t.Errorf("PodName(api) = %q", got)
	}
	exact := strings.Repeat("a", 56) // 56 + len("-worker") = 63
	if got := standin.PodName(exact); got != exact+"-worker" {
		t.Errorf("PodName(56 chars) = %q, want it verbatim", got)
	}
	long := strings.Repeat("b", 44) + ".-" + strings.Repeat("c", 30)
	got := standin.PodName(long)
	if len(got) > 62 {
		t.Errorf("PodName(long) = %q is %d characters, want at most 62", got, len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("b", 44)+"-") || !strings.HasSuffix(got, "-worker") {
		t.Errorf("PodName(long) = %q, want the first 46 characters with trailing '.-' trimmed, a hash and -worker", got)
	}
	if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
		t.Errorf("PodName(long) = %q is not a DNS-1123 label: %v", got, errs)
	}
	// Different long names with the same prefix get different Pods.
	if other := standin.PodName(long + "x"); other == got {
		t.Errorf("PodName collides for %q and %q", long, long+"x")
	}
	if got2 := standin.PodName(long); got2 != got {
		t.Error("PodName is not deterministic")
	}
}

// TestTaintMatchesVK keeps standin's copy of the node taint equal to internal/vk's.
func TestTaintMatchesVK(t *testing.T) {
	ws := workerScript("api")
	pod, err := standin.NewBuilder(standin.PodConfig{NodeName: vk.DefaultNodeName, Image: "img"}).Desired(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(pod.Spec.Tolerations, func(tol corev1.Toleration) bool {
		return tol.Key == vk.TaintKey && tol.Value == vk.TaintValue && tol.Effect == corev1.TaintEffectNoSchedule
	}) {
		t.Errorf("tolerations %v do not tolerate %s=%s:NoSchedule", pod.Spec.Tolerations, vk.TaintKey, vk.TaintValue)
	}
}

func TestBuilderDesired(t *testing.T) {
	ws := workerScript("api")
	ws.Spec.ForProvider = &workersv1alpha1.WorkerScriptParameters{ScriptName: "team-a-api"}
	cfg := standin.PodConfig{NodeName: "cf-workers", Image: "ghcr.io/x/flare-operator:1.0",
		ExtraLabels: map[string]string{"team": "a", workersv1alpha1.StandInLabelRole: "spoofed"}}
	pod, err := standin.NewBuilder(cfg).Desired(ws)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Name != "api-worker" || pod.Namespace != "team-a" {
		t.Errorf("name %s/%s", pod.Namespace, pod.Name)
	}
	wantLabels := map[string]string{
		workersv1alpha1.StandInLabelWorkerScript:    "api",
		workersv1alpha1.StandInLabelWorkerScriptUID: "uid-api",
		workersv1alpha1.StandInLabelRole:            workersv1alpha1.StandInRoleWorker,
		"app.kubernetes.io/managed-by":              "flare-operator",
		"team":                                      "a",
	}
	if !mapsEqual(pod.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", pod.Labels, wantLabels)
	}
	if pod.Annotations[workersv1alpha1.StandInAnnotationScriptName] != "team-a-api" || len(pod.Annotations) != 1 {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v", pod.OwnerReferences)
	}
	ref := pod.OwnerReferences[0]
	if ref.APIVersion != workersv1alpha1.GroupVersion.String() || ref.Kind != "WorkerScript" || ref.Name != "api" || ref.UID != "uid-api" ||
		!ptr.Deref(ref.Controller, false) || ref.BlockOwnerDeletion == nil || *ref.BlockOwnerDeletion {
		t.Errorf("ownerReference = %+v, want controller, blockOwnerDeletion false", ref)
	}

	s := pod.Spec
	if s.NodeName != "cf-workers" || ptr.Deref(s.TerminationGracePeriodSeconds, -1) != 0 ||
		ptr.Deref(s.AutomountServiceAccountToken, true) || ptr.Deref(s.EnableServiceLinks, true) || s.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("spec = %+v", s)
	}
	for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
		if !slices.ContainsFunc(s.Tolerations, func(tol corev1.Toleration) bool {
			return tol.Key == key && tol.Operator == corev1.TolerationOpExists && tol.Effect == corev1.TaintEffectNoExecute && tol.TolerationSeconds == nil
		}) {
			t.Errorf("no toleration of %s without tolerationSeconds: %v", key, s.Tolerations)
		}
	}
	if len(s.Containers) != 1 {
		t.Fatalf("containers = %v", s.Containers)
	}
	c := s.Containers[0]
	if c.Name != "worker" || c.Image != cfg.Image || c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("container = %+v", c)
	}
	for _, rl := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
		if !rl.Cpu().Equal(resource.MustParse("1m")) || !rl.Memory().Equal(resource.MustParse("1Mi")) {
			t.Errorf("resources = %+v, want requests = limits = 1m/1Mi", c.Resources)
		}
	}
	checkRestricted(t, pod)

	// The builder copies its config: later changes to the caller's map do not leak in.
	cfg.ExtraLabels["team"] = "b"
	if pod2, _ := standin.NewBuilder(standin.PodConfig{NodeName: "n", Image: "i", ExtraLabels: map[string]string{"x": "1"}}).Desired(ws); pod2.Labels["team"] != "" {
		t.Errorf("labels leaked: %v", pod2.Labels)
	}

	// Custom requests are requests and limits.
	rl := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("4Mi")}
	pod, _ = standin.NewBuilder(standin.PodConfig{NodeName: "n", Image: "i", Requests: rl}).Desired(ws)
	if !pod.Spec.Containers[0].Resources.Limits.Cpu().Equal(resource.MustParse("5m")) {
		t.Errorf("custom resources = %+v", pod.Spec.Containers[0].Resources)
	}

	// A long name: hashed Pod name, hashed label value.
	long := workerScript(strings.Repeat("w", 70))
	pod, _ = standin.NewBuilder(standin.PodConfig{NodeName: "n", Image: "i"}).Desired(long)
	if v := pod.Labels[workersv1alpha1.StandInLabelWorkerScript]; !strings.HasPrefix(v, "h-") || len(v) != 42 {
		t.Errorf("label of a 70-character name = %q, want h-<40 hex>", v)
	}
	if pod.Name != standin.PodName(long.Name) {
		t.Errorf("name = %q", pod.Name)
	}
}

// checkRestricted checks the Pod Security "restricted" profile's requirements
// (https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted).
func checkRestricted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	psc := pod.Spec.SecurityContext
	if psc == nil || !ptr.Deref(psc.RunAsNonRoot, false) || ptr.Deref(psc.RunAsUser, 0) != 65532 ||
		psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v", psc)
	}
	for _, c := range pod.Spec.Containers {
		sc := c.SecurityContext
		if sc == nil || ptr.Deref(sc.AllowPrivilegeEscalation, true) || ptr.Deref(sc.Privileged, true) ||
			!ptr.Deref(sc.ReadOnlyRootFilesystem, false) || !ptr.Deref(sc.RunAsNonRoot, false) ||
			sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) > 0 {
			t.Errorf("container %s securityContext = %+v", c.Name, sc)
		}
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || len(pod.Spec.Volumes) > 0 {
		t.Errorf("host namespaces or volumes set: %+v", pod.Spec)
	}
}

func TestBuilderErrors(t *testing.T) {
	ok := standin.PodConfig{NodeName: "n", Image: "i"}
	noUID := workerScript("api")
	noUID.UID = ""
	for name, tc := range map[string]struct {
		cfg standin.PodConfig
		ws  *workersv1alpha1.WorkerScript
	}{
		"nil":      {ok, nil},
		"no UID":   {ok, noUID},
		"no node":  {standin.PodConfig{Image: "i"}, workerScript("api")},
		"no image": {standin.PodConfig{NodeName: "n"}, workerScript("api")},
	} {
		if _, err := standin.NewBuilder(tc.cfg).Desired(tc.ws); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func readyCond(status metav1.ConditionStatus, reason, msg string, at time.Time) metav1.Condition {
	return metav1.Condition{Type: commonv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(at)}
}

func podCond(st corev1.PodStatus, typ corev1.PodConditionType) *corev1.PodCondition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == typ {
			return &st.Conditions[i]
		}
	}
	return nil
}

func TestStatusMapper(t *testing.T) {
	created := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	readyAt := created.Add(time.Minute)
	now := created.Add(time.Hour)
	ws := workerScript("api")
	ws.Spec.ForProvider = &workersv1alpha1.WorkerScriptParameters{ScriptName: "team-a-api"}
	pod, err := standin.NewBuilder(standin.PodConfig{NodeName: "cf-workers", Image: "img"}).Desired(ws)
	if err != nil {
		t.Fatal(err)
	}
	pod.CreationTimestamp = metav1.NewTime(created)
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	m := standin.NewStatusMapper()

	for _, tc := range []struct {
		name        string
		cond        *metav1.Condition
		version     string
		phase       corev1.PodPhase
		running     bool
		ready       bool
		reason      string
		containerID string
	}{
		{"ready", ptr.To(readyCond(metav1.ConditionTrue, "Available", "", readyAt)), "v1", corev1.PodRunning, true, true, "", "cloudflare-workers://team-a-api@v1"},
		{"never deployed", ptr.To(readyCond(metav1.ConditionFalse, "AccountNotReady", "account acct is not Ready", readyAt)), "", corev1.PodPending, false, false, "AccountNotReady", ""},
		{"not reconciled yet", nil, "", corev1.PodPending, false, false, "ContainerCreating", ""},
		{"failing after a deployment", ptr.To(readyCond(metav1.ConditionFalse, "DeploymentFailed", "boom", readyAt)), "v2", corev1.PodRunning, true, false, "DeploymentFailed", "cloudflare-workers://team-a-api@v2"},
		{"unknown after a deployment", ptr.To(readyCond(metav1.ConditionUnknown, "Creating", "", readyAt)), "v2", corev1.PodRunning, true, false, "Creating", "cloudflare-workers://team-a-api@v2"},
		{"ready without a version (observed)", ptr.To(readyCond(metav1.ConditionTrue, "Available", "", readyAt)), "", corev1.PodRunning, true, true, "", "cloudflare-workers://team-a-api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ws.DeepCopy()
			if tc.cond != nil {
				w.Status.Conditions = []metav1.Condition{*tc.cond}
			}
			w.Status.AtProvider.VersionID = tc.version
			st := m.Status(w, pod, now)
			if st.Phase != tc.phase {
				t.Errorf("phase = %s, want %s", st.Phase, tc.phase)
			}
			if st.PodIP != "" || st.HostIP != "" || len(st.PodIPs) > 0 {
				t.Errorf("pod/host IP set: %+v", st)
			}
			if st.QOSClass != corev1.PodQOSGuaranteed {
				t.Errorf("qosClass = %q, want it kept", st.QOSClass)
			}
			if st.StartTime == nil || !st.StartTime.Time.Equal(created) {
				t.Errorf("startTime = %v, want the creation time", st.StartTime)
			}
			if len(st.ContainerStatuses) != 1 || st.ContainerStatuses[0].Name != "worker" || st.ContainerStatuses[0].Image != "img" {
				t.Fatalf("containerStatuses = %+v", st.ContainerStatuses)
			}
			cs := st.ContainerStatuses[0]
			if (cs.State.Running != nil) != tc.running || (cs.State.Waiting != nil) == tc.running || cs.State.Terminated != nil {
				t.Errorf("container state = %+v, running %v", cs.State, tc.running)
			}
			if !tc.running && (cs.State.Waiting.Reason != tc.reason) {
				t.Errorf("waiting reason = %q, want %q", cs.State.Waiting.Reason, tc.reason)
			}
			if cs.Ready != tc.ready || cs.ContainerID != tc.containerID || cs.ImageID != tc.containerID {
				t.Errorf("container ready %v id %q image %q, want %v %q", cs.Ready, cs.ContainerID, cs.ImageID, tc.ready, tc.containerID)
			}
			for _, typ := range []corev1.PodConditionType{corev1.PodReady, corev1.ContainersReady} {
				c := podCond(st, typ)
				want := corev1.ConditionFalse
				if tc.ready {
					want = corev1.ConditionTrue
				}
				if c == nil || c.Status != want || c.Reason != tc.reason {
					t.Errorf("%s = %+v, want %s reason %q", typ, c, want, tc.reason)
				}
				if tc.cond != nil && !c.LastTransitionTime.Time.Equal(readyAt) {
					t.Errorf("%s lastTransitionTime = %v, want the WorkerScript's Ready transition %v", typ, c.LastTransitionTime, readyAt)
				}
			}
			for _, typ := range []corev1.PodConditionType{corev1.PodScheduled, corev1.PodInitialized} {
				if c := podCond(st, typ); c == nil || c.Status != corev1.ConditionTrue {
					t.Errorf("%s = %+v", typ, c)
				}
			}
			if st.Phase == corev1.PodFailed || st.Phase == corev1.PodSucceeded {
				t.Error("terminal phase")
			}

			// Pure: the same inputs, or the status applied and mapped again later, give the same
			// status.
			again := m.Status(w, pod, now.Add(time.Hour))
			if !jsonEqual(st, again) {
				t.Errorf("not deterministic:\n%s\n%s", js(st), js(again))
			}
			applied := pod.DeepCopy()
			applied.Status = st
			if next := m.Status(w, applied, now.Add(2*time.Hour)); !jsonEqual(st, next) {
				t.Errorf("status changes when mapped from itself:\n%s\n%s", js(st), js(next))
			}
		})
	}

	// A new version restarts the container (new ID and start time); Ready flapping keeps the
	// container's start time.
	w := ws.DeepCopy()
	w.Status.Conditions = []metav1.Condition{readyCond(metav1.ConditionTrue, "Available", "", readyAt)}
	w.Status.AtProvider.VersionID = "v1"
	applied := pod.DeepCopy()
	applied.Status = m.Status(w, pod, now)
	started := applied.Status.ContainerStatuses[0].State.Running.StartedAt
	w.Status.Conditions = []metav1.Condition{readyCond(metav1.ConditionFalse, "ReconcileError", "x", readyAt.Add(time.Hour))}
	st := m.Status(w, applied, now)
	if got := st.ContainerStatuses[0].State.Running.StartedAt; !got.Equal(&started) {
		t.Errorf("startedAt moved from %v to %v on a Ready change", started, got)
	}
	if c := podCond(st, corev1.PodReady); c.LastTransitionTime.Time.Equal(readyAt) {
		t.Error("Ready lastTransitionTime kept although the status changed")
	}
	w.Status.Conditions = []metav1.Condition{readyCond(metav1.ConditionTrue, "Available", "", readyAt.Add(2*time.Hour))}
	w.Status.AtProvider.VersionID = "v2"
	st = m.Status(w, applied, now)
	if got := st.ContainerStatuses[0].State.Running.StartedAt; !got.Time.Equal(readyAt.Add(2 * time.Hour)) {
		t.Errorf("startedAt after a new version = %v, want the new Ready time", got)
	}
}

func TestStatusMapperForeign(t *testing.T) {
	created := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ds-x", CreationTimestamp: metav1.NewTime(created)},
		Status: corev1.PodStatus{Phase: corev1.PodPending, QOSClass: corev1.PodQOSBestEffort,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}}}
	st := standin.NewStatusMapper().Foreign(pod, created.Add(time.Hour))
	if st.Phase != corev1.PodFailed || st.Reason != "UnsupportedOnVirtualNode" || st.Message == "" {
		t.Errorf("foreign status = %+v", st)
	}
	if st.QOSClass != corev1.PodQOSBestEffort || len(st.Conditions) != 1 || st.StartTime == nil || !st.StartTime.Time.Equal(created) {
		t.Errorf("foreign status lost fields: %+v", st)
	}
	if pod.Status.Phase != corev1.PodPending {
		t.Error("Foreign modified its input")
	}
}

func TestParseNamespaceSelector(t *testing.T) {
	for _, s := range []string{"", "  "} {
		sel, err := standin.ParseNamespaceSelector(s)
		if err != nil || !sel.Empty() {
			t.Errorf("%q: %v %v", s, sel, err)
		}
	}
	sel, err := standin.ParseNamespaceSelector("logs in (on,yes),!private")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(labels.Set{"logs": "on"}) || sel.Matches(labels.Set{"logs": "on", "private": ""}) || sel.Matches(labels.Set{}) {
		t.Errorf("selector %s matches wrongly", sel)
	}
	if _, err := standin.ParseNamespaceSelector("a in ("); err == nil {
		t.Error("malformed selector parsed")
	}
}

// TestReconcilerNoClientCalls checks that a Reconcile of a WorkerScript that no longer exists is
// a no-op (garbage collection deletes its Pod).
func TestReconcilerMissingWorkerScript(t *testing.T) {
	r := &standin.Reconciler{Client: fakeClient(t), Builder: standin.NewBuilder(standin.PodConfig{NodeName: "n", Image: "i"})}
	if _, err := r.Reconcile(context.Background(), reqFor("team-a", "gone")); err != nil {
		t.Fatal(err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonEqual(a, b any) bool { return js(a) == js(b) }
