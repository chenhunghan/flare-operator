package workerscript

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
)

// TestAPICallsAgainstSpec runs every Workers call of this controller against flarefake with
// pinned-spec request validation (multipart bodies are parsed by the Workers profile, see
// internal/fake/spec.go) and fails on a schema violation.
func TestAPICallsAgainstSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec for request validation")
	}
	spec, err := fake.LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := fake.New(fake.Options{Spec: spec})
	hs := httptest.NewServer(s)
	defer hs.Close()
	cf, err := cfclient.New(cfclient.Options{Token: "t", BaseURL: hs.URL + "/client/v4", RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const acct, name = "0123456789abcdef0123456789abcdef", "flare-spike-spec"

	yes := true
	md := apiUploadMetadata{MainModule: "index.js", apiSettingsBody: apiSettingsBody{
		CompatibilityDate: "2026-09-01",
		Bindings:          []map[string]any{{"type": "plain_text", "name": "A", "text": "x"}},
		Observability:     &apiObservability{Enabled: true, Logs: &apiObsLogs{Enabled: true, InvocationLogs: &yes}},
	}}
	mods := []module{{Name: "index.js", Type: workersv1alpha1.ModuleESM, Content: []byte("export default {\n  async fetch() { return new Response('ok') }\n};\n")}}
	up, err := uploadScript(ctx, cf, acct, name, md, mods)
	if err != nil {
		t.Fatal(err)
	}
	if up.Tag == "" || up.Etag == "" || len(up.Handlers) != 1 {
		t.Fatalf("upload result %+v", up)
	}
	if got, err := getSettings(ctx, cf, acct, name); err != nil || got == nil || len(got.Bindings) != 1 {
		t.Fatalf("settings %+v %v", got, err)
	}
	md.Bindings = nil
	if err := patchSettings(ctx, cf, acct, name, md.apiSettingsBody); err != nil {
		t.Fatal(err)
	}
	if got, err := getSettings(ctx, cf, acct, name); err != nil || len(got.Bindings) != 0 {
		t.Fatalf("settings after PATCH %+v %v", got, err)
	}
	if item, err := findScript(ctx, cf, acct, name); err != nil || item == nil || item.Tag != up.Tag {
		t.Fatalf("list %+v %v", item, err)
	}
	if d, err := activeDeployment(ctx, cf, acct, name); err != nil || d.servedVersion() == "" {
		t.Fatalf("deployment %+v %v", d, err)
	}
	if _, err := setSubdomain(ctx, cf, acct, name, apiSubdomain{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if sd, err := getSubdomain(ctx, cf, acct, name); err != nil || !sd.Enabled {
		t.Fatalf("subdomain %+v %v", sd, err)
	}
	if sub, err := accountSubdomain(ctx, cf, acct); err != nil || sub == "" {
		t.Fatalf("account subdomain %q %v", sub, err)
	}
	if err := deleteScript(ctx, cf, acct, name); err != nil {
		t.Fatal(err)
	}
	if got, err := getSettings(ctx, cf, acct, name); err != nil || got != nil {
		t.Fatalf("settings after delete %+v %v", got, err)
	}
	if err := deleteScript(ctx, cf, acct, name); !cfclient.IsNotFound(err) || !cfclient.HasCode(err, codeScriptNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	for _, j := range s.Journal() {
		if j.SchemaViolation != "" {
			t.Errorf("%s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}
