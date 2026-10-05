package standin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := workersv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func reqFor(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

type unit struct {
	t   *testing.T
	c   client.Client
	rec *events.FakeRecorder
	r   *standin.Reconciler
}

func newUnit(t *testing.T, cfg standin.PodConfig, c client.Client) *unit {
	rec := events.NewFakeRecorder(20)
	return &unit{t: t, c: c, rec: rec, r: &standin.Reconciler{Client: c, Builder: standin.NewBuilder(cfg), Recorder: rec}}
}

func (u *unit) reconcile(ws *workersv1alpha1.WorkerScript) ctrl.Result {
	u.t.Helper()
	res, err := u.r.Reconcile(context.Background(), reqFor(ws.Namespace, ws.Name))
	if err != nil {
		u.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (u *unit) pod(name string) *corev1.Pod {
	u.t.Helper()
	var p corev1.Pod
	err := u.c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, &p)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		u.t.Fatal(err)
	}
	return &p
}

func (u *unit) events() []string {
	var out []string
	for {
		select {
		case e := <-u.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var cfgA = standin.PodConfig{NodeName: "cf-workers", Image: "img:a"}

func TestReconcileLifecycle(t *testing.T) {
	ws := workerScript("api")
	u := newUnit(t, cfgA, fakeClient(t, ws))

	// Create.
	u.reconcile(ws)
	p := u.pod("api-worker")
	if p == nil {
		t.Fatal("no stand-in pod created")
	}
	if !metav1.IsControlledBy(p, ws) || p.Spec.NodeName != "cf-workers" || p.Annotations[standin.AnnotationSpecHash] == "" {
		t.Errorf("pod = %+v", p.ObjectMeta)
	}
	uid, rv := p.UID, p.ResourceVersion

	// Idempotent: nothing written.
	u.reconcile(ws)
	if p = u.pod("api-worker"); p.ResourceVersion != rv {
		t.Errorf("second reconcile wrote the pod (resourceVersion %s -> %s)", rv, p.ResourceVersion)
	}

	// Metadata drift is repaired in place; labels others add are kept.
	p.Labels[workersv1alpha1.StandInLabelRole] = "x"
	p.Labels["extra"] = "kept"
	delete(p.Annotations, workersv1alpha1.StandInAnnotationScriptName)
	if err := u.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ws.Annotations = map[string]string{"flare.dev/external-id": "pinned-script"}
	if err := u.c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	p = u.pod("api-worker")
	if p.UID != uid || p.Labels[workersv1alpha1.StandInLabelRole] != "worker" || p.Labels["extra"] != "kept" ||
		p.Annotations[workersv1alpha1.StandInAnnotationScriptName] != "pinned-script" {
		t.Errorf("metadata not repaired in place: uid %s, labels %v, annotations %v", p.UID, p.Labels, p.Annotations)
	}

	// Opt out: deleted. Opt back in: created again.
	ws.Annotations[workersv1alpha1.AnnotationStandInPod] = "false"
	if err := u.c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	if u.pod("api-worker") != nil {
		t.Error("opted-out WorkerScript kept its pod")
	}
	u.reconcile(ws) // nothing to do
	ws.Annotations[workersv1alpha1.AnnotationStandInPod] = "true"
	if err := u.c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	if u.pod("api-worker") == nil {
		t.Error("opted-in WorkerScript has no pod")
	}
	if ev := u.events(); len(ev) != 0 {
		t.Errorf("events = %v, want none", ev)
	}
}

// TestReconcileImageChangePatchesInPlace: a new placeholder image (an upgrade) patches the
// container image; it never deletes the Pod.
func TestReconcileImageChangePatchesInPlace(t *testing.T) {
	ws := workerScript("api")
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ws).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	newUnit(t, cfgA, c).reconcile(ws)
	old := newUnit(t, cfgA, c).pod("api-worker")
	u := newUnit(t, standin.PodConfig{NodeName: "cf-workers", Image: "img:b"}, c)
	if res := u.reconcile(ws); res.RequeueAfter != 0 {
		t.Errorf("requeueAfter %v after an image change", res.RequeueAfter)
	}
	p := u.pod("api-worker")
	if p == nil || p.Spec.Containers[0].Image != "img:b" || deletes != 0 {
		t.Fatalf("image not patched in place (deletes %d): %+v", deletes, p)
	}
	if p.Annotations[standin.AnnotationSpecHash] != old.Annotations[standin.AnnotationSpecHash] {
		t.Error("the spec hash depends on the image")
	}
	if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].SecurityContext == nil || p.Spec.Containers[0].Resources.Limits.Cpu().IsZero() {
		t.Errorf("the patch lost container fields: %+v", p.Spec.Containers)
	}
}

// TestReconcileRecreatesOnSpecChange: other spec changes (resources) recreate the Pod, at most
// RecreatesPerSecond per second.
func TestReconcileRecreatesOnSpecChange(t *testing.T) {
	var wss []client.Object
	for _, n := range []string{"a", "b", "c"} {
		wss = append(wss, workerScript(n))
	}
	c := fakeClient(t, wss...)
	for _, o := range wss {
		newUnit(t, cfgA, c).reconcile(o.(*workersv1alpha1.WorkerScript))
	}
	bigger := standin.PodConfig{NodeName: "cf-workers", Image: "img:a",
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2m"), corev1.ResourceMemory: resource.MustParse("2Mi")}}
	u := newUnit(t, bigger, c)
	u.r.RecreatesPerSecond = 1
	var limited int
	for _, o := range wss {
		ws := o.(*workersv1alpha1.WorkerScript)
		res := u.reconcile(ws)
		if res.RequeueAfter == 0 {
			t.Errorf("%s: no requeue", ws.Name)
		}
		if u.pod(ws.Name+"-worker") != nil {
			limited++
		}
	}
	if limited != 2 {
		t.Errorf("%d of 3 outdated pods kept by the rate limit, want 2 (one recreation per second)", limited)
	}
	ws := wss[0].(*workersv1alpha1.WorkerScript)
	u.reconcile(ws)
	p := u.pod("a-worker")
	if p == nil || !p.Spec.Containers[0].Resources.Limits.Cpu().Equal(resource.MustParse("2m")) {
		t.Errorf("pod not recreated with the new spec: %+v", p)
	}
}

// TestReconcileStaleTerminating: a stale Pod that is already terminating is not deleted again;
// the request is retried with back-off.
func TestReconcileStaleTerminating(t *testing.T) {
	ws := workerScript("api")
	prev := workerScript("api")
	prev.UID = "old-uid"
	stale, err := standin.NewBuilder(cfgA).Desired(prev)
	if err != nil {
		t.Fatal(err)
	}
	stale.Finalizers = []string{"example.com/hold"}
	stale.DeletionTimestamp = ptr.To(metav1.NewTime(time.Now().Add(-30 * time.Second)))
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ws, stale).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	u := newUnit(t, cfgA, c)
	res := u.reconcile(ws)
	if deletes != 0 {
		t.Errorf("terminating stale pod deleted again (%d deletes)", deletes)
	}
	if res.RequeueAfter < 20*time.Second || res.RequeueAfter > time.Minute {
		t.Errorf("requeueAfter = %v, want a back-off that grows with the terminating time (about 30s)", res.RequeueAfter)
	}
}

func TestReconcileForeignPod(t *testing.T) {
	ws := workerScript("api")
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "api-worker",
		Labels: map[string]string{workersv1alpha1.StandInLabelRole: "worker", workersv1alpha1.StandInLabelWorkerScript: "api"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "mine"}}}}
	u := newUnit(t, cfgA, fakeClient(t, ws, foreign))
	res := u.reconcile(ws)
	if res.RequeueAfter != standin.DefaultConflictRetry {
		t.Errorf("requeueAfter = %v, want %v", res.RequeueAfter, standin.DefaultConflictRetry)
	}
	p := u.pod("api-worker")
	if p.Spec.Containers[0].Image != "mine" || len(p.OwnerReferences) != 0 || p.Annotations[standin.AnnotationSpecHash] != "" {
		t.Errorf("foreign pod modified: %+v", p)
	}
	ev := u.events()
	if len(ev) != 1 || !strings.Contains(ev[0], "Warning StandInPodConflict") {
		t.Errorf("events = %v, want one StandInPodConflict warning", ev)
	}
	// Opted out: the foreign Pod is still left alone, without an event.
	ws.Annotations = map[string]string{workersv1alpha1.AnnotationStandInPod: "false"}
	if err := u.c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	if u.pod("api-worker") == nil || len(u.events()) != 0 {
		t.Error("foreign pod deleted, or an event recorded, for an opted-out WorkerScript")
	}
	// A foreign Pod controlled by another kind is not stale either.
	foreign2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web-worker",
		Labels:          map[string]string{workersv1alpha1.StandInLabelRole: "worker"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web", UID: "rs", Controller: ptr.To(true)}}}}
	ws2 := workerScript("web")
	u = newUnit(t, cfgA, fakeClient(t, ws2, foreign2))
	u.reconcile(ws2)
	if p := u.pod("web-worker"); p == nil || p.UID != foreign2.UID {
		t.Error("pod controlled by a ReplicaSet was touched")
	}
}

// TestReconcileStalePod: the Pod of an earlier WorkerScript with the same name (another UID) is
// replaced.
func TestReconcileStalePod(t *testing.T) {
	ws := workerScript("api")
	prev := workerScript("api")
	prev.UID = "old-uid"
	stale, err := standin.NewBuilder(cfgA).Desired(prev)
	if err != nil {
		t.Fatal(err)
	}
	u := newUnit(t, cfgA, fakeClient(t, ws, stale))
	u.reconcile(ws)
	if u.pod("api-worker") != nil {
		t.Fatal("stale pod not deleted")
	}
	u.reconcile(ws)
	if p := u.pod("api-worker"); p == nil || !metav1.IsControlledBy(p, ws) {
		t.Errorf("no pod for the new WorkerScript: %+v", p)
	}
}

func TestReconcileNamespaceSelector(t *testing.T) {
	ws := workerScript("api")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	u := newUnit(t, cfgA, fakeClient(t, ws, ns))
	sel, err := standin.ParseNamespaceSelector("flare.dev/worker-logs=on")
	if err != nil {
		t.Fatal(err)
	}
	u.r.NamespaceSelector = sel
	u.reconcile(ws)
	if u.pod("api-worker") != nil {
		t.Fatal("pod created in a namespace the selector excludes")
	}
	ns.Labels = map[string]string{"flare.dev/worker-logs": "on"}
	if err := u.c.Update(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	if u.pod("api-worker") == nil {
		t.Fatal("no pod in a selected namespace")
	}
	ns.Labels = nil
	if err := u.c.Update(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	u.reconcile(ws)
	if u.pod("api-worker") != nil {
		t.Error("pod kept after the namespace stopped matching")
	}
	u.r.NamespaceSelector = labels.Everything()
	u.reconcile(ws)
	if u.pod("api-worker") == nil {
		t.Error("an empty selector does not select every namespace")
	}
}

func TestReconcileCreateFailure(t *testing.T) {
	ws := workerScript("api")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ws).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewForbidden(corev1.Resource("pods"), obj.GetName(), errQuota)
		},
	}).Build()
	u := newUnit(t, cfgA, c)
	if _, err := u.r.Reconcile(context.Background(), reqFor("team-a", "api")); err == nil {
		t.Error("no error returned for a refused create (no back-off retry)")
	}
	ev := u.events()
	if len(ev) != 1 || !strings.Contains(ev[0], "Warning StandInPodFailed") || !strings.Contains(ev[0], "exceeded quota") {
		t.Errorf("events = %v, want one StandInPodFailed warning naming the cause", ev)
	}

	// A terminating namespace refuses new objects: no event, no error.
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ws).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			err := apierrors.NewForbidden(corev1.Resource("pods"), obj.GetName(), errTerminating)
			err.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: corev1.NamespaceTerminatingCause}}
			return err
		},
	}).Build()
	u = newUnit(t, cfgA, c)
	if _, err := u.r.Reconcile(context.Background(), reqFor("team-a", "api")); err != nil {
		t.Errorf("terminating namespace: %v", err)
	}
	if ev := u.events(); len(ev) != 0 {
		t.Errorf("terminating namespace: events %v", ev)
	}
}

type constErr string

func (e constErr) Error() string { return string(e) }

const (
	errQuota       = constErr(`exceeded quota: q, requested: pods=1, used: pods=1, limited: pods=1`)
	errTerminating = constErr(`unable to create new content in namespace team-a because it is being terminated`)
)
