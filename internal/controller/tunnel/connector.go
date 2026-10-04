package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
)

// Labels and annotations on owned objects.
const (
	// LabelTunnel names the Tunnel that owns a cloudflared pod, Deployment, Secret or policy.
	LabelTunnel = "flare.dev/tunnel"
	// AnnotationSpecHash records the hash of the desired spec last applied to an owned object,
	// so an unchanged spec is never rewritten (API-server defaulting would otherwise look like
	// drift).
	AnnotationSpecHash = "flare.dev/spec-hash"
	// AnnotationTunnelID records the Cloudflare tunnel ID on the token Secret and the pod
	// template (a new tunnel ID rolls the pods).
	AnnotationTunnelID = "flare.dev/tunnel-id"
	// TokenKey is the token Secret's data key.
	TokenKey = "token"
	// MetricsPort serves cloudflared's /ready and /metrics.
	MetricsPort = 2000
)

// cloudflaredUID is the distroless "nonroot" user of the cloudflared image. UNVERIFIED: the
// in-cluster spike (§2.1) did not record its pod security context.
const cloudflaredUID = 65532

// DeploymentName, TokenSecretName and NetworkPolicyName name the owned objects of Tunnel t.
func DeploymentName(t *tunnelsv1alpha1.Tunnel) string    { return t.Name + "-cloudflared" }
func TokenSecretName(t *tunnelsv1alpha1.Tunnel) string   { return t.Name + "-cloudflared-token" }
func NetworkPolicyName(t *tunnelsv1alpha1.Tunnel) string { return t.Name + "-cloudflared" }

// selectorLabels select the cloudflared pods of t (they never change: a Deployment's selector
// is immutable).
func selectorLabels(t *tunnelsv1alpha1.Tunnel) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "cloudflared", LabelTunnel: t.Name}
}

// objectLabels are set on every owned object and pod.
func objectLabels(t *tunnelsv1alpha1.Tunnel) map[string]string {
	l := selectorLabels(t)
	l["app.kubernetes.io/instance"] = t.Name
	l["app.kubernetes.io/component"] = "tunnel-connector"
	l["app.kubernetes.io/managed-by"] = "flare-operator"
	return l
}

// desiredReplicas is spec.connector.replicas or DefaultConnectorReplicas.
func desiredReplicas(t *tunnelsv1alpha1.Tunnel) int32 {
	if r := t.Spec.Connector.Replicas; r != nil {
		return *r
	}
	return tunnelsv1alpha1.DefaultConnectorReplicas
}

func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
}

// deploymentSpec is the cloudflared Deployment of spike §2.1: `cloudflared tunnel
// --no-autoupdate --metrics 0.0.0.0:2000 run` with TUNNEL_TOKEN from the token Secret, and a
// readiness probe on /ready:2000 (during a bad rollout it kept the old pod serving).
func deploymentSpec(t *tunnelsv1alpha1.Tunnel, tunnelID string) appsv1.DeploymentSpec {
	c := t.Spec.Connector
	image := c.Image
	if image == "" {
		image = tunnelsv1alpha1.DefaultCloudflaredImage
	}
	res := defaultResources()
	if c.Resources != nil {
		res = *c.Resources.DeepCopy()
	}
	sel := selectorLabels(t)
	var tolerations []corev1.Toleration
	for _, tol := range c.Tolerations {
		tolerations = append(tolerations, *tol.DeepCopy())
	}
	var nodeSelector map[string]string
	if len(c.NodeSelector) > 0 {
		nodeSelector = map[string]string{}
		for k, v := range c.NodeSelector {
			nodeSelector[k] = v
		}
	}
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/ready", Port: intstr.FromInt32(MetricsPort), Scheme: corev1.URISchemeHTTP,
		}},
		PeriodSeconds:    10,
		TimeoutSeconds:   1,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	return appsv1.DeploymentSpec{
		Replicas: ptr.To(desiredReplicas(t)),
		Selector: &metav1.LabelSelector{MatchLabels: sel},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      objectLabels(t),
				Annotations: map[string]string{AnnotationTunnelID: tunnelID},
			},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptr.To(true),
					RunAsUser:      ptr.To[int64](cloudflaredUID),
					RunAsGroup:     ptr.To[int64](cloudflaredUID),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				NodeSelector: nodeSelector,
				Tolerations:  tolerations,
				Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							TopologyKey:   corev1.LabelHostname,
							LabelSelector: &metav1.LabelSelector{MatchLabels: sel},
						},
					}},
				}},
				Containers: []corev1.Container{{
					Name:            "cloudflared",
					Image:           image,
					ImagePullPolicy: c.ImagePullPolicy,
					Args:            []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"},
					Env: []corev1.EnvVar{{
						Name: "TUNNEL_TOKEN",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: TokenSecretName(t)},
							Key:                  TokenKey,
						}},
					}},
					Ports:          []corev1.ContainerPort{{Name: "metrics", ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP}},
					ReadinessProbe: probe,
					Resources:      res,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						ReadOnlyRootFilesystem:   ptr.To(true),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				}},
			},
		},
	}
}

// specHash hashes a desired spec (JSON, which sorts map keys).
func specHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // desired specs are plain API structs
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}
