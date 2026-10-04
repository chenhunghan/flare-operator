package vk

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/vk/standin"
)

// errWorkerScriptMissing: the Pod has the shape of a stand-in Pod but its WorkerScript is not
// (or no longer, or not yet in the cache) the object it names. Unlike ErrNotStandIn this can
// change, so the Pod is not failed for it.
var errWorkerScriptMissing = errors.New("the stand-in pod's WorkerScript does not exist (or has a different UID)")

// standInOwner returns the controller ownerReference of a stand-in Pod, or ErrNotStandIn when the
// Pod is not shaped like one: the stand-in role label, a controller ownerReference to a
// WorkerScript (flare.dev), a StandInLabelWorkerScriptUID equal to that reference's UID, and the
// name standin.PodName gives that WorkerScript.
// Annotations are never consulted.
func standInOwner(pod *corev1.Pod) (*metav1.OwnerReference, error) {
	if pod.Labels[workersv1alpha1.StandInLabelRole] != workersv1alpha1.StandInRoleWorker {
		return nil, ErrNotStandIn
	}
	ref := metav1.GetControllerOfNoCopy(pod)
	if ref == nil || ref.Kind != "WorkerScript" || ref.APIVersion != workersv1alpha1.GroupVersion.String() {
		return nil, ErrNotStandIn
	}
	uid := pod.Labels[workersv1alpha1.StandInLabelWorkerScriptUID]
	if uid == "" || types.UID(uid) != ref.UID {
		return nil, ErrNotStandIn
	}
	// Only the Pod name the stand-in Builder gives that WorkerScript: a forged Pod under another
	// name is not served even when its labels and owner reference are right. (The spec-hash
	// annotation is not checked: whoever can forge the labels can copy it.)
	if pod.Name != standin.PodName(ref.Name) {
		return nil, ErrNotStandIn
	}
	return ref, nil
}

// standInWorkerScript returns the WorkerScript a stand-in Pod shows (a cache object: do not
// modify it). Errors: ErrNotStandIn, errWorkerScriptMissing, or a read error.
func standInWorkerScript(ctx context.Context, r client.Reader, pod *corev1.Pod) (*workersv1alpha1.WorkerScript, error) {
	ref, err := standInOwner(pod)
	if err != nil {
		return nil, err
	}
	var ws workersv1alpha1.WorkerScript
	if err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("WorkerScript %s/%s: %w", pod.Namespace, ref.Name, errWorkerScriptMissing)
		}
		return nil, err
	}
	if ws.UID != ref.UID {
		return nil, fmt.Errorf("WorkerScript %s/%s: %w", pod.Namespace, ref.Name, errWorkerScriptMissing)
	}
	return &ws, nil
}
