package standin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// The virtual node's taint. These mirror internal/vk's TaintKey and TaintValue (package vk
// imports standin, so standin cannot import vk; TestTaintMatchesVK keeps them equal).
const (
	taintKey   = "virtual-kubelet.io/provider"
	taintValue = "cloudflare"
)

// The managed-by label every stand-in Pod carries.
const (
	labelManagedBy      = "app.kubernetes.io/managed-by"
	labelManagedByValue = "flare-operator"
)

// runAsUser is the UID of the placeholder container (the distroless "nonroot" user, as the
// operator image runs). Nothing runs, but Pod Security "restricted" wants a non-root identity.
const runAsUser = 65532

// maxPodName is the longest stand-in Pod name used verbatim (a DNS-1123 label, so the name also
// works as a hostname).
const maxPodName = 63

// PodName is the stand-in Pod's name for a WorkerScript name (api/workers/v1alpha1
// StandInPodNameSuffix describes the rule): <name>-worker when that fits in 63 characters,
// else the first 46 characters of the name with trailing "-" and "." trimmed, "-", 8 hex digits
// of the SHA-256 of the full name, and "-worker".
func PodName(workerScript string) string {
	if n := workerScript + workersv1alpha1.StandInPodNameSuffix; len(n) <= maxPodName {
		return n
	}
	sum := sha256.Sum256([]byte(workerScript))
	prefix := strings.TrimRight(workerScript[:46], "-.")
	return prefix + "-" + hex.EncodeToString(sum[:4]) + workersv1alpha1.StandInPodNameSuffix
}

// NewBuilder returns the Builder of stand-in Pods for cfg. Requests default to
// DefaultRequests. NodeName and Image are required; Desired fails without them.
func NewBuilder(cfg PodConfig) Builder {
	if len(cfg.Requests) == 0 {
		cfg.Requests = DefaultRequests
	}
	cfg.Requests = cfg.Requests.DeepCopy()
	cfg.ExtraLabels = maps.Clone(cfg.ExtraLabels)
	return &podBuilder{cfg: cfg}
}

type podBuilder struct{ cfg PodConfig }

// Desired implements Builder.
func (b *podBuilder) Desired(ws *workersv1alpha1.WorkerScript) (*corev1.Pod, error) {
	switch {
	case ws == nil:
		return nil, errors.New("stand-in pod: no WorkerScript")
	case ws.Name == "" || ws.Namespace == "":
		return nil, errors.New("stand-in pod: the WorkerScript has no name or namespace")
	case ws.UID == "":
		return nil, fmt.Errorf("stand-in pod for WorkerScript %s/%s: it has no UID yet", ws.Namespace, ws.Name)
	case b.cfg.NodeName == "":
		return nil, errors.New("stand-in pod: no node name configured")
	case b.cfg.Image == "":
		return nil, errors.New("stand-in pod: no placeholder image configured")
	}

	labels := make(map[string]string, len(b.cfg.ExtraLabels)+4)
	maps.Copy(labels, b.cfg.ExtraLabels)
	// Ours win over podLabels: the virtual kubelet and kubectl -l depend on them.
	labels[workersv1alpha1.StandInLabelWorkerScript] = reconcile.AccountLabelValue(ws.Name)
	labels[workersv1alpha1.StandInLabelWorkerScriptUID] = string(ws.UID)
	labels[workersv1alpha1.StandInLabelRole] = workersv1alpha1.StandInRoleWorker
	labels[labelManagedBy] = labelManagedByValue

	gvk := workersv1alpha1.GroupVersion.WithKind("WorkerScript")
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        PodName(ws.Name),
			Namespace:   ws.Namespace,
			Labels:      labels,
			Annotations: map[string]string{workersv1alpha1.StandInAnnotationScriptName: ws.ScriptName()},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         gvk.GroupVersion().String(),
				Kind:               gvk.Kind,
				Name:               ws.Name,
				UID:                ws.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:                      b.cfg.NodeName,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			RestartPolicy:                 corev1.RestartPolicyAlways,
			Tolerations: []corev1.Toleration{
				{Key: taintKey, Operator: corev1.TolerationOpEqual, Value: taintValue, Effect: corev1.TaintEffectNoSchedule},
				// No tolerationSeconds: a virtual kubelet outage never evicts the Pod.
				{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
				{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
			},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](runAsUser),
				RunAsGroup:     ptr.To[int64](runAsUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            workersv1alpha1.StandInContainerName,
				Image:           b.cfg.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Resources: corev1.ResourceRequirements{
					Requests: b.cfg.Requests.DeepCopy(),
					Limits:   b.cfg.Requests.DeepCopy(),
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Privileged:               ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
	return pod, nil
}
