package pagesdeployment

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"flare.dev/operator/internal/fake"
)

// The deployment create body keeps wrangler's encoding (form fields without a Content-Type,
// files as application/octet-stream), which the live API accepts on every `pages deploy`
// (SOURCED, relies: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/api/pages/deploy.ts#L271-L312).
// The pinned spec's multipart encoding asks for application/json and text/plain parts
// instead: with those content types the same fields validate, and the operator's body fails
// only on them.
func TestDeployFormConformsToSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := fake.LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	f := deployForm{manifest: map[string]string{"/index.html": strings.Repeat("a", 32)}, branch: "main", commit: strings.Repeat("b", 40),
		message: "m", files: map[string][]byte{"_headers": []byte("/*\n  X: y\n"), "_redirects": []byte("/a /b\n"),
			"_routes.json": []byte(`{"version":1,"include":["/*"],"exclude":[]}`), "_worker.js": []byte("export default {}")}}
	validate := func(body []byte, ct string) error {
		req, _ := http.NewRequest(http.MethodPost, "http://x/client/v4/accounts/0123456789abcdef0123456789abcdef/pages/projects/site/deployments", bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		return spec.ValidateRequest(req, body)
	}
	body, ct, err := f.encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(body, ct); err == nil || !strings.Contains(err.Error(), "not matching content types") {
		t.Fatalf("the wrangler-shaped body: %v, want only the known content-type mismatch", err)
	}
	// The same parts with the spec's content types. The manifest and _routes.json are left
	// out: the spec types them as strings (binary) with the encoding application/json, which
	// kin-openapi decodes into JSON values that the string schema then refuses, so no such part
	// validates (a defect of the spec, not of the body).
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part := func(name, file, ct string, b []byte) {
		h := textproto.MIMEHeader{}
		cd := fmt.Sprintf(`form-data; name=%q`, name)
		if file != "" {
			cd += fmt.Sprintf(`; filename=%q`, file)
		}
		h.Set("Content-Disposition", cd)
		h.Set("Content-Type", ct)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(b)
	}
	part("branch", "", "text/plain", []byte(f.branch))
	part("commit_message", "", "text/plain", []byte(f.message))
	part("commit_hash", "", "text/plain", []byte(f.commit))
	part("_headers", "_headers", "text/plain", f.files["_headers"])
	part("_redirects", "_redirects", "text/plain", f.files["_redirects"])
	part("_worker.js", "_worker.js", "application/javascript+module", f.files["_worker.js"])
	_ = w.Close()
	if err := validate(buf.Bytes(), w.FormDataContentType()); err != nil {
		t.Fatalf("the spec-typed body: %v", err)
	}
}
