//go:build differential

package differential

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhunghan/flare-operator/internal/controller/pagesdeployment"
	"github.com/chenhunghan/flare-operator/test/differential/harness"
)

const pagesProject = "flare-diff-pages"

// knownWranglerPagesSpecViolations are requests wrangler's `pages deploy` makes on every run
// that break the pinned spec, so the live API accepts them (SOURCED, relies): the deployment
// create sends manifest (and the other fields) as plain form fields, while the spec's
// multipart encoding wants the manifest as application/json,
// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/api/pages/deploy.ts#L271-L312 (the bundle
// wrangler@4.143.0:wrangler-dist/cli.js#L299080 is the same code).
var knownWranglerPagesSpecViolations = []string{
	`POST /accounts/` + harness.AccountID + `/pages/projects/` + pagesProject + `/deployments: request body has an error: failed to decode request body: path manifest: not matching content types: header "text/plain", encoding "application/json"`,
}

// TestWranglerPages drives the pinned wrangler's Pages commands against flarefake: project
// create, a Direct Upload `pages deploy` of a directory (upload token, check-missing, upload,
// upsert-hashes, deployment create, status polling), a second deploy that uploads nothing new,
// deployment list and project delete. The hashes and asset content types wrangler computes must
// equal the operator's (pagesdeployment.HashFile, pagesdeployment.ContentType).
func TestWranglerPages(t *testing.T) {
	bin := wranglerBin(t)
	f := harness.Start(t, harness.Options{})
	w := newWrangler(t, f, bin)
	defer harness.WriteCapture(t, f, "wrangler-pages-"+WranglerVersion)
	site := filepath.Join(w.proj, "site")
	files := map[string]string{
		"index.html":     "<h1>flare-diff pages</h1>\n",
		"css/app.css":    "body { color: red }\n",
		".well-known/x":  "dotdir\n",
		"_headers":       "/*\n  X-Flare: diff\n",
		"_redirects":     "/old / 301\n",
		"node_modules/a": "ignored\n",
		// Types beyond the common ones: the stored contentType must be wrangler's (mime 3).
		"js/app.js":       "console.log(1)\n",
		"favicon.ico":     "ico\n",
		"subs/en.vtt":     "WEBVTT\n",
		"feed.jsonld":     "{}\n",
		"LICENSE":         "none\n",
		"data/Report.PDF": "pdf\n",
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(site, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(site, p), body)
	}

	t.Run("project-create", func(t *testing.T) {
		r := w.ok(t, "pages", "project", "create", pagesProject, "--production-branch", "main")
		if !strings.Contains(r.stdout, pagesProject+".pages.dev") {
			t.Errorf("output does not name the subdomain:\n%s", r.stdout)
		}
		var p struct {
			ProductionBranch string `json:"production_branch"`
		}
		if get(t, f, acctPath("/pages/projects/"+pagesProject), &p) != http.StatusOK || p.ProductionBranch != "main" {
			t.Errorf("flarefake project %+v", p)
		}
	})
	t.Run("project-list", func(t *testing.T) {
		if r := w.ok(t, "pages", "project", "list"); !strings.Contains(r.stdout, pagesProject) {
			t.Errorf("project list:\n%s", r.stdout)
		}
	})

	var deploymentID string
	t.Run("deploy", func(t *testing.T) {
		mark := f.Mark()
		r := w.ok(t, "pages", "deploy", "site", "--project-name", pagesProject, "--branch", "main", "--commit-dirty=true")
		if !strings.Contains(r.stdout, "Deployment complete") || strings.Contains(r.stdout, "couldn't ascertain") {
			t.Errorf("deploy output:\n%s\n%s", r.stdout, r.stderr)
		}
		var used []string
		for _, q := range f.RequestsSince(mark) {
			used = append(used, q.Method+" "+q.Path)
		}
		for _, want := range []string{"GET " + acctPath("/pages/projects/"+pagesProject+"/upload-token"), "POST /pages/assets/check-missing",
			"POST /pages/assets/upload", "POST /pages/assets/upsert-hashes", "POST " + acctPath("/pages/projects/"+pagesProject+"/deployments")} {
			found := false
			for _, u := range used {
				found = found || u == want
			}
			if !found {
				t.Errorf("deploy did not call %s (calls: %v)", want, used)
			}
		}
		var ds []struct {
			ID          string `json:"id"`
			Environment string `json:"environment"`
		}
		if get(t, f, acctPath("/pages/projects/"+pagesProject+"/deployments"), &ds); len(ds) != 1 || ds[0].Environment != "production" {
			t.Fatalf("deployments %+v", ds)
		}
		deploymentID = ds[0].ID
		manifest, parts, ok := f.Server.PagesDeploymentFiles(harness.AccountID, pagesProject, deploymentID)
		if !ok {
			t.Fatal("no deployment in flarefake")
		}
		want := map[string]string{}
		for p, body := range files {
			if !strings.HasPrefix(p, "_") && !strings.HasPrefix(p, "node_modules/") {
				want["/"+p] = body
			}
		}
		if len(manifest) != len(want) {
			t.Errorf("manifest %v, want the paths of %v (wrangler ignores node_modules and the routing files)", manifest, want)
		}
		for p, body := range want {
			// wrangler's hash (blake3-wasm) equals the operator's (lukechampine.com/blake3).
			if got, mine := manifest[p], pagesdeployment.HashFile(strings.TrimPrefix(p, "/"), []byte(body)); got != mine {
				t.Errorf("%s: wrangler hashed %q, the operator hashes %q", p, got, mine)
			}
			b, ct, ok := f.Server.PagesAsset(harness.AccountID, pagesProject, manifest[p])
			if !ok || string(b) != body {
				t.Errorf("%s: stored asset %q, want %q", p, b, body)
			}
			if mine := pagesdeployment.ContentType(strings.TrimPrefix(p, "/")); ct != mine {
				t.Errorf("%s: wrangler sent contentType %q, the operator sends %q", p, ct, mine)
			}
		}
		if string(parts["_headers"]) != files["_headers"] || string(parts["_redirects"]) != files["_redirects"] {
			t.Errorf("routing files %v", parts)
		}
	})
	t.Run("redeploy-uploads-nothing-new", func(t *testing.T) {
		mark := f.Mark()
		w.ok(t, "pages", "deploy", "site", "--project-name", pagesProject, "--branch", "main", "--commit-dirty=true")
		for _, q := range f.RequestsSince(mark) {
			if q.Method == http.MethodPost && q.Path == "/pages/assets/upload" {
				t.Errorf("the second deploy uploaded again: %s", q.Body)
			}
		}
	})
	t.Run("deployment-list", func(t *testing.T) {
		r := w.ok(t, "pages", "deployment", "list", "--project-name", pagesProject)
		if !strings.Contains(r.stdout, deploymentID) {
			t.Errorf("deployment list does not name %s:\n%s", deploymentID, r.stdout)
		}
	})
	t.Run("preview-deploy-and-delete", func(t *testing.T) {
		w.ok(t, "pages", "deploy", "site", "--project-name", pagesProject, "--branch", "feature-x", "--commit-dirty=true")
		var ds []struct {
			ID          string   `json:"id"`
			Environment string   `json:"environment"`
			Aliases     []string `json:"aliases"`
		}
		get(t, f, acctPath("/pages/projects/"+pagesProject+"/deployments?env=preview"), &ds)
		if len(ds) != 1 || len(ds[0].Aliases) != 1 || ds[0].Aliases[0] != "https://feature-x."+pagesProject+".pages.dev" {
			t.Fatalf("preview deployments %+v", ds)
		}
		// Without --force wrangler asks for a confirmation, which a non-interactive run declines
		// (packages/wrangler/src/pages/deployments.ts#L177-L186): nothing is sent. With --force
		// it sends DELETE ?force=true, which also removes the branch alias.
		mark := f.Mark()
		w.run(t, "pages", "deployment", "delete", ds[0].ID, "--project-name", pagesProject)
		for _, q := range f.RequestsSince(mark) {
			if q.Method == http.MethodDelete {
				t.Errorf("a declined confirmation still deleted: %s %s", q.Method, q.Path)
			}
		}
		w.ok(t, "pages", "deployment", "delete", ds[0].ID, "--project-name", pagesProject, "--force")
		if st := get(t, f, acctPath("/pages/projects/"+pagesProject+"/deployments/"+ds[0].ID), nil); st != http.StatusNotFound {
			t.Errorf("preview deployment after delete --force: %d", st)
		}
	})
	t.Run("project-delete", func(t *testing.T) {
		w.ok(t, "pages", "project", "delete", pagesProject, "--yes")
		if st := get(t, f, acctPath("/pages/projects/"+pagesProject), nil); st != http.StatusNotFound {
			t.Errorf("project after delete: %d", st)
		}
	})
	t.Run("spec-and-routes", func(t *testing.T) {
		for _, v := range f.UnexplainedSchemaViolations(knownWranglerPagesSpecViolations...) {
			t.Errorf("wrangler request broke the pinned spec (new discrepancy?): %s", v)
		}
		for _, u := range f.Unanswered() {
			t.Errorf("wrangler called a route flarefake does not emulate (new discrepancy?): %s", u)
		}
	})
}
