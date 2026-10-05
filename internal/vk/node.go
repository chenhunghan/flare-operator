package vk

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// Reasons of the virtual Node's Ready condition. Ready turns True once the kubelet API serves a
// certificate (ServingCert.Ready). These are Node (and, in provider.go, Pod) reasons, not
// reasons of flare.dev objects, so they are not named Reason* and not listed in
// hack/apidocs/reasons.yaml (the API reference's table).
const (
	NodeReadyReason           = "KubeletReady"
	NodeCertPendingReason     = "ServingCertificatePending"
	messageKubeletReady       = "the Cloudflare Workers virtual kubelet serves kubectl logs"
	messageServingCertPending = "waiting for the kubelet API serving certificate"
)

// KubeletVersion is the node's status.nodeInfo.kubeletVersion (display only; a valid semver).
const KubeletVersion = "v1.37.1-flare-workers"

// nodeCapacity is display only: stand-in Pods bypass the scheduler (spec.nodeName).
var nodeCapacity = corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("1000"),
	corev1.ResourceMemory: resource.MustParse("1Ti"),
	corev1.ResourcePods:   resource.MustParse("10000"),
}

// ErrForeignNode: a Node with the configured name exists and is not owned by this release's
// ClusterRole (another release's virtual node, or a real node). The virtual kubelet refuses to
// take it over.
var ErrForeignNode = errors.New("a Node with this name exists and is not owned by this release's ClusterRole")

// Taint is the node's NoSchedule taint.
func Taint() corev1.Taint {
	return corev1.Taint{Key: TaintKey, Value: TaintValue, Effect: corev1.TaintEffectNoSchedule}
}

// NodeLabels are the virtual node's labels. There is deliberately no kubernetes.io/os or
// kubernetes.io/arch label (DaemonSets selecting kubernetes.io/os=linux never target the node).
func NodeLabels(nodeName string) map[string]string {
	return map[string]string{
		LabelNodeType:                    LabelNodeTypeValue,
		LabelNodeRole:                    LabelNodeRoleValue,
		corev1.LabelHostname:             nodeName,
		"kubernetes.io/role":             "agent",
		corev1.LabelNodeExcludeBalancers: "true",
	}
}

// OwnerReference is the Node's ownerReference to the release's ClusterRole. blockOwnerDeletion
// is false: deleting the ClusterRole never waits for the Node (true would also need update on
// the ClusterRole's finalizers).
func OwnerReference(cr *rbacv1.ClusterRole) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         rbacv1.SchemeGroupVersion.String(),
		Kind:               "ClusterRole",
		Name:               cr.Name,
		UID:                cr.UID,
		BlockOwnerDeletion: ptr.To(false),
	}
}

// DesiredNode is the Node the virtual kubelet registers: taint, labels, owner, capacity,
// addresses and the kubelet endpoint, Ready per ready.
func DesiredNode(c Config, owner metav1.OwnerReference, ready bool) (*corev1.Node, error) {
	port, err := c.ListenPort()
	if err != nil {
		return nil, err
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            c.NodeName,
			Labels:          NodeLabels(c.NodeName),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{Taint()}},
		Status: corev1.NodeStatus{
			Phase:       corev1.NodeRunning,
			Capacity:    nodeCapacity.DeepCopy(),
			Allocatable: nodeCapacity.DeepCopy(),
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: c.Address},
				{Type: corev1.NodeHostName, Address: c.NodeName},
			},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: port}},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          KubeletVersion,
				OperatingSystem:         "cloudflare-workers",
				OSImage:                 "Cloudflare Workers",
				ContainerRuntimeVersion: "cloudflare-workers://",
			},
		},
	}
	setNodeConditions(n, ready, metav1.Now())
	return n, nil
}

func setNodeConditions(n *corev1.Node, ready bool, now metav1.Time) {
	readyCond := corev1.NodeCondition{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse,
		Reason: NodeCertPendingReason, Message: messageServingCertPending,
	}
	if ready {
		readyCond.Status, readyCond.Reason, readyCond.Message = corev1.ConditionTrue, NodeReadyReason, messageKubeletReady
	}
	conds := []corev1.NodeCondition{readyCond}
	for _, t := range []struct {
		typ    corev1.NodeConditionType
		reason string
	}{
		{corev1.NodeMemoryPressure, "KubeletHasSufficientMemory"},
		{corev1.NodeDiskPressure, "KubeletHasNoDiskPressure"},
		{corev1.NodePIDPressure, "KubeletHasSufficientPID"},
		{corev1.NodeNetworkUnavailable, "RouteCreated"},
	} {
		conds = append(conds, corev1.NodeCondition{Type: t.typ, Status: corev1.ConditionFalse, Reason: t.reason})
	}
	for i := range conds {
		conds[i].LastHeartbeatTime, conds[i].LastTransitionTime = now, now
		for _, old := range n.Status.Conditions {
			if old.Type == conds[i].Type && old.Status == conds[i].Status {
				conds[i].LastTransitionTime = old.LastTransitionTime
			}
		}
	}
	n.Status.Conditions = conds
}

// nodeOwnership classifies an existing Node against the release's ClusterRole.
type nodeOwnership int

const (
	nodeOurs    nodeOwnership = iota // owned by the ClusterRole (same UID)
	nodeStale                        // owned only by an earlier ClusterRole of the same name: GC will delete it
	nodeForeign                      // anything else
)

func classifyNode(n *corev1.Node, cr *rbacv1.ClusterRole) nodeOwnership {
	stale := false
	for _, o := range n.OwnerReferences {
		if o.Kind != "ClusterRole" || o.APIVersion != rbacv1.SchemeGroupVersion.String() || o.Name != cr.Name {
			continue
		}
		if o.UID == cr.UID {
			return nodeOurs
		}
		stale = true
	}
	if stale && len(n.OwnerReferences) == 1 {
		return nodeStale
	}
	return nodeForeign
}

// PrepareNode makes the Node ready for virtual-kubelet's NodeController and returns the Node to
// register:
//   - no Node: the desired one (the NodeController creates it);
//   - a Node owned by the ClusterRole: its labels, taint and ownerReference are restored (the
//     NodeController only patches status);
//   - a Node owned only by an earlier ClusterRole of the same name (the release was reinstalled
//     before garbage collection removed it): it is deleted (UID precondition) and recreated;
//   - any other Node: ErrForeignNode.
func PrepareNode(ctx context.Context, cs kubernetes.Interface, c Config) (*corev1.Node, error) {
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, c.OwnerClusterRole, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get the owner ClusterRole %q: %w", c.OwnerClusterRole, err)
	}
	owner := OwnerReference(cr)
	desired, err := DesiredNode(c, owner, false)
	if err != nil {
		return nil, err
	}
	nodes := cs.CoreV1().Nodes()
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := nodes.Get(ctx, c.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		switch classifyNode(cur, cr) {
		case nodeForeign:
			return fmt.Errorf("node %q (owners %v): %w", c.NodeName, ownerNames(cur), ErrForeignNode)
		case nodeStale:
			uid := cur.UID
			err := nodes.Delete(ctx, c.NodeName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return fmt.Errorf("delete the stale node %q: %w", c.NodeName, err)
			}
			return waitNodeGone(ctx, cs, c.NodeName, uid)
		}
		changed := false
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		for k, v := range desired.Labels {
			if cur.Labels[k] != v {
				cur.Labels[k], changed = v, true
			}
		}
		if !slices.ContainsFunc(cur.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&desired.Spec.Taints[0]) && t.Value == TaintValue }) {
			cur.Spec.Taints = append(slices.DeleteFunc(cur.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&desired.Spec.Taints[0]) }),
				desired.Spec.Taints[0])
			changed = true
		}
		if cur.Spec.Unschedulable {
			cur.Spec.Unschedulable, changed = false, true
		}
		if !changed {
			return nil
		}
		_, err = nodes.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, err
	}
	return desired, nil
}

func ownerNames(n *corev1.Node) []string {
	out := make([]string, 0, len(n.OwnerReferences))
	for _, o := range n.OwnerReferences {
		out = append(out, o.Kind+"/"+o.Name)
	}
	return out
}

// waitNodeGone waits until the Node is gone, or (uid set) is no longer the one with that UID.
func waitNodeGone(ctx context.Context, cs kubernetes.Interface, name string, uid types.UID) error {
	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return uid != "" && n.UID != uid, nil
	})
}

// nodeProvider is virtual-kubelet's node.NodeProvider: Ping always succeeds (the node is only a
// front for the kubelet API), and the Ready condition follows the serving certificate.
type nodeProvider struct {
	mu     sync.Mutex
	node   *corev1.Node
	notify func(*corev1.Node)
	ready  <-chan struct{}
}

func newNodeProvider(n *corev1.Node, certReady <-chan struct{}) *nodeProvider {
	return &nodeProvider{node: n.DeepCopy(), ready: certReady}
}

// Ping implements node.NodeProvider.
func (p *nodeProvider) Ping(ctx context.Context) error { return ctx.Err() }

// NotifyNodeStatus implements node.NodeProvider: once the serving certificate is ready, the
// node turns Ready.
func (p *nodeProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	p.mu.Lock()
	p.notify = cb
	p.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-p.ready:
			p.mu.Lock()
			setNodeConditions(p.node, true, metav1.Now())
			n := p.node.DeepCopy()
			p.mu.Unlock()
			select {
			case <-ctx.Done():
			default:
				cb(n)
			}
		}
	}()
}
