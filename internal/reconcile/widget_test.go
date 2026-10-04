package reconcile_test

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
)

// Widget is a minimal managed kind for helper tests.
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WidgetSpec   `json:"spec"`
	Status            WidgetStatus `json:"status,omitempty"`
}

type WidgetSpec struct {
	commonv1alpha1.ResourceSpec `json:",inline"`
	Size                        int `json:"size,omitempty"`
}

type WidgetStatus struct {
	commonv1alpha1.ResourceStatus `json:",inline"`
	Note                          string `json:"note,omitempty"`
}

func (w *Widget) GetResourceSpec() *commonv1alpha1.ResourceSpec     { return &w.Spec.ResourceSpec }
func (w *Widget) GetResourceStatus() *commonv1alpha1.ResourceStatus { return &w.Status.ResourceStatus }

func (w *Widget) DeepCopyObject() runtime.Object {
	out := *w
	w.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	w.Spec.ResourceSpec.DeepCopyInto(&out.Spec.ResourceSpec)
	w.Status.ResourceStatus.DeepCopyInto(&out.Status.ResourceStatus)
	return &out
}

type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

func (l *WidgetList) DeepCopyObject() runtime.Object {
	out := *l
	l.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = nil
	for i := range l.Items {
		out.Items = append(out.Items, *l.Items[i].DeepCopyObject().(*Widget))
	}
	return &out
}

var widgetGV = schema.GroupVersion{Group: "test.cloudflare.flare.dev", Version: "v1alpha1"}

func addWidget(s *runtime.Scheme) error {
	s.AddKnownTypes(widgetGV, &Widget{}, &WidgetList{})
	metav1.AddToGroupVersion(s, widgetGV)
	return nil
}
