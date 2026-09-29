package tunnel

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
)

func TestDrifted(t *testing.T) {
	tun := &tunnelsv1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec: tunnelsv1alpha1.TunnelSpec{ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "a"}}}}
	want := deploymentSpec(tun, "id")

	// API-server defaults and extra annotations are not drift.
	live := *want.DeepCopy()
	live.RevisionHistoryLimit = ptr.To[int32](10)
	live.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
	live.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = "now"
	live.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	live.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	c := &live.Template.Spec.Containers[0]
	c.TerminationMessagePath = "/dev/termination-log"
	c.ImagePullPolicy = corev1.PullIfNotPresent
	c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0.01")
	if drifted(want, live) {
		t.Error("defaults reported as drift")
	}

	for name, mut := range map[string]func(*appsv1.DeploymentSpec){
		"image": func(s *appsv1.DeploymentSpec) { s.Template.Spec.Containers[0].Image = "evil:latest" },
		"args":  func(s *appsv1.DeploymentSpec) { s.Template.Spec.Containers[0].Args = []string{"tunnel", "run"} },
		"env": func(s *appsv1.DeploymentSpec) {
			s.Template.Spec.Containers[0].Env = append(s.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "X"})
		},
		"probe":    func(s *appsv1.DeploymentSpec) { s.Template.Spec.Containers[0].ReadinessProbe = nil },
		"replicas": func(s *appsv1.DeploymentSpec) { s.Replicas = ptr.To[int32](7) },
		"sidecar": func(s *appsv1.DeploymentSpec) {
			s.Template.Spec.Containers = append(s.Template.Spec.Containers, corev1.Container{Name: "x"})
		},
		"memory": func(s *appsv1.DeploymentSpec) {
			s.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
		},
	} {
		l := *live.DeepCopy()
		mut(&l)
		if !drifted(want, l) {
			t.Errorf("%s edit not reported as drift", name)
		}
	}

	proto := corev1.ProtocolTCP
	port := intstr.FromInt32(7844)
	np := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}},
		}},
	}
	if drifted(np, *np.DeepCopy()) {
		t.Error("identical policy reported as drift")
	}
	for name, mut := range map[string]func(*networkingv1.NetworkPolicySpec){
		"extra rule": func(s *networkingv1.NetworkPolicySpec) {
			s.Egress = append(s.Egress, networkingv1.NetworkPolicyEgressRule{})
		},
		"all ports": func(s *networkingv1.NetworkPolicySpec) { s.Egress[0].Ports = nil },
		"no except": func(s *networkingv1.NetworkPolicySpec) { s.Egress[0].To[0].IPBlock.Except = nil },
		"extra peer": func(s *networkingv1.NetworkPolicySpec) {
			s.Egress[0].To = append(s.Egress[0].To, networkingv1.NetworkPolicyPeer{})
		},
		"pod selector": func(s *networkingv1.NetworkPolicySpec) { s.PodSelector.MatchLabels = map[string]string{"x": "y"} },
		"policy types": func(s *networkingv1.NetworkPolicySpec) { s.PolicyTypes = nil },
	} {
		l := *np.DeepCopy()
		mut(&l)
		if !drifted(np, l) {
			t.Errorf("%s edit not reported as drift", name)
		}
	}
}
