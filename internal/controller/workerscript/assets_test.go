package workerscript_test

import (
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/testenv"
)

const apiModule = `export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname.startsWith("/api/")) return Response.json({ ok: true });
    return env.ASSETS.fetch(request);
  }
};
`

// siteConfigMap is a labelled artifact ConfigMap with a small site: the root files, and css/
// through items.
func (h *harness) siteConfigMap(name string, files map[string]string) *corev1.ConfigMap {
	h.t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{sharedv1alpha1.LabelArtifact: "true"}},
		Data: files}
	h.create(cm)
	return cm
}

// siteSource maps the ConfigMap's keys: "css__x" → css/x (ConfigMap keys cannot hold "/").
func siteSource(cm string, keys ...string) sharedv1alpha1.ArtifactSource {
	var items []sharedv1alpha1.ArtifactKeyToPath
	for _, k := range keys {
		items = append(items, sharedv1alpha1.ArtifactKeyToPath{Key: k, Path: strings.ReplaceAll(k, "__", "/")})
	}
	return sharedv1alpha1.ArtifactSource{ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{
		ConfigMaps: []sharedv1alpha1.ConfigMapArtifact{{Name: cm, Items: items}}}}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assetCalls counts the asset session and bucket upload requests of script name in j.
func (h *harness) assetCalls(j []fake.JournalEntry, name string) (sessions, buckets int) {
	return testenv.CountPath(j, http.MethodPost, h.scriptPath(name)+"/assets-upload-session"),
		testenv.CountPath(j, http.MethodPost, "/accounts/"+h.acct.AccountID+"/workers/assets/upload")
}

// assetManifest is flarefake's stored manifest of the deployed version (path → hash).
func (h *harness) assetManifest(name string) map[string]string {
	h.t.Helper()
	m, _, ok := h.e.Fake.WorkerAssets(h.acct.AccountID, name)
	if !ok {
		h.t.Fatalf("flarefake has no assets for %s", name)
	}
	return m
}

func wantHash(content, p string) string {
	return fake.AssetHash(base64.StdEncoding.EncodeToString([]byte(content)), p)
}

// A static site and an API Worker in one script: the first deploy is one session, one bucket
// per two files (the fake's bucket size is set so) and one upload; unchanged assets cost nothing;
// one changed file is one session, one bucket and one upload; changed code alone keeps the
// assets (keep_assets) without an asset call; a changed config opens a session with nothing to
// upload.
func TestStaticSiteWithAPIWorker(t *testing.T) {
	h := start(t)
	site := map[string]string{
		"index.html": "<h1>home</h1>", "404.html": "<h1>404</h1>", "css__site.css": "body{margin:0}",
		"_headers": "/*\n  X-Site: 1\n", ".assetsignore": "*.draft\n", "wip.draft": "not served",
	}
	cm := h.siteConfigMap("site", site)
	fp := params(apiModule)
	fp.Assets = &workersv1alpha1.WorkerAssets{Source: siteSource("site", keysOf(site)...),
		Config: &workersv1alpha1.WorkerAssetsConfig{NotFoundHandling: "404-page", RunWorkerFirstPaths: []string{"/api/*"}}}
	fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "ASSETS", Type: "assets"}}
	h.e.Fake.SetAssetBuckets(2, 0) // this package's tests run one at a time, on their own fake
	defer h.e.Fake.SetAssetBuckets(0, 0)
	m := h.mark()
	h.newScript("web", fp, nil)
	ws := h.waitScript("web", scriptReady)
	j := h.since(m)
	if s, b := h.assetCalls(j, "web"); s != 1 || b != 2 || h.uploads(j, "web") != 1 {
		t.Fatalf("first deploy: %d sessions, %d buckets, %d uploads; want 1, 2, 1:\n%s", s, b, h.uploads(j, "web"), testenv.Summary(j))
	}
	want := map[string]string{"/index.html": wantHash("<h1>home</h1>", "index.html"), "/404.html": wantHash("<h1>404</h1>", "404.html"),
		"/css/site.css": wantHash("body{margin:0}", "css/site.css")}
	if got := h.assetManifest("web"); len(got) != len(want) || got["/index.html"] != want["/index.html"] || got["/css/site.css"] != want["/css/site.css"] {
		t.Fatalf("manifest %v, want %v", got, want)
	}
	_, cfg, _ := h.e.Fake.WorkerAssets(h.acct.AccountID, "web")
	if cfg["not_found_handling"] != "404-page" || cfg["_headers"] != "/*\n  X-Site: 1\n" {
		t.Fatalf("assets config %v", cfg)
	}
	if rwf, _ := cfg["run_worker_first"].([]any); len(rwf) != 1 || rwf[0] != "/api/*" {
		t.Fatalf("run_worker_first %v", cfg["run_worker_first"])
	}
	a := ws.Status.Artifacts
	if ws.Status.AssetsHash == "" || a == nil || a.Assets == nil || a.Assets.Files != 6 || a.AssetFiles != 3 || !ws.Status.AtProvider.HasAssets {
		t.Fatalf("status: assetsHash %q, artifacts %+v, has_assets %v", ws.Status.AssetsHash, a, ws.Status.AtProvider.HasAssets)
	}
	if b := binding(h.settings("web"), "ASSETS"); b["type"] != "assets" {
		t.Fatalf("assets binding %v", b)
	}
	h.assertNoWrites("web")

	step := func(what string, mut func(), wantSessions, wantBuckets int) *workersv1alpha1.WorkerScript {
		t.Helper()
		prev := h.waitScript("web", scriptReady)
		m := h.mark()
		mut()
		ws := h.waitScript("web", func(ws *workersv1alpha1.WorkerScript) bool {
			return scriptReady(ws) && ws.Status.AtProvider.VersionID != prev.Status.AtProvider.VersionID
		})
		h.settle(script("web"))
		j := h.since(m)
		if s, b := h.assetCalls(j, "web"); s != wantSessions || b != wantBuckets || h.uploads(j, "web") != 1 {
			t.Fatalf("%s: %d sessions, %d buckets, %d uploads; want %d, %d, 1:\n%s", what, s, b, h.uploads(j, "web"), wantSessions, wantBuckets, testenv.Summary(j))
		}
		return ws
	}
	ws2 := step("one changed file", func() {
		cm.Data["css__site.css"] = "body{margin:1px}"
		if err := h.e.Client.Update(h.ctx(), cm); err != nil {
			t.Fatal(err)
		}
	}, 1, 1)
	newHash := wantHash("body{margin:1px}", "css/site.css")
	if got := h.assetManifest("web"); got["/css/site.css"] != newHash || got["/index.html"] != want["/index.html"] {
		t.Fatalf("manifest after the change %v", got)
	}
	if af, ok := h.e.Fake.UploadedAsset(h.acct.AccountID, "web", newHash); !ok || string(af.Content) != "body{margin:1px}" || af.ContentType != "text/css; charset=utf-8" {
		t.Fatalf("uploaded file %+v %v", af, ok)
	}
	if ws2.Status.AssetsHash == ws.Status.AssetsHash {
		t.Fatal("assetsHash did not change")
	}
	step("code only", func() {
		h.updateScript("web", func(ws *workersv1alpha1.WorkerScript) {
			ws.Spec.ForProvider.Modules["index.js"] = workersv1alpha1.WorkerModule{Type: "esm", Content: strings.Replace(apiModule, "ok: true", "ok: 2", 1)}
		})
	}, 0, 0)
	if got := h.assetManifest("web"); got["/css/site.css"] != newHash {
		t.Fatalf("keep_assets lost the assets: %v", got)
	}
	step("config only", func() {
		h.updateScript("web", func(ws *workersv1alpha1.WorkerScript) {
			ws.Spec.ForProvider.Assets.Config.HTMLHandling = workersv1alpha1.HTMLHandlingDropTrailingSlash
		})
	}, 1, 0)
	if _, cfg, _ := h.e.Fake.WorkerAssets(h.acct.AccountID, "web"); cfg["html_handling"] != "drop-trailing-slash" {
		t.Fatalf("config after the change %v", cfg)
	}
	h.assertNoWrites("web")

	// Someone else deploys the script without assets: the next reconcile restores both, through
	// a session (the deployed version's assets are not this object's), which uploads nothing.
	m = h.mark()
	h.apiUpload("web", fetchModule)
	h.poke(script("web"))
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		j := h.since(m)
		return h.uploads(j, "web") == 2, testenv.Summary(j)
	})
	h.waitScript("web", scriptReady)
	j = h.since(m)
	if s, b := h.assetCalls(j, "web"); s != 1 || b != 0 {
		t.Fatalf("after an out-of-band upload: %d sessions, %d buckets; want 1 and 0:\n%s", s, b, testenv.Summary(j))
	}
	if got := h.assetManifest("web"); got["/css/site.css"] != newHash {
		t.Fatalf("assets not restored: %v", got)
	}
	h.assertNoWrites("web")
}

// An assets-only Worker: no modules, no main_module; the upload carries only metadata.
func TestAssetsOnlyWorker(t *testing.T) {
	h := start(t)
	site := map[string]string{"index.html": "<h1>spa</h1>", "app.js": "console.log(1)"}
	h.siteConfigMap("spa", site)
	h.newScript("spa", &workersv1alpha1.WorkerScriptParameters{CompatibilityDate: "2026-09-01",
		Assets: &workersv1alpha1.WorkerAssets{Source: siteSource("spa", keysOf(site)...),
			Config: &workersv1alpha1.WorkerAssetsConfig{NotFoundHandling: workersv1alpha1.NotFoundHandlingSPA}},
		WorkersDev: &workersv1alpha1.WorkersDev{Enabled: true}}, nil)
	ws := h.waitScript("spa", scriptReady)
	if !ws.Status.AtProvider.HasAssets || ws.Status.AtProvider.URL == "" || len(ws.Status.AtProvider.Handlers) != 0 {
		t.Fatalf("atProvider %+v", ws.Status.AtProvider)
	}
	if got := h.assetManifest("spa"); len(got) != 2 || got["/app.js"] != wantHash("console.log(1)", "app.js") {
		t.Fatalf("manifest %v", got)
	}
	h.assertNoWrites("spa")
}

// Modules from an artifact ConfigMap (opt-in label required; a label added later is noticed).
func TestModuleSourceArtifact(t *testing.T) {
	h := start(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bundle"}, Data: map[string]string{
		"index.js": `import { msg } from "./lib/msg.js";` + "\n" + `export default { async fetch() { return new Response(msg); } };` + "\n",
		"msg.js":   `export const msg = "from an artifact";` + "\n",
	}}
	h.create(cm)
	src := &sharedv1alpha1.ArtifactSource{ConfigMapRef: &sharedv1alpha1.ConfigMapArtifactSource{ConfigMaps: []sharedv1alpha1.ConfigMapArtifact{{
		Name: "bundle", Items: []sharedv1alpha1.ArtifactKeyToPath{{Key: "index.js", Path: "index.js"}, {Key: "msg.js", Path: "lib/msg.js"}}}}}}
	m := h.mark()
	h.newScript("bundled", &workersv1alpha1.WorkerScriptParameters{MainModule: "index.js", CompatibilityDate: "2026-09-01", ModuleSource: src}, nil)
	h.waitScript("bundled", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, commonv1alpha1.ReasonDependency) &&
			strings.Contains(condOf(ws.Status.Conditions, "Synced"), sharedv1alpha1.LabelArtifact)
	})
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("an unlabelled ConfigMap was uploaded:\n%s", testenv.Summary(w))
	}
	cm.Labels = map[string]string{sharedv1alpha1.LabelArtifact: "true"}
	if err := h.e.Client.Update(h.ctx(), cm); err != nil {
		t.Fatal(err)
	}
	ws := h.waitScript("bundled", scriptReady)
	if a := ws.Status.Artifacts; a == nil || a.Modules == nil || a.Modules.Files != 2 || !strings.HasPrefix(a.Modules.Digest, "sha256:") {
		t.Fatalf("artifacts %+v", a)
	}
	if fmt := ws.Status.AtProvider.Handlers; len(fmt) != 1 || fmt[0] != "fetch" {
		t.Fatalf("handlers %v", fmt)
	}
	h.assertNoWrites("bundled")
	// A file whose module type cannot be told is InvalidSpec until moduleTypes names it.
	cm.Data["notes.rst"] = "notes"
	src.ConfigMapRef.ConfigMaps[0].Items = append(src.ConfigMapRef.ConfigMaps[0].Items, sharedv1alpha1.ArtifactKeyToPath{Key: "notes.rst", Path: "notes.rst"})
	if err := h.e.Client.Update(h.ctx(), cm); err != nil {
		t.Fatal(err)
	}
	h.updateScript("bundled", func(ws *workersv1alpha1.WorkerScript) { ws.Spec.ForProvider.ModuleSource = src })
	h.waitScript("bundled", func(ws *workersv1alpha1.WorkerScript) bool {
		return hasCond(ws.Status.Conditions, ws.Generation, "Synced", metav1.ConditionFalse, "InvalidSpec")
	})
	h.updateScript("bundled", func(ws *workersv1alpha1.WorkerScript) { ws.Spec.ForProvider.ModuleTypes = map[string]string{"notes.rst": "text"} })
	h.waitScript("bundled", scriptReady)
}

// r2_bucket and send_email bindings are uploaded as the spec (and wrangler) shape them.
func TestR2AndSendEmailBindings(t *testing.T) {
	h := start(t)
	fp := params(fetchModule)
	eu := "eu"
	fp.Bindings = []workersv1alpha1.WorkerBinding{
		{Name: "BUCKET", Type: "r2_bucket", BucketName: str("flare-spike-media"), Jurisdiction: &eu},
		{Name: "MAIL", Type: "send_email", DestinationAddress: str("ops@example.com"), AllowedSenderAddresses: []string{"noreply@example.com"}},
		{Name: "MAIL2", Type: "send_email", AllowedDestinationAddresses: []string{"a@example.com", "b@example.com"}},
	}
	h.newScript("bind", fp, nil)
	h.waitScript("bind", scriptReady)
	s := h.settings("bind")
	if b := binding(s, "BUCKET"); b["bucket_name"] != "flare-spike-media" || b["jurisdiction"] != "eu" || b["type"] != "r2_bucket" {
		t.Fatalf("r2 binding %v", b)
	}
	if b := binding(s, "MAIL"); b["destination_address"] != "ops@example.com" || b["allowed_destination_addresses"] != nil {
		t.Fatalf("send_email binding %v", b)
	}
	if b := binding(s, "MAIL2"); len(b["allowed_destination_addresses"].([]any)) != 2 {
		t.Fatalf("send_email binding %v", b)
	}
	h.assertNoWrites("bind")
}

// Observe-only on a Worker with assets reads it (has_assets) and never writes, and never loads
// an artifact.
func TestObserveOnlyAssets(t *testing.T) {
	h := start(t)
	site := map[string]string{"index.html": "x"}
	h.siteConfigMap("obs-site", site)
	h.newScript("obs", &workersv1alpha1.WorkerScriptParameters{CompatibilityDate: "2026-09-01",
		Assets: &workersv1alpha1.WorkerAssets{Source: siteSource("obs-site", "index.html")}}, nil)
	h.waitScript("obs", scriptReady)
	m := h.mark()
	h.newScript("watcher", &workersv1alpha1.WorkerScriptParameters{CompatibilityDate: "2026-09-01",
		Assets: &workersv1alpha1.WorkerAssets{Source: siteSource("missing-cm", "index.html")}}, func(ws *workersv1alpha1.WorkerScript) {
		ws.Annotations = map[string]string{commonv1alpha1.AnnotationExternalID: "obs"}
		ws.Spec.ManagementPolicies = []commonv1alpha1.ManagementAction{commonv1alpha1.ManageObserve}
	})
	ws := h.waitScript("watcher", scriptReady)
	if !ws.Status.AtProvider.HasAssets || ws.Status.Artifacts != nil {
		t.Fatalf("observe-only status: has_assets %v, artifacts %+v", ws.Status.AtProvider.HasAssets, ws.Status.Artifacts)
	}
	h.settle(script("watcher"))
	if w := testenv.Writes(h.since(m)); len(w) != 0 {
		t.Fatalf("observe-only wrote:\n%s", testenv.Summary(w))
	}
}

// The CRD's CEL rules for the full-stack fields.
func TestValidationAssets(t *testing.T) {
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	ctx := testenv.Context(t, 20*time.Second)
	src := siteSource("cm", "index.html")
	try := func(what string, fp *workersv1alpha1.WorkerScriptParameters, wantErr string) {
		t.Helper()
		ws := &workersv1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
			Spec: workersv1alpha1.WorkerScriptSpec{ResourceSpec: accountRef(), ForProvider: fp}}
		err := e.Client.Create(ctx, ws, client.DryRunAll)
		switch {
		case wantErr == "" && err != nil:
			t.Errorf("%s: %v", what, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("%s: got %v, want an error containing %q", what, err, wantErr)
		}
	}
	withAssets := func(mut func(*workersv1alpha1.WorkerScriptParameters)) *workersv1alpha1.WorkerScriptParameters {
		fp := params(apiModule)
		fp.Assets = &workersv1alpha1.WorkerAssets{Source: src}
		mut(fp)
		return fp
	}
	yes := true
	try("worker with assets", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "ASSETS", Type: "assets"}}
	}), "")
	try("assets-only", &workersv1alpha1.WorkerScriptParameters{Assets: &workersv1alpha1.WorkerAssets{Source: src}}, "")
	try("nothing", &workersv1alpha1.WorkerScriptParameters{MainModule: "index.js"}, "exactly one of modules, sourceRef or moduleSource")
	try("no main_module with code", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) { fp.MainModule = "" }), "exactly one of modules")
	try("assets-only with bindings", &workersv1alpha1.WorkerScriptParameters{Assets: &workersv1alpha1.WorkerAssets{Source: src},
		Bindings: []workersv1alpha1.WorkerBinding{{Name: "T", Type: "plain_text", Text: str("x")}}}, "an assets-only Worker (no main_module) has no bindings")
	try("assets-only with run_worker_first", &workersv1alpha1.WorkerScriptParameters{Assets: &workersv1alpha1.WorkerAssets{Source: src,
		Config: &workersv1alpha1.WorkerAssetsConfig{RunWorkerFirst: &yes}}}, "run_worker_first needs a Worker's code")
	try("assets binding without assets", func() *workersv1alpha1.WorkerScriptParameters {
		fp := params(apiModule)
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "ASSETS", Type: "assets"}}
		return fp
	}(), "an assets binding needs forProvider.assets")
	try("two assets bindings", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "A", Type: "assets"}, {Name: "B", Type: "assets"}}
	}), "at most one")
	try("both run_worker_first forms", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Assets.Config = &workersv1alpha1.WorkerAssetsConfig{RunWorkerFirst: &yes, RunWorkerFirstPaths: []string{"/api/*"}}
	}), "not both")
	try("only negative rules", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Assets.Config = &workersv1alpha1.WorkerAssetsConfig{RunWorkerFirstPaths: []string{"!/x"}}
	}), "at least one rule that is not negative")
	try("rule without slash", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Assets.Config = &workersv1alpha1.WorkerAssetsConfig{RunWorkerFirstPaths: []string{"api/*"}}
	}), "run_worker_first_paths")
	try("two artifact sources", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Assets.Source.URL = &sharedv1alpha1.URLArtifactSource{URL: "https://example.com/a.tgz", SHA256: strings.Repeat("a", 64)}
	}), "set exactly one of configMapRef, ociRef or url")
	try("moduleTypes without moduleSource", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.ModuleTypes = map[string]string{"a.x": "text"}
	}), "moduleTypes is only valid with moduleSource")
	try("modules and moduleSource", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.ModuleSource = &src
	}), "exactly one of modules, sourceRef or moduleSource")
	try("bad module type", &workersv1alpha1.WorkerScriptParameters{MainModule: "index.js", ModuleSource: &src,
		ModuleTypes: map[string]string{"index.js": "python"}}, "module types are esm, cjs, text, json or wasm")
	try("r2 without bucket", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "B", Type: "r2_bucket"}}
	}), "type r2_bucket needs bucket_name")
	try("jurisdiction on kv", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		eu := "eu"
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "K", Type: "kv_namespace", NamespaceID: str("x"), Jurisdiction: &eu}}
	}), "only valid with that type, as is jurisdiction")
	try("send_email both addresses", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "M", Type: "send_email", DestinationAddress: str("a@example.com"),
			AllowedDestinationAddresses: []string{"b@example.com"}}}
	}), "not both")
	try("destination on plain_text", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "T", Type: "plain_text", Text: str("x"), DestinationAddress: str("a@example.com")}}
	}), "only valid with type send_email")
	try("bad email", withAssets(func(fp *workersv1alpha1.WorkerScriptParameters) {
		fp.Bindings = []workersv1alpha1.WorkerBinding{{Name: "M", Type: "send_email", DestinationAddress: str("nope")}}
	}), "destination_address")
}
