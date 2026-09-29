package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Managed is implemented by every flare-operator kind (generated kinds get
// these two methods emitted by cmd/flaregen). Shared reconcile helpers in
// internal/reconcile operate on this interface.
// +kubebuilder:object:generate=false
type Managed interface {
	metav1.Object
	runtime.Object
	GetResourceSpec() *ResourceSpec
	GetResourceStatus() *ResourceStatus
}
