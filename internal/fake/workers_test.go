package fake

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

// Workers script behaviors beyond what the conformance recordings replay. Anything asserted here
// that no recording shows is marked UNVERIFIED in workers.go.

const moduleSrc = `export default {
  async fetch(req, env) {
    return env.PRIVATE.fetch("http://svc/");
  },
  async scheduled(event, env, ctx) {}
};
`

// multipartBody builds a Workers upload body: a JSON part named partName, then the given modules.
func multipartBody(t *testing.T, partName string, meta any, modules map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+partName+`"; filename="blob"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_ = json.NewEncoder(pw).Encode(meta)
	for name, src := range modules {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+name+`"; filename="`+name+`"`)
		h.Set("Content-Type", "application/javascript+module")
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write([]byte(src))
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func (c *client) raw(method, path, contentType string, body []byte) (int, testEnv, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env testEnv
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env, resp.Header
}

func (c *client) upload(name string, meta map[string]any) (int, testEnv, http.Header) {
	c.t.Helper()
	body, ct := multipartBody(c.t, "metadata", meta, map[string]string{"index.js": moduleSrc})
	return c.raw("PUT", acct+"/workers/scripts/"+name, ct, body)
}

func (c *client) createVPCService(name string) string {
	c.t.Helper()
	st, env, _ := c.do("POST", acct+"/connectivity/directory/services",
		map[string]any{"name": name, "type": "http", "http_port": 80, "host": map[string]any{"ipv4": "10.0.0.1"}})
	if st != 200 {
		c.t.Fatalf("vpc create: %d %v", st, env.Errors)
	}
	return env.Result.(map[string]any)["service_id"].(string)
}

func resultMap(t *testing.T, env testEnv) map[string]any {
	t.Helper()
	m, ok := env.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want object: %+v", env.Result, env)
	}
	return m
}

func TestWorkerUploadLifecycle(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	meta := map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01"}

	st, env, h := c.upload("w1", meta)
	if st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	if p := h.Get("Ratelimit-Policy"); p != `"workers_script_upload";q=180000;w=300` {
		t.Errorf("upload Ratelimit-Policy = %q", p)
	}
	up := resultMap(t, env)
	tag, created, etag1 := up["tag"].(string), up["created_on"].(string), up["etag"].(string)
	if len(tag) != 32 || len(etag1) != 64 || up["has_modules"] != true || up["entry_point"] != "index.js" {
		t.Errorf("upload result: %v", up)
	}
	if got := up["handlers"].([]any); len(got) != 2 || got[0] != "fetch" || got[1] != "scheduled" {
		t.Errorf("handlers = %v, want [fetch scheduled] (env.PRIVATE.fetch is a call, not a handler)", got)
	}
	v1 := up["deployment_id"].(string)

	// Re-upload with changed code: same tag and created_on, new version, new etag.
	body, ct := multipartBody(t, "metadata", meta, map[string]string{"index.js": moduleSrc + "// v2\n"})
	st, env, _ = c.raw("PUT", acct+"/workers/scripts/w1", ct, body)
	if st != 200 {
		t.Fatalf("re-upload: %d %v", st, env.Errors)
	}
	up2 := resultMap(t, env)
	if up2["tag"] != tag || up2["created_on"] != created || up2["etag"] == etag1 || up2["deployment_id"] == v1 {
		t.Errorf("re-upload: %v", up2)
	}

	st, env, _ = c.do("GET", acct+"/workers/scripts/w1/versions", nil)
	items := resultMap(t, env)["items"].([]any)
	if st != 200 || len(items) != 2 || env.Errors != nil {
		t.Fatalf("versions: %d %v %v", st, items, env.Errors)
	}
	newest := items[0].(map[string]any)
	if newest["number"].(float64) != 2 || strings.ReplaceAll(newest["id"].(string), "-", "") != up2["deployment_id"] {
		t.Errorf("newest version = %v", newest)
	}
	if info := env.ResultInfo.(map[string]any); info["total_count"].(float64) != 2 || info["per_page"].(float64) != 10 {
		t.Errorf("versions result_info = %v", info)
	}
	// Pagination over versions.
	_, env, _ = c.do("GET", acct+"/workers/scripts/w1/versions?per_page=1&page=2", nil)
	if items := resultMap(t, env)["items"].([]any); len(items) != 1 || items[0].(map[string]any)["number"].(float64) != 1 {
		t.Errorf("versions page 2 = %v", items)
	}

	st, env, _ = c.do("GET", acct+"/workers/scripts/w1/deployments", nil)
	deps := resultMap(t, env)["deployments"].([]any)
	if st != 200 || len(deps) != 2 {
		t.Fatalf("deployments: %d %v", st, deps)
	}
	vs := deps[0].(map[string]any)["versions"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["version_id"] != newest["id"] || vs[0].(map[string]any)["percentage"].(float64) != 100 {
		t.Errorf("latest deployment = %v", deps[0])
	}

	_, env, _ = c.do("GET", acct+"/workers/scripts", nil)
	if list := env.Result.([]any); len(list) != 1 || list[0].(map[string]any)["deployment_id"] != "" {
		t.Errorf("list = %v", list)
	}

	st, env, h = c.do("DELETE", acct+"/workers/scripts/w1", nil)
	if st != 200 || resultMap(t, env)["id"] != tag || !strings.HasPrefix(h.Get("Ratelimit"), `"workers_script_modify"`) {
		t.Fatalf("delete: %d %v %q", st, env.Result, h.Get("Ratelimit"))
	}
	for _, p := range []string{"/settings", "/versions", "/deployments", "/subdomain", ""} {
		method := "GET"
		if p == "" {
			method = "DELETE"
		}
		st, env, _ := c.do(method, acct+"/workers/scripts/w1"+p, nil)
		if st != 404 || len(env.Errors) != 1 || env.Errors[0].Code != 10007 {
			t.Errorf("%s w1%s after delete: %d %v", method, p, st, env.Errors)
		}
	}
	if _, env, _ := c.do("GET", acct+"/workers/scripts", nil); len(env.Result.([]any)) != 0 {
		t.Errorf("list after delete = %v", env.Result)
	}
}

func TestWorkerVPCBindingValidation(t *testing.T) {
	c := newClient(t, New(Options{}))
	svc := c.createVPCService("svc-a")
	good := map[string]any{"type": "vpc_service", "name": "A", "service_id": svc}
	bogus := map[string]any{"type": "vpc_service", "name": "B", "service_id": "00000000-0000-7000-8000-000000000000"}

	st, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "bindings": []any{good, bogus}})
	if st != 400 || len(env.Errors) != 1 || env.Errors[0].Code != 10180 ||
		!strings.Contains(env.Errors[0].Message, "00000000-0000-7000-8000-000000000000") ||
		env.Errors[0].DocumentationURL == "" || env.Result != nil {
		t.Fatalf("bogus binding: %d %+v", st, env)
	}
	if st, _, _ := c.do("GET", acct+"/workers/scripts/w/settings", nil); st != 404 {
		t.Errorf("a rejected first upload must not create the script: %d", st)
	}

	kv := map[string]any{"type": "kv_namespace", "name": "KV", "namespace_id": "anything"} // not validated
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "bindings": []any{good, kv}}); st != 200 {
		t.Fatalf("valid upload: %d %v", st, env.Errors)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts/w/settings", nil)
	set := resultMap(t, env)
	if b := set["bindings"].([]any); len(b) != 2 || b[0].(map[string]any)["service_id"] != svc {
		t.Errorf("settings bindings = %v", b)
	}
	if ann := set["annotations"].(map[string]any); ann["workers/triggered_by"] != "upload" {
		t.Errorf("annotations = %v", ann)
	}

	// Deleting a bound VPC service is allowed and the binding stays (0089, 0091); a later
	// upload that still references it fails (0106).
	if st, _, _ := c.do("DELETE", acct+"/connectivity/directory/services/"+svc, nil); st != 200 {
		t.Fatalf("vpc delete: %d", st)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts/w/settings", nil)
	if b := resultMap(t, env)["bindings"].([]any); len(b) != 2 {
		t.Errorf("bindings after service delete = %v", b)
	}
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "bindings": []any{good}}); st != 400 || env.Errors[0].Code != 10180 {
		t.Errorf("upload referencing a deleted service: %d %v", st, env.Errors)
	}
}

func TestWorkerSettingsPatch(t *testing.T) {
	c := newClient(t, New(Options{}))
	svc := c.createVPCService("svc-p")
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01"}); st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	patch := map[string]any{"bindings": []any{map[string]any{"type": "vpc_service", "name": "P", "service_id": svc}},
		"compatibility_flags": []string{"nodejs_compat"}, "logpush": true}
	body, ct := multipartBody(t, "settings", patch, nil)
	st, env, _ := c.raw("PATCH", acct+"/workers/scripts/w/settings", ct, body)
	if st != 200 {
		t.Fatalf("patch: %d %v", st, env.Errors)
	}
	set := resultMap(t, env)
	if set["compatibility_date"] != "2026-09-01" || set["logpush"] != true ||
		len(set["bindings"].([]any)) != 1 || set["compatibility_flags"].([]any)[0] != "nodejs_compat" {
		t.Errorf("patched settings = %v", set)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts/w/versions", nil)
	if items := resultMap(t, env)["items"].([]any); len(items) != 2 {
		t.Errorf("a settings change adds a version: %v", items)
	}

	// A plain JSON body is accepted too; an unknown VPC service is rejected like on upload.
	bad := map[string]any{"bindings": []any{map[string]any{"type": "vpc_service", "name": "X", "service_id": "nope"}}}
	if st, env, _ := c.do("PATCH", acct+"/workers/scripts/w/settings", bad); st != 400 || env.Errors[0].Code != 10180 {
		t.Errorf("patch with unknown service: %d %v", st, env.Errors)
	}
	if st, env, _ := c.do("PATCH", acct+"/workers/scripts/w/settings", map[string]any{"logpush": false}); st != 200 || resultMap(t, env)["logpush"] != false {
		t.Errorf("JSON patch: %d %v", st, env.Result)
	}
}

func TestWorkerSubdomain(t *testing.T) {
	c := newClient(t, New(Options{WorkersSubdomain: "acme"}))
	if _, env, _ := c.do("GET", acct+"/workers/subdomain", nil); resultMap(t, env)["subdomain"] != "acme" {
		t.Errorf("account subdomain = %v", env.Result)
	}
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js"}); st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	if _, env, _ := c.do("GET", acct+"/workers/scripts/w/subdomain", nil); resultMap(t, env)["enabled"] != false {
		t.Errorf("new script subdomain = %v", env.Result)
	}
	st, env, h := c.do("POST", acct+"/workers/scripts/w/subdomain", map[string]any{"enabled": true, "previews_enabled": false})
	if st != 200 || resultMap(t, env)["enabled"] != true || !strings.HasPrefix(h.Get("Ratelimit"), `"workers_route_update"`) {
		t.Fatalf("enable: %d %v %q", st, env.Result, h.Get("Ratelimit"))
	}
	// Re-uploading keeps the workers.dev setting.
	c.upload("w", map[string]any{"main_module": "index.js"})
	if _, env, _ := c.do("GET", acct+"/workers/scripts/w/subdomain", nil); resultMap(t, env)["enabled"] != true {
		t.Errorf("subdomain after re-upload = %v", env.Result)
	}
	if _, env, _ := c.do("DELETE", acct+"/workers/scripts/w/subdomain", nil); resultMap(t, env)["enabled"] != false {
		t.Errorf("subdomain after delete = %v", env.Result)
	}
}

func TestWorkerMalformedUploads(t *testing.T) {
	c := newClient(t, New(Options{}))
	for name, tc := range map[string]struct {
		ct   string
		body func() []byte
	}{
		"json body": {"application/json", func() []byte { return []byte(`{"main_module":"index.js"}`) }},
		"no metadata": {"", func() []byte {
			b, _ := multipartBody(t, "other", map[string]any{}, map[string]string{"index.js": moduleSrc})
			return b
		}},
		"no main module": {"", func() []byte {
			b, _ := multipartBody(t, "metadata", map[string]any{"compatibility_date": "2026-09-01"}, map[string]string{"index.js": moduleSrc})
			return b
		}},
		"missing module part": {"", func() []byte {
			b, _ := multipartBody(t, "metadata", map[string]any{"main_module": "main.js"}, map[string]string{"index.js": moduleSrc})
			return b
		}},
		"bad metadata json": {"", func() []byte {
			b, _ := multipartBody(t, "metadata", "not an object", nil)
			return b
		}},
	} {
		body := tc.body()
		ct := tc.ct
		if ct == "" {
			ct = "multipart/form-data; boundary=" + string(bytes.TrimPrefix(bytes.SplitN(body, []byte("\r\n"), 2)[0], []byte("--")))
		}
		if st, env, _ := c.raw("PUT", acct+"/workers/scripts/w", ct, body); st != 400 || len(env.Errors) != 1 {
			t.Errorf("%s: %d %v", name, st, env.Errors)
		}
	}
	if _, env, _ := c.do("GET", acct+"/workers/scripts", nil); len(env.Result.([]any)) != 0 {
		t.Errorf("malformed uploads created scripts: %v", env.Result)
	}
}

func TestWorkerObservabilityDefaults(t *testing.T) {
	c := newClient(t, New(Options{}))
	obs := map[string]any{"enabled": true, "logs": map[string]any{"enabled": true}}
	_, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "observability": obs})
	echo := resultMap(t, env)["observability"].(map[string]any)
	if echo["head_sampling_rate"] != nil || echo["logs"].(map[string]any)["persist"] != nil {
		t.Errorf("upload echo must keep unset fields null: %v", echo)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts", nil)
	ex := env.Result.([]any)[0].(map[string]any)["observability"].(map[string]any)
	if ex["head_sampling_rate"].(float64) != 1 || ex["logs"].(map[string]any)["persist"] != true ||
		ex["traces"].(map[string]any)["enabled"] != false {
		t.Errorf("list must fill in defaults: %v", ex)
	}
}

func TestWorkerStartupTimeResets(t *testing.T) {
	s := New(Options{})
	s.SetWorkerStartupTime(7)
	c := newClient(t, s)
	if _, env, _ := c.upload("w", map[string]any{"main_module": "index.js"}); resultMap(t, env)["startup_time_ms"].(float64) != 7 {
		t.Errorf("startup_time_ms = %v", env.Result)
	}
	s.Reset()
	if _, env, _ := c.upload("w", map[string]any{"main_module": "index.js"}); resultMap(t, env)["startup_time_ms"].(float64) != workerDefaultStartupMs {
		t.Errorf("startup_time_ms after reset = %v", env.Result)
	}
}

// kin-openapi cannot decode the spec's multipart schemas; valid uploads must not be journaled as
// violations (or rejected in reject mode).
func TestWorkerMultipartPassesSpecValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the 26 MB spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Spec: spec, RejectSchemaViolations: true})
	c := newClient(t, s)
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js", "compatibility_date": "2026-09-01"}); st != 200 {
		t.Fatalf("upload in reject mode: %d %v", st, env.Errors)
	}
	body, ct := multipartBody(t, "settings", map[string]any{"logpush": true}, nil)
	if st, env, _ := c.raw("PATCH", acct+"/workers/scripts/w/settings", ct, body); st != 200 {
		t.Fatalf("settings patch in reject mode: %d %v", st, env.Errors)
	}
	for _, j := range s.Journal() {
		if j.SchemaViolation != "" {
			t.Errorf("%s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}
