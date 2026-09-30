package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

const pagesBase = acct + "/pages/projects"

func pagesServer(t *testing.T) (*Server, *client) {
	t.Helper()
	s := New(Options{})
	s.Clock.Set(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	return s, newClient(t, s)
}

func (c *client) pagesCreate(name string, body map[string]any) map[string]any {
	c.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["name"] = name
	if _, ok := body["production_branch"]; !ok {
		body["production_branch"] = "main"
	}
	st, env, _ := c.do("POST", pagesBase, body)
	if st != 200 {
		c.t.Fatalf("create %s: %d %v", name, st, env.Errors)
	}
	return env.Result.(map[string]any)
}

// jwtClient returns a client that authenticates with the project's upload JWT.
func (c *client) jwtClient(project string) *client {
	c.t.Helper()
	st, env, _ := c.do("GET", pagesBase+"/"+project+"/upload-token", nil)
	if st != 200 {
		c.t.Fatalf("upload-token: %d %v", st, env.Errors)
	}
	return &client{t: c.t, base: c.base, tok: env.Result.(map[string]any)["jwt"].(string)}
}

// deployForm is a Direct Upload create body: manifest and branch fields, routing files.
func deployForm(t *testing.T, manifest map[string]string, fields map[string]string, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	m, _ := json.Marshal(manifest)
	_ = w.WriteField("manifest", string(m))
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for name, content := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, name, name))
		h.Set("Content-Type", "application/octet-stream")
		p, _ := w.CreatePart(h)
		_, _ = p.Write([]byte(content))
	}
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

func TestPagesProjectLifecycle(t *testing.T) {
	_, c := pagesServer(t)
	p := c.pagesCreate("site", map[string]any{"deployment_configs": map[string]any{
		"production": map[string]any{"compatibility_date": "2026-09-01", "env_vars": map[string]any{
			"A": map[string]any{"type": "plain_text", "value": "1"}, "S": map[string]any{"type": "secret_text", "value": "hush"}},
			"kv_namespaces": map[string]any{"KV": map[string]any{"namespace_id": "0123456789abcdef0123456789abcdef"}}},
	}})
	if p["subdomain"] != "site.pages.dev" || p["latest_deployment"] != nil || p["canonical_deployment"] != nil {
		t.Fatalf("created project %v", p)
	}
	prod := p["deployment_configs"].(map[string]any)["production"].(map[string]any)
	evs := prod["env_vars"].(map[string]any)
	if evs["S"].(map[string]any)["value"] != "" || evs["A"].(map[string]any)["value"] != "1" || prod["compatibility_date"] != "2026-09-01" {
		t.Fatalf("production config %v (secret values are never returned)", prod)
	}
	if st, env, _ := c.do("POST", pagesBase, map[string]any{"name": "site", "production_branch": "main"}); st != http.StatusConflict {
		t.Fatalf("duplicate create: %d %v", st, env.Errors)
	}
	if st, _, _ := c.do("POST", pagesBase, map[string]any{"name": "Bad_Name", "production_branch": "main"}); st != 400 {
		t.Fatalf("invalid name: %d", st)
	}
	// PATCH merges maps key by key; null removes a key.
	st, env, _ := c.do("PATCH", pagesBase+"/site", map[string]any{"production_branch": "prod", "deployment_configs": map[string]any{
		"production": map[string]any{"env_vars": map[string]any{"A": nil, "B": map[string]any{"type": "plain_text", "value": "2"}}},
	}})
	if st != 200 {
		t.Fatalf("patch: %d %v", st, env.Errors)
	}
	prod = env.Result.(map[string]any)["deployment_configs"].(map[string]any)["production"].(map[string]any)
	evs = prod["env_vars"].(map[string]any)
	if _, has := evs["A"]; has || evs["B"] == nil || evs["S"] == nil || prod["kv_namespaces"] == nil || env.Result.(map[string]any)["production_branch"] != "prod" {
		t.Fatalf("after patch: %v", env.Result)
	}
	if st, _, _ := c.do("PATCH", pagesBase+"/site", map[string]any{"deployment_configs": map[string]any{
		"production": map[string]any{"env_vars": map[string]any{"X": map[string]any{"type": "other", "value": "v"}}}}}); st != 400 {
		t.Fatalf("invalid env var type: %d", st)
	}
	c.pagesCreate("other", nil)
	_, env, _ = c.do("GET", pagesBase+"?per_page=1&page=2", nil)
	if items := env.Result.([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "other" {
		t.Fatalf("page 2 of 1: %v", env.Result)
	}
	if st, _, _ := c.do("DELETE", pagesBase+"/site", nil); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	st, env, _ = c.do("GET", pagesBase+"/site", nil)
	if st != 404 || env.Errors[0].Code != codePagesProjectNotFound {
		t.Fatalf("get after delete: %d %v", st, env.Errors)
	}
}

func TestPagesDirectUpload(t *testing.T) {
	s, c := pagesServer(t)
	c.pagesCreate("site", nil)
	j := c.jwtClient("site")
	claims := strings.Split(j.tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(claims[1])
	var cl pagesClaims
	if err := json.Unmarshal(raw, &cl); err != nil || cl.MaxFileCountAllowed != pagesMaxFileCount || cl.Exp <= s.Clock.Now().Unix() {
		t.Fatalf("jwt claims %s: %v", raw, err)
	}
	// The account token is not an upload token.
	if st, env, _ := c.do("POST", "/client/v4/pages/assets/check-missing", map[string]any{"hashes": []string{}}); st != 401 || env.Errors[0].Code != codePagesUnauthorized {
		t.Fatalf("check-missing with the API token: %d %v", st, env.Errors)
	}
	h1, h2 := strings.Repeat("a", 32), strings.Repeat("b", 32)
	_, env, _ := j.do("POST", "/client/v4/pages/assets/check-missing", map[string]any{"hashes": []string{h1, h2}})
	if fmt.Sprint(env.Result) != fmt.Sprint([]any{h1, h2}) {
		t.Fatalf("check-missing: %v", env.Result)
	}
	st, env, _ := j.do("POST", "/client/v4/pages/assets/upload", []map[string]any{{"key": h1, "value": base64.StdEncoding.EncodeToString([]byte("<h1>hi</h1>")),
		"metadata": map[string]any{"contentType": "text/html"}, "base64": true}})
	if st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	if st, _, _ := j.do("POST", "/client/v4/pages/assets/upsert-hashes", map[string]any{"hashes": []string{h1}}); st != 200 {
		t.Fatalf("upsert: %d", st)
	}
	if b, ct, ok := s.PagesAsset("0123456789abcdef0123456789abcdef", "site", h1); !ok || string(b) != "<h1>hi</h1>" || ct != "text/html" {
		t.Fatalf("stored asset %q %q %v", b, ct, ok)
	}
	_, env, _ = j.do("POST", "/client/v4/pages/assets/check-missing", map[string]any{"hashes": []string{h1, h2}})
	if fmt.Sprint(env.Result) != fmt.Sprint([]any{h2}) {
		t.Fatalf("check-missing after upload: %v", env.Result)
	}
	// A manifest naming a hash that was never uploaded is refused.
	body, ct := deployForm(t, map[string]string{"/index.html": h1, "/x.css": h2}, nil, nil)
	if st, env, _ := c.raw("POST", pagesBase+"/site/deployments", ct, body); st != 400 || env.Errors[0].Code != codePagesMissingAssets {
		t.Fatalf("deploy with a missing asset: %d %v", st, env.Errors)
	}
	body, ct = deployForm(t, map[string]string{"/index.html": h1}, map[string]string{"commit_hash": "abc123", "commit_message": "m"},
		map[string]string{"_headers": "/*\n  X-A: b\n", "_redirects": "/a /b 301\n"})
	st, env, _ = c.raw("POST", pagesBase+"/site/deployments", ct, body)
	if st != 200 {
		t.Fatalf("deploy: %d %v", st, env.Errors)
	}
	d := env.Result.(map[string]any)
	id := d["id"].(string)
	if d["environment"] != "production" || d["latest_stage"].(map[string]any)["name"] != "queued" ||
		d["deployment_trigger"].(map[string]any)["metadata"].(map[string]any)["branch"] != "main" || !strings.HasPrefix(d["url"].(string), "https://"+d["short_id"].(string)+".site.pages.dev") {
		t.Fatalf("new deployment %v", d)
	}
	manifest, files, ok := s.PagesDeploymentFiles("0123456789abcdef0123456789abcdef", "site", id)
	if !ok || manifest["/index.html"] != h1 || string(files["_headers"]) != "/*\n  X-A: b\n" || string(files["_redirects"]) != "/a /b 301\n" {
		t.Fatalf("stored deployment %v %v", manifest, files)
	}
	// Stages progress on the emulated clock.
	s.Clock.Advance(DefaultPagesDeployDelay / 2)
	_, env, _ = c.do("GET", pagesBase+"/site/deployments/"+id, nil)
	if ls := env.Result.(map[string]any)["latest_stage"].(map[string]any); ls["name"] != "deploy" || ls["status"] != "active" {
		t.Fatalf("halfway: %v", ls)
	}
	_, env, _ = c.do("GET", pagesBase+"/site", nil)
	if env.Result.(map[string]any)["canonical_deployment"] != nil {
		t.Fatalf("canonical before the deploy stage succeeded: %v", env.Result)
	}
	s.Clock.Advance(DefaultPagesDeployDelay)
	_, env, _ = c.do("GET", pagesBase+"/site/deployments/"+id, nil)
	if ls := env.Result.(map[string]any)["latest_stage"].(map[string]any); ls["name"] != "deploy" || ls["status"] != "success" || ls["ended_on"] == nil {
		t.Fatalf("done: %v", ls)
	}
	_, env, _ = c.do("GET", pagesBase+"/site", nil)
	if can := env.Result.(map[string]any)["canonical_deployment"]; can == nil || can.(map[string]any)["id"] != id {
		t.Fatalf("canonical: %v", can)
	}
	// The live production deployment cannot be deleted, even with force.
	st, env, _ = c.do("DELETE", pagesBase+"/site/deployments/"+id+"?force=true", nil)
	if st != 400 || env.Errors[0].Code != codePagesLiveDeployment {
		t.Fatalf("delete live production: %d %v", st, env.Errors)
	}
	// A preview deployment holds its branch alias: deleted only with force.
	body, ct = deployForm(t, map[string]string{"/index.html": h1}, map[string]string{"branch": "Feature/X"}, nil)
	_, env, _ = c.raw("POST", pagesBase+"/site/deployments", ct, body)
	pv := env.Result.(map[string]any)
	if pv["environment"] != "preview" || fmt.Sprint(pv["aliases"]) != "[https://feature-x.site.pages.dev]" {
		t.Fatalf("preview deployment %v", pv)
	}
	_, env, _ = c.do("GET", pagesBase+"/site/deployments?env=preview", nil)
	if items := env.Result.([]any); len(items) != 1 || items[0].(map[string]any)["id"] != pv["id"] {
		t.Fatalf("env=preview list: %v", env.Result)
	}
	if st, env, _ := c.do("DELETE", pagesBase+"/site/deployments/"+pv["id"].(string), nil); st != 400 || env.Errors[0].Code != codePagesAliasedDeployment {
		t.Fatalf("delete aliased preview without force: %d %v", st, env.Errors)
	}
	if st, _, _ := c.do("DELETE", pagesBase+"/site/deployments/"+pv["id"].(string)+"?force=true", nil); st != 200 {
		t.Fatalf("delete aliased preview with force: %d", st)
	}
	// A failed production deployment keeps the older one live.
	s.FailPagesDeployments(1)
	body, ct = deployForm(t, map[string]string{"/index.html": h1}, nil, nil)
	_, env, _ = c.raw("POST", pagesBase+"/site/deployments", ct, body)
	failed := env.Result.(map[string]any)["id"].(string)
	s.Clock.Advance(2 * DefaultPagesDeployDelay)
	_, env, _ = c.do("GET", pagesBase+"/site/deployments/"+failed, nil)
	if ls := env.Result.(map[string]any)["latest_stage"].(map[string]any); ls["status"] != "failure" {
		t.Fatalf("failed deployment: %v", ls)
	}
	_, env, _ = c.do("GET", pagesBase+"/site", nil)
	res := env.Result.(map[string]any)
	if res["canonical_deployment"].(map[string]any)["id"] != id || res["latest_deployment"].(map[string]any)["id"] != failed {
		t.Fatalf("after a failed deployment: canonical %v latest %v", res["canonical_deployment"], res["latest_deployment"])
	}
	if st, _, _ := c.do("DELETE", pagesBase+"/site/deployments/"+failed, nil); st != 200 {
		t.Fatalf("delete a failed production deployment: %d", st)
	}
	// Deleting the project invalidates its upload token.
	if st, _, _ := c.do("DELETE", pagesBase+"/site", nil); st != 200 {
		t.Fatal("delete project")
	}
	if st, _, _ := j.do("POST", "/client/v4/pages/assets/check-missing", map[string]any{"hashes": []string{h1}}); st != 401 {
		t.Fatalf("token of a deleted project: %d", st)
	}
}

func TestPagesUploadTokenExpires(t *testing.T) {
	s, c := pagesServer(t)
	c.pagesCreate("site", nil)
	j := c.jwtClient("site")
	s.Clock.Advance(pagesJWTLifetime)
	if st, env, _ := j.do("POST", "/client/v4/pages/assets/check-missing", map[string]any{"hashes": []string{}}); st != 401 || env.Errors[0].Code != codePagesUnauthorized {
		t.Fatalf("expired token: %d %v", st, env.Errors)
	}
}

func TestBranchAlias(t *testing.T) {
	for in, want := range map[string]string{"main": "main", "Feature/X": "feature-x", "a__b": "a-b", "-x-": "x",
		strings.Repeat("abc", 20): strings.Repeat("abc", 9) + "a"} {
		if got := branchAlias(in); got != want {
			t.Errorf("branchAlias(%q) = %q, want %q", in, got, want)
		}
	}
}
