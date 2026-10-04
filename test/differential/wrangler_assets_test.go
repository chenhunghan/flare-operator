//go:build differential

package differential

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhunghan/flare-operator/test/differential/harness"
)

// Static assets: `wrangler deploy` of a Worker with an assets directory and of an assets-only
// site, against flarefake's assets upload flow (internal/fake/workers_assets.go). wrangler's
// side: syncAssets and buildAssetManifest,
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/assets.ts#L72-L436.

const (
	assetsWorker = "flare-diff-assets"
	assetsSite   = "flare-diff-site"
)

// assetsProject writes a Worker with a public/ directory (with the metafiles wrangler keeps out
// of the manifest) and an assets-only site/ directory, and their configs.
func assetsProject(t *testing.T, w *wrangler) {
	t.Helper()
	files := map[string]string{
		"src/api.js": `export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname.startsWith("/api/")) return Response.json({ ok: true });
    return env.ASSETS.fetch(request);
  },
};
`,
		"public/index.html":       "<h1>flare-diff</h1>\n",
		"public/about/index.html": "<h1>about</h1>\n",
		"public/css/site.css":     "body { margin: 0 }\n",
		"public/404.html":         "<h1>not found</h1>\n",
		"public/_headers":         "/*\n  X-Flare-Diff: 1\n",
		"public/_redirects":       "/old /about/ 301\n",
		"public/.assetsignore":    "secret.txt\nnotes.md\n",
		"public/secret.txt":       "not served\n",
		// wrangler matches .assetsignore case-insensitively (ignore@5.3.1 defaults to ignorecase).
		"public/docs/NOTES.MD": "not served either\n",
		"site/index.html":      "<h1>site</h1>\n",
		"site/app.js":          "console.log('spa')\n",
		"wrangler-assets.json": fmt.Sprintf(`{
  "name": %q,
  "main": "src/api.js",
  "compatibility_date": "2026-09-01",
  "workers_dev": true,
  "assets": {
    "directory": "./public",
    "binding": "ASSETS",
    "html_handling": "force-trailing-slash",
    "not_found_handling": "404-page",
    "run_worker_first": ["/api/*"]
  }
}
`, assetsWorker),
		"wrangler-site.json": fmt.Sprintf(`{
  "name": %q,
  "compatibility_date": "2026-09-01",
  "workers_dev": true,
  "assets": {"directory": "./site", "not_found_handling": "single-page-application"}
}
`, assetsSite),
	}
	for p, c := range files {
		full := filepath.Join(w.proj, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, full, c)
	}
}

// assetCalls counts the session and bucket upload requests in reqs.
func assetCalls(reqs []harness.Request, script string) (sessions, uploads int) {
	for _, r := range reqs {
		switch {
		case r.Method == http.MethodPost && r.Path == acctPath("/workers/scripts/"+script+"/assets-upload-session"):
			sessions++
		case r.Method == http.MethodPost && r.Path == acctPath("/workers/assets/upload"):
			uploads++
		}
	}
	return sessions, uploads
}

// checkManifest compares flarefake's stored manifest of script with the files wrangler should
// have sent (want: path → content) and checks every stored hash against the content.
func checkManifest(t *testing.T, f *harness.Fake, script string, want map[string]string) map[string]any {
	t.Helper()
	m, cfg, ok := f.Server.WorkerAssets(harness.AccountID, script)
	if !ok {
		t.Fatalf("flarefake has no assets for %s", script)
	}
	if len(m) != len(want) {
		t.Errorf("manifest %v, want exactly the paths of %v", m, want)
	}
	for p, content := range want {
		h, ok := m[p]
		if !ok {
			t.Errorf("manifest lacks %s: %v", p, m)
			continue
		}
		af, ok := f.Server.UploadedAsset(harness.AccountID, script, h)
		if !ok || string(af.Content) != content {
			t.Errorf("%s (%s): stored %q (%v), want %q", p, h, af.Content, ok, content)
		}
	}
	return cfg
}

func TestWranglerAssets(t *testing.T) {
	bin := wranglerBin(t)
	f := harness.Start(t, harness.Options{})
	w := newWrangler(t, f, bin)
	assetsProject(t, w)
	defer harness.WriteCapture(t, f, "wrangler-assets-"+WranglerVersion)

	served := map[string]string{
		"/index.html": "<h1>flare-diff</h1>\n", "/about/index.html": "<h1>about</h1>\n",
		"/css/site.css": "body { margin: 0 }\n", "/404.html": "<h1>not found</h1>\n",
	}
	t.Run("deploy-with-assets", func(t *testing.T) {
		// Two files per bucket: the four files take two uploads, the first answered 202 {} and
		// the last 201 {jwt} (wrangler keeps the jwt of whichever has one, assets.ts#L318-L342).
		f.Server.SetAssetBuckets(2, 0)
		defer f.Server.SetAssetBuckets(0, 0)
		mark := f.Mark()
		w.ok(t, "deploy", "--config", "wrangler-assets.json")
		if s, u := assetCalls(f.RequestsSince(mark), assetsWorker); s != 1 || u != 2 {
			t.Errorf("first deploy: %d sessions, %d bucket uploads; want 1 and 2", s, u)
		}
		cfg := checkManifest(t, f, assetsWorker, served)
		for k, want := range map[string]any{"html_handling": "force-trailing-slash", "not_found_handling": "404-page",
			"_headers": "/*\n  X-Flare-Diff: 1\n", "_redirects": "/old /about/ 301\n"} {
			if cfg[k] != want {
				t.Errorf("assets config %s = %v, want %q (config %v)", k, cfg[k], want, cfg)
			}
		}
		if rwf, _ := cfg["run_worker_first"].([]any); len(rwf) != 1 || rwf[0] != "/api/*" {
			t.Errorf("run_worker_first %v, want [/api/*]", cfg["run_worker_first"])
		}
		var settings struct {
			Bindings []map[string]any `json:"bindings"`
		}
		get(t, f, acctPath("/workers/scripts/"+assetsWorker+"/settings"), &settings)
		if len(settings.Bindings) != 1 || settings.Bindings[0]["type"] != "assets" || settings.Bindings[0]["name"] != "ASSETS" {
			t.Errorf("bindings %v, want the ASSETS assets binding", settings.Bindings)
		}
	})
	t.Run("redeploy-unchanged", func(t *testing.T) {
		mark := f.Mark()
		w.ok(t, "deploy", "--config", "wrangler-assets.json")
		// wrangler always opens a session; with nothing new the session's jwt is the completion
		// token and nothing is uploaded (assets.ts#L114-L134).
		if s, u := assetCalls(f.RequestsSince(mark), assetsWorker); s != 1 || u != 0 {
			t.Errorf("unchanged redeploy: %d sessions, %d bucket uploads; want 1 and 0", s, u)
		}
		checkManifest(t, f, assetsWorker, served)
	})
	t.Run("redeploy-one-changed", func(t *testing.T) {
		write(t, filepath.Join(w.proj, "public", "css", "site.css"), "body { margin: 1px }\n")
		mark := f.Mark()
		w.ok(t, "deploy", "--config", "wrangler-assets.json")
		reqs := f.RequestsSince(mark)
		if s, u := assetCalls(reqs, assetsWorker); s != 1 || u != 1 {
			t.Errorf("one changed file: %d sessions, %d bucket uploads; want 1 and 1", s, u)
		}
		for _, r := range reqs {
			if r.Path == acctPath("/workers/assets/upload") && strings.Count(r.Body, "Content-Disposition") != 1 {
				t.Errorf("the bucket upload carries %d parts, want 1", strings.Count(r.Body, "Content-Disposition"))
			}
		}
		served["/css/site.css"] = "body { margin: 1px }\n"
		checkManifest(t, f, assetsWorker, served)
	})
	t.Run("deploy-assets-only", func(t *testing.T) {
		mark := f.Mark()
		w.ok(t, "deploy", "--config", "wrangler-site.json")
		if s, u := assetCalls(f.RequestsSince(mark), assetsSite); s != 1 || u != 1 {
			t.Errorf("assets-only deploy: %d sessions, %d bucket uploads; want 1 and 1", s, u)
		}
		cfg := checkManifest(t, f, assetsSite, map[string]string{"/index.html": "<h1>site</h1>\n", "/app.js": "console.log('spa')\n"})
		if cfg["not_found_handling"] != "single-page-application" {
			t.Errorf("assets config %v", cfg)
		}
		mark = f.Mark()
		w.ok(t, "deploy", "--config", "wrangler-site.json")
		if s, u := assetCalls(f.RequestsSince(mark), assetsSite); s != 1 || u != 0 {
			t.Errorf("unchanged assets-only redeploy: %d sessions, %d bucket uploads; want 1 and 0", s, u)
		}
	})
	t.Run("delete", func(t *testing.T) {
		w.ok(t, "delete", "--config", "wrangler-assets.json", "--force")
		w.ok(t, "delete", "--config", "wrangler-site.json", "--force")
		for _, n := range []string{assetsWorker, assetsSite} {
			if st := get(t, f, acctPath("/workers/scripts/"+n+"/settings"), nil); st != http.StatusNotFound {
				t.Errorf("%s still there after wrangler delete: GET settings %d", n, st)
			}
		}
	})
	t.Run("spec-and-routes", func(t *testing.T) {
		known := []string{
			`GET /accounts/` + harness.AccountID + `/workers/services/` + assetsWorker + `: no such operation in pinned spec: no matching operation was found`,
			`DELETE /accounts/` + harness.AccountID + `/workers/services/` + assetsWorker + `: no such operation in pinned spec: no matching operation was found`,
			`GET /accounts/` + harness.AccountID + `/workers/services/` + assetsSite + `: no such operation in pinned spec: no matching operation was found`,
			`DELETE /accounts/` + harness.AccountID + `/workers/services/` + assetsSite + `: no such operation in pinned spec: no matching operation was found`,
		}
		for _, v := range f.UnexplainedSchemaViolations(known...) {
			t.Errorf("wrangler request broke the pinned spec (new discrepancy?): %s", v)
		}
		for _, u := range f.Unanswered() {
			t.Errorf("wrangler called a route flarefake does not emulate (new discrepancy?): %s", u)
		}
	})
}
