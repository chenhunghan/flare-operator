package workerscript

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"testing"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
)

type part struct {
	name, file, contentType string
	data                    []byte
}

func parseMultipart(t *testing.T, body []byte, ct string) []part {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("content type %q: %v", ct, err)
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []part
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		out = append(out, part{name: p.FormName(), file: p.FileName(), contentType: p.Header.Get("Content-Type"), data: b})
	}
}

// The upload has the shape of recordings 0036/0062/0183: a metadata part (application/json)
// then one part per module named after it, ES modules as application/javascript+module.
func TestBuildMultipartUpload(t *testing.T) {
	md := apiUploadMetadata{MainModule: "index.js", apiSettingsBody: apiSettingsBody{
		CompatibilityDate: "2026-09-01",
		Bindings:          []map[string]any{{"type": "vpc_service", "name": "PRIVATE", "service_id": "svc-1"}},
	}}
	mods := []module{
		{Name: "index.js", Type: workersv1alpha1.ModuleESM, Content: []byte("export default {async fetch(){return new Response('ok')}}")},
		{Name: "lib.cjs", Type: workersv1alpha1.ModuleCommonJS, Content: []byte("module.exports = 1")},
		{Name: "a.txt", Type: workersv1alpha1.ModuleText, Content: []byte("hi")},
		{Name: "d.json", Type: workersv1alpha1.ModuleJSON, Content: []byte(`{"a":1}`)},
		{Name: "m.wasm", Type: workersv1alpha1.ModuleWasmBase64, Content: []byte{0, 'a', 's', 'm'}},
	}
	md.apiSettingsBody = md.normalized()
	body, ct, err := buildMultipart("metadata", "metadata.json", md, mods)
	if err != nil {
		t.Fatal(err)
	}
	parts := parseMultipart(t, body, ct)
	if len(parts) != 6 {
		t.Fatalf("got %d parts", len(parts))
	}
	if p := parts[0]; p.name != "metadata" || p.file != "metadata.json" || p.contentType != "application/json" {
		t.Fatalf("metadata part %+v", p)
	}
	var got map[string]any
	if err := json.Unmarshal(parts[0].data, &got); err != nil {
		t.Fatal(err)
	}
	if got["main_module"] != "index.js" || got["compatibility_date"] != "2026-09-01" {
		t.Fatalf("metadata %v", got)
	}
	if fl, ok := got["compatibility_flags"].([]any); !ok || len(fl) != 0 {
		t.Fatalf("compatibility_flags must be sent (empty list): %v", got["compatibility_flags"])
	}
	want := map[string]string{"index.js": "application/javascript+module", "lib.cjs": "application/javascript",
		"a.txt": "text/plain", "d.json": "application/json", "m.wasm": "application/wasm"}
	for _, p := range parts[1:] {
		if p.file != p.name || want[p.name] != p.contentType {
			t.Errorf("part %s: file %q content type %q", p.name, p.file, p.contentType)
		}
	}
	if !bytes.Equal(parts[5].data, []byte{0, 'a', 's', 'm'}) {
		t.Errorf("wasm bytes %v", parts[5].data)
	}
}

func TestSettingsPatchBody(t *testing.T) {
	body, ct, err := buildMultipart("settings", "settings.json", apiSettingsBody{}.normalized(), nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := parseMultipart(t, body, ct)
	if len(parts) != 1 || parts[0].name != "settings" || parts[0].contentType != "application/json" {
		t.Fatalf("parts %+v", parts)
	}
	// An empty desired state still names bindings and flags, so a PATCH clears them.
	if s := string(parts[0].data); s != `{"compatibility_flags":[],"bindings":[]}` {
		t.Fatalf("settings part %s", s)
	}
}

func TestSettingsDrift(t *testing.T) {
	yes := true
	want := apiSettingsBody{
		CompatibilityDate:  "2026-09-01",
		CompatibilityFlags: []string{"nodejs_compat"},
		Bindings: []map[string]any{
			{"type": "vpc_service", "name": "A", "service_id": "s1"},
			{"type": "secret_text", "name": "S", "text": "hunter2"},
			{"type": "json", "name": "J", "json": map[string]any{"n": 1}},
		},
		Logpush: &yes,
	}
	// As GET …/settings reports it (0065): extra fields allowed, no secret value, other order.
	got := &apiSettings{
		CompatibilityDate: "2026-09-01", CompatibilityFlags: []string{"nodejs_compat"}, Logpush: &yes,
		Bindings: []map[string]any{
			{"type": "secret_text", "name": "S"},
			{"type": "json", "name": "J", "json": map[string]any{"n": float64(1)}},
			{"type": "vpc_service", "name": "A", "service_id": "s1", "extra": "x"},
		},
	}
	if d := settingsDrift(want, got); len(d) != 0 {
		t.Fatalf("unexpected drift %v", d)
	}
	got.Bindings[2]["service_id"] = "s2"
	got.CompatibilityFlags = nil
	if d := settingsDrift(want, got); len(d) != 2 {
		t.Fatalf("drift %v", d)
	}
	got.Bindings = got.Bindings[:2]
	if d := settingsDrift(want, got); len(d) != 2 || d[1] != "bindings" {
		t.Fatalf("drift %v", d)
	}
}

func TestHashes(t *testing.T) {
	a := []module{{Name: "a.js", Type: "esm", Content: []byte("x")}}
	b := []module{{Name: "a.js", Type: "esm", Content: []byte("y")}}
	if contentHash("a.js", a) == contentHash("a.js", b) || contentHash("a.js", a) != contentHash("a.js", a) {
		t.Fatal("content hash")
	}
	s := apiSettingsBody{Bindings: []map[string]any{{"type": "secret_text", "name": "S", "text": "v1"}}}
	s2 := apiSettingsBody{Bindings: []map[string]any{{"type": "secret_text", "name": "S", "text": "v2"}}}
	if hashJSON(withoutSecretText(s)) != hashJSON(withoutSecretText(s2)) {
		t.Fatal("settings hash must not depend on secret values")
	}
	if s.Bindings[0]["text"] != "v1" {
		t.Fatal("withoutSecretText modified its input")
	}
	if secretsHash(map[string]string{"S": "v1"}) == secretsHash(map[string]string{"S": "v2"}) || secretsHash(nil) != "" {
		t.Fatal("secrets hash")
	}
}

func TestModuleType(t *testing.T) {
	for k, want := range map[string]string{"index.js": "esm", "x.mjs": "esm", "x.cjs": "cjs", "d.json": "json", "r.txt": "text", "m.wasm": "wasm-base64"} {
		if got := moduleType(k); got != want {
			t.Errorf("%s: %s, want %s", k, got, want)
		}
	}
}
