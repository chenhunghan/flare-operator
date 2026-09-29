package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// SecretTypeHelmRelease is the type of the Secrets in which Helm 3 stores release history.
const SecretTypeHelmRelease corev1.SecretType = "helm.sh/release.v1"

// CacheTransform is the manager cache's DefaultTransform. The controllers watch Secrets,
// ConfigMaps, Deployments and Services cluster-wide, so the cache holds every one of them; this
// keeps what the operator never reads out of memory:
//   - managedFields of every object;
//   - the data of Helm release Secrets (type helm.sh/release.v1) and of Helm's ConfigMap storage
//     (label owner=helm, or OWNER=TILLER for Helm 2), which can be hundreds of KB per revision;
//   - the data of service account token Secrets, which a secret_text binding refuses
//     (workerscript.LabelWorkerBinding) and no CloudflareAccount token can be.
//
// Their metadata stays, so watches and finalizer handling work as before.
func CacheTransform(obj any) (any, error) {
	if a, err := meta.Accessor(obj); err == nil {
		a.SetManagedFields(nil)
	}
	switch o := obj.(type) {
	case *corev1.Secret:
		if o.Type == SecretTypeHelmRelease || o.Type == corev1.SecretTypeServiceAccountToken {
			o.Data, o.StringData = nil, nil
		}
	case *corev1.ConfigMap:
		if o.Labels["owner"] == "helm" || o.Labels["OWNER"] == "TILLER" {
			o.Data, o.BinaryData = nil, nil
		}
	}
	return obj, nil
}
