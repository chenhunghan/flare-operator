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

// wranglerSrc prefixes evidence citations into wrangler's source at the pinned release
// (tag wrangler@4.143.0 = commit 3bdcd0d46102289e7ef417c4fb3086ea77a18fb1). wranglerDist cites
// the published bundle where the code's source file was not located.
const (
	wranglerSrc  = "cloudflare/workers-sdk@3bdcd0d:"
	wranglerDist = "wrangler@4.143.0:wrangler-dist/cli.js"
	wranglerID   = "wrangler@" + WranglerVersion
)

// Deterministic IDs (queued in flarefake before each create), so the Worker's config can name
// them before they exist.
const (
	kvID = "0f2ac74b498b48028cb68387c421e279"
	d1ID = "8b1f2e4c-3a5d-4e6f-9a7b-0c1d2e3f4a5b"
)

const workerName = "flare-diff-worker"

// Known wrangler discrepancies. Each has a Check in TestWrangler; the ones marked "shimmed"
// also have a shim (wrangler_shims_test.go) so the deploy scenario can run past them.
var (
	dWrServiceGet = harness.Discrepancy{
		ID: "WR-SERVICE-GET", Client: wranglerID,
		Summary: "GET /accounts/{a}/workers/services/{name} is not emulated (404/7000, and absent from the pinned spec). " +
			"wrangler deploy needs 404 with code 10007 (or 10090) for a new Worker and default_environment.script.tag for an existing one; with 7000 it aborts before uploading. (shimmed)",
		Evidence: wranglerDist + "#L174943 (deploy: serviceMetaData.default_environment.script); " +
			wranglerSrc + "packages/deploy-helpers/src/deploy/helpers/worker-not-found-error.ts#L4,L9",
	}
	dWrSecrets = harness.Discrepancy{
		ID: "WR-SECRETS-LIST", Client: wranglerID,
		Summary:  "GET /accounts/{a}/workers/scripts/{name}/secrets is not emulated (404/7000). wrangler deploy lists secrets whenever the config has bindings or vars and only tolerates 404/10007. (shimmed)",
		Evidence: wranglerSrc + "packages/deploy-helpers/src/deploy/helpers/check-remote-secrets-override.ts#L10,L35-L42",
	}
	dWrWorkerGet = harness.Discrepancy{
		ID: "WR-WORKER-GET", Client: wranglerID,
		Summary:  "GET /accounts/{a}/workers/workers/{name} (Workers resource API) is not emulated (404/7000). wrangler reads subdomain.enabled/previews_enabled from it after every upload with workers_dev. (shimmed)",
		Evidence: wranglerSrc + "packages/deploy-helpers/src/triggers/subdomain.ts#L159-L173",
	}
	dWrServiceDelete = harness.Discrepancy{
		ID: "WR-SERVICE-DELETE", Client: wranglerID,
		Summary:  "DELETE /accounts/{a}/workers/services/{name}?force=… is not emulated (404/7000); `wrangler delete` uses it instead of DELETE …/workers/scripts/{name}. (shimmed)",
		Evidence: wranglerSrc + "packages/wrangler/src/delete.ts#L154-L159",
	}
	dWrRedeploy = harness.Discrepancy{
		ID: "WR-REDEPLOY-VERSIONS", Client: wranglerID,
		Summary: "Redeploying an existing Worker uses the versions API: POST …/scripts/{name}/versions (405/10405 in flarefake), then POST …/deployments and PATCH …/script-settings (neither emulated). A second `wrangler deploy` fails.",
		Evidence: wranglerSrc + "packages/deploy-helpers/src/deploy/deploy.ts#L423-L432,L517-L525; " +
			wranglerSrc + "packages/deploy-helpers/src/deploy/helpers/versions-api.ts#L145,L180",
	}
	dWrVersionGet = harness.Discrepancy{
		ID: "WR-VERSION-GET", Client: wranglerID,
		Summary:  "GET /accounts/{a}/workers/scripts/{name}/versions/{id} is not emulated (404/7000); `wrangler versions view` fails.",
		Evidence: wranglerSrc + "packages/deploy-helpers/src/deploy/helpers/versions-api.ts#L27-L31",
	}
	dWrQueueListCounts = harness.Discrepancy{
		ID: "WR-QUEUE-LIST-COUNTS", Client: wranglerID,
		Summary:  "Queue list items lack producers_total_count/consumers_total_count (flarefake adds them only to GET; the list item shape is UNVERIFIED, 0148 is an empty list). `wrangler queues list` crashes: Cannot read properties of undefined (reading 'toString').",
		Evidence: wranglerSrc + "packages/wrangler/src/queues/cli/commands/list.ts#L45-L46,L56-L57",
	}
	dWrQueueCreateStrict = harness.Discrepancy{
		ID: "WR-QUEUE-CREATE-CONTENT-TYPE", Client: wranglerID,
		Summary:  "wrangler sends the queue-create JSON body without a Content-Type header, so undici sends text/plain;charset=UTF-8. flarefake's spec validation flags it, and with -reject-schema-violations it answers 400/10001, so wrangler fails against strict mode (the live API accepts it: wrangler works in production).",
		Evidence: wranglerSrc + "packages/wrangler/src/queues/client.ts#L55-L64",
	}
	dWrKVValues = harness.Discrepancy{
		ID: "WR-KV-VALUES", Client: wranglerID,
		Summary:  "KV values (PUT/GET/DELETE …/storage/kv/namespaces/{id}/values/{key}) are not emulated (404/7000); `wrangler kv key put/get` fail.",
		Evidence: wranglerSrc + "packages/wrangler/src/kv/helpers.ts#L247-L252",
	}
	dWrD1Execute = harness.Discrepancy{
		ID: "WR-D1-EXECUTE", Client: wranglerID,
		Summary:  "POST /accounts/{a}/d1/database/{id}/query answers 501/99999 (D1 SQL execution is not emulated); `wrangler d1 execute --remote` fails.",
		Evidence: wranglerSrc + "packages/wrangler/src/d1/execute.ts#L630-L640",
	}
)

// knownWranglerSpecViolations are the requests wrangler makes that break the pinned spec,
// each covered by a discrepancy above (harness.KnownSpecDefects covers the rest).
var knownWranglerSpecViolations = []string{
	// WR-QUEUE-CREATE-CONTENT-TYPE
	`POST /accounts/` + harness.AccountID + `/queues: request body has an error: header Content-Type has unexpected value "text/plain;charset=UTF-8"`,
	// WR-KV-VALUES: wrangler sends a value as text/plain; the spec allows only multipart/form-data
	// and application/octet-stream, so an emulated values route would also need this relaxed.
	`PUT /accounts/` + harness.AccountID + `/storage/kv/namespaces/` + kvID + `/values/k1: request body has an error: header Content-Type has unexpected value "text/plain;charset=UTF-8"`,
	// WR-SERVICE-GET, WR-SERVICE-DELETE
	`GET /accounts/` + harness.AccountID + `/workers/services/` + workerName + `: no such operation in pinned spec: no matching operation was found`,
	`DELETE /accounts/` + harness.AccountID + `/workers/services/` + workerName + `: no such operation in pinned spec: no matching operation was found`,
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

// TestWrangler drives the pinned wrangler through KV, Queues, D1 and a Worker deploy with
// bindings to all three against flarefake. Each command's requests are logged; with
// FLARE_DIFF_CAPTURE_DIR set, all of them are written to wrangler-<version>.json there.
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
	dWrKVValues.Check(t, func() error {
		if r := w.run(t, "kv", "key", "put", "--namespace-id", kvID, "--remote", "k1", "v1"); r.err != nil {
			return r.failed()
		}
		r := w.run(t, "kv", "key", "get", "--namespace-id", kvID, "--remote", "k1")
		if r.err == nil && strings.TrimSpace(r.stdout) != "v1" {
			return fmt.Errorf("kv key get printed %q, want v1", r.stdout)
		}
		return r.failed()
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
	dWrQueueListCounts.Check(t, func() error { return w.run(t, "queues", "list").failed() })
	dWrQueueCreateStrict.Check(t, func() error {
		strict := harness.Start(t, harness.Options{RejectSchemaViolations: true})
		return newWrangler(t, strict, bin).run(t, "queues", "create", "flare-diff-strict-q").failed()
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
	dWrD1Execute.Check(t, func() error {
		return w.run(t, "d1", "execute", "flare-diff-d1", "--remote", "--command", "select 1 as one", "--json").failed()
	})

	// The deploy flow needs routes flarefake does not emulate. Check the first two directly
	// (the others need a deployed Worker), then register all shims so the rest of the flow
	// still runs against flarefake.
	dWrServiceGet.Check(t, func() error {
		status, env := f.Call(http.MethodGet, acctPath("/workers/services/"+workerName), nil)
		if status != http.StatusNotFound || len(env.Errors) == 0 || (env.Errors[0].Code != 10007 && env.Errors[0].Code != 10090) {
			return fmt.Errorf("GET …/workers/services/%s for a missing Worker: %d %+v, want 404 code 10007", workerName, status, env.Errors)
		}
		return nil
	})
	dWrSecrets.Check(t, func() error {
		status, env := f.Call(http.MethodGet, acctPath("/workers/scripts/"+workerName+"/secrets"), nil)
		if status != http.StatusNotFound || len(env.Errors) == 0 || env.Errors[0].Code != 10007 {
			return fmt.Errorf("GET …/scripts/%s/secrets for a missing Worker: %d %+v, want 404 code 10007", workerName, status, env.Errors)
		}
		return nil
	})
	f.AddShim(shimServiceGet(f, dWrServiceGet.ID))
	f.AddShim(shimScriptSub(f, dWrSecrets.ID, http.MethodGet, "secrets", []any{}))
	f.AddShim(shimWorkerGet(f, dWrWorkerGet.ID))
	f.AddShim(shimServiceDelete(dWrServiceDelete.ID))

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
		var sub struct{ Enabled bool }
		if get(t, f, acctPath("/workers/scripts/"+workerName+"/subdomain"), &sub); !sub.Enabled {
			t.Errorf("workers.dev not enabled after deploy with workers_dev: true")
		}
	})
	dWrWorkerGet.Check(t, func() error {
		if status, _ := f.Call(http.MethodGet, acctPath("/workers/workers/"+workerName), nil); status != http.StatusOK {
			return fmt.Errorf("GET …/workers/workers/%s for a deployed Worker: %d, want 200", workerName, status)
		}
		return nil
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
	dWrVersionGet.Check(t, func() error { return w.run(t, "versions", "view", versionID, "--json").failed() })
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
	dWrRedeploy.Check(t, func() error { return w.run(t, "deploy").failed() })
	dWrServiceDelete.Check(t, func() error {
		status, env := f.Call(http.MethodDelete, acctPath("/workers/services/"+workerName+"?force=true"), nil)
		if status != http.StatusOK {
			return fmt.Errorf("DELETE …/workers/services/%s: %d %+v, want 200", workerName, status, env.Errors)
		}
		return nil
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

	t.Run("spec-and-routes", func(t *testing.T) {
		for _, v := range f.UnexplainedSchemaViolations(knownWranglerSpecViolations...) {
			t.Errorf("wrangler request broke the pinned spec (new discrepancy?): %s", v)
		}
		known := []string{ // routes of the discrepancies above
			"/storage/kv/namespaces/" + kvID + "/values/k1",
			"/workers/scripts/" + workerName + "/versions",
			"/workers/scripts/" + workerName + "/versions/" + versionID,
		}
		for _, u := range f.Unanswered() {
			if !slices.ContainsFunc(known, func(k string) bool { return strings.HasSuffix(u, acctPath(k)) }) {
				t.Errorf("wrangler called a route flarefake does not emulate (new discrepancy?): %s", u)
			}
		}
	})
}
