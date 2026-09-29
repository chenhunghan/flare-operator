//go:build e2e

package e2e

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
)

// testObserveOnly pins (external-id annotation) an existing KV namespace and tunnel with
// managementPolicies [Observe] and deletionPolicy Delete. Both are observed (Ready, Synced,
// status.id and status.atProvider), but nothing is ever written: no Cloudflare write (not
// even an ownership tag), no connector objects for the tunnel, and deleting the objects keeps
// both resources.
func (s *suite) testObserveOnly(t *testing.T) {
	name := "flare-e2e-" + s.ns + "-observe"
	var kv struct{ ID string }
	if err := s.cfDo("POST", "/storage/kv/namespaces", map[string]string{"title": name}, &kv); err != nil {
		t.Fatal(err)
	}
	tunObj := s.tunnelSpec("observe-tun")
	var tun struct{ ID string }
	if err := s.cfDo("POST", "/cfd_tunnel", map[string]string{"name": tunObj.Spec.ForProvider.Name, "config_src": "cloudflare"}, &tun); err != nil {
		t.Fatal(err)
	}
	observe := commonv1alpha1.ResourceSpec{AccountRef: commonv1alpha1.LocalRef{Name: "acct"}, DeletionPolicy: commonv1alpha1.DeletionDelete,
		ManagementPolicies: []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}}
	kvObj := &kvv1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "observe-kv",
		Annotations: map[string]string{commonv1alpha1.AnnotationExternalID: kv.ID}},
		Spec: kvv1alpha1.KVNamespaceSpec{ResourceSpec: observe, ForProvider: kvv1alpha1.KVNamespaceParameters{Title: ptr.To(name)}}}
	tunObj.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: tun.ID}
	tunObj.Spec.ResourceSpec = observe

	s.clearJournal()
	s.create(kvObj)
	s.create(tunObj)
	for _, mg := range []commonv1alpha1.Managed{kvObj, tunObj} {
		s.waitManaged(mg, time.Minute)
	}
	if kvObj.Status.ID != kv.ID || tunObj.Status.ID != tun.ID {
		t.Errorf("status.id: KV %q (want %q), Tunnel %q (want %q)", kvObj.Status.ID, kv.ID, tunObj.Status.ID, tun.ID)
	}
	if tunObj.Status.AtProvider.Name != tunObj.Spec.ForProvider.Name {
		t.Errorf("Tunnel status.atProvider = %+v, want the observed tunnel %s", tunObj.Status.AtProvider, tunObj.Spec.ForProvider.Name)
	}
	if w := writes(s.journal()); len(w) > 0 {
		t.Errorf("observe-only objects wrote to Cloudflare while first observing: %v", w)
	}

	// Re-reconcile both (an annotation change) and wait until both resources were read again.
	s.clearJournal()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, obj := range []client.Object{kvObj, tunObj} {
		s.patch(obj, func() {
			a := obj.GetAnnotations()
			a[pokeAnnotation] = stamp
			obj.SetAnnotations(a)
		})
	}
	eventually(t, time.Minute, "a GET of both observed resources", func() (bool, string) {
		var missing []string
		j := s.journal()
		for _, id := range []string{kv.ID, tun.ID} {
			seen := false
			for _, e := range j {
				seen = seen || (e.Method == "GET" && strings.Contains(e.Path, id))
			}
			if !seen {
				missing = append(missing, id)
			}
		}
		return len(missing) == 0, fmt.Sprintf("not yet read: %v", missing)
	})
	time.Sleep(5 * time.Second) // let any follow-up write land
	j := s.journal()
	if w := writes(j); len(w) > 0 {
		t.Errorf("observe-only objects wrote to Cloudflare: %v", w)
	}
	s.checkJournalClean(j)

	// No connector objects for an observed tunnel.
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "observe-tun-cloudflared"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "observe-tun-cloudflared-token"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "observe-tun-cloudflared"}},
	} {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("observe-only Tunnel: %T %s exists (err %v)", obj, obj.GetName(), err)
		}
	}
	if c := tunObj.Status.Connector; c != (tunnelsv1alpha1.ConnectorStatus{}) {
		t.Errorf("observe-only Tunnel status.connector = %+v, want empty", c)
	}
	if n := tunObj.Status.NetworkPolicy; n.Name != "" {
		t.Errorf("observe-only Tunnel status.networkPolicy = %+v, want empty", n)
	}

	// Deleting the objects deletes nothing in Cloudflare (Observe does not allow Delete).
	s.clearJournal()
	for _, obj := range []client.Object{kvObj, tunObj} {
		if err := s.c.Delete(s.ctx(), obj); err != nil {
			t.Fatal(err)
		}
		s.waitGone(obj, time.Minute)
	}
	time.Sleep(3 * time.Second)
	if w := writes(s.journal()); len(w) > 0 {
		t.Errorf("Cloudflare writes while deleting observe-only objects: %v", w)
	}
	var kvGot struct{ ID string }
	if err := s.cfGet("/storage/kv/namespaces/"+kv.ID, &kvGot); err != nil || kvGot.ID != kv.ID {
		t.Errorf("observed KV namespace %s after its object was deleted: %+v, %v", kv.ID, kvGot, err)
	}
	var tunGot struct {
		DeletedAt *string `json:"deleted_at"`
	}
	if err := s.cfGet("/cfd_tunnel/"+tun.ID, &tunGot); err != nil || tunGot.DeletedAt != nil {
		t.Errorf("observed tunnel %s after its object was deleted: deleted_at %v, %v", tun.ID, tunGot.DeletedAt, err)
	}
	var tagged []tagIndexEntry
	if err := s.cfGet("/tags/resources?id="+url.QueryEscape(kv.ID)+"&id="+url.QueryEscape(tun.ID), &tagged); err != nil {
		t.Errorf("list tags of the observed resources: %v", err)
	} else if len(tagged) > 0 {
		t.Errorf("observed resources carry tags: %+v", tagged)
	}

	// Remove them here (the teardown step checks that the account ends up empty).
	if err := s.cfDo("DELETE", "/storage/kv/namespaces/"+kv.ID, nil, nil); err != nil {
		t.Error(err)
	}
	if err := s.cfDo("DELETE", "/cfd_tunnel/"+tun.ID, nil, nil); err != nil {
		t.Error(err)
	}
}

// tagIndexEntry is one item of GET /accounts/{id}/tags/resources.
type tagIndexEntry struct {
	Type string            `json:"type"`
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
}

// tagItemPaths are the item paths (relative to the account) of the resource types this run
// tags, used to check that a tag-index entry names a resource that is gone.
var tagItemPaths = map[string]string{
	"kv_namespace":       "/storage/kv/namespaces/",
	"queue":              "/queues/",
	"d1_database":        "/d1/database/",
	"cloudflared_tunnel": "/cfd_tunnel/",
}

// checkTagIndexClean asserts, after namespace teardown, that the account's tag index names no
// resource that still exists: every tagged resource of the run was deleted.
//
// Tolerated, and only this: an entry whose resource is verifiably gone (404, or a tunnel with
// deleted_at). flarefake keeps a resource's tag record when the resource is deleted, as
// documented in internal/fake/tags.go ("The emulator does not check that the tagged resource
// exists"); whether the live index drops such entries is UNVERIFIED. Any other entry (a live
// resource, or a resource type this run never tags) fails the step.
func (s *suite) checkTagIndexClean(t *testing.T) {
	var tagged []tagIndexEntry
	if err := s.cfGet("/tags/resources", &tagged); err != nil {
		t.Errorf("list tagged resources: %v", err)
		return
	}
	stale := 0
	for _, e := range tagged {
		p, ok := tagItemPaths[e.Type]
		if !ok {
			t.Errorf("tag index lists a %s (%s, tags %v), a type this run never tags", e.Type, e.ID, e.Tags)
			continue
		}
		var got struct {
			DeletedAt *string `json:"deleted_at"`
		}
		err := s.cfGet(p+e.ID, &got)
		switch {
		case apierrors.IsNotFound(err):
			stale++
		case err != nil:
			t.Errorf("tag index lists %s %s: cannot check whether it still exists: %v", e.Type, e.ID, err)
		case e.Type == "cloudflared_tunnel" && got.DeletedAt != nil:
			stale++ // soft-deleted (0101)
		default:
			t.Errorf("tag index lists %s %s (%q, tags %v), which still exists after teardown", e.Type, e.ID, e.Name, e.Tags)
		}
	}
	if stale > 0 {
		t.Logf("tag index keeps %d entr(ies) for deleted resources (flarefake keeps tag records of deleted resources, internal/fake/tags.go)", stale)
	}
}
