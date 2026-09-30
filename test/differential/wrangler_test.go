//go:build differential

package differential

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"flare.dev/operator/test/differential/harness"
)

// Evidence citations into wrangler's source use the pinned release: tag wrangler@4.143.0 =
// cloudflare/workers-sdk commit 3bdcd0d46102289e7ef417c4fb3086ea77a18fb1, or the published
// bundle wrangler@4.143.0:wrangler-dist/cli.js where the source file was not located. A new
// mismatch becomes a harness.Discrepancy with such a citation (docs/differential-testing.md).

// Deterministic IDs (queued in flarefake before each create), so the Worker's config can name
// them before they exist.
const (
	kvID = "0f2ac74b498b48028cb68387c421e279"
	d1ID = "8b1f2e4c-3a5d-4e6f-9a7b-0c1d2e3f4a5b"
)

const workerName = "flare-diff-worker"

// knownWranglerSpecViolations are requests wrangler makes on every such call that break the
// pinned spec, so the live API must accept them (SOURCED, relies); flarefake journals them and
// answers normally. harness.KnownSpecDefects covers the recording-proven ones.
var knownWranglerSpecViolations = []string{
	// The services routes are absent from the pinned spec; wrangler deploy and delete use them
	// (internal/fake/workers_versions.go workerServiceGet, workerServiceDelete).
	`GET /accounts/` + harness.AccountID + `/workers/services/` + workerName + `: no such operation in pinned spec: no matching operation was found`,
	`DELETE /accounts/` + harness.AccountID + `/workers/services/` + workerName + `: no such operation in pinned spec: no matching operation was found`,
	// `kv key delete` sends no body; the spec requires one (as for the namespace DELETE, 0013).
	// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/kv/helpers.ts#L279-L292.
	`DELETE /accounts/` + harness.AccountID + `/storage/kv/namespaces/` + kvID + `/values/k1: request body has an error: value is required but missing`,
	// `kv key put --metadata` sends metadata as a plain form field (text/plain); the spec's
	// multipart encoding wants application/json. helpers.ts#L254-L258.
	`PUT /accounts/` + harness.AccountID + `/storage/kv/namespaces/` + kvID + `/values/dir/k2: request body has an error: failed to decode request body: path metadata: not matching content types: header "text/plain", encoding "application/json"`,
}

type wrangler struct {
	bin, home, proj string
	env             []string
	f               *harness.Fake
}

// wranglerBin returns the pinned wrangler, or skips the test.
func wranglerBin(t *testing.T) string {
	return findClient(t, "FLARE_DIFF_WRANGLER",
		filepath.Join(cacheDir(), "wrangler-"+WranglerVersion, "node_modules", ".bin", "wrangler"), "wrangler", false)
}

func newWrangler(t *testing.T, f *harness.Fake, bin string) *wrangler {
	w := &wrangler{bin: bin, home: t.TempDir(), proj: t.TempDir(), f: f}
	w.env = []string{
		"HOME=" + w.home, "XDG_CONFIG_HOME=" + filepath.Join(w.home, ".config"),
		// The API base-URL override: workers-sdk@3bdcd0d:packages/workers-utils/src/
		// environment-variables/misc-variables.ts#L153-L166 (getCloudflareApiBaseUrl).
		"CLOUDFLARE_API_BASE_URL=" + f.API,
		"CLOUDFLARE_API_TOKEN=" + harness.Token,
		"CLOUDFLARE_ACCOUNT_ID=" + harness.AccountID, // skips the /memberships account lookup
		// Telemetry and error reports off (misc-variables.ts#L40-L66), no banner and so no npm
		// update check (src/wrangler-banner.ts), no agent-skills prompt, non-interactive.
		"WRANGLER_SEND_METRICS=false", "WRANGLER_SEND_ERROR_REPORTS=false", "DO_NOT_TRACK=1",
		"WRANGLER_HIDE_BANNER=true", "WRANGLER_NO_SKILLS_UPDATE_PROMPTS=1", "CI=1",
		"CLOUDFLARE_CF_FETCH_ENABLED=false",
		"WRANGLER_LOG_PATH=" + filepath.Join(w.home, "logs"),
	}
	if err := os.MkdirAll(filepath.Join(w.proj, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.proj, "src", "index.js"), `export default {
  async fetch(request, env) {
    await env.KV.get("k");
    return new Response("hello from flare-diff");
  },
};
`)
	w.config(t, "wrangler.json", true)
	w.config(t, "wrangler-nodev.json", false)
	return w
}

// config writes a Worker config (an ES module Worker bound to the KV namespace, queue and D1
// database the test creates).
func (w *wrangler) config(t *testing.T, name string, workersDev bool) {
	write(t, filepath.Join(w.proj, name), fmt.Sprintf(`{
  "name": %q,
  "main": "src/index.js",
  "compatibility_date": "2026-09-01",
  "workers_dev": %t,
  "kv_namespaces": [{"binding": "KV", "id": %q}],
  "queues": {"producers": [{"binding": "Q", "queue": "flare-diff-q"}]},
  "d1_databases": [{"binding": "DB", "database_name": "flare-diff-d1", "database_id": %q}]
}
`, workerName, workersDev, kvID, d1ID))
}

func write(t *testing.T, file, content string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run runs wrangler in the project directory and logs the requests it made.
func (w *wrangler) run(t *testing.T, args ...string) result {
	t.Helper()
	mark := w.f.Mark()
	r := run(t, w.proj, w.env, w.bin, args...)
	logRequests(t, w.f, mark)
	return r
}

// ok runs wrangler and fails the (sub)test if it exits non-zero.
func (w *wrangler) ok(t *testing.T, args ...string) result {
	t.Helper()
	r := w.run(t, args...)
	if r.err != nil {
		t.Errorf("%v", r.failed())
	}
	return r
}

// get reads a flarefake resource (bypassing capture and shims) and decodes its result into v.
func get(t *testing.T, f *harness.Fake, path string, v any) int {
	t.Helper()
	status, env := f.Call(http.MethodGet, path, nil)
	if status == http.StatusOK && v != nil {
		if err := json.Unmarshal(env.Result, v); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	return status
}

// jsonOut decodes the JSON a wrangler --json command printed (after any warnings).
func jsonOut(t *testing.T, r result, v any) {
	t.Helper()
	s := r.stdout
	if i := strings.IndexAny(s, "[{"); i >= 0 {
		s = s[i:]
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("decode output of %s: %v\n%s", strings.Join(r.args, " "), err, r.stdout)
	}
}

func acctPath(p string) string { return "/accounts/" + harness.AccountID + p }

// TestWrangler drives the pinned wrangler through KV (namespaces and keys), Queues, D1 and a
// Worker deploy with bindings to all three, a redeploy through the versions API, and a delete,
// against flarefake. Each command's requests are logged; with FLARE_DIFF_CAPTURE_DIR set, all of
// them are written to wrangler-<version>.json there.
func TestWrangler(t *testing.T) {
	bin := wranglerBin(t)
	f := harness.Start(t, harness.Options{})
	w := newWrangler(t, f, bin)
	defer harness.WriteCapture(t, f, "wrangler-"+WranglerVersion)

	t.Run("kv/namespace-create", func(t *testing.T) {
		f.Server.EnqueueIDs(kvID)
		r := w.ok(t, "kv", "namespace", "create", "flare-diff-kv")
		if !strings.Contains(r.stdout, kvID) {
			t.Errorf("output does not name the new namespace %s:\n%s", kvID, r.stdout)
		}
		var ns struct{ Title string }
		if get(t, f, acctPath("/storage/kv/namespaces/"+kvID), &ns) != http.StatusOK || ns.Title != "flare-diff-kv" {
			t.Errorf("flarefake namespace %s: %+v", kvID, ns)
		}
	})
	t.Run("kv/namespace-list", func(t *testing.T) {
		var list []struct{ ID, Title string }
		jsonOut(t, w.ok(t, "kv", "namespace", "list"), &list)
		if len(list) != 1 || list[0].ID != kvID || list[0].Title != "flare-diff-kv" {
			t.Errorf("namespace list %+v", list)
		}
	})
	t.Run("kv/namespace-rename", func(t *testing.T) {
		w.ok(t, "kv", "namespace", "rename", "--namespace-id", kvID, "--new-name", "flare-diff-kv2")
		var ns struct{ Title string }
		if get(t, f, acctPath("/storage/kv/namespaces/"+kvID), &ns); ns.Title != "flare-diff-kv2" {
			t.Errorf("title after rename %q", ns.Title)
		}
	})
	t.Run("kv/key-put-get-list-delete", func(t *testing.T) {
		w.ok(t, "kv", "key", "put", "--namespace-id", kvID, "--remote", "k1", "v1")
		w.ok(t, "kv", "key", "put", "--namespace-id", kvID, "--remote", "dir/k2", "v2", "--metadata", `{"m":1}`)
		if r := w.ok(t, "kv", "key", "get", "--namespace-id", kvID, "--remote", "k1"); strings.TrimSpace(r.stdout) != "v1" {
			t.Errorf("kv key get printed %q, want v1", r.stdout)
		}
		var keys []struct {
			Name     string
			Metadata map[string]any
		}
		jsonOut(t, w.ok(t, "kv", "key", "list", "--namespace-id", kvID, "--remote"), &keys)
		if len(keys) != 2 || keys[0].Name != "dir/k2" || keys[1].Name != "k1" || keys[0].Metadata["m"] != float64(1) {
			t.Errorf("kv key list %+v, want dir/k2 (with metadata) and k1", keys)
		}
		w.ok(t, "kv", "key", "delete", "--namespace-id", kvID, "--remote", "k1")
		if r := w.run(t, "kv", "key", "get", "--namespace-id", kvID, "--remote", "k1"); r.err == nil && strings.TrimSpace(r.stdout) == "v1" {
			t.Errorf("kv key get after delete still printed v1")
		}
	})

	t.Run("queues/create", func(t *testing.T) {
		w.ok(t, "queues", "create", "flare-diff-q")
		var list []struct {
			QueueName string `json:"queue_name"`
		}
		if get(t, f, acctPath("/queues"), &list); len(list) != 1 || list[0].QueueName != "flare-diff-q" {
			t.Errorf("flarefake queues %+v", list)
		}
	})
	t.Run("queues/info", func(t *testing.T) {
		r := w.ok(t, "queues", "info", "flare-diff-q")
		if !strings.Contains(r.stdout, "flare-diff-q") {
			t.Errorf("queues info output:\n%s", r.stdout)
		}
	})
	t.Run("queues/list", func(t *testing.T) {
		if r := w.ok(t, "queues", "list"); !strings.Contains(r.stdout, "flare-diff-q") {
			t.Errorf("queues list output:\n%s", r.stdout)
		}
	})
	// wrangler sends the create body without a Content-Type (text/plain); strict mode must still
	// accept it (internal/fake/spec.go plainTextBodies).
	t.Run("queues/create-strict", func(t *testing.T) {
		strict := harness.Start(t, harness.Options{RejectSchemaViolations: true})
		newWrangler(t, strict, bin).ok(t, "queues", "create", "flare-diff-strict-q")
	})

	t.Run("d1/create", func(t *testing.T) {
		f.Server.EnqueueIDs(d1ID)
		r := w.ok(t, "d1", "create", "flare-diff-d1")
		if !strings.Contains(r.stdout, d1ID) {
			t.Errorf("output does not name the new database %s:\n%s", d1ID, r.stdout)
		}
	})
	t.Run("d1/list", func(t *testing.T) {
		var list []struct{ UUID, Name string }
		jsonOut(t, w.ok(t, "d1", "list", "--json"), &list)
		if len(list) != 1 || list[0].UUID != d1ID || list[0].Name != "flare-diff-d1" {
			t.Errorf("d1 list %+v", list)
		}
	})
	t.Run("d1/execute", func(t *testing.T) {
		var out []struct {
			Results []map[string]any
			Success bool
		}
		jsonOut(t, w.ok(t, "d1", "execute", "flare-diff-d1", "--remote", "--command", "select 1 as one; select 'x' as s", "--json"), &out)
		if len(out) != 2 || !out[0].Success || len(out[0].Results) != 1 || out[0].Results[0]["one"] != float64(1) || out[1].Results[0]["s"] != "x" {
			t.Errorf("d1 execute %+v, want [{one: 1}] and [{s: x}]", out)
		}
	})

	var versionID string
	t.Run("deploy", func(t *testing.T) {
		r := w.ok(t, "deploy")
		for _, want := range []string{"env.KV (" + kvID + ")", "env.Q (flare-diff-q)", "env.DB (flare-diff-d1)", workerName + ".example-subdomain.workers.dev"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("deploy output lacks %q:\n%s", want, r.stdout)
			}
		}
		var settings struct {
			Bindings []map[string]any `json:"bindings"`
		}
		if get(t, f, acctPath("/workers/scripts/"+workerName+"/settings"), &settings) != http.StatusOK {
			t.Fatalf("flarefake has no script %s after deploy", workerName)
		}
		got := map[string]string{}
		for _, b := range settings.Bindings {
			got[fmt.Sprint(b["name"])] = fmt.Sprint(b["type"])
		}
		if want := map[string]string{"KV": "kv_namespace", "Q": "queue", "DB": "d1"}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("stored bindings %v, want %v (settings %+v)", got, want, settings.Bindings)
		}
		var worker struct {
			Name      string
			Subdomain struct{ Enabled bool }
		}
		if get(t, f, acctPath("/workers/workers/"+workerName), &worker) != http.StatusOK || worker.Name != workerName || !worker.Subdomain.Enabled {
			t.Errorf("GET …/workers/workers/%s after deploy with workers_dev: true: %+v", workerName, worker)
		}
	})
	t.Run("versions/list", func(t *testing.T) {
		var vs []struct {
			ID     string
			Number int
		}
		jsonOut(t, w.ok(t, "versions", "list", "--json"), &vs)
		if len(vs) != 1 || vs[0].ID == "" {
			t.Fatalf("versions %+v", vs)
		}
		versionID = vs[0].ID
	})
	t.Run("deployments/list", func(t *testing.T) {
		var ds []struct {
			Versions []struct {
				VersionID  string  `json:"version_id"`
				Percentage float64 `json:"percentage"`
			}
		}
		jsonOut(t, w.ok(t, "deployments", "list", "--json"), &ds)
		if len(ds) != 1 || len(ds[0].Versions) != 1 || ds[0].Versions[0].VersionID != versionID || ds[0].Versions[0].Percentage != 100 {
			t.Errorf("deployments %+v, want one at 100%% to %s", ds, versionID)
		}
	})
	t.Run("deployments/status", func(t *testing.T) {
		r := w.ok(t, "deployments", "status", "--json")
		if !strings.Contains(r.stdout, versionID) {
			t.Errorf("deployments status does not name %s:\n%s", versionID, r.stdout)
		}
	})
	t.Run("versions/view", func(t *testing.T) {
		var v struct {
			ID        string
			Resources struct {
				Bindings []map[string]any
				Script   struct{ Handlers []string }
			}
		}
		jsonOut(t, w.ok(t, "versions", "view", versionID, "--json"), &v)
		if v.ID != versionID || len(v.Resources.Bindings) != 3 || fmt.Sprint(v.Resources.Script.Handlers) != "[fetch]" {
			t.Errorf("versions view %+v, want %s with 3 bindings and handlers [fetch]", v, versionID)
		}
		w.ok(t, "versions", "view", versionID) // the human-readable form reads resources.* too
	})
	t.Run("workers-dev-toggle", func(t *testing.T) {
		w.ok(t, "triggers", "deploy", "--config", "wrangler-nodev.json")
		var sub struct{ Enabled bool }
		if get(t, f, acctPath("/workers/scripts/"+workerName+"/subdomain"), &sub); sub.Enabled {
			t.Errorf("workers.dev still enabled after triggers deploy with workers_dev: false")
		}
		w.ok(t, "triggers", "deploy")
		if get(t, f, acctPath("/workers/scripts/"+workerName+"/subdomain"), &sub); !sub.Enabled {
			t.Errorf("workers.dev not re-enabled by triggers deploy with workers_dev: true")
		}
	})
	// A second deploy of an existing Worker goes through the versions API: POST …/versions,
	// POST …/deployments at 100%, PATCH …/script-settings.
	t.Run("redeploy", func(t *testing.T) {
		mark := f.Mark()
		w.ok(t, "deploy")
		var used []string
		for _, r := range f.RequestsSince(mark) {
			used = append(used, r.Method+" "+strings.TrimPrefix(r.Path, acctPath("/workers/scripts/"+workerName)))
		}
		for _, want := range []string{"POST /versions", "POST /deployments", "PATCH /script-settings"} {
			if !slices.Contains(used, want) {
				t.Errorf("redeploy did not call %s (calls: %v)", want, used)
			}
		}
		var vs []struct{ ID string }
		jsonOut(t, w.ok(t, "versions", "list", "--json"), &vs)
		var ds []struct {
			Versions []struct {
				VersionID string `json:"version_id"`
			}
		}
		jsonOut(t, w.ok(t, "deployments", "list", "--json"), &ds)
		if len(vs) != 2 || len(ds) != 2 {
			t.Fatalf("after redeploy: %d versions, %d deployments, want 2 and 2", len(vs), len(ds))
		}
		newID := vs[0].ID // the version that is not the first deploy's (list order is not asserted)
		if newID == versionID {
			newID = vs[1].ID
		}
		found := false
		for _, d := range ds {
			found = found || len(d.Versions) == 1 && d.Versions[0].VersionID == newID
		}
		if !found {
			t.Errorf("no deployment of the new version %s: %+v", newID, ds)
		}
		var settings struct {
			Bindings []map[string]any `json:"bindings"`
		}
		if get(t, f, acctPath("/workers/scripts/"+workerName+"/settings"), &settings); len(settings.Bindings) != 3 {
			t.Errorf("bindings after redeploy %+v, want KV, Q and DB", settings.Bindings)
		}
	})
	t.Run("delete", func(t *testing.T) {
		w.ok(t, "delete", "--force")
		if st := get(t, f, acctPath("/workers/scripts/"+workerName+"/settings"), nil); st != http.StatusNotFound {
			t.Errorf("script still there after wrangler delete: GET settings %d", st)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		w.ok(t, "queues", "delete", "flare-diff-q")
		w.ok(t, "d1", "delete", "flare-diff-d1", "-y")
		w.ok(t, "kv", "namespace", "delete", "--namespace-id", kvID, "--skip-confirmation")
		var qs, dbs, nss []any
		get(t, f, acctPath("/queues"), &qs)
		get(t, f, acctPath("/d1/database"), &dbs)
		get(t, f, acctPath("/storage/kv/namespaces"), &nss)
		if len(qs)+len(dbs)+len(nss) != 0 {
			t.Errorf("left over: queues %v, d1 %v, kv %v", qs, dbs, nss)
		}
	})

	// R2 buckets (flarefake's generic profile with the R2Bucket extensions): one bucket in the
	// default jurisdiction, one in the EU; wrangler sends -J as cf-r2-jurisdiction and the storage
	// class update as a bodiless PATCH with cf-r2-storage-class (src/r2/helpers/bucket.ts,
	// bundled in wrangler@4.143.0:wrangler-dist/cli.js#L208065-L208262).
	t.Run("r2/bucket-create", func(t *testing.T) {
		w.ok(t, "r2", "bucket", "create", "flare-diff-r2", "--storage-class", "InfrequentAccess")
		w.ok(t, "r2", "bucket", "create", "flare-diff-r2-eu", "-J", "eu")
		var b struct {
			Name, Jurisdiction string
			StorageClass       string `json:"storage_class"`
		}
		if get(t, f, acctPath("/r2/buckets/flare-diff-r2"), &b) != http.StatusOK || b.StorageClass != "InfrequentAccess" || b.Jurisdiction != "default" {
			t.Errorf("flarefake bucket %+v", b)
		}
		if st := get(t, f, acctPath("/r2/buckets/flare-diff-r2-eu"), nil); st != http.StatusNotFound {
			t.Errorf("the EU bucket is visible without the jurisdiction header: %d", st)
		}
	})
	t.Run("r2/bucket-list", func(t *testing.T) {
		if r := w.ok(t, "r2", "bucket", "list"); !strings.Contains(r.stdout, "flare-diff-r2") || strings.Contains(r.stdout, "flare-diff-r2-eu") {
			t.Errorf("r2 bucket list (default jurisdiction) output:\n%s", r.stdout)
		}
		if r := w.ok(t, "r2", "bucket", "list", "-J", "eu"); !strings.Contains(r.stdout, "flare-diff-r2-eu") {
			t.Errorf("r2 bucket list -J eu output:\n%s", r.stdout)
		}
	})
	t.Run("r2/bucket-update-storage-class", func(t *testing.T) {
		w.ok(t, "r2", "bucket", "update", "storage-class", "flare-diff-r2", "-s", "Standard")
		var b struct {
			StorageClass string `json:"storage_class"`
		}
		if get(t, f, acctPath("/r2/buckets/flare-diff-r2"), &b); b.StorageClass != "Standard" {
			t.Errorf("storage class after update %q", b.StorageClass)
		}
	})
	t.Run("r2/cors", func(t *testing.T) {
		file := filepath.Join(w.proj, "cors.json")
		write(t, file, `{"rules": [{"allowed": {"origins": ["https://example.com"], "methods": ["GET"]}, "maxAgeSeconds": 3600}]}`)
		w.ok(t, "r2", "bucket", "cors", "set", "flare-diff-r2-eu", "-J", "eu", "--file", file, "--force")
		if r := w.ok(t, "r2", "bucket", "cors", "list", "flare-diff-r2-eu", "-J", "eu"); !strings.Contains(r.stdout, "https://example.com") {
			t.Errorf("r2 bucket cors list output:\n%s", r.stdout)
		}
		w.ok(t, "r2", "bucket", "cors", "delete", "flare-diff-r2-eu", "-J", "eu", "--force")
	})
	t.Run("r2/bucket-delete", func(t *testing.T) {
		w.ok(t, "r2", "bucket", "delete", "flare-diff-r2")
		w.ok(t, "r2", "bucket", "delete", "flare-diff-r2-eu", "-J", "eu")
		var res struct {
			Buckets []any
		}
		if get(t, f, acctPath("/r2/buckets"), &res); len(res.Buckets) != 0 {
			t.Errorf("buckets left in the default jurisdiction: %v", res.Buckets)
		}
		if r := w.ok(t, "r2", "bucket", "list", "-J", "eu"); strings.Contains(r.stdout, "flare-diff-r2-eu") {
			t.Errorf("the EU bucket is still listed:\n%s", r.stdout)
		}
	})

	t.Run("spec-and-routes", func(t *testing.T) {
		for _, v := range f.UnexplainedSchemaViolations(knownWranglerSpecViolations...) {
			t.Errorf("wrangler request broke the pinned spec (new discrepancy?): %s", v)
		}
		for _, u := range f.Unanswered() {
			t.Errorf("wrangler called a route flarefake does not emulate (new discrepancy?): %s", u)
		}
	})
}
