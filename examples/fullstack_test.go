package examples_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller/pagesdeployment"
	"flare.dev/operator/internal/controller/pagesproject"
	"flare.dev/operator/internal/controller/workerscript"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	_ "flare.dev/operator/internal/generic/kinds" // KVNamespace, D1Database, R2Bucket controllers
	"flare.dev/operator/internal/kustomizelite"
	"flare.dev/operator/internal/testenv"
)

const fullstackDir = "fullstack"

// fullstackFiles returns every manifest file of examples/fullstack (all *.yaml but the
// kustomization).
func fullstackFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(fullstackDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") && d.Name() != "kustomization.yaml" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests in %s: %v", fullstackDir, err)
	}
	return files
}

// fullstackObjects is `kubectl kustomize examples/fullstack`.
func fullstackObjects(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	objs, err := kustomizelite.Build(fullstackDir)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// dryRun server-side dry-run creates u with strict field validation.
func dryRun(t *testing.T, e *testenv.Env, u *unstructured.Unstructured) {
	t.Helper()
	if err := e.Client.Create(testenv.Context(t, 30*time.Second), u, client.DryRunAll, client.FieldValidation("Strict")); err != nil {
		t.Fatalf("%s %s: %v", u.GetKind(), u.GetName(), err)
	}
}

// TestFullStackFilesValidate dry-run creates every document of every file in examples/fullstack
// (the ConfigMaps come from the kustomization, see TestFullStackKustomizeValidates).
func TestFullStackFilesValidate(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	for _, f := range fullstackFiles(t) {
		for i, u := range docs(t, f) {
			t.Run(fmt.Sprintf("%s#%d-%s", f, i, u.GetName()), func(t *testing.T) {
				if u.GetNamespace() != "" {
					t.Fatalf("manifests must not set metadata.namespace (the kustomization does); got %q", u.GetNamespace())
				}
				if u.GetKind() != "Namespace" {
					u.SetNamespace(ns)
				}
				dryRun(t, e, u)
			})
		}
	}
}

// TestFullStackKustomizeValidates dry-run creates the kustomize output (in its own namespace,
// which is created for real so the namespaced dry runs find it), and checks its wiring: every
// ConfigMap, Secret and object the manifests refer to is part of the output.
func TestFullStackKustomizeValidates(t *testing.T) {
	e := testenv.Require(t, env)
	objs := fullstackObjects(t)
	have := map[string]bool{}
	var namespaces int
	for _, u := range objs {
		have[u.GetKind()+"/"+u.GetName()] = true
		if u.GetKind() == "Namespace" {
			namespaces++
			if err := e.Client.Create(testenv.Context(t, 30*time.Second), u.DeepCopy()); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
		}
	}
	if namespaces != 1 {
		t.Fatalf("want exactly one Namespace in the output, got %d", namespaces)
	}
	for _, u := range objs {
		if u.GetKind() == "Namespace" {
			continue
		}
		t.Run(u.GetKind()+"-"+u.GetName(), func(t *testing.T) { dryRun(t, e, u) })
	}
	for _, ref := range fullstackRefs(objs) {
		if !have[ref] {
			t.Errorf("the example refers to %s, which the kustomize output does not contain", ref)
		}
	}
}

// fullstackRefs returns "<Kind>/<name>" of every object the example's manifests refer to.
func fullstackRefs(objs []*unstructured.Unstructured) []string {
	var refs []string
	var walk func(field string, v any)
	walk = func(field string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				walk(k, c)
			}
			if field == "configMapRef" {
				for _, cm := range v["configMaps"].([]any) {
					refs = append(refs, "ConfigMap/"+cm.(map[string]any)["name"].(string))
				}
			}
			kind := map[string]string{"accountRef": "CloudflareAccount", "tokenSecretRef": "Secret", "secretKeyRef": "Secret",
				"d1DatabaseRef": "D1Database", "r2BucketRef": "R2Bucket", "kvNamespaceRef": "KVNamespace", "projectRef": "PagesProject"}[field]
			if kind != "" {
				refs = append(refs, kind+"/"+v["name"].(string))
			}
		case []any:
			for _, c := range v {
				walk(field, c)
			}
		}
	}
	for _, u := range objs {
		walk("", u.Object["spec"])
	}
	sort.Strings(refs)
	return slices.Compact(refs)
}

// TestFullStackKustomizeMatchesKubectl compares kustomizelite's rendering with kubectl's, when
// kubectl is installed.
func TestFullStackKustomizeMatchesKubectl(t *testing.T) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	out, err := exec.Command(kubectl, "kustomize", fullstackDir).Output()
	if err != nil {
		t.Fatalf("kubectl kustomize: %v", err)
	}
	f := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(f, out, 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := kustomizelite.ReadManifests(f)
	if err != nil {
		t.Fatal(err)
	}
	key := func(u *unstructured.Unstructured) string {
		return u.GetAPIVersion() + " " + u.GetKind() + " " + u.GetNamespace() + "/" + u.GetName()
	}
	got := map[string]*unstructured.Unstructured{}
	for _, u := range fullstackObjects(t) {
		got[key(u)] = u
	}
	if len(got) != len(want) {
		t.Errorf("kustomizelite renders %d objects, kubectl %d", len(got), len(want))
	}
	for _, w := range want {
		g, ok := got[key(w)]
		if !ok {
			t.Errorf("kustomizelite does not render %s", key(w))
			continue
		}
		if !reflect.DeepEqual(g.Object, w.Object) {
			gb, _ := json.MarshalIndent(g.Object, "", "  ")
			wb, _ := json.MarshalIndent(w.Object, "", "  ")
			t.Errorf("%s differs:\nkustomizelite: %s\nkubectl: %s", key(w), gb, wb)
		}
	}
}

// Controllers the full-stack example needs, by the names their reconciles are logged under.
var fullstackControllers = map[string]string{
	"CloudflareAccount": "cloudflareaccount",
	"KVNamespace":       "kvnamespace",
	"D1Database":        "d1database",
	"R2Bucket":          "r2bucket",
	"WorkerScript":      workerscript.Name,
	"PagesProject":      pagesproject.Name,
	"PagesDeployment":   pagesdeployment.Name,
}

// TestFullStack applies the kustomize output of examples/fullstack (as `kubectl apply -k`
// would, all at once, into a test namespace, with the account pointed at the in-process
// flarefake) and runs it against the operator's controllers:
//   - every managed object becomes Ready and Synced;
//   - flarefake holds the Worker with its bindings (the D1 database ID, R2 bucket name, KV
//     namespace ID, the secret and the assets) and its assets manifest and config, and the
//     Pages project with its bindings and exactly one deployment of the site and _worker.js;
//   - reconciling everything again writes nothing;
//   - deleting the namespace deletes what deletionPolicy Delete covers (Worker, KV namespace,
//     Pages project) and keeps the D1 database and the R2 bucket (Orphan, their default).
func TestFullStack(t *testing.T) {
	if testing.Short() {
		t.Skip("R2 buckets are served by the fake's generic profile, which needs the pinned spec")
	}
	e := testenv.Require(t, env)
	saved := generic.ReferrerRetry
	generic.ReferrerRetry = 500 * time.Millisecond // a KV namespace waiting for its referrers
	t.Cleanup(func() { generic.ReferrerRetry = saved })

	ns := e.Namespace(t)
	names := []string{"kvnamespace", "d1database", "r2bucket", workerscript.Name, pagesproject.Name, pagesdeployment.Name}
	m := e.StartManager(t, testenv.ManagerOptions{Controllers: names, Namespaces: []string{ns}})
	accountID, token := testenv.RandomAccountID(), "tok-"+testenv.RandomHex(12)
	e.Fake.AddToken(fake.Token{Value: token, AccountID: accountID})
	cf, err := cfclient.New(cfclient.Options{Token: token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	apiGet := func(p string, out any) error {
		resp, err := cf.Do(testenv.Context(t, 20*time.Second), cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + accountID + p})
		if err != nil {
			return err
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}

	// kubectl apply -k, with the placeholders replaced.
	var managed []*unstructured.Unstructured
	secretValue := ""
	for _, u := range fullstackObjects(t) {
		switch u.GetKind() {
		case "Namespace":
			continue // the test's own namespace instead
		case "CloudflareAccount":
			must(t, unstructured.SetNestedField(u.Object, accountID, "spec", "accountID"))
			must(t, unstructured.SetNestedField(u.Object, e.BaseURL, "spec", "baseURL"))
			must(t, unstructured.SetNestedMap(u.Object, map[string]any{"requestsPerFiveMinutes": int64(300000), "burst": int64(1000)}, "spec", "rateLimit"))
		case "Secret":
			if u.GetName() == "cloudflare-token" {
				must(t, unstructured.SetNestedField(u.Object, token, "stringData", "token"))
			} else {
				secretValue, _, _ = unstructured.NestedString(u.Object, "stringData", "session-secret")
			}
		}
		u.SetNamespace(ns)
		if _, ok := fullstackControllers[u.GetKind()]; ok {
			managed = append(managed, u)
		}
		if err := e.Client.Create(testenv.Context(t, 20*time.Second), u); err != nil {
			t.Fatalf("create %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
	if len(managed) != len(fullstackControllers) {
		t.Fatalf("the example has %d managed objects, want one of each of %d kinds", len(managed), len(fullstackControllers))
	}

	// Every managed object Ready and Synced.
	got := map[string]*unstructured.Unstructured{}
	for _, u := range managed {
		got[u.GetKind()] = waitReadySynced(t, e, u, 2*time.Minute)
	}
	id := func(kind string) string {
		s, _, _ := unstructured.NestedString(got[kind].Object, "status", "id")
		if s == "" {
			t.Fatalf("%s has no status.id", kind)
		}
		return s
	}
	d1ID, r2Name, kvID, script, project := id("D1Database"), id("R2Bucket"), id("KVNamespace"), id("WorkerScript"), id("PagesProject")
	if script != "notes-app" || project != "flare-notes-example" || r2Name != "notes-files" {
		t.Fatalf("status IDs: script %q, project %q, bucket %q", script, project, r2Name)
	}

	// The Worker in flarefake: bindings, assets and the workers.dev URL.
	var settings struct {
		Bindings []map[string]any `json:"bindings"`
	}
	must(t, apiGet("/workers/scripts/"+script+"/settings", &settings))
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
		w, ok := want[fmt.Sprint(b["name"])]
		if !ok {
			t.Errorf("unexpected Worker binding %v", b)
			continue
		}
		for k, v := range w {
			if b[k] != v {
				t.Errorf("Worker binding %v: %s = %v, want %v", b, k, b[k], v)
			}
		}
	}
	manifest, config, ok := e.Fake.WorkerAssets(accountID, script)
	if !ok {
		t.Fatal("the Worker has no assets in flarefake")
	}
	for _, f := range []string{"index.html", "app.js", "style.css"} {
		hash, ok := manifest["/"+f]
		if !ok {
			t.Errorf("assets manifest %v lacks /%s", manifest, f)
			continue
		}
		asset, ok := e.Fake.UploadedAsset(accountID, script, hash)
		if !ok || !bytes.Equal(asset.Content, readFile(t, "fullstack/public/"+f)) {
			t.Errorf("asset /%s (hash %s): uploaded %v, content differs from public/%s", f, hash, ok, f)
		}
	}
	if len(manifest) != 3 {
		t.Errorf("assets manifest %v, want the 3 files of public/", manifest)
	}
	if config["not_found_handling"] != "single-page-application" || fmt.Sprint(config["run_worker_first"]) != "[/api/*]" {
		t.Errorf("assets config %v", config)
	}
	ws := got["WorkerScript"]
	if u, _, _ := unstructured.NestedString(ws.Object, "status", "atProvider", "url"); u != "https://notes-app.example-subdomain.workers.dev" {
		t.Errorf("WorkerScript status.atProvider.url = %q", u)
	}
	if has, _, _ := unstructured.NestedBool(ws.Object, "status", "atProvider", "has_assets"); !has {
		t.Error("WorkerScript status.atProvider.has_assets is not true")
	}
	if n, _, _ := unstructured.NestedInt64(ws.Object, "status", "artifacts", "modules", "files"); n != 1 {
		t.Errorf("WorkerScript status.artifacts.modules.files = %d, want 1 (worker/index.js)", n)
	}
	workerJS := readFile(t, "fullstack/worker/index.js")
	if mainModule, code, ok := e.Fake.WorkerMainModule(accountID, script); !ok || mainModule != "index.js" || !bytes.Equal(code, workerJS) {
		t.Errorf("the Worker in flarefake: main_module %q, %d bytes of code (found %v), want index.js with worker/index.js (%d bytes)", mainModule, len(code), ok, len(workerJS))
	}
	// The code uses every binding under the name the manifest gives it.
	for name := range want {
		if !bytes.Contains(workerJS, []byte("env."+name)) {
			t.Errorf("worker/index.js does not use binding %s (env.%s)", name, name)
		}
	}

	// The Pages project in flarefake: bindings, and one deployment of the site and _worker.js.
	cfg, ok := e.Fake.PagesProjectConfig(accountID, project, "production")
	if !ok {
		t.Fatal("no Pages project in flarefake")
	}
	for k, w := range map[string]string{
		"d1_databases":  fmt.Sprintf("map[DB:map[id:%s]]", d1ID),
		"r2_buckets":    fmt.Sprintf("map[FILES:map[name:%s]]", r2Name),
		"kv_namespaces": fmt.Sprintf("map[SESSIONS:map[namespace_id:%s]]", kvID),
		"env_vars":      fmt.Sprintf("map[SESSION_SECRET:map[type:secret_text value:%s]]", secretValue),
	} {
		if g := fmt.Sprint(cfg[k]); g != w {
			t.Errorf("Pages production %s = %s, want %s", k, g, w)
		}
	}
	var deployments []struct {
		ID string `json:"id"`
	}
	must(t, apiGet("/pages/projects/"+project+"/deployments", &deployments))
	if len(deployments) != 1 {
		t.Fatalf("Pages deployments %v, want exactly one", deployments)
	}
	if dep, _, _ := unstructured.NestedString(got["PagesDeployment"].Object, "status", "id"); dep != deployments[0].ID {
		t.Errorf("PagesDeployment status.id = %q, want %q", dep, deployments[0].ID)
	}
	pm, pf, ok := e.Fake.PagesDeploymentFiles(accountID, project, deployments[0].ID)
	if !ok {
		t.Fatal("no deployment files in flarefake")
	}
	if keys := sortedKeys(pm); fmt.Sprint(keys) != "[/app.js /index.html /style.css]" {
		t.Errorf("Pages manifest %v", keys)
	}
	if !bytes.Equal(pf["_worker.js"], readFile(t, "fullstack/worker/index.js")) || !bytes.Equal(pf["_routes.json"], readFile(t, "fullstack/pages/site/_routes.json")) {
		t.Errorf("Pages routing files: _worker.js %d bytes, _routes.json %q", len(pf["_worker.js"]), pf["_routes.json"])
	}
	if u, _, _ := unstructured.NestedString(got["PagesProject"].Object, "status", "atProvider", "url"); u != "https://flare-notes-example.pages.dev" {
		t.Errorf("PagesProject status.atProvider.url = %q", u)
	}
	if u, _, _ := unstructured.NestedString(got["PagesDeployment"].Object, "status", "atProvider", "url"); u == "" {
		t.Error("PagesDeployment has no status.atProvider.url")
	}

	// A second reconcile of everything writes nothing. (The CloudflareAccount is left out: it
	// reconciles on spec changes only, and it never writes to Cloudflare.)
	since := len(e.Journal(t))
	mark := m.Mark()
	var poked []*unstructured.Unstructured
	for _, u := range managed {
		if u.GetKind() != "CloudflareAccount" {
			poke(t, e, u)
			poked = append(poked, u)
		}
	}
	for _, u := range poked {
		m.WaitReconciled(t, fullstackControllers[u.GetKind()], client.ObjectKeyFromObject(u), mark, 1, 2*time.Minute)
	}
	j := testenv.ForAccount(e.Journal(t)[since:], accountID)
	deployment := id("PagesDeployment")
	for _, id := range []string{d1ID, r2Name, kvID, "/workers/scripts/" + script, "/pages/projects/" + project, deployment} {
		if testenv.Count(j, http.MethodGet, id) == 0 {
			t.Errorf("the reconciles after the poke did not read %s:\n%s", id, testenv.Summary(j))
		}
	}
	if w := testenv.Writes(j); len(w) != 0 {
		t.Fatalf("a reconcile without changes wrote to Cloudflare:\n%s", testenv.Summary(w))
	}

	// Delete the namespace. envtest runs no namespace controller, so delete everything in it
	// the way that controller would: all at once, the account and its Secret included.
	since = len(e.Journal(t))
	ctx := testenv.Context(t, 30*time.Second)
	if err := e.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	all := fullstackObjects(t)
	for _, u := range all {
		if u.GetKind() == "Namespace" {
			continue
		}
		u.SetNamespace(ns)
		if err := e.Client.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
	for _, u := range all {
		if u.GetKind() == "Namespace" {
			continue
		}
		testenv.Eventually(t, 90*time.Second, func() (bool, string) {
			err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(u), u.DeepCopy())
			return apierrors.IsNotFound(err), fmt.Sprintf("%s %s still exists (%v)", u.GetKind(), u.GetName(), err)
		})
	}
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
		err := apiGet(c.path, nil)
		switch {
		case c.kept && err != nil:
			t.Errorf("%s was not kept: GET %s: %v", c.what, c.path, err)
		case !c.kept && !cfclient.IsNotFound(err):
			t.Errorf("%s was not deleted: GET %s: %v", c.what, c.path, err)
		}
	}
	for _, e := range testenv.Writes(testenv.ForAccount(e.Journal(t)[since:], accountID)) {
		if e.Method == http.MethodDelete && (strings.Contains(e.Path, "/d1/database/") || strings.Contains(e.Path, "/r2/buckets/")) {
			t.Errorf("teardown deleted orphaned data: %s %s", e.Method, e.Path)
		}
	}

	// Every request of the run matched the pinned spec (but for its known defects), and none
	// got a server error (but GET …/tags of a never-tagged resource, internal/fake/tags.go).
	for _, e := range e.Journal(t) {
		if !strings.Contains(e.Path, accountID) {
			continue
		}
		if e.SchemaViolation != "" && !knownSpecDefect(e) {
			t.Errorf("request violates the pinned spec: %s %s: %s", e.Method, e.Path, e.SchemaViolation)
		}
		if e.Status >= 500 && !(e.Method == http.MethodGet && strings.HasSuffix(e.Path, "/tags")) {
			t.Errorf("flarefake answered %s %s with %d", e.Method, e.Path, e.Status)
		}
	}
}

// knownSpecDefect reports requests the real API (or wrangler, a real client) sends or accepts
// but the pinned spec rejects: the list of test/e2e/helpers_test.go.
func knownSpecDefect(e fake.JournalEntry) bool {
	for _, k := range []struct{ method, pathPart, contains string }{
		{"POST", "/d1/database", `"/primary_location_hint": value is not one of the allowed values`},                     // 0019
		{"", "/d1/database/", `parameter "database_id" in path has an error: input matches more than one oneOf schemas`}, // 0020
		{"DELETE", "/storage/kv/namespaces/", "request body has an error: value is required but missing"},                // 0013
		// wrangler sends the Pages deployment manifest as a plain form field; the spec's encoding
		// says application/json (internal/controller/pagesdeployment/spec_test.go).
		{"POST", "/pages/projects/", "path manifest: not matching content types"},
	} {
		if (k.method == "" || k.method == e.Method) && strings.Contains(e.Path, k.pathPart) && strings.Contains(e.SchemaViolation, k.contains) {
			return true
		}
	}
	return false
}

// waitReadySynced waits until u (re-read) has Ready and Synced True for its generation, and
// returns it.
func waitReadySynced(t *testing.T, e *testenv.Env, u *unstructured.Unstructured, timeout time.Duration) *unstructured.Unstructured {
	t.Helper()
	cur := u.DeepCopy()
	testenv.Eventually(t, timeout, func() (bool, string) {
		if err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(u), cur); err != nil {
			return false, err.Error()
		}
		conds, _, _ := unstructured.NestedSlice(cur.Object, "status", "conditions")
		var cs []metav1.Condition
		b, _ := json.Marshal(conds)
		_ = json.Unmarshal(b, &cs)
		og, _, _ := unstructured.NestedInt64(cur.Object, "status", "observedGeneration")
		r, s := meta.FindStatusCondition(cs, "Ready"), meta.FindStatusCondition(cs, "Synced")
		ok := r != nil && s != nil && r.Status == metav1.ConditionTrue && s.Status == metav1.ConditionTrue && og == cur.GetGeneration()
		return ok, fmt.Sprintf("%s %s: generation %d, observed %d, conditions %s", u.GetKind(), u.GetName(), cur.GetGeneration(), og, b)
	})
	return cur
}

// poke changes an annotation so the object is reconciled again without a spec change.
func poke(t *testing.T, e *testenv.Env, u *unstructured.Unstructured) {
	t.Helper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"test.flare.dev/poke":%q}}}`, time.Now().Format(time.RFC3339Nano))
	if err := e.Client.Patch(testenv.Context(t, 10*time.Second), u.DeepCopy(), client.RawPatch("application/merge-patch+json", []byte(patch))); err != nil {
		t.Fatalf("poke %s %s: %v", u.GetKind(), u.GetName(), err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
