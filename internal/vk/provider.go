package vk

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

// Status reasons of Pods the provider cannot map yet.
const (
	// PodWorkerScriptNotFoundReason: a Pod shaped like a stand-in whose WorkerScript is missing from
	// the cache (not synced yet, or deleted and the Pod about to be garbage-collected). The Pod
	// stays Pending; it is never failed for this, since the WorkerScript may appear.
	PodWorkerScriptNotFoundReason = "WorkerScriptNotFound"
)

// Provider is virtual-kubelet's pod lifecycle handler (node.PodLifecycleHandler and
// node.PodNotifier) for the Workers virtual node. Nothing runs anywhere: "creating" a Pod only
// records it, and its status is computed from its WorkerScript (standin.StatusMapper). A Pod
// bound to the node that is not a stand-in Pod is failed (StatusMapper.Foreign). WorkerScript
// changes reach the PodController through NotifyPods (OnWorkerScript).
type Provider struct {
	reader client.Reader
	mapper standin.StatusMapper
	now    func() time.Time
	log    logr.Logger

	mu     sync.Mutex
	pods   map[types.NamespacedName]*corev1.Pod // last Pod seen, with the status last notified
	notify func(*corev1.Pod)
}

// NewProvider returns a Provider reading WorkerScripts through reader (the manager's cache).
func NewProvider(reader client.Reader, mapper standin.StatusMapper, now func() time.Time, log logr.Logger) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{reader: reader, mapper: mapper, now: now, log: log, pods: map[types.NamespacedName]*corev1.Pod{}}
}

// status computes pod's status. pod is not modified.
func (p *Provider) status(ctx context.Context, pod *corev1.Pod) (corev1.PodStatus, error) {
	now := p.now()
	ws, err := standInWorkerScript(ctx, p.reader, pod)
	switch {
	case errors.Is(err, ErrNotStandIn):
		return p.mapper.Foreign(pod, now), nil
	case errors.Is(err, errWorkerScriptMissing):
		return pendingStatus(pod, PodWorkerScriptNotFoundReason, err.Error(), now), nil
	case err != nil:
		return corev1.PodStatus{}, err
	}
	return p.mapper.Status(ws, pod, now), nil
}

// pendingStatus keeps a Pod Pending with its containers waiting for reason.
func pendingStatus(pod *corev1.Pod, reason, message string, now time.Time) corev1.PodStatus {
	st := corev1.PodStatus{Phase: corev1.PodPending, Reason: reason, Message: message}
	if pod.Status.StartTime != nil {
		st.StartTime = pod.Status.StartTime
	} else {
		st.StartTime = &metav1.Time{Time: now}
	}
	for _, c := range pod.Spec.Containers {
		st.ContainerStatuses = append(st.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		})
	}
	return st
}

// sync records pod with a freshly computed status and notifies the PodController when the
// status changed (or force is set).
func (p *Provider) sync(ctx context.Context, pod *corev1.Pod, force bool) error {
	st, err := p.status(ctx, pod)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	next := pod.DeepCopy()
	next.Status = st
	p.mu.Lock()
	prev := p.pods[key]
	if prev != nil && prev.UID != pod.UID {
		prev = nil
	}
	p.pods[key] = next
	notify := p.notify
	p.mu.Unlock()
	if notify != nil && (force || prev == nil || !equality.Semantic.DeepEqual(prev.Status, st)) {
		notify(next.DeepCopy())
	}
	return nil
}

// CreatePod implements node.PodLifecycleHandler.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	return p.sync(ctx, pod, true)
}

// UpdatePod implements node.PodLifecycleHandler.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	return p.sync(ctx, pod, false)
}

// DeletePod implements node.PodLifecycleHandler: the Pod is forgotten and reported terminated,
// so the PodController completes a graceful deletion at once.
func (p *Provider) DeletePod(_ context.Context, pod *corev1.Pod) error {
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	p.mu.Lock()
	known, ok := p.pods[key]
	if ok && known.UID == pod.UID {
		delete(p.pods, key)
	}
	notify := p.notify
	p.mu.Unlock()
	if !ok {
		return errdefs.NotFoundf("pod %s is not known to the provider", key)
	}
	if notify == nil || known.UID != pod.UID {
		// A notification is keyed by namespace/name: for an older Pod of the same name it would
		// land on the current one.
		return nil
	}
	done := known.DeepCopy()
	now := metav1.NewTime(p.now())
	done.Status.Phase = corev1.PodSucceeded
	done.Status.Reason = "Deleted"
	done.Status.Message = "the stand-in pod was deleted"
	for i := range done.Status.Conditions {
		if done.Status.Conditions[i].Type == corev1.PodReady || done.Status.Conditions[i].Type == corev1.ContainersReady {
			done.Status.Conditions[i].Status = corev1.ConditionFalse
			done.Status.Conditions[i].LastTransitionTime = now
		}
	}
	prev := map[string]corev1.ContainerStatus{}
	for _, cs := range done.Status.ContainerStatuses {
		prev[cs.Name] = cs
	}
	done.Status.ContainerStatuses = nil
	for _, c := range done.Spec.Containers {
		cs := prev[c.Name]
		var started metav1.Time
		if cs.State.Running != nil {
			started = cs.State.Running.StartedAt
		}
		cs.Name, cs.Image, cs.Ready, cs.Started = c.Name, c.Image, false, new(bool)
		cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Reason: "Completed", StartedAt: started, FinishedAt: now, ContainerID: cs.ContainerID,
		}}
		done.Status.ContainerStatuses = append(done.Status.ContainerStatuses, cs)
	}
	// The PodController drops the Pod from its known set before calling DeletePod for a Pod
	// already gone from the API; the notification then only times out, so do not block on it.
	go notify(done)
	return nil
}

// GetPod implements node.PodLifecycleHandler.
func (p *Provider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pod, ok := p.pods[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil, errdefs.NotFoundf("pod %s/%s is not known to the provider", namespace, name)
	}
	return pod.DeepCopy(), nil
}

// GetPodStatus implements node.PodLifecycleHandler.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	st, err := p.status(ctx, pod)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetPods implements node.PodLifecycleHandler.
func (p *Provider) GetPods(context.Context) ([]*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		out = append(out, pod.DeepCopy())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

// NotifyPods implements node.PodNotifier.
func (p *Provider) NotifyPods(_ context.Context, cb func(*corev1.Pod)) {
	p.mu.Lock()
	p.notify = cb
	p.mu.Unlock()
}

// OnWorkerScript recomputes the status of the Pods that show the WorkerScript namespace/name
// (call it from a WorkerScript informer on every add, update and delete).
func (p *Provider) OnWorkerScript(ctx context.Context, namespace, name string) {
	p.mu.Lock()
	var pods []*corev1.Pod
	for key, pod := range p.pods {
		if key.Namespace != namespace {
			continue
		}
		if ref, err := standInOwner(pod); err == nil && ref.Name == name {
			pods = append(pods, pod.DeepCopy())
		}
	}
	p.mu.Unlock()
	for _, pod := range pods {
		if err := p.sync(ctx, pod, false); err != nil {
			p.log.Error(err, "recompute stand-in pod status", "pod", client.ObjectKeyFromObject(pod), "workerScript", name)
		}
	}
}

// workerScriptKey returns the namespace and name of a WorkerScript informer object, including
// a deleted-final-state tombstone.
func workerScriptKey(obj any) (string, string, bool) {
	if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		ns, name, err := toolscache.SplitMetaNamespaceKey(t.Key)
		return ns, name, err == nil
	}
	if o, ok := obj.(client.Object); ok {
		return o.GetNamespace(), o.GetName(), true
	}
	return "", "", false
}
