package workerscript

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/artifact"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/fake"
)

// TestAssetHashMatchesWrangler: golden hashes computed with the blake3-wasm of the pinned
// wrangler@4.143.0 exactly as its hash.ts does (see internal/fake TestAssetHashMatchesWrangler),
// and the same as flarefake's independent implementation for other inputs.
func TestAssetHashMatchesWrangler(t *testing.T) {
	for _, c := range []struct {
		content []byte
		path    string
		want    string
	}{
		{[]byte("<h1>hello</h1>\n"), "index.html", "f17fd9f85e7289b774b95ae8215bee61"},
		{[]byte(""), "empty", "af1349b9f5f9a1a6a0404dea36dcc949"},
		{[]byte{0, 1, 2, 255}, "x/y.bin", "2172bd7be4ca4461cd74312a85d2a041"},
	} {
		if got := assetHash(c.content, c.path); got != c.want {
			t.Errorf("assetHash(%q, %s) = %s, want %s", c.content, c.path, got, c.want)
		}
	}
	for _, p := range []string{"a.js", ".bashrc", "..foo", "a.", "a.tar.gz", "dir.x/file", "x/.hidden.txt", "...", "a..b"} {
		content := []byte("content of " + p)
		if got, want := assetHash(content, p), fake.AssetHash(base64.StdEncoding.EncodeToString(content), "/"+p); got != want {
			t.Errorf("%s: operator %s, flarefake %s", p, got, want)
		}
	}
}

// TestAssetContentTypeMatchesWrangler: the want values are wrangler's getContentType of each path,
// computed by running the mime@4.0.7 module bundled in the pinned wrangler@4.143.0 under node.
func TestAssetContentTypeMatchesWrangler(t *testing.T) {
	for p, want := range map[string]string{
		"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8",
		"x.mjs": "text/javascript; charset=utf-8", "site.CSS": "text/css; charset=utf-8",
		"favicon.ico": "image/vnd.microsoft.icon", "data.yaml": "text/yaml; charset=utf-8", "c.yml": "text/yaml; charset=utf-8",
		"blob.bin": "application/octet-stream", "a.exe": "application/octet-stream", "m.ts": "video/mp2t",
		"App.jsx": "text/jsx; charset=utf-8", "p.xhtml": "application/xhtml+xml", "subs.vtt": "text/vtt; charset=utf-8",
		"f.woff2": "font/woff2", "m.wasm": "application/wasm", "s.svg": "image/svg+xml", "d.json": "application/json",
		"x.map": "application/json", "README": "application/null", "Makefile": "application/null", ".bashrc": "application/null",
		"x/.hidden.txt": "text/plain; charset=utf-8", "..foo": "application/null", "a.": "application/null", "...": "application/null",
		"a.tar.gz": "application/gzip", "dir.x/file": "application/null", "data.unknownext": "application/null",
		"manifest.webmanifest": "application/manifest+json",
	} {
		if got := assetContentType(p); got != want {
			t.Errorf("assetContentType(%q) = %q, want %q", p, got, want)
		}
	}
}

func files(m map[string]string) []artifact.File {
	var out []artifact.File
	for p, c := range m {
		out = append(out, artifact.File{Path: p, Content: []byte(c), ContentType: artifact.ContentType(p)})
	}
	// Tree order (by path), as Tree.Files returns them.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Path < out[i].Path {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func manifestPaths(m *assetManifest) []string {
	var out []string
	for _, f := range m.files {
		out = append(out, f.path)
	}
	return out
}

func TestBuildAssetManifest(t *testing.T) {
	m, p := buildAssetManifest(files(map[string]string{
		"index.html": "<h1>home</h1>", "css/site.css": "body{}", "_headers": "/*\n  X-A: 1\n", "_redirects": "/a /b 301\n",
		".assetsignore": "# comment\nsecret.txt\n*.map\n/drafts/\n!keep.map\n", "secret.txt": "no", "js/app.js.map": "{}",
		"keep.map": "{}", "drafts/post.html": "wip", "sub/drafts/ok.html": "published", "sub/_headers": "not a metafile here",
		"data.unknownext": "?",
	}))
	if p != nil {
		t.Fatal(p.msg)
	}
	want := "/css/site.css /data.unknownext /index.html /keep.map /sub/_headers /sub/drafts/ok.html"
	if got := strings.Join(manifestPaths(m), " "); got != want {
		t.Errorf("manifest %s\nwant     %s", got, want)
	}
	if m.headers != "/*\n  X-A: 1\n" || m.redirects != "/a /b 301\n" {
		t.Errorf("headers %q redirects %q", m.headers, m.redirects)
	}
	for _, f := range m.files {
		switch f.path {
		case "/index.html":
			if f.contentType != "text/html; charset=utf-8" || f.size != 13 || f.hash != assetHash([]byte("<h1>home</h1>"), "index.html") {
				t.Errorf("index.html entry %+v", f)
			}
		case "/data.unknownext":
			if f.contentType != assetNullContentType {
				t.Errorf("unknown extension served as %q, want %s", f.contentType, assetNullContentType)
			}
		}
	}

	// _worker.js without .assetsignore would publish server code (wrangler refuses it too).
	for _, p := range []string{"_worker.js", "_worker.js/index.js"} {
		if _, prob := buildAssetManifest(files(map[string]string{"index.html": "x", p: "export default {}"})); prob == nil || prob.reason != ReasonInvalidSpec {
			t.Errorf("%s without .assetsignore accepted", p)
		}
	}
	if m, prob := buildAssetManifest(files(map[string]string{"index.html": "x", "_worker.js": "y", ".assetsignore": "_worker.js\n"})); prob != nil || len(m.files) != 1 {
		t.Errorf("_worker.js listed in .assetsignore: %v %v", prob, m)
	}
	if _, prob := buildAssetManifest([]artifact.File{{Path: "big.bin", Content: make([]byte, maxAssetBytes+1)}}); prob == nil {
		t.Error("a file over 25 MiB accepted")
	}

	// .assetsignore and the root metafile patterns match case-insensitively, as wrangler's.
	m, p = buildAssetManifest(files(map[string]string{".assetsignore": "secret.txt\n*.draft\n", "index.html": "x",
		"Secret.TXT": "password", "a/SECRET.txt": "password", "x.DRAFT": "wip", "_HEADERS": "/*\n  X-A: 1\n"}))
	if p != nil {
		t.Fatal(p.msg)
	}
	if got := strings.Join(manifestPaths(m), " "); got != "/index.html" {
		t.Errorf("case-insensitive ignore: manifest %s, want /index.html", got)
	}
}

func TestIgnoreMatcher(t *testing.T) {
	m := newIgnoreMatcher([]string{"/top.txt", "*.log", "build/", "docs/**/*.pdf", "!important.log", "a?c", `\#hash`, "trailing  ", "dir/", "!dir/keep"})
	for p, want := range map[string]bool{
		"top.txt": true, "sub/top.txt": false, "x.log": true, "deep/x/y.log": true, "important.log": false,
		"build/out.js": true, "src/build/out.js": true, "build": false, "docs/a.pdf": true, "docs/x/y/a.pdf": true, "a.pdf": false,
		"abc": true, "abbc": false, "#hash": true, "trailing": true, "dir/keep": true, "index.html": false,
		// Case-insensitive, as wrangler's ignore (ignorecase defaults to true).
		"TOP.txt": true, "X.LOG": true, "Build/out.js": true, "DOCS/A.PDF": true, "ABC": true, "Important.LOG": false,
	} {
		if got := m.ignored(p); got != want {
			t.Errorf("ignored(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestAssetsFlowAgainstSpec runs the assets upload flow (several buckets), the script upload
// that redeems its token, a keep_assets upload and an assets-only upload against flarefake with
// strict pinned-spec request validation.
func TestAssetsFlowAgainstSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec for request validation")
	}
	spec, err := fake.LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := fake.New(fake.Options{Spec: spec, RejectSchemaViolations: true})
	s.SetAssetBuckets(1, 0)
	hs := httptest.NewServer(s)
	defer hs.Close()
	cf, err := cfclient.New(cfclient.Options{Token: "t-assets", BaseURL: hs.URL + "/client/v4", RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const acct = "0123456789abcdef0123456789abcdef"
	m, p := buildAssetManifest(files(map[string]string{"index.html": "<h1>x</h1>", "a.css": "a{}", "b.js": "1", "_redirects": "/x /y 302\n"}))
	if p != nil {
		t.Fatal(p.msg)
	}
	yes := true
	plan := newAssetPlan(m, &workersv1alpha1.WorkerAssetsConfig{NotFoundHandling: "404-page", RunWorkerFirst: &yes})
	jwt, buckets, err := uploadAssets(ctx, cf, acct, "w", plan)
	if err != nil || buckets != 3 || jwt == "" {
		t.Fatalf("uploadAssets: %q %d %v, want a token after 3 buckets", jwt, buckets, err)
	}
	mods := []module{{Name: "index.js", Type: workersv1alpha1.ModuleESM, Content: []byte("export default { async fetch(r, env) { return env.ASSETS.fetch(r) } };\n")}}
	md := apiUploadMetadata{MainModule: "index.js", apiSettingsBody: apiSettingsBody{CompatibilityDate: "2026-09-01",
		Bindings: []map[string]any{{"type": "assets", "name": "ASSETS"}}}, Assets: &apiAssets{JWT: jwt, Config: &plan.config}}
	if up, err := uploadScript(ctx, cf, acct, "w", md, mods); err != nil || !up.HasAssets {
		t.Fatalf("upload: %+v %v", up, err)
	}
	got, cfg, has := s.WorkerAssets(acct, "w")
	if !has || len(got) != 3 || cfg["not_found_handling"] != "404-page" || cfg["run_worker_first"] != true || cfg["_redirects"] != "/x /y 302\n" {
		t.Fatalf("stored assets %v %v %v", got, cfg, has)
	}
	// Unchanged: the session needs no bucket.
	if _, buckets, err := uploadAssets(ctx, cf, acct, "w", plan); err != nil || buckets != 0 {
		t.Fatalf("second session: %d buckets, %v", buckets, err)
	}
	md.Assets, md.KeepAssets = nil, &yes
	if up, err := uploadScript(ctx, cf, acct, "w", md, mods); err != nil || !up.HasAssets {
		t.Fatalf("keep_assets upload: %+v %v", up, err)
	}
	// Assets-only.
	jwt, _, err = uploadAssets(ctx, cf, acct, "site", plan)
	if err != nil {
		t.Fatal(err)
	}
	site := newAssetPlan(m, nil)
	if up, err := uploadScript(ctx, cf, acct, "site", apiUploadMetadata{Assets: &apiAssets{JWT: jwt, Config: &site.config}}, nil); err != nil || !up.HasAssets {
		t.Fatalf("assets-only upload: %+v %v", up, err)
	}
	for _, j := range s.Journal() {
		if j.SchemaViolation != "" {
			t.Errorf("%s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}

// FuzzIgnoreMatcher: any .assetsignore content and path is handled without a panic, and the
// root metafiles stay ignored unless a pattern negates.
func FuzzIgnoreMatcher(f *testing.F) {
	for _, s := range []string{"*.map\n!keep.map\n", "[z-a]\n[[:alpha:]\n\\", "**/\n/**\n**", "a/**/b\n#x\n\\#y  \n"} {
		f.Add(s, "a/b/c.map")
	}
	f.Fuzz(func(t *testing.T, ignore, p string) {
		m := newIgnoreMatcher(append([]string{"/" + assetsIgnoreFile, "/" + assetsHeadersFile}, strings.Split(ignore, "\n")...))
		_ = m.ignored(p)
		// (A negated pattern may re-include them, as in wrangler's ignore package.)
		if !strings.Contains(ignore, "!") && (!m.ignored(assetsIgnoreFile) || !m.ignored(assetsHeadersFile)) {
			t.Fatalf("a root metafile is not ignored with %q", ignore)
		}
	})
}

func TestAssetPlanHash(t *testing.T) {
	m, _ := buildAssetManifest(files(map[string]string{"index.html": "x"}))
	a := newAssetPlan(m, nil)
	b := newAssetPlan(m, &workersv1alpha1.WorkerAssetsConfig{HTMLHandling: "none"})
	m2, _ := buildAssetManifest(files(map[string]string{"index.html": "y"}))
	c := newAssetPlan(m2, nil)
	if a.hash == b.hash || a.hash == c.hash || a.hash != newAssetPlan(m, nil).hash {
		t.Errorf("hashes: %s %s %s", a.hash, b.hash, c.hash)
	}
}
