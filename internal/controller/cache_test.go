package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCacheTransform(t *testing.T) {
	mf := []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	data := map[string][]byte{"release": make([]byte, 1<<20)}
	helm := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.x.v1", ManagedFields: mf}, Type: SecretTypeHelmRelease, Data: data}
	sa := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sa"}, Type: corev1.SecretTypeServiceAccountToken, Data: map[string][]byte{"token": []byte("t")}}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf", ManagedFields: mf}, Data: map[string][]byte{"token": []byte("cf")}}
	helmCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "r", Labels: map[string]string{"owner": "helm"}}, Data: map[string]string{"release": "x"}}
	code := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "code"}, Data: map[string]string{"index.js": "export default {}"}}
	for _, o := range []any{helm, sa, token, helmCM, code} {
		if _, err := CacheTransform(o); err != nil {
			t.Fatal(err)
		}
	}
	if helm.Data != nil || sa.Data != nil || helmCM.Data != nil {
		t.Errorf("Helm release or service account token data kept: %d %d %d", len(helm.Data), len(sa.Data), len(helmCM.Data))
	}
	if string(token.Data["token"]) != "cf" || code.Data["index.js"] == "" {
		t.Error("data the operator reads was dropped")
	}
	if helm.ManagedFields != nil || token.ManagedFields != nil {
		t.Error("managedFields kept")
	}
	if helm.Name == "" || helm.Type != SecretTypeHelmRelease {
		t.Error("metadata dropped")
	}
	if _, err := CacheTransform("not an object"); err != nil {
		t.Errorf("non-object: %v", err)
	}
}
