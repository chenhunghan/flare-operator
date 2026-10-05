package standin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
)

// Name is the stand-in controller's name (controller-runtime controller and event reporter).
const Name = "workers-standin"

// Event reasons the Reconciler records on WorkerScripts (contract.go; documented in
// hack/apidocs/reasons.yaml). It never writes a WorkerScript's status.
const (
	// EventReasonStandInPodFailed: the stand-in Pod could not be created, updated or deleted
	// (a ResourceQuota, a LimitRange or an admission policy refused it).
	EventReasonStandInPodFailed = "StandInPodFailed"
	// EventReasonStandInPodConflict: a Pod with the stand-in Pod's name exists and is not
	// controlled by this WorkerScript; it is left alone.
	EventReasonStandInPodConflict = "StandInPodConflict"
)

// AnnotationSpecHash is set by the Reconciler on every stand-in Pod it creates: a hash of the
// Pod spec the Builder rendered, without the container image. Most of a Pod's spec is
// immutable, so a Pod whose hash differs from the current Builder's (another node name,
// resources or tolerations) is deleted and created again, at most RecreatesPerSecond Pods per
// second across the cluster. A different image alone is patched in place (a container's image
// is mutable), so changing the placeholder image never recreates Pods.
const AnnotationSpecHash = "flare.dev/stand-in-spec-hash"

// DefaultImage is the placeholder image of stand-in Pods when none is configured. It is never
// pulled (nothing runs), and it is pinned rather than following operator releases, so an
// upgrade does not touch every stand-in Pod.
const DefaultImage = "registry.k8s.io/pause:3.10"

// DefaultRecreatesPerSecond limits how fast stand-in Pods whose spec changed are recreated, so a
// settings change of the virtual kubelet does not delete every stand-in Pod at once.
const DefaultRecreatesPerSecond = 2

// DefaultConflictRetry is how often a WorkerScript whose stand-in name is taken by a foreign Pod
// is checked again.
const DefaultConflictRetry = time.Minute

// Reconciler keeps one stand-in Pod per WorkerScript (docs/workers-logs-design.md §5): it
// creates the Pod, keeps its metadata, recreates it when its spec changes, and deletes it when
// the WorkerScript opts out (AnnotationStandInPod "false") or its namespace stops matching
// NamespaceSelector. A deleted WorkerScript's Pod is left to the garbage collector (its
// ownerReference). A Pod it does not control is never touched: it records
// EventReasonStandInPodConflict and retries.
//
// It reads WorkerScripts, Pods and (with a NamespaceSelector) Namespaces through Client, usually
// the manager's cache. The cache may limit Pods to those labelled StandInLabelRole; a foreign Pod
// outside it is found when the create fails with AlreadyExists (read through APIReader).
type Reconciler struct {
	Client client.Client
	// APIReader reads a Pod uncached after a create conflict; nil uses Client.
	APIReader client.Reader
	Builder   Builder
	// NamespaceSelector limits the namespaces whose WorkerScripts get stand-in Pods; nil or
	// empty selects every namespace (ParseNamespaceSelector).
	NamespaceSelector labels.Selector
	// Recorder records events on WorkerScripts (nil records none).
	Recorder events.EventRecorder
	// ConflictRetry defaults to DefaultConflictRetry.
	ConflictRetry time.Duration
	// RecreatesPerSecond limits recreations of stand-in Pods whose spec changed (default
	// DefaultRecreatesPerSecond; the burst is the same number).
	RecreatesPerSecond float64

	limiterOnce sync.Once
	limiter     *rate.Limiter
}

// ParseNamespaceSelector parses a label selector string (vk.Config.NamespaceSelector); "" selects
// every namespace.
func ParseNamespaceSelector(s string) (labels.Selector, error) {
	if strings.TrimSpace(s) == "" {
		return labels.Everything(), nil
	}
	sel, err := labels.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("namespace selector %q: %w", s, err)
	}
	return sel, nil
}

func (r *Reconciler) selects() bool {
	return r.NamespaceSelector != nil && !r.NamespaceSelector.Empty()
}

// SetupWithManager registers the controller: WorkerScripts (spec and annotation changes),
// Pods mapped back to their WorkerScript, and, with a NamespaceSelector, Namespace label
// changes.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&workersv1alpha1.WorkerScript{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(workerScriptsForPod), builder.WithPredicates(standInCandidate()))
	if r.selects() {
		b = b.Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.workerScriptsInNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{}))
	}
	return b.Complete(r)
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws workersv1alpha1.WorkerScript
	if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
		// Gone: the garbage collector deletes the Pod through its ownerReference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want, err := r.wanted(ctx, &ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired, err := r.Builder.Desired(&ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	hash, err := specHash(&desired.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired.Annotations[AnnotationSpecHash] = hash

	var pod corev1.Pod
	err = r.Client.Get(ctx, client.ObjectKeyFromObject(desired), &pod)
	switch {
	case apierrors.IsNotFound(err):
		if !want || ws.DeletionTimestamp != nil {
			return ctrl.Result{}, nil
		}
		err = r.Client.Create(ctx, desired)
		if err == nil {
			log.FromContext(ctx).V(1).Info("created stand-in pod", "pod", desired.Name)
			return ctrl.Result{}, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, r.failed(&ws, "Create", desired.Name, err)
		}
		// Not in the cache: a foreign Pod outside its label filter, or cache lag.
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), &pod); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	case err != nil:
		return ctrl.Result{}, err
	}

	if !metav1.IsControlledBy(&pod, &ws) {
		if stalePod(&pod, &ws) {
			if pod.DeletionTimestamp != nil {
				// Already being deleted: its deletion event brings us back; poll with back-off
				// in case that event is missed.
				return ctrl.Result{RequeueAfter: terminatingBackoff(&pod)}, nil
			}
			// Left by an earlier WorkerScript of the same name; the garbage collector would
			// delete it too, but the new one should not wait for it.
			if err := r.delete(ctx, &pod); err != nil {
				return ctrl.Result{}, r.failed(&ws, "Delete", pod.Name, err)
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if !want {
			return ctrl.Result{}, nil
		}
		r.event(&ws, corev1.EventTypeWarning, EventReasonStandInPodConflict, "Create",
			"pod %s exists and is not controlled by this WorkerScript; it is left alone, so `kubectl logs` is not available for this WorkerScript until it is removed", pod.Name)
		return ctrl.Result{RequeueAfter: r.conflictRetry()}, nil
	}
	if pod.DeletionTimestamp != nil {
		// Its deletion event brings us back.
		return ctrl.Result{}, nil
	}
	if !want {
		if err := r.delete(ctx, &pod); err != nil {
			return ctrl.Result{}, r.failed(&ws, "Delete", pod.Name, err)
		}
		log.FromContext(ctx).V(1).Info("deleted stand-in pod: opted out or namespace not selected", "pod", pod.Name)
		return ctrl.Result{}, nil
	}
	if pod.Annotations[AnnotationSpecHash] != hash {
		// Immutable fields changed: recreate, rate-limited across all WorkerScripts. The
		// deletion event brings us back to create it.
		if wait := r.recreateDelay(); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		if err := r.delete(ctx, &pod); err != nil {
			return ctrl.Result{}, r.failed(&ws, "Delete", pod.Name, err)
		}
		log.FromContext(ctx).V(1).Info("recreating stand-in pod: its spec changed", "pod", pod.Name)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if patch := podPatch(&pod, desired); patch != nil {
		if err := r.Client.Patch(ctx, &pod, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, r.failed(&ws, "Update", pod.Name, err)
		}
	}
	return ctrl.Result{}, nil
}

// wanted reports whether ws should have a stand-in Pod.
func (r *Reconciler) wanted(ctx context.Context, ws *workersv1alpha1.WorkerScript) (bool, error) {
	if ws.Annotations[workersv1alpha1.AnnotationStandInPod] == "false" {
		return false, nil
	}
	if !r.selects() {
		return true, nil
	}
	var ns corev1.Namespace
	if err := r.Client.Get(ctx, client.ObjectKey{Name: ws.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return r.NamespaceSelector.Matches(labels.Set(ns.Labels)), nil
}

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// recreateDelay reserves one recreation and returns 0, or returns how long to wait for one.
func (r *Reconciler) recreateDelay() time.Duration {
	r.limiterOnce.Do(func() {
		n := r.RecreatesPerSecond
		if n <= 0 {
			n = DefaultRecreatesPerSecond
		}
		r.limiter = rate.NewLimiter(rate.Limit(n), max(1, int(n)))
	})
	res := r.limiter.Reserve()
	if d := res.Delay(); d > 0 {
		res.Cancel()
		return d
	}
	return 0
}

// terminatingBackoff grows with the time a Pod has been terminating: 1s, then up to 1m.
func terminatingBackoff(pod *corev1.Pod) time.Duration {
	d := time.Since(pod.DeletionTimestamp.Time)
	return min(max(d, time.Second), time.Minute)
}

func (r *Reconciler) conflictRetry() time.Duration {
	if r.ConflictRetry > 0 {
		return r.ConflictRetry
	}
	return DefaultConflictRetry
}

// delete deletes pod if it is still the same object (UID precondition); NotFound is success.
func (r *Reconciler) delete(ctx context.Context, pod *corev1.Pod) error {
	err := r.Client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil // gone, or replaced by another Pod (the next reconcile looks at it)
	}
	return err
}

// failed records EventReasonStandInPodFailed for err and returns it (so the request is retried
// with back-off). A namespace that is being deleted refuses new objects; that is not a failure.
func (r *Reconciler) failed(ws *workersv1alpha1.WorkerScript, action, pod string, err error) error {
	if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		return nil
	}
	r.event(ws, corev1.EventTypeWarning, EventReasonStandInPodFailed, action, "stand-in pod %s: %v", pod, err)
	return err
}

func (r *Reconciler) event(ws *workersv1alpha1.WorkerScript, typ, reason, action, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(ws, nil, typ, reason, action, format, args...)
	}
}

// stalePod reports whether pod is the stand-in Pod of an earlier WorkerScript with ws's name
// (controller reference to a WorkerScript of that name with another UID).
func stalePod(pod *corev1.Pod, ws *workersv1alpha1.WorkerScript) bool {
	ref := metav1.GetControllerOf(pod)
	return ref != nil && isWorkerScriptRef(ref) && ref.Name == ws.Name && ref.UID != ws.UID &&
		pod.Labels[workersv1alpha1.StandInLabelRole] == workersv1alpha1.StandInRoleWorker
}

func isWorkerScriptRef(ref *metav1.OwnerReference) bool {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == workersv1alpha1.GroupVersion.Group && ref.Kind == "WorkerScript"
}

// specHash is a stable hash of a Pod spec (JSON of the Builder's output) without the container
// images, which are mutable and patched in place.
func specHash(spec *corev1.PodSpec) (string, error) {
	s := spec.DeepCopy()
	for i := range s.Containers {
		s.Containers[i].Image = ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// podPatch is a strategic merge patch that sets desired's labels, annotations and container
// images on pod, or nil when they are already set. Labels and annotations others add are kept.
func podPatch(pod, desired *corev1.Pod) []byte {
	out := map[string]any{}
	var containers []map[string]string
	for _, dc := range desired.Spec.Containers {
		for _, pc := range pod.Spec.Containers {
			if pc.Name == dc.Name && pc.Image != dc.Image {
				containers = append(containers, map[string]string{"name": dc.Name, "image": dc.Image})
			}
		}
	}
	if len(containers) > 0 {
		out["spec"] = map[string]any{"containers": containers}
	}
	md := map[string]map[string]string{}
	for k, v := range desired.Labels {
		if cur, ok := pod.Labels[k]; !ok || cur != v {
			if md["labels"] == nil {
				md["labels"] = map[string]string{}
			}
			md["labels"][k] = v
		}
	}
	for k, v := range desired.Annotations {
		if cur, ok := pod.Annotations[k]; !ok || cur != v {
			if md["annotations"] == nil {
				md["annotations"] = map[string]string{}
			}
			md["annotations"][k] = v
		}
	}
	if len(md) > 0 {
		out["metadata"] = md
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

// standInCandidate passes events of Pods that may be stand-in Pods: labelled as one, or
// controlled by a WorkerScript, or named like one (a foreign Pod in the way).
func standInCandidate() predicate.Predicate {
	ok := func(o client.Object) bool {
		if _, has := o.GetLabels()[workersv1alpha1.StandInLabelRole]; has {
			return true
		}
		if ref := metav1.GetControllerOfNoCopy(o); ref != nil && isWorkerScriptRef(ref) {
			return true
		}
		return strings.HasSuffix(o.GetName(), workersv1alpha1.StandInPodNameSuffix)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return ok(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return ok(e.ObjectNew) || ok(e.ObjectOld) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return ok(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return ok(e.Object) },
	}
}

// workerScriptsForPod maps a Pod to the WorkerScripts it may belong to: its controller (a
// WorkerScript), and the WorkerScript its name is derived from (<name>-worker), which is the
// one a foreign Pod blocks.
func workerScriptsForPod(_ context.Context, o client.Object) []ctrlreconcile.Request {
	names := map[string]bool{}
	if ref := metav1.GetControllerOfNoCopy(o); ref != nil && isWorkerScriptRef(ref) {
		names[ref.Name] = true
	}
	if n, ok := strings.CutSuffix(o.GetName(), workersv1alpha1.StandInPodNameSuffix); ok && n != "" {
		names[n] = true
	}
	if n := o.GetLabels()[workersv1alpha1.StandInLabelWorkerScript]; n != "" {
		names[n] = true
	}
	reqs := make([]ctrlreconcile.Request, 0, len(names))
	for n := range names {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: n}})
	}
	return reqs
}

// workerScriptsInNamespace enqueues every WorkerScript of a Namespace whose labels changed.
func (r *Reconciler) workerScriptsInNamespace(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	var l workersv1alpha1.WorkerScriptList
	if err := r.Client.List(ctx, &l, client.InNamespace(o.GetName())); err != nil {
		log.FromContext(ctx).Error(err, "list WorkerScripts for a namespace label change", "namespace", o.GetName())
		return nil
	}
	reqs := make([]ctrlreconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: l.Items[i].Namespace, Name: l.Items[i].Name}})
	}
	return reqs
}
