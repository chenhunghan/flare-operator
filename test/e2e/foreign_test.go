//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
)

// testForeignOwner pins (external-id annotation, deletionPolicy Delete) a KV namespace and a
// tunnel that carry another cluster's owner tag: neither is managed (Synced=False, no
// connector), and deleting the objects keeps both in Cloudflare with a Warning Event
// (ExternalResourceKept / ForeignOwnerTunnelKept) delivered through events.k8s.io/v1.
func (s *suite) testForeignOwner(t *testing.T) {
	const foreign = "other-cluster/other-ns/other"
	var kv struct{ ID string }
	if err := s.cfDo("POST", "/storage/kv/namespaces", map[string]string{"title": "flare-e2e-" + s.ns + "-foreign"}, &kv); err != nil {
		t.Fatal(err)
	}
	var tun struct{ ID string }
	if err := s.cfDo("POST", "/cfd_tunnel", map[string]string{"name": "flare-e2e-" + s.ns + "-foreign", "config_src": "cloudflare"}, &tun); err != nil {
		t.Fatal(err)
	}
	for typ, id := range map[string]string{"kv_namespace": kv.ID, "cloudflared_tunnel": tun.ID} {
		if err := s.cfDo("PUT", "/tags", map[string]any{"resource_type": typ, "resource_id": id,
			"tags": map[string]string{"flare.dev/owner": foreign}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	kvObj := &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "foreign-kv",
		Annotations: map[string]string{commonv1alpha1.AnnotationExternalID: kv.ID}},
		Spec: kvv1alpha1.KVNamespaceSpec{
			ResourceSpec: commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete},
			ForProvider:  kvv1alpha1.KVNamespaceParameters{Title: ptr.To("flare-e2e-" + s.ns + "-foreign")},
		}}
	tunObj := s.tunnelSpec("foreign-tun")
	tunObj.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: tun.ID}
	s.create(kvObj)
	s.create(tunObj)
	for _, mg := range []commonv1alpha1.Managed{kvObj, tunObj} {
		eventually(t, time.Minute, fmt.Sprintf("%T %s to report the foreign owner", mg, mg.GetName()), func() (bool, string) {
			if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(mg), mg); err != nil {
				return false, err.Error()
			}
			sy := cond(mg.GetResourceStatus().Conditions, commonv1alpha1.ConditionSynced)
			return sy != nil && sy.Status == metav1.ConditionFalse && strings.Contains(sy.Message, foreign), condString(mg.GetResourceStatus().Conditions)
		})
	}
	var dep appsv1.Deployment
	if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: s.ns, Name: "foreign-tun-cloudflared"}, &dep); !apierrors.IsNotFound(err) {
		t.Errorf("a connector Deployment exists for a tunnel owned by %s (err %v)", foreign, err)
	}

	s.clearJournal()
	for _, obj := range []client.Object{kvObj, tunObj} {
		if err := s.c.Delete(s.ctx(), obj); err != nil {
			t.Fatal(err)
		}
		s.waitGone(obj, time.Minute)
	}
	if w := writes(s.journal()); len(w) > 0 {
		t.Errorf("Cloudflare writes while releasing foreign-owned resources: %v", w)
	}
	for name, reason := range map[string]string{"foreign-kv": "ExternalResourceKept", "foreign-tun": "ForeignOwnerTunnelKept"} {
		eventually(t, 30*time.Second, fmt.Sprintf("Warning Event %s on %s", reason, name), func() (bool, string) {
			var el eventsv1.EventList
			if err := s.c.List(s.ctx(), &el, client.InNamespace(s.ns)); err != nil {
				return false, err.Error()
			}
			var seen []string
			for _, e := range el.Items {
				if e.Regarding.Name == name {
					if e.Reason == reason && e.Type == corev1.EventTypeWarning {
						return true, ""
					}
					seen = append(seen, e.Reason)
				}
			}
			return false, fmt.Sprintf("events on %s: %v", name, seen)
		})
	}

	// Both are still in Cloudflare, the tunnel with its foreign tag. Remove them here (the
	// teardown step checks that the account ends up empty).
	var kvGot struct{ ID string }
	if err := s.cfGet("/storage/kv/namespaces/"+kv.ID, &kvGot); err != nil || kvGot.ID != kv.ID {
		t.Errorf("foreign KV namespace %s: %+v, %v", kv.ID, kvGot, err)
	}
	var tunGot struct {
		DeletedAt *string `json:"deleted_at"`
	}
	if err := s.cfGet("/cfd_tunnel/"+tun.ID, &tunGot); err != nil || tunGot.DeletedAt != nil {
		t.Errorf("foreign tunnel %s: deleted_at %v, %v", tun.ID, tunGot.DeletedAt, err)
	}
	var tags struct {
		Tags map[string]string `json:"tags"`
	}
	if err := s.cfGet("/tags?resource_type=cloudflared_tunnel&resource_id="+tun.ID, &tags); err != nil || tags.Tags["flare.dev/owner"] != foreign {
		t.Errorf("foreign tunnel tags = %v, %v", tags.Tags, err)
	}
	if err := s.cfDo("DELETE", "/storage/kv/namespaces/"+kv.ID, nil, nil); err != nil {
		t.Error(err)
	}
	if err := s.cfDo("DELETE", "/cfd_tunnel/"+tun.ID, nil, nil); err != nil {
		t.Error(err)
	}
}
