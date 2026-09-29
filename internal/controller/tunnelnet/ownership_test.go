package tunnelnet

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestCreatedByThis(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  string
		ann  map[string]string
		want bool
	}{
		{"matching UID", "u1", map[string]string{AnnotationCreatedByUID: "u1"}, true},
		{"copied manifest (other UID)", "u2", map[string]string{AnnotationCreatedByUID: "u1"}, false},
		{"no annotation", "u1", nil, false},
		{"no UID yet", "", map[string]string{AnnotationCreatedByUID: ""}, false},
	} {
		o := &metav1.ObjectMeta{UID: types.UID(tc.uid), Annotations: tc.ann}
		if got := CreatedByThis(o); got != tc.want {
			t.Errorf("%s: CreatedByThis = %v, want %v", tc.name, got, tc.want)
		}
	}
}
