package workerscript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// Workers script API calls (x-fern-sdk-group-name "workers.legacy.scripts"). Shapes follow the
// recordings: upload 0036/0062/0104/0183 (multipart, a "metadata" JSON part and one part per
// module), 0106 (400/10180 for an unknown VPC service), settings 0065/0091, versions 0038,
// deployments 0039, per-script subdomain 0037/0063/0184, account subdomain 0001, list
// 0002/0114/0120, delete 0108/0142/0197 and 404/10007 0109/0143/0211.

// Content types of the module parts. application/javascript+module is recorded (0036, 0062,
// 0183); the others are from the pinned spec's list of part content types, except
// application/json (UNVERIFIED: not in that list).
var moduleContentTypes = map[string]string{
	workersv1alpha1.ModuleESM:        "application/javascript+module",
	workersv1alpha1.ModuleCommonJS:   "application/javascript",
	workersv1alpha1.ModuleText:       "text/plain",
	workersv1alpha1.ModuleJSON:       "application/json",
	workersv1alpha1.ModuleWasmBase64: "application/wasm",
	workersv1alpha1.ModuleWasm:       "application/wasm",
}

// codeScriptNotFound is Workers' "This Worker does not exist on your account." (0109).
const codeScriptNotFound = 10007

func scriptsPath(accountID string) string { return "/accounts/" + accountID + "/workers/scripts" }

func scriptPath(accountID, name string) string {
	return scriptsPath(accountID) + "/" + url.PathEscape(name)
}

// apiObsLogs / apiObservability are metadata.observability (0036).
type apiObsLogs struct {
	Enabled          bool         `json:"enabled"`
	InvocationLogs   *bool        `json:"invocation_logs,omitempty"`
	HeadSamplingRate *json.Number `json:"head_sampling_rate,omitempty"`
}

type apiObservability struct {
	Enabled          bool         `json:"enabled"`
	HeadSamplingRate *json.Number `json:"head_sampling_rate,omitempty"`
	Logs             *apiObsLogs  `json:"logs,omitempty"`
}

// apiSettingsBody is the part of the upload metadata that is also a settings PATCH body. In a
// PATCH an omitted field is left unchanged, so bindings and compatibility_flags are always sent
// (an empty list removes them).
type apiSettingsBody struct {
	CompatibilityDate  string            `json:"compatibility_date,omitempty"`
	CompatibilityFlags []string          `json:"compatibility_flags"`
	Bindings           []map[string]any  `json:"bindings"`
	Observability      *apiObservability `json:"observability,omitempty"`
	Logpush            *bool             `json:"logpush,omitempty"`
}

// normalized returns s with nil lists replaced by empty ones (sent as [], never null).
func (s apiSettingsBody) normalized() apiSettingsBody {
	if s.CompatibilityFlags == nil {
		s.CompatibilityFlags = []string{}
	}
	if s.Bindings == nil {
		s.Bindings = []map[string]any{}
	}
	return s
}

// apiUploadMetadata is the upload's "metadata" part (0036, 0062, 0183). An assets-only Worker
// has no main_module (assets.go).
type apiUploadMetadata struct {
	MainModule string `json:"main_module,omitempty"`
	apiSettingsBody
	// Assets redeems the completion token of an assets upload; KeepAssets keeps the deployed
	// version's assets instead (assets.go).
	Assets     *apiAssets `json:"assets,omitempty"`
	KeepAssets *bool      `json:"keep_assets,omitempty"`
}

// apiScript is the upload response (0036) and a script list item (0114).
type apiScript struct {
	ID         string   `json:"id"`
	Tag        string   `json:"tag"`
	Etag       string   `json:"etag"`
	Handlers   []string `json:"handlers"`
	CreatedOn  string   `json:"created_on"`
	ModifiedOn string   `json:"modified_on"`
	// DeploymentID of the upload response is the new version's ID without dashes (0036 vs 0038).
	DeploymentID      string `json:"deployment_id"`
	CompatibilityDate string `json:"compatibility_date"`
	UsageModel        string `json:"usage_model"`
	// HasAssets: the script has static assets (0036: false; true UNVERIFIED).
	HasAssets bool `json:"has_assets"`
}

// apiSettings is GET …/settings (0065, 0091): bindings as uploaded, compatibility settings.
type apiSettings struct {
	CompatibilityDate  string           `json:"compatibility_date"`
	CompatibilityFlags []string         `json:"compatibility_flags"`
	UsageModel         string           `json:"usage_model"`
	Logpush            *bool            `json:"logpush"`
	Bindings           []map[string]any `json:"bindings"`
	// Observability is not in the recorded settings responses (the scripts had none, or it was
	// not echoed): nil means "not reported" and is not compared (UNVERIFIED).
	Observability map[string]any `json:"observability"`
}

type apiDeployment struct {
	ID       string `json:"id"`
	Versions []struct {
		VersionID  string  `json:"version_id"`
		Percentage float64 `json:"percentage"`
	} `json:"versions"`
}

type apiSubdomain struct {
	Enabled         bool `json:"enabled"`
	PreviewsEnabled bool `json:"previews_enabled"`
}

// module is one resolved module part.
type module struct {
	Name, Type string
	Content    []byte
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// buildMultipart writes a multipart/form-data body with one JSON part (jsonName, filename
// jsonFile, application/json) followed by the modules (part and file name = module name).
func buildMultipart(jsonName, jsonFile string, jsonValue any, modules []module) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	raw, err := json.Marshal(jsonValue)
	if err != nil {
		return nil, "", err
	}
	part := func(name, file, contentType string, data []byte) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscaper.Replace(name), quoteEscaper.Replace(file)))
		h.Set("Content-Type", contentType)
		pw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = pw.Write(data)
		return err
	}
	if err := part(jsonName, jsonFile, "application/json", raw); err != nil {
		return nil, "", err
	}
	for _, m := range modules {
		ct, ok := moduleContentTypes[m.Type]
		if !ok {
			return nil, "", fmt.Errorf("module %s: unknown type %q", m.Name, m.Type)
		}
		if err := part(m.Name, m.Name, ct, m.Content); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// uploadScript PUTs the script (a create or a full replace). Each upload creates a version and
// deploys it at 100% (0038, 0039).
func uploadScript(ctx context.Context, cf cfclient.Client, accountID, name string, md apiUploadMetadata, modules []module) (*apiScript, error) {
	md.apiSettingsBody = md.normalized()
	body, ct, err := buildMultipart("metadata", "metadata.json", md, modules)
	if err != nil {
		return nil, err
	}
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPut, Path: scriptPath(accountID, name), RawBody: body, ContentType: ct})
	if err != nil {
		return nil, err
	}
	var s apiScript
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return nil, fmt.Errorf("decode upload result: %w", err)
	}
	return &s, nil
}

// patchSettings sends PATCH …/settings with a multipart "settings" part, the only body the
// pinned spec allows (the route itself is UNVERIFIED: not recorded).
func patchSettings(ctx context.Context, cf cfclient.Client, accountID, name string, s apiSettingsBody) error {
	body, ct, err := buildMultipart("settings", "settings.json", s.normalized(), nil)
	if err != nil {
		return err
	}
	_, err = cf.Do(ctx, cfclient.Request{Method: http.MethodPatch, Path: scriptPath(accountID, name) + "/settings", RawBody: body, ContentType: ct})
	return err
}

// getSettings returns the script's settings, or nil when the script does not exist (404/10007,
// 0109).
func getSettings(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiSettings, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: scriptPath(accountID, name) + "/settings"})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var s apiSettings
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return nil, fmt.Errorf("decode script settings: %w", err)
	}
	return &s, nil
}

// findScript returns the list item of script name (tag, etag, handlers), or nil. The list is
// one unpaginated array (0002, 0114).
func findScript(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiScript, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: scriptsPath(accountID)})
	if err != nil {
		return nil, err
	}
	var list []apiScript
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		return nil, fmt.Errorf("decode script list: %w", err)
	}
	for i := range list {
		if list[i].ID == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// activeDeployment returns the newest deployment (0039; newest-first ordering UNVERIFIED), or
// nil when there is none or the script is gone.
func activeDeployment(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiDeployment, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: scriptPath(accountID, name) + "/deployments"})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var r struct {
		Deployments []apiDeployment `json:"deployments"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("decode deployments: %w", err)
	}
	if len(r.Deployments) == 0 {
		return nil, nil
	}
	return &r.Deployments[0], nil
}

// servedVersion is the version a deployment serves at 100% ("" for a split deployment).
func (d *apiDeployment) servedVersion() string {
	if d == nil || len(d.Versions) != 1 {
		return ""
	}
	return d.Versions[0].VersionID
}

// getSubdomain reads the script's workers.dev route (UNVERIFIED: GET not recorded; same shape
// as the POST result, 0037).
func getSubdomain(ctx context.Context, cf cfclient.Client, accountID, name string) (*apiSubdomain, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: scriptPath(accountID, name) + "/subdomain"})
	if err != nil {
		return nil, err
	}
	var s apiSubdomain
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return nil, fmt.Errorf("decode subdomain: %w", err)
	}
	return &s, nil
}

// setSubdomain POSTs {enabled, previews_enabled} (0037, 0063, 0184).
func setSubdomain(ctx context.Context, cf cfclient.Client, accountID, name string, s apiSubdomain) (*apiSubdomain, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: scriptPath(accountID, name) + "/subdomain", Body: s})
	if err != nil {
		return nil, err
	}
	var out apiSubdomain
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("decode subdomain: %w", err)
	}
	return &out, nil
}

// accountSubdomain returns the account's workers.dev subdomain (0001).
func accountSubdomain(ctx context.Context, cf cfclient.Client, accountID string) (string, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + accountID + "/workers/subdomain"})
	if err != nil {
		return "", err
	}
	var s struct {
		Subdomain string `json:"subdomain"`
	}
	if err := json.Unmarshal(resp.Result, &s); err != nil {
		return "", fmt.Errorf("decode account subdomain: %w", err)
	}
	return s.Subdomain, nil
}

// deleteScript deletes the script (0108; no force, so Cloudflare's own dependency checks apply).
// 404/10007 means it is already gone (the caller treats a 404 as success).
func deleteScript(ctx context.Context, cf cfclient.Client, accountID, name string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: scriptPath(accountID, name)})
	return err
}
