package fake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The real Hyperdrive create response (0195) omits the writeOnly password that
// hyperdrive_hyperdrive-database-full requires in an allOf sibling; OpenAPI applies such a
// required only to requests, and liftAccessRequired makes kin-openapi do so. Required fields
// that are not writeOnly still fail, and a returned writeOnly field still fails.
func TestLiftAccessRequired(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	var rec195 recording
	for _, rec := range loadRecordings(t) {
		if strings.HasPrefix(rec.file, "0195-") {
			rec195 = rec
		}
	}
	if rec195.file == "" {
		t.Fatal("no recording 0195")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://x/client/v4/accounts/a/hyperdrive/configs", nil)
	h := http.Header{"Content-Type": {"application/json"}}
	validate := func(body map[string]any) []string {
		b, _ := json.Marshal(body)
		_, errs := spec.ValidateResponse(req, 200, h, b)
		return errs
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rec195.ResponseBody), &body); err != nil {
		t.Fatal(err)
	}
	if errs := validate(body); len(errs) > 0 {
		t.Errorf("0195 violates the spec: %v", errs)
	}
	origin := body["result"].(map[string]any)["origin"].(map[string]any)
	delete(origin, "user")
	if errs := validate(body); len(errs) == 0 {
		t.Error("an origin without its required user validates")
	}
	origin["user"] = "spike"
	origin["password"] = "leak"
	if errs := validate(body); len(errs) == 0 {
		t.Error("an origin returning the writeOnly password validates")
	}
}

// The Pages asset operations require the undeclared security scheme pages_upload_token;
// declared by LoadSpec, their requests validate, and a bad body still fails.
func TestDeclareSecuritySchemes(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	if s := spec.Doc.Components.SecuritySchemes["pages_upload_token"]; s == nil || s.Value.Type != "http" {
		t.Fatalf("pages_upload_token not declared: %+v", s)
	}
	if again := declareSecuritySchemes(spec.Doc); len(again) != 0 {
		t.Fatalf("declared twice: %v", again)
	}
	check := func(body string) error {
		req, _ := http.NewRequest(http.MethodPost, "http://x/client/v4/pages/assets/check-missing", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer a.b.c")
		return spec.ValidateRequest(req, []byte(body))
	}
	if err := check(`{"hashes":["a948904f2f0f479b8f936b8a0c5d9882"]}`); err != nil {
		t.Fatalf("a valid check-missing request: %v", err)
	}
	if err := check(`{"hashes":"nope"}`); err == nil {
		t.Fatal("an invalid check-missing body validated")
	}
}
