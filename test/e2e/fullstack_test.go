//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"flare.dev/operator/internal/kustomizelite"
)

// fullstackManaged are the kinds of examples/fullstack that the operator reconciles.
var fullstackManaged = map[string]bool{"CloudflareAccount": true, "KVNamespace": true, "D1Database": true, "R2Bucket": true,
	"WorkerScript": true, "PagesProject": true, "PagesDeployment": true}

// testFullStack applies examples/fullstack (its kustomize output, as `kubectl apply -k` would)
// into a namespace of its own, with its own account ID and the account pointed at the
// in-cluster flarefake, and checks: every managed object Ready and Synced; the Worker's
// bindings carry the D1 database ID, R2 bucket name and KV namespace ID, and its static assets
// were uploaded; the Pages project binds the same three and has exactly one deployment; a
// reconcile of everything again writes nothing; deleting the namespace deletes the Worker, KV
// namespace and Pages project and keeps the D1 database and R2 bucket (deletionPolicy Orphan).
func (s *suite) testFullStack(t *testing.T) {
	ns, acct := s.ns+"-fs", randomHex(16)
	objs, err := kustomizelite.Build("../../examples/fullstack")
	if err != nil {
		t.Fatal(err)
	}
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"flare.dev/e2e": "true"}}})
	t.Cleanup(func() { s.cleanupFullStack(t, ns, objs) })

	var managed []*unstructured.Unstructured
	for _, u := range objs {
		u = u.DeepCopy()
		switch u.GetKind() {
		case "Namespace":
			continue
		case "CloudflareAccount":
			for _, f := range []struct {
				v    any
				path []string
			}{
				{acct, []string{"spec", "accountID"}},
				{s.cfg.baseURL(), []string{"spec", "baseURL"}},
				{map[string]any{"requestsPerFiveMinutes": int64(300000), "burst": int64(1000)}, []string{"spec", "rateLimit"}},
			} {
				if err := unstructured.SetNestedField(u.Object, f.v, f.path...); err != nil {
					t.Fatal(err)
				}
			}
		case "Secret":
			if u.GetName() == "cloudflare-token" {
				_ = unstructured.SetNestedField(u.Object, "e2e-token-"+randomHex(8), "stringData", "token")
			}
		}
		u.SetNamespace(ns)
		s.create(u)
		if fullstackManaged[u.GetKind()] {
			managed = append(managed, u)
		}
	}
	if len(managed) != len(fullstackManaged) {
		t.Fatalf("the example has %d managed objects, want one of each of %d kinds", len(managed), len(fullstackManaged))
	}
	got := map[string]*unstructured.Unstructured{}
	for _, u := range managed {
		got[u.GetKind()] = s.waitReadySynced(t, u, 3*time.Minute)
	}
	id := func(kind string) string {
		v, _, _ := unstructured.NestedString(got[kind].Object, "status", "id")
		if v == "" {
			t.Fatalf("%s has no status.id", kind)
		}
		return v
	}
	d1ID, r2Name, kvID, script, project, deployment := id("D1Database"), id("R2Bucket"), id("KVNamespace"), id("WorkerScript"),
		id("PagesProject"), id("PagesDeployment")
	get := func(p string, into any) error {
		b, err := s.fake("GET", "/client/v4/accounts/"+acct+p, nil)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(b, &env); err != nil {
			return fmt.Errorf("decode %s: %w", p, err)
		}
		return json.Unmarshal(env.Result, into)
	}

	// The Worker: bindings by reference, static assets, workers.dev.
	var settings struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := get("/workers/scripts/"+script+"/settings", &settings); err != nil {
		t.Fatalf("Worker settings: %v", err)
	}
	want := map[string]map[string]any{
		"ASSETS":         {"type": "assets"},
		"DB":             {"type": "d1", "database_id": d1ID},
		"FILES":          {"type": "r2_bucket", "bucket_name": r2Name},
		"SESSIONS":       {"type": "kv_namespace", "namespace_id": kvID},
		"SESSION_SECRET": {"type": "secret_text"},
	}
	if len(settings.Bindings) != len(want) {
		t.Errorf("Worker bindings %v, want %d", settings.Bindings, len(want))
	}
	for _, b := range settings.Bindings {
		for k, v := range want[fmt.Sprint(b["name"])] {
			if b[k] != v {
				t.Errorf("Worker binding %v: %s = %v, want %v", b, k, b[k], v)
			}
		}
	}
	ws := got["WorkerScript"].Object
	url, _, _ := unstructured.NestedString(ws, "status", "atProvider", "url")
	hasAssets, _, _ := unstructured.NestedBool(ws, "status", "atProvider", "has_assets")
	assetFiles, _, _ := unstructured.NestedInt64(ws, "status", "artifacts", "assetFiles")
	assetsHash, _, _ := unstructured.NestedString(ws, "status", "assetsHash")
	if !strings.HasPrefix(url, "https://"+script+".") || !strings.HasSuffix(url, ".workers.dev") || !hasAssets || assetFiles != 3 || assetsHash == "" {
		t.Errorf("WorkerScript status: url %q, has_assets %v, assetFiles %d (want 3), assetsHash %q", url, hasAssets, assetFiles, assetsHash)
	}

	// The Pages project: the same bindings, one deployment.
	var pp struct {
		Subdomain         string `json:"subdomain"`
		DeploymentConfigs struct {
			Production struct {
				D1   map[string]map[string]any `json:"d1_databases"`
				KV   map[string]map[string]any `json:"kv_namespaces"`
				R2   map[string]map[string]any `json:"r2_buckets"`
				Vars map[string]map[string]any `json:"env_vars"`
			} `json:"production"`
		} `json:"deployment_configs"`
	}
	if err := get("/pages/projects/"+project, &pp); err != nil {
		t.Fatalf("Pages project: %v", err)
	}
	prod := pp.DeploymentConfigs.Production
	if prod.D1["DB"]["id"] != d1ID || prod.KV["SESSIONS"]["namespace_id"] != kvID || prod.R2["FILES"]["name"] != r2Name ||
		prod.Vars["SESSION_SECRET"]["type"] != "secret_text" {
		t.Errorf("Pages production bindings: d1 %v, kv %v, r2 %v, env_vars %v", prod.D1, prod.KV, prod.R2, prod.Vars)
	}
	var deployments []struct {
		ID string `json:"id"`
	}
	if err := get("/pages/projects/"+project+"/deployments", &deployments); err != nil {
		t.Fatalf("Pages deployments: %v", err)
	}
	if len(deployments) != 1 || deployments[0].ID != deployment {
		t.Errorf("Pages deployments %v, want exactly %s", deployments, deployment)
	}
	if u, _, _ := unstructured.NestedString(got["PagesProject"].Object, "status", "atProvider", "url"); u != "https://"+pp.Subdomain {
		t.Errorf("PagesProject status.atProvider.url = %q, subdomain %q", u, pp.Subdomain)
	}

	// Steady state: a reconcile of every object reads each resource and writes nothing.
	s.clearJournal()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, u := range managed {
		if u.GetKind() == "CloudflareAccount" {
			continue // reconciles on spec changes only, and never writes to Cloudflare
		}
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, pokeAnnotation, stamp)
		if err := s.c.Patch(s.ctx(), u.DeepCopy(), client.RawPatch("application/merge-patch+json", []byte(patch))); err != nil {
			t.Fatalf("poke %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
	ids := []string{d1ID, r2Name, kvID, "/workers/scripts/" + script, "/pages/projects/" + project, deployment}
	eventually(t, 3*time.Minute, "a GET of every resource after the poke", func() (bool, string) {
		j := s.journalFor(acct)
		var missing []string
		for _, id := range ids {
			if !slices.ContainsFunc(j, func(e journalEntry) bool { return e.Method == "GET" && strings.Contains(e.Path, id) }) {
				missing = append(missing, id)
			}
		}
		return len(missing) == 0, "not yet read: " + strings.Join(missing, ", ")
	})
	time.Sleep(5 * time.Second) // let any follow-up write of those reconciles land
	j := s.journalFor(acct)
	if w := writes(j); len(w) > 0 {
		t.Errorf("%d Cloudflare write(s) in steady state: %v", len(w), w)
	}
	s.checkJournalClean(j)

	// Teardown: deleting the namespace deletes per deletionPolicy.
	s.clearJournal()
	if err := s.c.Delete(s.ctx(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	s.waitGone(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, 4*time.Minute)
	for _, c := range []struct {
		what, path string
		kept       bool
	}{
		{"D1 database (Orphan)", "/d1/database/" + d1ID, true},
		{"R2 bucket (Orphan)", "/r2/buckets/" + r2Name, true},
		{"KV namespace (Delete)", "/storage/kv/namespaces/" + kvID, false},
		{"Worker (Delete)", "/workers/scripts/" + script + "/settings", false},
		{"Pages project (Delete)", "/pages/projects/" + project, false},
	} {
		var v map[string]any
		err := get(c.path, &v)
		switch {
		case c.kept && err != nil:
			t.Errorf("%s was not kept: GET %s: %v", c.what, c.path, err)
		case !c.kept && !apierrors.IsNotFound(err):
			t.Errorf("%s was not deleted: GET %s: %v", c.what, c.path, err)
		}
	}
	j = s.journalFor(acct)
	for _, e := range j {
		if e.Method == "DELETE" && (strings.Contains(e.Path, "/d1/database/") || strings.Contains(e.Path, "/r2/buckets/")) {
			t.Errorf("teardown deleted orphaned data: %s", e)
		}
	}
	s.checkJournalClean(j)
	t.Logf("teardown writes: %v", writes(j))
}

// waitReadySynced waits until u has Ready and Synced True for its generation, and returns it.
func (s *suite) waitReadySynced(t *testing.T, u *unstructured.Unstructured, timeout time.Duration) *unstructured.Unstructured {
	t.Helper()
	cur := u.DeepCopy()
	eventually(t, timeout, fmt.Sprintf("%s %s Ready and Synced", u.GetKind(), u.GetName()), func() (bool, string) {
		if err := s.c.Get(s.ctx(), client.ObjectKeyFromObject(u), cur); err != nil {
			return false, err.Error()
		}
		raw, _, _ := unstructured.NestedSlice(cur.Object, "status", "conditions")
		var conds []metav1.Condition
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &conds)
		og, _, _ := unstructured.NestedInt64(cur.Object, "status", "observedGeneration")
		r, sy := cond(conds, "Ready"), cond(conds, "Synced")
		ok := r != nil && sy != nil && r.Status == metav1.ConditionTrue && sy.Status == metav1.ConditionTrue && og == cur.GetGeneration()
		return ok, fmt.Sprintf("generation %d observed %d: %s", cur.GetGeneration(), og, condString(conds))
	})
	return cur
}

// cleanupFullStack deletes the step's namespace if it is still there, and removes stuck
// finalizers after 3 minutes (reporting them).
func (s *suite) cleanupFullStack(t *testing.T, ns string, objs []*unstructured.Unstructured) {
	var n corev1.Namespace
	if err := s.c.Get(s.ctx(), client.ObjectKey{Name: ns}, &n); apierrors.IsNotFound(err) {
		return
	}
	_ = s.c.Delete(s.ctx(), &n)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := s.c.Get(s.ctx(), client.ObjectKey{Name: ns}, &n); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("namespace %s still exists after 3m; removing finalizers", ns)
	for _, u := range objs {
		if u.GetKind() == "Namespace" {
			continue
		}
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(u.GroupVersionKind())
		if err := s.c.Get(s.ctx(), client.ObjectKey{Namespace: ns, Name: u.GetName()}, cur); err != nil {
			continue
		}
		if len(cur.GetFinalizers()) > 0 {
			t.Logf("stuck: %s %s finalizers %v", cur.GetKind(), cur.GetName(), cur.GetFinalizers())
			_ = s.c.Patch(s.ctx(), cur, client.RawPatch("application/merge-patch+json", []byte(`{"metadata":{"finalizers":null}}`)))
		}
	}
}
