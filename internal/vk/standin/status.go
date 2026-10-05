package standin

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
)

// ContainerIDScheme prefixes a stand-in container's containerID and imageID:
// cloudflare-workers://<script>@<version_id>.
const ContainerIDScheme = "cloudflare-workers://"

// Reasons and messages of stand-in Pod status that do not come from the WorkerScript.
const (
	// waitingReason is the container's waiting reason before the WorkerScript has a Ready
	// condition (the manager has not reconciled it yet).
	waitingReason  = "ContainerCreating"
	waitingMessage = "waiting for the WorkerScript to be reconciled"
	// foreignMessage explains ForeignReason.
	foreignMessage = "the Cloudflare Workers virtual node runs only the stand-in Pods of WorkerScripts; this Pod was bound to it but cannot run there"
)

// NewStatusMapper returns the StatusMapper of docs/workers-logs-design.md §5.2.
func NewStatusMapper() StatusMapper { return statusMapper{} }

type statusMapper struct{}

// containerID is cloudflare-workers://<script>@<version> (or without @<version> when the
// version is unknown, e.g. an observe-only script read before its deployment).
func containerID(script, version string) string {
	if version == "" {
		return ContainerIDScheme + script
	}
	return ContainerIDScheme + script + "@" + version
}

// Status implements StatusMapper.
func (statusMapper) Status(ws *workersv1alpha1.WorkerScript, pod *corev1.Pod, now time.Time) corev1.PodStatus {
	ready := meta.FindStatusCondition(ws.Status.Conditions, commonv1alpha1.ConditionReady)
	isReady := ready != nil && ready.Status == metav1.ConditionTrue
	version := ws.Status.AtProvider.VersionID
	created := pod.CreationTimestamp
	if created.IsZero() {
		created = metav1.NewTime(now)
	}

	reason, message := waitingReason, waitingMessage
	if ready != nil {
		reason, message = ready.Reason, ready.Message
	}
	// readySince is when the WorkerScript's Ready condition last changed, never before the Pod.
	readySince := created
	if ready != nil && ready.LastTransitionTime.After(created.Time) {
		readySince = ready.LastTransitionTime
	}

	image := ""
	for _, c := range pod.Spec.Containers {
		if c.Name == workersv1alpha1.StandInContainerName {
			image = c.Image
		}
	}
	cs := corev1.ContainerStatus{
		Name:         workersv1alpha1.StandInContainerName,
		Image:        image,
		RestartCount: 0,
	}
	phase := corev1.PodRunning
	if !isReady && version == "" {
		// Never deployed: the "container" is still being created.
		phase = corev1.PodPending
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: reason, Message: message}
		cs.Started = ptr.To(false)
	} else {
		id := containerID(ws.ScriptName(), version)
		cs.ContainerID, cs.ImageID = id, id
		cs.Ready = isReady
		cs.Started = ptr.To(true)
		// A running container keeps its start time until it changes identity (a new version).
		started := readySince
		if prev := previousContainer(pod); prev != nil && prev.ContainerID == id && prev.State.Running != nil {
			started = prev.State.Running.StartedAt
		}
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: started}
	}

	readyStatus := corev1.ConditionFalse
	readyReason, readyMessage := reason, message
	if isReady {
		readyStatus, readyReason, readyMessage = corev1.ConditionTrue, "", ""
	}
	started := phase == corev1.PodRunning
	conds := []corev1.PodCondition{
		condition(pod, corev1.PodReadyToStartContainers, boolStatus(started), "", "", readySince),
		condition(pod, corev1.PodInitialized, corev1.ConditionTrue, "", "", created),
		condition(pod, corev1.ContainersReady, readyStatus, readyReason, readyMessage, readySince),
		condition(pod, corev1.PodReady, readyStatus, readyReason, readyMessage, readySince),
		condition(pod, corev1.PodScheduled, corev1.ConditionTrue, "", "", created),
	}

	return corev1.PodStatus{
		Phase:             phase,
		Conditions:        conds,
		StartTime:         startTime(pod, created),
		ContainerStatuses: []corev1.ContainerStatus{cs},
		// Set by the API server at creation and immutable since in-place resize.
		QOSClass: pod.Status.QOSClass,
	}
}

// Foreign implements StatusMapper.
func (statusMapper) Foreign(pod *corev1.Pod, now time.Time) corev1.PodStatus {
	st := *pod.Status.DeepCopy()
	st.Phase = corev1.PodFailed
	st.Reason = ForeignReason
	st.Message = foreignMessage
	created := pod.CreationTimestamp
	if created.IsZero() {
		created = metav1.NewTime(now)
	}
	st.StartTime = startTime(pod, created)
	return st
}

func boolStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func startTime(pod *corev1.Pod, created metav1.Time) *metav1.Time {
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.DeepCopy()
	}
	return &created
}

func previousContainer(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == workersv1alpha1.StandInContainerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// condition builds a Pod condition. Its lastTransitionTime is kept from the Pod's current
// condition of that type when the status is unchanged, else it is since, so repeated calls give
// the same result.
func condition(pod *corev1.Pod, typ corev1.PodConditionType, status corev1.ConditionStatus, reason, message string, since metav1.Time) corev1.PodCondition {
	c := corev1.PodCondition{Type: typ, Status: status, Reason: reason, Message: message,
		LastTransitionTime: since}
	for _, prev := range pod.Status.Conditions {
		if prev.Type == typ && prev.Status == status && !prev.LastTransitionTime.IsZero() {
			c.LastTransitionTime = prev.LastTransitionTime
		}
	}
	return c
}
