// Package standin keeps one stand-in Pod per WorkerScript on the Workers virtual node and maps
// a WorkerScript's state to its Pod's status. Design: docs/workers-logs-design.md §5.
//
// Frozen contract; change deliberately. Workstream C implements it (plus the Reconciler, a
// controller-runtime reconciler of WorkerScripts that owns their Pods); workstream B's provider
// calls StatusMapper and wires Notify into virtual-kubelet's NotifyPods.
//
// Ownership of a stand-in Pod is split by field, never shared:
//
//   - the stand-in Reconciler creates and deletes it and owns metadata and spec;
//   - virtual-kubelet's PodController (internal/vk) owns status, from StatusMapper.
package standin

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
)

// Workstream C provides:
//
//	// PodName is the stand-in Pod's name for a WorkerScript name (api/workers/v1alpha1
//	// StandInPodNameSuffix describes the rule).
//	func PodName(workerScript string) string
//
//	// NewBuilder and NewStatusMapper return the implementations of the interfaces below.
//	func NewBuilder(cfg PodConfig) Builder
//	func NewStatusMapper() StatusMapper

// PodConfig is what every stand-in Pod shares.
type PodConfig struct {
	// NodeName is the virtual node (spec.nodeName; the scheduler is bypassed).
	NodeName string
	// Image is the placeholder image (never pulled: nothing runs).
	Image string
	// Resources of the container; requests equal limits so that namespaces with a ResourceQuota
	// or LimitRange accept the Pod (default cpu 1m, memory 1Mi).
	Requests corev1.ResourceList
	// ExtraLabels are added to every stand-in Pod (chart workersLogs.podLabels).
	ExtraLabels map[string]string
}

// DefaultRequests are PodConfig.Requests when none are configured.
var DefaultRequests = corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("1m"),
	corev1.ResourceMemory: resource.MustParse("1Mi"),
}

// Builder renders the desired stand-in Pod of a WorkerScript.
//
// The Pod: name PodName(ws.Name) in ws's namespace; labels StandInLabelWorkerScript,
// StandInLabelWorkerScriptUID, StandInLabelRole=worker, app.kubernetes.io/managed-by=
// flare-operator; annotation StandInAnnotationScriptName; one controller ownerReference to ws
// with blockOwnerDeletion false (a foreground delete of the WorkerScript never waits on the
// virtual kubelet); spec.nodeName, terminationGracePeriodSeconds 0 (deletion is immediate, so a
// stopped virtual kubelet never leaves Pods Terminating), automountServiceAccountToken false,
// enableServiceLinks false, restartPolicy Always; tolerations for TaintKey=TaintValue:NoSchedule
// and, without tolerationSeconds, node.kubernetes.io/not-ready and unreachable (NoExecute), so a
// virtual kubelet outage never evicts; one container StandInContainerName with the image,
// imagePullPolicy IfNotPresent and the requests as requests and limits; Pod Security
// "restricted": runAsNonRoot, runAsUser 65532, seccompProfile RuntimeDefault,
// allowPrivilegeEscalation false, capabilities drop ALL, readOnlyRootFilesystem.
type Builder interface {
	Desired(ws *workersv1alpha1.WorkerScript) (*corev1.Pod, error)
}

// StatusMapper computes a stand-in Pod's status from its WorkerScript. It is pure: the same
// inputs give the same status (times come from the objects; now only stamps a first
// transition), so virtual-kubelet's status sync does not write unchanged statuses.
//
// Mapping (docs/workers-logs-design.md §5.2): WorkerScript Ready=True → phase Running, container
// running, Ready and ContainersReady True. Ready not True and the script was never deployed
// (status.atProvider.version_id empty) → Pending, container waiting with the Ready condition's
// reason and message. Ready not True after a deployment → Running, container running, Ready
// False with that reason and message. Never Failed or Succeeded: Kubernetes does not let a Pod
// leave a terminal phase, and a WorkerScript recovers. The container's imageID and containerID
// are cloudflare-workers://<script>@<version_id>; podIP and hostIP stay empty.
type StatusMapper interface {
	Status(ws *workersv1alpha1.WorkerScript, pod *corev1.Pod, now time.Time) corev1.PodStatus
	// Foreign is the status of a Pod bound to the node that is not a stand-in Pod: phase
	// Failed, reason ForeignReason.
	Foreign(pod *corev1.Pod, now time.Time) corev1.PodStatus
}

// ForeignReason is the status reason of a Pod the virtual node refuses to run.
const ForeignReason = "UnsupportedOnVirtualNode"

// Event reasons the stand-in Reconciler records on WorkerScripts (it never writes their status,
// which belongs to the manager's WorkerScript controller). Workstream C declares them as
// EventReasonStandInPodFailed and EventReasonStandInPodConflict and documents them in
// hack/apidocs/reasons.yaml (TestReasonsDocumented) when it implements the Reconciler:
//
//   - StandInPodFailed: the Pod could not be created or updated (quota, admission).
//   - StandInPodConflict: a Pod with the stand-in name exists and is not controlled by this
//     WorkerScript; it is left alone.
