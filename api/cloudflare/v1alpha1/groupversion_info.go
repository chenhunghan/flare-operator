// Package v1alpha1 contains the flare.dev API group: CloudflareAccount, the
// credentials and account binding every managed kind refers to through spec.accountRef.
//
// +kubebuilder:object:generate=true
// +groupName=flare.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "flare.dev", Version: "v1alpha1"}

	// SchemeBuilder registers this package's kinds. It is apimachinery's runtime.SchemeBuilder
	// (as in the flaregen-generated packages), not controller-runtime's deprecated
	// scheme.Builder, so the API package imports only apimachinery.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &CloudflareAccount{}, &CloudflareAccountList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
