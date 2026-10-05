package vk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

// fakeMapper is a small StatusMapper: Running when the WorkerScript is Ready, else Pending.
type fakeMapper struct{}

func (fakeMapper) Status(ws *workersv1alpha1.WorkerScript, pod *corev1.Pod, now time.Time) corev1.PodStatus {
	ready := meta.IsStatusConditionTrue(ws.Status.Conditions, commonv1alpha1.ConditionReady)
	st := corev1.PodStatus{Phase: corev1.PodPending, StartTime: &metav1.Time{Time: now.Truncate(time.Second)}}
	if pod.Status.StartTime != nil {
		st.StartTime = pod.Status.StartTime
	}
	cs := corev1.ContainerStatus{Name: workersv1alpha1.StandInContainerName, ImageID: "cloudflare-workers://" + ws.ScriptName() + "@" + ws.Status.AtProvider.VersionID}
	if ready {
		st.Phase = corev1.PodRunning
		cs.Ready = true
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: *st.StartTime}
	} else {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "Creating"}
	}
	st.ContainerStatuses = []corev1.ContainerStatus{cs}
	return st
}

func (fakeMapper) Foreign(_ *corev1.Pod, _ time.Time) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodFailed, Reason: standin.ForeignReason, Message: "not a stand-in"}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, workersv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func workerScript(ns, name string, uid types.UID, ready bool) *workersv1alpha1.WorkerScript {
	ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: uid}}
	ws.Spec.AccountRef.Name = "acct"
	st := metav1.ConditionFalse
	if ready {
		st = metav1.ConditionTrue
	}
	ws.Status.Conditions = []metav1.Condition{{Type: commonv1alpha1.ConditionReady, Status: st, Reason: "Test", LastTransitionTime: metav1.Now()}}
	return ws
}

// standInPod is a stand-in Pod of ws as the stand-in Builder shapes one (the parts the virtual
// kubelet checks).
func standInPod(ws *workersv1alpha1.WorkerScript, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ws.Namespace, Name: ws.Name + workersv1alpha1.StandInPodNameSuffix, UID: types.UID("pod-" + ws.Name),
			Labels: map[string]string{
				workersv1alpha1.StandInLabelRole:            workersv1alpha1.StandInRoleWorker,
				workersv1alpha1.StandInLabelWorkerScript:    ws.Name,
				workersv1alpha1.StandInLabelWorkerScriptUID: string(ws.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workersv1alpha1.GroupVersion.String(), Kind: "WorkerScript", Name: ws.Name, UID: ws.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			AutomountServiceAccountToken:  ptr.To(false),
			Tolerations:                   []corev1.Toleration{{Key: TaintKey, Value: TaintValue, Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual}},
			Containers:                    []corev1.Container{{Name: workersv1alpha1.StandInContainerName, Image: "example.com/placeholder:1"}},
		},
	}
}

type notifications struct {
	mu   sync.Mutex
	pods []*corev1.Pod
}

func (n *notifications) add(p *corev1.Pod) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pods = append(n.pods, p)
}

func (n *notifications) wait(t *testing.T, count int) []*corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n.mu.Lock()
		got := append([]*corev1.Pod(nil), n.pods...)
		n.mu.Unlock()
		if len(got) >= count {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications, want %d", len(got), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProviderStatus(t *testing.T) {
	ctx := context.Background()
	ready := workerScript("ns", "api", "uid-api", true)
	pending := workerScript("ns", "web", "uid-web", false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ready, pending).WithStatusSubresource(&workersv1alpha1.WorkerScript{}).Build()
	p := NewProvider(c, fakeMapper{}, func() time.Time { return testNow }, logrDiscard())
	var n notifications
	p.NotifyPods(ctx, n.add)

	missing := standInPod(workerScript("ns", "gone", "uid-gone", true), "cf-workers")
	wrongUID := standInPod(ready, "cf-workers")
	wrongUID.Name, wrongUID.UID = "impostor", "pod-impostor"
	wrongUID.Labels[workersv1alpha1.StandInLabelWorkerScriptUID] = "other"
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "daemon-x", UID: "pod-daemon"},
		Spec: corev1.PodSpec{NodeName: "cf-workers", Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}

	count := 0
	for _, tc := range []struct {
		pod    *corev1.Pod
		phase  corev1.PodPhase
		reason string
	}{
		{standInPod(ready, "cf-workers"), corev1.PodRunning, ""},
		{standInPod(pending, "cf-workers"), corev1.PodPending, ""},
		{missing, corev1.PodPending, PodWorkerScriptNotFoundReason},
		{wrongUID, corev1.PodFailed, standin.ForeignReason},
		{foreign, corev1.PodFailed, standin.ForeignReason},
	} {
		if err := p.CreatePod(ctx, tc.pod); err != nil {
			t.Fatalf("%s: %v", tc.pod.Name, err)
		}
		count++
		got := n.wait(t, count)
		last := got[len(got)-1]
		if last.Name != tc.pod.Name || last.Status.Phase != tc.phase || last.Status.Reason != tc.reason {
			t.Errorf("%s: notified %s phase %s reason %q, want phase %s reason %q", tc.pod.Name, last.Name, last.Status.Phase, last.Status.Reason, tc.phase, tc.reason)
		}
		st, err := p.GetPodStatus(ctx, tc.pod.Namespace, tc.pod.Name)
		if err != nil || st.Phase != tc.phase {
			t.Errorf("%s: GetPodStatus %v, %v", tc.pod.Name, st, err)
		}
	}
	if pods, _ := p.GetPods(ctx); len(pods) != 5 {
		t.Errorf("GetPods: %d pods", len(pods))
	}

	// A WorkerScript turning Ready re-notifies its Pod; an unrelated one does not.
	before := len(n.wait(t, 5))
	upd := pending.DeepCopy()
	meta.SetStatusCondition(&upd.Status.Conditions, metav1.Condition{Type: commonv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Available"})
	if err := c.Status().Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	p.OnWorkerScript(ctx, "ns", "web")
	got := n.wait(t, before+1)
	if last := got[len(got)-1]; last.Name != "web-worker" || last.Status.Phase != corev1.PodRunning {
		t.Errorf("after Ready: %s %s", last.Name, last.Status.Phase)
	}
	p.OnWorkerScript(ctx, "ns", "web") // unchanged: no notification
	p.OnWorkerScript(ctx, "other", "web")
	time.Sleep(50 * time.Millisecond)
	if l := len(n.wait(t, before+1)); l != before+1 {
		t.Errorf("unchanged status notified again (%d notifications)", l-before)
	}

	// Delete reports the Pod terminated and forgets it.
	if err := p.DeletePod(ctx, standInPod(ready, "cf-workers")); err != nil {
		t.Fatal(err)
	}
	got = n.wait(t, before+2)
	last := got[len(got)-1]
	if last.Name != "api-worker" || last.Status.ContainerStatuses[0].State.Terminated == nil {
		t.Errorf("delete notified %s %+v", last.Name, last.Status.ContainerStatuses)
	}
	if _, err := p.GetPod(ctx, "ns", "api-worker"); !errdefs.IsNotFound(err) {
		t.Errorf("GetPod after delete: %v", err)
	}
	if err := p.DeletePod(ctx, standInPod(ready, "cf-workers")); !errdefs.IsNotFound(err) {
		t.Errorf("second delete: %v", err)
	}
}

func TestWorkerScriptKey(t *testing.T) {
	ws := workerScript("ns", "api", "u", true)
	for _, obj := range []any{ws, toolscacheTombstone("ns/api", ws)} {
		if ns, name, ok := workerScriptKey(obj); !ok || ns != "ns" || name != "api" {
			t.Errorf("%T: %s/%s %v", obj, ns, name, ok)
		}
	}
	if _, _, ok := workerScriptKey("x"); ok {
		t.Error("a string was accepted")
	}
	var _ client.Object = ws
}

// gatedReader reads through client.Reader; while gate is set, a Get of a WorkerScript reads and
// then waits for the gate to close before it returns (a sync that read the WorkerScript just
// before it changed).
type gatedReader struct {
	client.Reader
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func (g *gatedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := g.Reader.Get(ctx, key, obj, opts...)
	g.mu.Lock()
	gate, entered := g.gate, g.entered
	g.gate = nil
	g.mu.Unlock()
	if _, ok := obj.(*workersv1alpha1.WorkerScript); ok && gate != nil {
		close(entered)
		<-gate
	}
	return err
}

// A sync that read the WorkerScript before it turned Ready must not record and notify its stale
// Pending status after the Ready change's own sync (OnWorkerScript) notified Running.
func TestProviderStaleSyncDoesNotWin(t *testing.T) {
	ctx := context.Background()
	ws := workerScript("ns", "api", "uid-api", false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ws).WithStatusSubresource(&workersv1alpha1.WorkerScript{}).Build()
	g := &gatedReader{Reader: c}
	p := NewProvider(g, fakeMapper{}, func() time.Time { return testNow }, logrDiscard())
	var n notifications
	p.NotifyPods(ctx, n.add)
	pod := standInPod(ws, "cf-workers")
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	n.wait(t, 1)

	gate, entered := make(chan struct{}), make(chan struct{})
	g.mu.Lock()
	g.gate, g.entered = gate, entered
	g.mu.Unlock()
	upd := pod.DeepCopy()
	upd.Labels["extra"] = "x"
	stale := make(chan error, 1)
	go func() { stale <- p.UpdatePod(ctx, upd) }()
	<-entered // UpdatePod has read the WorkerScript (not Ready)

	ready := ws.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(ws), ready); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{Type: commonv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Available"})
	if err := c.Status().Update(ctx, ready); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{})
	go func() { p.OnWorkerScript(ctx, "ns", "api"); close(changed) }()
	select {
	case <-changed: // without serialization OnWorkerScript notifies Running before UpdatePod ends
	case <-time.After(100 * time.Millisecond): // serialized: it waits for UpdatePod
	}
	close(gate)
	if err := <-stale; err != nil {
		t.Fatal(err)
	}
	<-changed

	got := n.wait(t, 2)
	if last := got[len(got)-1]; last.Status.Phase != corev1.PodRunning {
		t.Fatalf("last notification: phase %s, want Running (a stale sync won)", last.Status.Phase)
	}
	if st, err := p.GetPodStatus(ctx, "ns", pod.Name); err != nil || st.Phase != corev1.PodRunning {
		t.Fatalf("recorded status %v %v, want Running", st, err)
	}
}
