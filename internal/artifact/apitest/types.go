// Package apitest holds a test-only kind, ArtifactHolder, whose spec embeds
// sharedv1alpha1.ArtifactSource and whose status embeds sharedv1alpha1.ArtifactStatus. No kind
// uses them yet, so `make manifests` generates this kind's CRD into testdata/ (not
// config/crd/bases: it is never installed by the chart) and the envtest test in this package
// checks the schema and CEL rules the shared types carry.
//
// +kubebuilder:object:generate=false
// +groupName=artifacttest.flare.dev
// +versionName=v1
package apitest

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sharedv1alpha1 "github.com/chenhunghan/flare-operator/api/shared/v1alpha1"
)

// ArtifactHolderSpec embeds an ArtifactSource.
type ArtifactHolderSpec struct {
	Source sharedv1alpha1.ArtifactSource `json:"source"`
}

// ArtifactHolderStatus embeds an ArtifactStatus.
type ArtifactHolderStatus struct {
	// +optional
	Artifact sharedv1alpha1.ArtifactStatus `json:"artifact,omitempty"`
}

// ArtifactHolder is a test-only kind.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type ArtifactHolder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ArtifactHolderSpec   `json:"spec"`
	Status ArtifactHolderStatus `json:"status,omitempty"`
}
