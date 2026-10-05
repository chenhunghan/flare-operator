package standin_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/testenv"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, &env, testenv.Options{AddToScheme: []func(*runtime.Scheme) error{workersv1alpha1.AddToScheme}})
}

const envTimeout = 20 * time.Second

// restrictedNamespace is a namespace that enforces Pod Security "restricted".
func restrictedNamespace(t *testing.T, e *testenv.Env) string {
	t.Helper()
	ns := e.Namespace(t)
	var o corev1.Namespace
	if err := e.Client.Get(context.Background(), client.ObjectKey{Name: ns}, &o); err != nil {
		t.Fatal(err)
	}
	if o.Labels == nil {
		o.Labels = map[string]string{}
	}
	o.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
	o.Labels["pod-security.kubernetes.io/enforce-version"] = "latest"
	if err := e.Client.Update(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	return ns
}

// startStandIn runs the stand-in Reconciler in a manager restricted to ns.
func startStandIn(t *testing.T, e *testenv.Env, ns string, cfg standin.PodConfig, sel labels.Selector) *testenv.Manager {
	t.Helper()
	return e.StartManager(t, testenv.ManagerOptions{Namespaces: []string{ns}, Setup: []func(ctrl.Manager, controller.Deps) error{
		func(mgr ctrl.Manager, _ controller.Deps) error {
			return (&standin.Reconciler{Builder: standin.NewBuilder(cfg), NamespaceSelector: sel,
				Recorder: mgr.GetEventRecorder(standin.Name), ConflictRetry: 2 * time.Second}).SetupWithManager(mgr)
		},
	}})
}

func newWorkerScript(t *testing.T, e *testenv.Env, ns, name string) *workersv1alpha1.WorkerScript {
	t.Helper()
	ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: workersv1alpha1.WorkerScriptSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"},
			ManagementPolicies: []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}}}}
	if err := e.Client.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func getPod(e *testenv.Env, ns, name string) (*corev1.Pod, error) {
	var p corev1.Pod
	err := e.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &p)
	return &p, err
}

// waitPod waits until the Pod exists and ok accepts it.
func waitPod(t *testing.T, e *testenv.Env, ns, name string, ok func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	var p *corev1.Pod
	testenv.Eventually(t, envTimeout, func() (bool, string) {
		var err error
		if p, err = getPod(e, ns, name); err != nil {
			return false, err.Error()
		}
		return ok == nil || ok(p), fmt.Sprintf("pod %s: uid %s labels %v", name, p.UID, p.Labels)
	})
	return p
}

func waitNoPod(t *testing.T, e *testenv.Env, ns, name string) {
	t.Helper()
	testenv.Eventually(t, envTimeout, func() (bool, string) {
		p, err := getPod(e, ns, name)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		return false, fmt.Sprintf("pod %s still exists (deletionTimestamp %v, err %v)", name, p.DeletionTimestamp, err)
	})
}

func setAnnotation(t *testing.T, e *testenv.Env, ws *workersv1alpha1.WorkerScript, k, v string) {
	t.Helper()
	if err := e.Client.Get(context.Background(), client.ObjectKeyFromObject(ws), ws); err != nil {
		t.Fatal(err)
	}
	base := ws.DeepCopy()
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	if v == "" {
		delete(ws.Annotations, k)
	} else {
		ws.Annotations[k] = v
	}
	if err := e.Client.Patch(context.Background(), ws, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

var envCfg = standin.PodConfig{NodeName: "cf-workers", Image: "registry.example/flare-operator:a"}

// TestEnvLifecycle: the Pod is created with the exact spec and admitted under Pod Security
// "restricted"; a user's delete is immediate (grace 0) and the Pod comes back; metadata is
// repaired; opting out deletes it and opting in again recreates it.
func TestEnvLifecycle(t *testing.T) {
	e := testenv.Require(t, env)
	ns := restrictedNamespace(t, e)

	// Enforcement is on: an ordinary Pod is refused.
	bad := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "privileged"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}}}
	if err := e.Client.Create(context.Background(), bad); !apierrors.IsForbidden(err) {
		t.Fatalf("a non-restricted pod in the enforcing namespace: err %v, want Forbidden (is PodSecurity admission on?)", err)
	}

	startStandIn(t, e, ns, envCfg, nil)
	ws := newWorkerScript(t, e, ns, "api")
	p := waitPod(t, e, ns, "api-worker", nil)
	want, err := standin.NewBuilder(envCfg).Desired(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(p, ws) || p.Spec.NodeName != "cf-workers" || p.Spec.Containers[0].Image != envCfg.Image ||
		ptr.Deref(p.Spec.TerminationGracePeriodSeconds, -1) != 0 || ptr.Deref(p.Spec.AutomountServiceAccountToken, true) {
		t.Errorf("pod = %+v", p)
	}
	for k, v := range want.Labels {
		if p.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, p.Labels[k], v)
		}
	}
	if p.Labels[workersv1alpha1.StandInLabelWorkerScriptUID] != string(ws.UID) {
		t.Errorf("uid label %q, WorkerScript uid %s", p.Labels[workersv1alpha1.StandInLabelWorkerScriptUID], ws.UID)
	}
	if p.Status.QOSClass != corev1.PodQOSGuaranteed {
		t.Errorf("qosClass = %q, want Guaranteed (requests = limits)", p.Status.QOSClass)
	}
	checkRestricted(t, p)

	// kubectl delete pod (default options): with grace 0 the API server removes it at once,
	// no kubelet needed; the Reconciler creates a new one.
	if err := e.Client.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if q, err := getPod(e, ns, "api-worker"); err == nil && q.UID == p.UID {
		t.Fatalf("pod still exists right after delete (deletionTimestamp %v): grace 0 is not immediate", q.DeletionTimestamp)
	}
	old := p.UID
	p = waitPod(t, e, ns, "api-worker", func(p *corev1.Pod) bool { return p.UID != old })

	// Metadata drift is repaired without recreating.
	base := p.DeepCopy()
	p.Labels[workersv1alpha1.StandInLabelRole] = "tampered"
	if err := e.Client.Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	waitPod(t, e, ns, "api-worker", func(q *corev1.Pod) bool {
		return q.UID == p.UID && q.Labels[workersv1alpha1.StandInLabelRole] == workersv1alpha1.StandInRoleWorker
	})
	// The script name follows the external-id annotation.
	setAnnotation(t, e, ws, commonv1alpha1.AnnotationExternalID, "pinned")
	waitPod(t, e, ns, "api-worker", func(q *corev1.Pod) bool {
		return q.UID == p.UID && q.Annotations[workersv1alpha1.StandInAnnotationScriptName] == "pinned"
	})

	// Opt out, then in again.
	setAnnotation(t, e, ws, workersv1alpha1.AnnotationStandInPod, "false")
	waitNoPod(t, e, ns, "api-worker")
	setAnnotation(t, e, ws, workersv1alpha1.AnnotationStandInPod, "")
	waitPod(t, e, ns, "api-worker", nil)
}

// TestEnvRecreateOnSpecChange: a virtual kubelet restarted with other settings. A new image is
// patched into the same Pod (the API server accepts the image change); new resources recreate
// it, since the rest of a Pod spec is immutable.
func TestEnvRecreateOnSpecChange(t *testing.T) {
	e := testenv.Require(t, env)
	ns := restrictedNamespace(t, e)
	mgr := startStandIn(t, e, ns, envCfg, nil)
	newWorkerScript(t, e, ns, "api")
	p := waitPod(t, e, ns, "api-worker", nil)
	mgr.Stop(t)

	cfg := envCfg
	cfg.Image = "registry.example/flare-operator:b"
	mgr = startStandIn(t, e, ns, cfg, nil)
	waitPod(t, e, ns, "api-worker", func(q *corev1.Pod) bool {
		return q.UID == p.UID && q.Spec.Containers[0].Image == cfg.Image
	})
	mgr.Stop(t)

	cfg.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2m"), corev1.ResourceMemory: resource.MustParse("2Mi")}
	startStandIn(t, e, ns, cfg, nil)
	waitPod(t, e, ns, "api-worker", func(q *corev1.Pod) bool {
		return q.UID != p.UID && q.Spec.Containers[0].Resources.Limits.Cpu().Equal(resource.MustParse("2m"))
	})
}

// TestEnvNamespaceSelector: only selected namespaces get Pods; a namespace that stops matching
// loses them.
func TestEnvNamespaceSelector(t *testing.T) {
	e := testenv.Require(t, env)
	ns := restrictedNamespace(t, e)
	sel, err := standin.ParseNamespaceSelector("flare.dev/worker-logs=on")
	if err != nil {
		t.Fatal(err)
	}
	startStandIn(t, e, ns, envCfg, sel)
	newWorkerScript(t, e, ns, "api")
	time.Sleep(2 * time.Second)
	if _, err := getPod(e, ns, "api-worker"); !apierrors.IsNotFound(err) {
		t.Fatalf("pod in an unselected namespace: %v", err)
	}
	label := func(v string) {
		var o corev1.Namespace
		if err := e.Client.Get(context.Background(), client.ObjectKey{Name: ns}, &o); err != nil {
			t.Fatal(err)
		}
		base := o.DeepCopy()
		if v == "" {
			delete(o.Labels, "flare.dev/worker-logs")
		} else {
			o.Labels["flare.dev/worker-logs"] = v
		}
		if err := e.Client.Patch(context.Background(), &o, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
	}
	label("on")
	waitPod(t, e, ns, "api-worker", nil)
	label("")
	waitNoPod(t, e, ns, "api-worker")
}

// TestEnvForeignPod: a Pod with the stand-in name that the WorkerScript does not control is
// left alone, with a StandInPodConflict event; once it is gone the stand-in Pod is created.
func TestEnvForeignPod(t *testing.T) {
	e := testenv.Require(t, env)
	ns := restrictedNamespace(t, e)
	foreignWS := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "not-a-real-uid"}}
	_, err := standin.NewBuilder(standin.PodConfig{NodeName: "elsewhere", Image: "mine"}).Desired(foreignWS)
	if err == nil {
		t.Fatal("builder accepted a WorkerScript without a namespace")
	}
	foreignWS.Namespace = ns
	foreign, err := standin.NewBuilder(standin.PodConfig{NodeName: "elsewhere", Image: "mine"}).Desired(foreignWS)
	if err != nil {
		t.Fatal(err)
	}
	foreign.OwnerReferences = nil // carries our labels, but nobody's controller reference
	if err := e.Client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	startStandIn(t, e, ns, envCfg, nil)
	ws := newWorkerScript(t, e, ns, "web")

	testenv.Eventually(t, envTimeout, func() (bool, string) {
		var l eventsv1.EventList
		if err := e.Client.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
			return false, err.Error()
		}
		for _, ev := range l.Items {
			if ev.Reason == standin.EventReasonStandInPodConflict && ev.Regarding.Name == "web" && ev.Regarding.Kind == "WorkerScript" &&
				ev.Type == corev1.EventTypeWarning {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d events, no StandInPodConflict", len(l.Items))
	})
	p, err := getPod(e, ns, "web-worker")
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != foreign.UID || p.Spec.NodeName != "elsewhere" || len(p.OwnerReferences) != 0 ||
		p.Annotations[standin.AnnotationSpecHash] != "" {
		t.Errorf("foreign pod was modified: %+v", p.ObjectMeta)
	}

	// The foreign Pod goes away: the stand-in Pod takes its place.
	if err := e.Client.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitPod(t, e, ns, "web-worker", func(q *corev1.Pod) bool { return metav1.IsControlledBy(q, ws) })
}
