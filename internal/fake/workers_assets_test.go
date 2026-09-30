package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"testing"
	"time"
)

// Workers static assets (workers_assets.go). Behaviors no recording shows are marked there.

// TestAssetHashMatchesWrangler: the golden hashes were computed with the blake3-wasm 2.1.5 of
// the pinned wrangler@4.143.0 (node_modules/blake3-wasm) exactly as hash.ts does:
// hash(Buffer.toString("base64") + extname(path).substring(1)).toString("hex").slice(0, 32).
func TestAssetHashMatchesWrangler(t *testing.T) {
	for _, c := range []struct {
		content []byte
		path    string
		want    string
	}{
		{[]byte("<h1>hello</h1>\n"), "/index.html", "f17fd9f85e7289b774b95ae8215bee61"},
		{[]byte(""), "/empty", "af1349b9f5f9a1a6a0404dea36dcc949"},
		{[]byte{0, 1, 2, 255}, "/x/y.bin", "2172bd7be4ca4461cd74312a85d2a041"},
	} {
		if got := AssetHash(base64.StdEncoding.EncodeToString(c.content), c.path); got != c.want {
			t.Errorf("AssetHash(%q, %s) = %s, want %s", c.content, c.path, got, c.want)
		}
	}
	// node's path.extname (node v25, printed by `node -e 'path.extname(...)'`).
	for in, want := range map[string]string{
		"a.js": ".js", ".bashrc": "", "..foo": ".foo", "a.": ".", "a.tar.gz": ".gz", "dir.x/file": "",
		".a.b": ".b", "...": ".", "a..b": ".b", "_headers": "", "x/.hidden.txt": ".txt", "..": "", ".": "",
		"a.b.": ".", "..a.": ".", ".a.": ".", "....": ".", "..a": ".a", "x/..": "", "x/..a": ".a", "/a/b.c/": ".c",
	} {
		if got := nodeExtname(in); got != want {
			t.Errorf("nodeExtname(%q) = %q, want %q", in, got, want)
		}
	}
}

// siteFiles is a small site: path → content.
var siteFiles = map[string]string{
	"/index.html":       "<h1>home</h1>",
	"/about/index.html": "<h1>about</h1>",
	"/css/site.css":     "body{margin:0}",
}

type assetFixture struct {
	t     *testing.T
	s     *Server
	c     *client
	files map[string]string
}

func (f *assetFixture) manifest() map[string]assetEntry {
	m := map[string]assetEntry{}
	for p, content := range f.files {
		m[p] = assetEntry{Hash: AssetHash(base64.StdEncoding.EncodeToString([]byte(content)), p), Size: int64(len(content))}
	}
	return m
}

type sessionResult struct {
	Buckets [][]string `json:"buckets"`
	JWT     string     `json:"jwt"`
}

func (f *assetFixture) session(script string, m map[string]assetEntry) sessionResult {
	f.t.Helper()
	st, env, _ := f.c.do("POST", acct+"/workers/scripts/"+script+"/assets-upload-session", map[string]any{"manifest": m})
	if st != 200 {
		f.t.Fatalf("session: %d %v", st, env.Errors)
	}
	b, _ := json.Marshal(env.Result)
	var r sessionResult
	if err := json.Unmarshal(b, &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// bucketBody is one bucket as wrangler sends it (assets.ts#L220-L249): per hash a part named and
// file-named by the hash, with the base64 content and the Content-Type to serve.
func (f *assetFixture) bucketBody(hashes []string, tamper bool) ([]byte, string) {
	byHash := map[string]string{}
	for p, e := range f.manifest() {
		byHash[e.Hash] = p
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, h := range hashes {
		hd := textproto.MIMEHeader{}
		hd.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, h, h))
		hd.Set("Content-Type", "text/html; charset=utf-8")
		pw, _ := mw.CreatePart(hd)
		content := f.files[byHash[h]]
		if tamper {
			content += "!"
		}
		_, _ = pw.Write([]byte(base64.StdEncoding.EncodeToString([]byte(content))))
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func (f *assetFixture) upload(jwt string, hashes []string) (int, testEnv) {
	f.t.Helper()
	body, ct := f.bucketBody(hashes, false)
	c := *f.c
	c.tok = jwt
	st, env, _ := c.raw("POST", acct+"/workers/assets/upload?base64=true", ct, body)
	return st, env
}

func (f *assetFixture) deploy(script string, meta map[string]any, withModule bool) (int, testEnv) {
	f.t.Helper()
	mods := map[string]string{}
	if withModule {
		mods["index.js"] = moduleSrc
	}
	body, ct := multipartBody(f.t, "metadata", meta, mods)
	st, env, _ := f.c.raw("PUT", acct+"/workers/scripts/"+script, ct, body)
	return st, env
}

func newAssetFixture(t *testing.T) *assetFixture {
	s := New(Options{})
	files := map[string]string{}
	for p, c := range siteFiles {
		files[p] = c
	}
	return &assetFixture{t: t, s: s, c: newClient(t, s), files: files}
}

func TestAssetsUploadFlow(t *testing.T) {
	f := newAssetFixture(t)
	f.s.SetAssetBuckets(2, 0) // 3 files → 2 buckets
	sess := f.session("site", f.manifest())
	if len(sess.Buckets) != 2 || len(sess.Buckets[0])+len(sess.Buckets[1]) != 3 || sess.JWT == "" {
		t.Fatalf("session %+v, want 3 hashes in 2 buckets", sess)
	}
	if st, env := f.upload(sess.JWT, sess.Buckets[0]); st != http.StatusAccepted || len(resultMap(t, env)) != 0 {
		t.Fatalf("first bucket: %d %+v, want 202 {}", st, env)
	}
	st, env := f.upload(sess.JWT, sess.Buckets[1])
	completion, _ := resultMap(t, env)["jwt"].(string)
	if st != http.StatusCreated || completion == "" {
		t.Fatalf("last bucket: %d %+v, want 201 with a completion jwt", st, env)
	}
	cfg := map[string]any{"html_handling": "auto-trailing-slash", "not_found_handling": "404-page"}
	st, env = f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": completion, "config": cfg},
		"bindings": []any{map[string]any{"name": "ASSETS", "type": "assets"}}}, true)
	if st != 200 || resultMap(t, env)["has_assets"] != true {
		t.Fatalf("script upload: %d %+v", st, env)
	}
	m, gotCfg, has := f.s.WorkerAssets(acctID, "site")
	if !has || len(m) != 3 || m["/css/site.css"] != f.manifest()["/css/site.css"].Hash || gotCfg["not_found_handling"] != "404-page" {
		t.Fatalf("stored assets %v %v %v", m, gotCfg, has)
	}
	if af, ok := f.s.UploadedAsset(acctID, "site", m["/index.html"]); !ok || string(af.Content) != "<h1>home</h1>" || af.ContentType != "text/html; charset=utf-8" {
		t.Errorf("stored file %+v %v", af, ok)
	}

	// Unchanged: no bucket, and the session's jwt is already the completion token (DOCS).
	again := f.session("site", f.manifest())
	if len(again.Buckets) != 0 {
		t.Fatalf("unchanged manifest: buckets %v, want none", again.Buckets)
	}
	if st, env := f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": again.JWT}}, true); st != 200 {
		t.Fatalf("redeem the short-circuit token: %d %v", st, env.Errors)
	}
	// One changed file: one bucket with its hash.
	f.files["/index.html"] = "<h1>home v2</h1>"
	changed := f.session("site", f.manifest())
	if len(changed.Buckets) != 1 || len(changed.Buckets[0]) != 1 || changed.Buckets[0][0] != f.manifest()["/index.html"].Hash {
		t.Fatalf("one changed file: buckets %v", changed.Buckets)
	}
	// keep_assets keeps the deployed version's assets (and config).
	if st, env := f.deploy("site", map[string]any{"main_module": "index.js", "keep_assets": true}, true); st != 200 || resultMap(t, env)["has_assets"] != true {
		t.Fatalf("keep_assets: %d %+v", st, env)
	}
	if m, _, has := f.s.WorkerAssets(acctID, "site"); !has || m["/index.html"] != AssetHash(base64.StdEncoding.EncodeToString([]byte("<h1>home</h1>")), "/index.html") {
		t.Errorf("after keep_assets: %v %v", m, has)
	}
	// Without either the new version has no assets, and an assets binding is refused.
	if st, _ := f.deploy("site", map[string]any{"main_module": "index.js", "bindings": []any{map[string]any{"name": "ASSETS", "type": "assets"}}}, true); st != 400 {
		t.Errorf("assets binding without assets: %d, want 400", st)
	}
	if st, env := f.deploy("site", map[string]any{"main_module": "index.js"}, true); st != 200 || resultMap(t, env)["has_assets"] != false {
		t.Errorf("upload without assets: %d %+v", st, env)
	}
}

func TestAssetsOnlyWorker(t *testing.T) {
	f := newAssetFixture(t)
	sess := f.session("static", f.manifest())
	var hashes []string
	for _, b := range sess.Buckets {
		hashes = append(hashes, b...)
	}
	_, env := f.upload(sess.JWT, hashes)
	completion := resultMap(t, env)["jwt"].(string)
	// wrangler's assets-only metadata: assets, compatibility_date, no module and no part
	// (create-worker-upload-form.ts#L99-L114).
	st, env := f.deploy("static", map[string]any{"assets": map[string]any{"jwt": completion, "config": map[string]any{}},
		"compatibility_date": "2026-09-01"}, false)
	if st != 200 || resultMap(t, env)["has_assets"] != true || resultMap(t, env)["has_modules"] != false {
		t.Fatalf("assets-only upload: %d %+v", st, env)
	}
	if st, _ := f.deploy("static2", map[string]any{"compatibility_date": "2026-09-01"}, false); st != 400 {
		t.Errorf("no module and no assets: %d, want 400", st)
	}
}

func TestAssetsTokens(t *testing.T) {
	f := newAssetFixture(t)
	sess := f.session("site", f.manifest())
	all := sess.Buckets[0]
	sort.Strings(all)
	if st, _ := f.upload("not-a-jwt", all); st != http.StatusUnauthorized {
		t.Errorf("garbage token: %d, want 401", st)
	}
	// A completion token does not authenticate uploads, and an upload token is not redeemable.
	other := newAssetFixture(t)
	if st, env := f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": sess.JWT}}, true); st != 400 {
		t.Errorf("upload token redeemed: %d %+v, want 400", st, env)
	}
	// Tokens of another emulator (another key) are refused.
	osess := other.session("site", other.manifest())
	if st, _ := f.upload(osess.JWT, all); st != http.StatusUnauthorized {
		t.Errorf("foreign token: %d, want 401", st)
	}
	// Tampered content does not match its hash.
	body, ct := f.bucketBody(all, true)
	c := *f.c
	c.tok = sess.JWT
	if st, _, _ := c.raw("POST", acct+"/workers/assets/upload?base64=true", ct, body); st != 400 {
		t.Errorf("tampered content: %d, want 400", st)
	}
	// A hash outside the session's manifest.
	if st, _ := f.upload(sess.JWT, []string{strings.Repeat("0", 32)}); st != 400 {
		t.Errorf("unknown hash: %d, want 400", st)
	}
	// Expiry: one hour (DOCS).
	f.s.Clock.Set(time.Now())
	late := f.session("site", f.manifest())
	f.s.Clock.Advance(time.Hour)
	if st, env := f.upload(late.JWT, late.Buckets[0]); st != http.StatusUnauthorized || !strings.Contains(env.Errors[0].Message, "expired") {
		t.Errorf("expired session token: %d %+v, want 401 expired", st, env)
	}
	fresh := f.session("site", f.manifest())
	_, env := f.upload(fresh.JWT, fresh.Buckets[0])
	completion := resultMap(t, env)["jwt"].(string)
	// The completion token is bound to its script.
	if st, _ := f.deploy("other", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": completion}}, true); st != 400 {
		t.Errorf("completion token of another script: %d, want 400", st)
	}
	f.s.Clock.Advance(time.Hour + time.Second)
	if st, _ := f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": completion}}, true); st != 400 {
		t.Errorf("expired completion token: %d, want 400", st)
	}
}

// TestAssetsRequestsMatchSpec: the flow's requests, as wrangler sends them, pass strict request
// validation (the assets upload's undeclared "assets_jwt" scheme is not held against them,
// spec.go withoutUndeclaredSecurity).
func TestAssetsRequestsMatchSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	f := newAssetFixture(t)
	f.s = New(Options{Spec: spec, RejectSchemaViolations: true})
	f.c = newClient(t, f.s)
	sess := f.session("site", f.manifest())
	st, env := f.upload(sess.JWT, sess.Buckets[0])
	if st != http.StatusCreated {
		t.Fatalf("upload: %d %+v", st, env)
	}
	if st, env := f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": resultMap(t, env)["jwt"]}}, true); st != 200 {
		t.Fatalf("script upload: %d %+v", st, env)
	}
	for _, j := range f.s.Journal() {
		if j.SchemaViolation != "" {
			t.Errorf("%s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
	// A missing base64=true still is a violation.
	body, ct := f.bucketBody(sess.Buckets[0], false)
	c := *f.c
	c.tok = sess.JWT
	if st, _, _ := c.raw("POST", acct+"/workers/assets/upload", ct, body); st != 400 {
		t.Errorf("upload without base64=true: %d, want 400", st)
	}
}

// TestAssetsSessionValidation: manifest paths, hashes and sizes.
func TestAssetsSessionValidation(t *testing.T) {
	f := newAssetFixture(t)
	for name, m := range map[string]map[string]assetEntry{
		"relative path": {"index.html": {Hash: strings.Repeat("a", 32), Size: 1}},
		"short hash":    {"/index.html": {Hash: "abc", Size: 1}},
		"too large":     {"/big.bin": {Hash: strings.Repeat("a", 32), Size: assetMaxFileBytes + 1}},
	} {
		if st, _, _ := f.c.do("POST", acct+"/workers/scripts/site/assets-upload-session", map[string]any{"manifest": m}); st != 400 {
			t.Errorf("%s: %d, want 400", name, st)
		}
	}
	if st, _, _ := f.c.do("POST", acct+"/workers/scripts/site/assets-upload-session", map[string]any{}); st != 400 {
		t.Errorf("no manifest: %d, want 400", st)
	}
	if sess := f.session("site", map[string]assetEntry{}); len(sess.Buckets) != 0 || sess.JWT == "" {
		t.Errorf("empty manifest: %+v, want a completion token at once", sess)
	}
}

// TestAssetsDeletedWithScript: deleting the script drops its uploaded files (UNVERIFIED), so a
// new session asks for them again.
func TestAssetsDeletedWithScript(t *testing.T) {
	f := newAssetFixture(t)
	sess := f.session("site", f.manifest())
	_, env := f.upload(sess.JWT, sess.Buckets[0])
	if st, _ := f.deploy("site", map[string]any{"main_module": "index.js", "assets": map[string]any{"jwt": resultMap(t, env)["jwt"]}}, true); st != 200 {
		t.Fatal(st)
	}
	if st, _, _ := f.c.do("DELETE", acct+"/workers/scripts/site", nil); st != 200 {
		t.Fatal(st)
	}
	if again := f.session("site", f.manifest()); len(again.Buckets) == 0 {
		t.Errorf("after the delete no file is asked for again")
	}
}
