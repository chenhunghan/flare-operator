package reconcile_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// fixedReader serves one ConfigMap (nil: NotFound) and counts its reads.
type fixedReader struct {
	cm    *corev1.ConfigMap
	reads int
}

func (f *fixedReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	f.reads++
	if f.cm == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
	}
	cp := f.cm.DeepCopy()
	if obj.(*corev1.ConfigMap).Annotations != nil {
		// Decode-like: keys the target already has survive unless overwritten (as with
		// json.Unmarshal into a non-empty map), so Get must not reuse the cached copy.
		for k, v := range cp.Annotations {
			obj.(*corev1.ConfigMap).Annotations[k] = v
		}
		cp.Annotations = obj.(*corev1.ConfigMap).Annotations
	}
	*obj.(*corev1.ConfigMap) = *cp
	return nil
}

func (f *fixedReader) List(context.Context, client.ObjectList, ...client.ListOption) error { return nil }

func cmAt(rv string, ann map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "o", ResourceVersion: rv, Annotations: ann}}
}

func TestViews(t *testing.T) {
	ctx := context.Background()
	key := client.ObjectKey{Namespace: "ns", Name: "o"}
	var v reconcile.Views
	cached := &fixedReader{cm: cmAt("1", map[string]string{"a": "1", "stale": "x"})}
	uncached := &fixedReader{cm: cmAt("2", map[string]string{"a": "2"})}

	// Nothing recorded yet: the cached copy is used.
	var got corev1.ConfigMap
	if err := v.Get(ctx, cached, uncached, key, &got); err != nil || got.ResourceVersion != "1" || uncached.reads != 0 {
		t.Fatalf("first read: rv %q err %v uncached reads %d", got.ResourceVersion, err, uncached.reads)
	}
	// The reconcile wrote the object (resourceVersion 2); the cache still serves 1.
	got.ResourceVersion = "2"
	v.Done(&got)
	got = corev1.ConfigMap{}
	if err := v.Get(ctx, cached, uncached, key, &got); err != nil || got.ResourceVersion != "2" || uncached.reads != 1 {
		t.Fatalf("lagging cache: rv %q err %v uncached reads %d", got.ResourceVersion, err, uncached.reads)
	}
	if _, ok := got.Annotations["stale"]; ok || got.Annotations["a"] != "2" {
		t.Fatalf("the uncached copy kept annotations of the cached one: %v", got.Annotations)
	}
	// The cache caught up: no uncached read.
	cached.cm = cmAt("2", map[string]string{"a": "2"})
	if err := v.Get(ctx, cached, uncached, key, &got); err != nil || uncached.reads != 1 {
		t.Fatalf("caught-up cache: err %v uncached reads %d", err, uncached.reads)
	}
	// Gone uncached: NotFound, and the record is dropped.
	cached.cm = cmAt("1", nil)
	uncached.cm = nil
	if err := v.Get(ctx, cached, uncached, key, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("gone object: %v", err)
	}
	if err := v.Get(ctx, cached, uncached, key, &got); err != nil || uncached.reads != 2 {
		t.Fatalf("after NotFound the record should be forgotten: err %v uncached reads %d", err, uncached.reads)
	}
}
