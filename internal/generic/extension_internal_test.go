package generic

import (
	"errors"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/reconcile"
)

func TestChangedSubResources(t *testing.T) {
	r := &Reconciler{Extension: Extension{SubResources: []SubResource{{Field: "cors", Path: "/cors", Delete: true}}}}
	rule := `{"rules":[{"allowed":{"methods":["GET"],"origins":["https://example.com"]}}]}`
	for _, c := range []struct {
		desired, observed string
		changed           bool
	}{
		{`{}`, `{"cors":` + rule + `}`, false},                    // unset: unmanaged
		{`{"cors":` + rule + `}`, `{"cors":` + rule + `}`, false}, // in sync
		{`{"cors":` + rule + `}`, `{}`, true},                     // not configured yet
		{`{"cors":{}}`, `{"cors":` + rule + `}`, true},            // clear (Covers alone: in sync)
		{`{"cors":{"rules":[]}}`, `{"cors":` + rule + `}`, true},  // clear, spelled out
		{`{"cors":{}}`, `{}`, false},                              // cleared: not configured
		{`{"cors":{}}`, `{"cors":{"rules":[]}}`, false},           // cleared: an empty policy
	} {
		desired, _ := jsonValue(t, c.desired).(map[string]any)
		obs, _ := jsonValue(t, c.observed).(map[string]any)
		if got := len(r.changedSubResources(desired, obs)) > 0; got != c.changed {
			t.Errorf("desired %s, observed %s: changed %v, want %v", c.desired, c.observed, got, c.changed)
		}
	}
}

func TestCreateRefused(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusForbidden: true, http.StatusUnauthorized: true, http.StatusNotFound: true, http.StatusUnprocessableEntity: true,
		http.StatusBadRequest: false, http.StatusConflict: false, http.StatusTooManyRequests: false, http.StatusRequestTimeout: false,
		http.StatusInternalServerError: false, http.StatusServiceUnavailable: false,
	} {
		if got := createRefused(&cfclient.APIError{Status: status}); got != want {
			t.Errorf("status %d: createRefused %v, want %v", status, got, want)
		}
	}
	if createRefused(errors.New("connection reset")) {
		t.Error("a transport error counts as a refusal")
	}
}

// TestAnnotationsChanged: the reconciler's own create-pending writes do not trigger a
// reconcile (a refused create would be retried at once, in a loop); other annotations do.
func TestAnnotationsChanged(t *testing.T) {
	obj := func(ann map[string]string) *kvv1alpha1.KVNamespace {
		return &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Annotations: ann}}
	}
	p := annotationsChanged()
	pending := reconcile.AnnotationCreatePending
	for _, c := range []struct {
		old, new map[string]string
		want     bool
	}{
		{nil, map[string]string{pending: "uid/x"}, false},
		{map[string]string{pending: "uid/x"}, nil, false},
		{map[string]string{pending: "uid/x", "a": "1"}, map[string]string{"a": "1"}, false},
		{nil, map[string]string{"a": "1"}, true},
		{map[string]string{"a": "1"}, map[string]string{"a": "2"}, true},
		{map[string]string{"a": "1", pending: "uid/x"}, map[string]string{pending: "uid/x"}, true},
	} {
		if got := p.Update(event.UpdateEvent{ObjectOld: obj(c.old), ObjectNew: obj(c.new)}); got != c.want {
			t.Errorf("%v -> %v: %v, want %v", c.old, c.new, got, c.want)
		}
	}
}

func TestObservedScopeHeader(t *testing.T) {
	x := Extension{Headers: []HeaderField{{Header: "cf-r2-jurisdiction", Field: "jurisdiction", Default: "default"}}}
	for _, c := range []struct {
		desired, observed map[string]any
		want              string
	}{
		{map[string]any{"jurisdiction": "eu"}, map[string]any{"jurisdiction": "eu"}, "eu"},
		{map[string]any{"jurisdiction": "eu"}, map[string]any{"jurisdiction": "default"}, ""},
		{map[string]any{}, map[string]any{"jurisdiction": "us"}, "us"},
		{map[string]any{"jurisdiction": "eu"}, nil, "eu"},
	} {
		if got := x.observedScopeHeader(c.desired, c.observed).Get("cf-r2-jurisdiction"); got != c.want {
			t.Errorf("desired %v, observed %v: header %q, want %q", c.desired, c.observed, got, c.want)
		}
	}
}
