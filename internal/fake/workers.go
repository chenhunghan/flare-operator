package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Workers scripts: /accounts/{account_id}/workers/scripts and the account's workers.dev
// subdomain. Source recordings: test/recordings/2026-09-29/0001, 0002, 0036…0039, 0062, 0063,
// 0065, 0091, 0104, 0106, 0108, 0109, 0114, 0120, 0142…0145, 0183, 0184, 0197, 0211, 0214.
//
// The versions API (POST …/versions, GET …/versions/{id}, POST …/deployments), script-settings,
// the secrets list, the services and Workers-resource reads that wrangler uses are in
// workers_versions.go.
//
// Not emulated yet: tails (0064, 0073…0075, 0139…0141), Workers Observability telemetry and
// live-tail (0047, 0053, 0054, 0077…0098, 0107, 0115…0138), GET of the script content, secret
// writes and schedules.

type workerScript struct {
	Name     string
	Tag      string
	Created  time.Time
	Modified time.Time
	Seq      int64
	// DeployedFrom is last_deployed_from: "wrangler" after an upload by wrangler, else "api"
	// (see uploadSource).
	DeployedFrom string

	// workerResources is the deployed version's code and bindings (the version with the largest
	// share of the latest deployment).
	workerResources
	Observability *workerObservability
	Logpush       bool
	Tags          []string
	TailConsumers []map[string]any

	Versions    []*workerVersion    // oldest first
	Deployments []*workerDeployment // oldest first

	Subdomain workerScriptSubdomain
}

// workerResources are the per-version parts of a script: what a version upload (POST
// …/versions) sets, as opposed to the non-versioned script settings.
type workerResources struct {
	MainModule  string // "" for service-worker syntax (body_part)
	EntryPoint  string
	CompatDate  string
	CompatFlags []string
	UsageModel  string
	Bindings    []map[string]any
	Placement   map[string]any
	Handlers    []string
	Etag        string
	StartupMs   int
	Assets      *workerAssets // nil without static assets (workers_assets.go)
}

type workerVersion struct {
	ID          string
	Number      int
	Created     time.Time
	TriggeredBy string
	Source      string            // metadata.source (see uploadSource)
	Annotations map[string]string // workers/message, workers/tag as uploaded (POST …/versions)
	res         workerResources
}

type workerDeployment struct {
	ID          string
	Created     time.Time
	Message     string
	TriggeredBy string // "" = not reported (explicit deployments, UNVERIFIED)
	Source      string
	Versions    []workerTraffic
}

type workerTraffic struct {
	VersionID  string  `json:"version_id"`
	Percentage float64 `json:"percentage"`
}

type workerScriptSubdomain struct {
	Enabled         bool `json:"enabled"`
	PreviewsEnabled bool `json:"previews_enabled"`
}

type workerObsLogs struct {
	Enabled          *bool    `json:"enabled"`
	HeadSamplingRate *float64 `json:"head_sampling_rate"`
	Persist          *bool    `json:"persist"`
	InvocationLogs   *bool    `json:"invocation_logs"`
}

type workerObsTraces struct {
	Enabled          *bool    `json:"enabled"`
	HeadSamplingRate *float64 `json:"head_sampling_rate"`
	Persist          *bool    `json:"persist"`
}

type workerObservability struct {
	Enabled           *bool            `json:"enabled"`
	HeadSamplingRate  *float64         `json:"head_sampling_rate"`
	RedactQueryString *bool            `json:"redact_query_string,omitempty"`
	Logs              *workerObsLogs   `json:"logs,omitempty"`
	Traces            *workerObsTraces `json:"traces,omitempty"`
}

// workerMetadata is the "metadata" part of a script upload (and, minus the module fields, the
// "settings" part of PATCH …/settings).
type workerMetadata struct {
	MainModule         *string              `json:"main_module"`
	BodyPart           *string              `json:"body_part"`
	CompatibilityDate  *string              `json:"compatibility_date"`
	CompatibilityFlags *[]string            `json:"compatibility_flags"`
	UsageModel         *string              `json:"usage_model"`
	Bindings           *[]map[string]any    `json:"bindings"`
	Observability      *workerObservability `json:"observability"`
	Logpush            *bool                `json:"logpush"`
	Tags               *[]string            `json:"tags"`
	TailConsumers      *[]map[string]any    `json:"tail_consumers"`
	Placement          *map[string]any      `json:"placement"`
	// Version uploads (POST …/versions) only; see workers_versions.go.
	Annotations  map[string]string `json:"annotations"`
	KeepBindings []string          `json:"keep_bindings"`
	// Static assets (workers_assets.go, workers_versions.go parseAssets).
	Assets     *workerMetadataAssets `json:"assets"`
	KeepAssets *bool                 `json:"keep_assets"`
}

// workerMetadataAssets is metadata.assets: the completion token of an assets upload and how
// the assets are served (the spec's workers_assets-2).
type workerMetadataAssets struct {
	JWT    *string        `json:"jwt"`
	Config map[string]any `json:"config"`
}

const (
	// Sanitized placeholders the recordings use for the token owner (0038, 0039). The emulator
	// reports them verbatim so replayed recordings match.
	workerAuthorID    = "USER_ID"
	workerAuthorEmail = "user@example.com"

	workerTriggeredUpload   = "upload"                                   // 0038, 0039, 0065
	workerMessageUpload     = "Automatic deployment on upload."          // 0039
	workerTriggeredSettings = "settings"                                 // UNVERIFIED: settings PATCH not recorded
	workerMessageSettings   = "Automatic deployment on settings update." // UNVERIFIED: settings PATCH not recorded
	workerDefaultUsageModel = "standard"                                 // 0036
	workerDefaultStartupMs  = 1                                          // 0036, 0183 (0062 and 0104 report 2)
	workerVPCDocsURL        = "https://developers.cloudflare.com/workers-vpc/get-started/"
)

// workerHandlerRe finds handler methods of the exported object: a method definition
// (`async fetch(req) {`) or a property (`fetch: async (req) =>`, `fetch: function`) at the start
// of a line or right after `{` or `,`, which also covers one-line modules such as
// `export default { async fetch() {…} }` and esbuild's `var src_default = { async fetch(…`.
// Calls like `env.X.fetch(url)` do not match (a `.` precedes them). Real handler detection
// runs the script; this heuristic is UNVERIFIED beyond the fetch-only scripts recorded (0036,
// 0062, 0183: handlers [fetch]).
var workerHandlerRe = regexp.MustCompile(`(?m)(?:^|[{,])\s*(?:async\s+)?(fetch|scheduled|queue|email|tail|trace|test)\s*(?:\(|:\s*(?:async\b\s*)?(?:function\b|\(|[A-Za-z_$][\w$]*\s*=>))`)

// workerNotFound is Workers' missing-script error (0109, 0143, 0211: GET …/settings). The code
// and message are the API's for a missing Worker on any route, per wrangler: SOURCED
// cloudflare/workers-sdk@485cfb3:packages/deploy-helpers/src/deploy/helpers/worker-not-found-error.ts#L1-L15
// (a statement, not route-specific). Where a route's status is not recorded, it stays marked.
func workerNotFound() response {
	return fail(http.StatusNotFound, 10007, "This Worker does not exist on your account.") // 0109, 0143, 0211
}

func (s *Server) registerWorkers() {
	s.handle(http.MethodGet, "/accounts/{account_id}/workers/subdomain", workerAccountSubdomainGet)
	base := "/accounts/{account_id}/workers/scripts"
	s.handle(http.MethodGet, base, workerList)
	s.handle(http.MethodPut, base+"/{script_name}", workerUpload)
	s.handle(http.MethodDelete, base+"/{script_name}", workerDelete)
	s.handle(http.MethodGet, base+"/{script_name}/settings", workerSettingsGet)
	s.handle(http.MethodPatch, base+"/{script_name}/settings", workerSettingsPatch)
	s.handle(http.MethodGet, base+"/{script_name}/versions", workerVersionsList)
	s.handle(http.MethodGet, base+"/{script_name}/deployments", workerDeploymentsList)
	s.handle(http.MethodGet, base+"/{script_name}/subdomain", workerSubdomainGet)
	s.handle(http.MethodPost, base+"/{script_name}/subdomain", workerSubdomainPost)
	s.handle(http.MethodDelete, base+"/{script_name}/subdomain", workerSubdomainDelete)
	s.registerWorkerVersions()
	s.registerWorkerAssets()
}

// SetWorkerStartupTime sets the startup_time_ms reported by subsequent script uploads. The real
// value is a measurement (1 or 2 ms in recordings 0036, 0062, 0104, 0183); tests replaying a
// recording set it so the response matches exactly.
func (s *Server) SetWorkerStartupTime(ms int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerStartupMs = ms
}

// ---- JSON shapes -----------------------------------------------------------------------------

func nullIfEmpty[T any](v []T) any {
	if v == nil {
		return nil
	}
	return v
}

func emptyIfNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// uploadEcho is observability as echoed by the upload response: the given fields, with the
// nullable log fields present as null (0036).
func (o *workerObservability) uploadEcho() map[string]any {
	m := map[string]any{"enabled": o.Enabled, "head_sampling_rate": o.HeadSamplingRate}
	if o.Logs != nil {
		m["logs"] = o.Logs // all four keys, nulls included (0036)
	}
	if o.Traces != nil {
		m["traces"] = o.Traces // UNVERIFIED: traces not recorded in an upload
	}
	if o.RedactQueryString != nil {
		m["redact_query_string"] = *o.RedactQueryString // UNVERIFIED
	}
	return m
}

// expanded is observability with server defaults filled in, as the script list reports it (0114,
// 0120). Defaults for fields the recorded script set explicitly (enabled, logs.enabled,
// logs.invocation_logs) and the defaults used when logs is omitted are UNVERIFIED.
func (o *workerObservability) expanded() map[string]any {
	b := func(p *bool, def bool) bool {
		if p != nil {
			return *p
		}
		return def
	}
	f := func(p *float64, def float64) float64 {
		if p != nil {
			return *p
		}
		return def
	}
	logs := o.Logs
	if logs == nil {
		logs = &workerObsLogs{}
	}
	traces := o.Traces
	if traces == nil {
		traces = &workerObsTraces{}
	}
	return map[string]any{
		"enabled":             b(o.Enabled, false),
		"head_sampling_rate":  f(o.HeadSamplingRate, 1),
		"redact_query_string": b(o.RedactQueryString, false), // 0114
		"logs": map[string]any{
			"enabled":            b(logs.Enabled, false),
			"head_sampling_rate": f(logs.HeadSamplingRate, 1), // 0114: null in upload → 1
			"persist":            b(logs.Persist, true),       // 0114: null in upload → true
			"invocation_logs":    b(logs.InvocationLogs, true),
		},
		"traces": map[string]any{ // 0114: absent in upload → these defaults
			"enabled":            b(traces.Enabled, false),
			"persist":            b(traces.Persist, true),
			"head_sampling_rate": f(traces.HeadSamplingRate, 1),
		},
	}
}

// common is the part shared by the upload response and the list item (0036, 0114).
func (w *workerScript) common() map[string]any {
	m := map[string]any{
		"created_on": tsMicro(w.Created), "modified_on": tsMicro(w.Modified),
		"id": w.Name, "tag": w.Tag,
		"tags":               nullIfEmpty(w.Tags),          // null when never set (0036); UNVERIFIED when set
		"tail_consumers":     nullIfEmpty(w.TailConsumers), // null when never set (0036); UNVERIFIED when set
		"logpush":            w.Logpush,
		"has_assets":         w.Assets != nil,    // false without assets (0036); true with them UNVERIFIED
		"has_modules":        w.MainModule != "", // true for module syntax (0036); false for body_part UNVERIFIED
		"etag":               w.Etag,
		"handlers":           emptyIfNil(w.Handlers),
		"last_deployed_from": w.DeployedFrom, // "api" in 0036, 0114; see uploadSource
		"compatibility_date": w.CompatDate,
		"usage_model":        w.UsageModel,
	}
	return m
}

// uploadJSON is the PUT response (0036, 0062, 0104, 0183). deployment_id is the new version's ID
// without dashes (0036 vs 0038).
func (w *workerScript) uploadJSON() map[string]any {
	m := w.common()
	m["entry_point"] = w.EntryPoint
	m["deployment_id"] = strings.ReplaceAll(w.currentVersion().ID, "-", "")
	m["startup_time_ms"] = w.StartupMs
	if w.Observability != nil {
		m["observability"] = w.Observability.uploadEcho() // absent when not set (0062)
	}
	return m
}

// listJSON is one item of GET …/workers/scripts (0114, 0120): no entry_point or startup_time_ms,
// an empty deployment_id, routes null and observability with defaults filled in.
func (w *workerScript) listJSON() map[string]any {
	m := w.common()
	m["deployment_id"] = ""
	m["routes"] = nil
	if w.Observability != nil {
		m["observability"] = w.Observability.expanded()
	}
	// UNVERIFIED: list shape of a script without observability (only 0114/0120's script, which
	// had it, was listed); the key is omitted like in the upload response (0062).
	return m
}

// settingsJSON is GET …/settings (0065, 0091).
func (w *workerScript) settingsJSON() map[string]any {
	placement := w.Placement
	if placement == nil {
		placement = map[string]any{}
	}
	m := map[string]any{
		"placement":           placement,
		"compatibility_date":  w.CompatDate,
		"compatibility_flags": emptyIfNil(w.CompatFlags),
		"usage_model":         w.UsageModel,
		"tags":                emptyIfNil(w.Tags),          // [] here, null in the upload response
		"tail_consumers":      emptyIfNil(w.TailConsumers), // [] here, null in the upload response
		"logpush":             w.Logpush,
		"annotations":         map[string]any{"workers/triggered_by": w.deployedVersion().TriggeredBy},
		"bindings":            readBindings(w.Bindings), // echoed as uploaded (0065), secrets withheld
	}
	if w.Observability != nil {
		m["observability"] = w.Observability.expanded() // UNVERIFIED: not recorded in settings
	}
	return m
}

func (w *workerScript) currentVersion() *workerVersion { return w.Versions[len(w.Versions)-1] }

// deployedVersion is the version with the largest share of the latest deployment (the only one
// after an upload). Which version a split deployment reports in settings is UNVERIFIED.
func (w *workerScript) deployedVersion() *workerVersion {
	d := w.Deployments[len(w.Deployments)-1]
	best := d.Versions[0]
	for _, t := range d.Versions[1:] {
		if t.Percentage > best.Percentage {
			best = t
		}
	}
	for _, v := range w.Versions {
		if v.ID == best.VersionID {
			return v
		}
	}
	return w.currentVersion()
}

func (v *workerVersion) json() map[string]any {
	ann := map[string]any{"workers/triggered_by": v.TriggeredBy}
	for k, val := range v.Annotations {
		ann[k] = val
	}
	return map[string]any{
		"id": v.ID, "number": v.Number,
		"metadata": map[string]any{
			"created_on": tsMicro(v.Created), "source": v.Source,
			"author_id": workerAuthorID, "author_email": workerAuthorEmail,
			"has_preview": true, // 0038 (even with previews_enabled false)
		},
		"annotations": ann,
	}
}

func (d *workerDeployment) json() map[string]any {
	ann := map[string]any{}
	if d.Message != "" {
		ann["workers/message"] = d.Message
	}
	if d.TriggeredBy != "" {
		ann["workers/triggered_by"] = d.TriggeredBy
	}
	versions := make([]any, 0, len(d.Versions))
	for _, t := range d.Versions {
		versions = append(versions, map[string]any{"version_id": t.VersionID, "percentage": t.Percentage})
	}
	return map[string]any{
		"id": d.ID, "source": d.Source, "strategy": "percentage", "author_email": workerAuthorEmail,
		"annotations": ann, "versions": versions, "created_on": tsMicro(d.Created),
	}
}

// ---- handlers --------------------------------------------------------------------------------

// workerAccountSubdomainGet: GET /accounts/{id}/workers/subdomain (0001). Accounts without a
// workers.dev subdomain are not emulated (every account has one); wrangler's fixture for that
// case answers code 10007 (packages/wrangler/src/__tests__/helpers/mock-workers-subdomain.ts#L28,
// a mock), so the real answer stays UNVERIFIED.
func workerAccountSubdomainGet(c *reqCtx) response {
	return ok(map[string]any{"subdomain": c.s.opts.WorkersSubdomain})
}

// workerList: GET …/workers/scripts — no result_info (0002, 0114). Ordering UNVERIFIED (only
// zero- or one-item lists recorded); creation order is used.
func workerList(c *reqCtx) response {
	out := []any{}
	for _, w := range sortedBySeq(c.account.scripts, func(w *workerScript) int64 { return w.Seq }) {
		out = append(out, w.listJSON())
	}
	return ok(out)
}

// readParts parses a multipart/form-data body into part name → content. Error codes for
// malformed uploads are UNVERIFIED (not recorded); 10021 is Workers' script-validation code
// (SOURCED fixtures: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/__tests__/deploy/build.test.ts#L1044,
// packages/wrangler/src/__tests__/deploy/entry-points.test.ts#L742; they show other messages).
func readParts(c *reqCtx) (map[string][]byte, *response) {
	bad := func(msg string) (map[string][]byte, *response) {
		r := fail(http.StatusBadRequest, 10021, msg) // UNVERIFIED
		return nil, &r
	}
	mt, params, err := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return bad("Expected a multipart/form-data request body.")
	}
	mr := multipart.NewReader(bytes.NewReader(c.body), params["boundary"])
	parts := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bad("Malformed multipart body: " + err.Error())
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return bad("Malformed multipart body: " + err.Error())
		}
		parts[p.FormName()] = data
	}
	return parts, nil
}

func decodeMetadata(raw []byte, into *workerMetadata) *response {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(into); err != nil {
		r := fail(http.StatusBadRequest, 10021, "Malformed metadata: "+err.Error()) // UNVERIFIED
		return &r
	}
	return nil
}

// bindingSecretFields are the binding fields the settings read never returns: the spec marks
// them writeOnly (workers_binding_kind_secret_text.text, workers_binding_kind_secret_key
// .key_base64/.key_jwk), and a response carrying them violates the spec. UNVERIFIED: no
// recording reads back a secret binding (0065/0091 have vpc_service bindings only); the
// operator already treats secret_text values as never returned (workerscript drift.go).
var bindingSecretFields = map[string][]string{
	"secret_text": {"text"},
	"secret_key":  {"key_base64", "key_jwk"},
}

// readBindings is bindings as the settings read reports them: as uploaded (0065), minus
// bindingSecretFields; never null.
func readBindings(bindings []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(bindings))
	for _, b := range bindings {
		t, _ := b["type"].(string)
		drop := bindingSecretFields[t]
		c := make(map[string]any, len(b))
		for k, v := range b {
			if !slices.Contains(drop, k) {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// validateBindings checks vpc_service bindings against the account's VPC services (0106: the
// first unknown service_id fails the whole upload with 400/10180). Other binding types are
// stored unvalidated (UNVERIFIED).
func validateBindings(a *account, bindings []map[string]any) *response {
	for _, b := range bindings {
		if b["type"] != "vpc_service" {
			continue
		}
		id, _ := b["service_id"].(string)
		if _, found := a.vpc[id]; !found {
			r := fail(http.StatusBadRequest, 10180, fmt.Sprintf("VPC Service '%s' not found. Verify the service exists in your account and that the service_id in your configuration is correct.", id))
			r.errors[0].DocumentationURL = workerVPCDocsURL // 0106
			return &r
		}
	}
	return nil
}

// newVersion adds a version (not deployed) and consumes its ID.
func (c *reqCtx) newVersion(w *workerScript, triggeredBy string) *workerVersion {
	n := 1
	if len(w.Versions) > 0 {
		n = w.currentVersion().Number + 1 // UNVERIFIED: only version 1 recorded
	}
	v := &workerVersion{ID: c.s.ids.next(uuidV4), Number: n, Created: c.now, TriggeredBy: triggeredBy, Source: uploadSource(c)}
	w.Versions = append(w.Versions, v)
	return v
}

// newVersionAndDeployment adds a version and a 100% deployment of it — every upload does this
// implicitly (0038, 0039). IDs are consumed version first, then deployment. The caller sets the
// version's resources (v.res) once they are final.
func (c *reqCtx) newVersionAndDeployment(w *workerScript, triggeredBy, message string) *workerVersion {
	v := c.newVersion(w, triggeredBy)
	d := &workerDeployment{ID: c.s.ids.next(uuidV4), Created: c.now, Message: message, TriggeredBy: triggeredBy,
		Source: v.Source, Versions: []workerTraffic{{VersionID: v.ID, Percentage: 100}}}
	w.Deployments = append(w.Deployments, d)
	return v
}

// uploadSource is the source the API reports for a version, deployment and last_deployed_from:
// "api" for the recorded uploads (0036, 0038, 0039, 0114, made with a plain HTTP client), and
// "wrangler" when wrangler uploads. SOURCED (statement): wrangler deploy warns that a Worker
// "was last updated via the script API" when last_deployed_from is "api",
// wrangler@4.143.0:wrangler-dist/cli.js#L174999-L175001, so its own uploads must report
// something else, and `versions list` knows the source "wrangler" (cli.js#L353249-L353262).
// That the API tells them apart by the User-Agent is UNVERIFIED.
func uploadSource(c *reqCtx) string {
	if strings.HasPrefix(c.r.Header.Get("User-Agent"), "wrangler/") {
		return "wrangler"
	}
	return "api"
}

// workerUpload: PUT …/workers/scripts/{name}, multipart with a "metadata" part and one part per
// module (0036, 0062, 0104, 0183). A re-upload keeps tag and created_on and replaces everything
// else (0104). Server-assigned values are consumed from the ID source in this order: tag (new
// scripts only), version ID, deployment ID, etag.
func workerUpload(c *reqCtx) response {
	name := c.params["script_name"]
	w, exists := c.account.scripts[name]
	up, r := parseScriptUpload(c, w)
	if r != nil {
		return *r
	}
	md := up.md
	if !exists {
		w = &workerScript{Name: name, Tag: c.s.ids.next(hex32), Created: c.now, Seq: c.s.nextSeq()}
	}
	w.Modified = c.now // a re-upload advances modified_on and keeps created_on (0104)
	w.DeployedFrom = uploadSource(c)
	w.workerResources = up.res
	w.Observability = md.Observability
	w.Logpush = md.Logpush != nil && *md.Logpush // false when omitted (0036)
	w.Tags, w.TailConsumers = derefSlice(md.Tags), derefSlice(md.TailConsumers)
	w.StartupMs = c.s.workerStartupMs
	v := c.newVersionAndDeployment(w, workerTriggeredUpload, workerMessageUpload)
	w.Etag = up.etag(c)
	v.res = w.workerResources
	c.account.scripts[name] = w
	return ok(w.uploadJSON())
}

// workerDelete: DELETE …/workers/scripts/{name}[?force=true] returns the script's tag (0108,
// 0142, 0197). force is accepted and ignored; dependency checks (Durable Object namespaces,
// service bindings from other scripts) are not emulated. Missing script: UNVERIFIED (10007 as
// recorded for settings).
func workerDelete(c *reqCtx) response {
	name := c.params["script_name"]
	w, found := c.account.scripts[name]
	if !found {
		return workerNotFound()
	}
	delete(c.account.scripts, name)
	// The script's uploaded assets and upload sessions go with it (UNVERIFIED).
	delete(c.account.assets, name)
	for id, sess := range c.account.assetSessions {
		if sess.Script == name {
			delete(c.account.assetSessions, id)
		}
	}
	return ok(map[string]any{"id": w.Tag})
}

func workerSettingsGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // 0109, 0143, 0211
	}
	return ok(w.settingsJSON())
}

// workerSettingsPatch: PATCH …/settings merges the given fields. No recording covers this route,
// so it is entirely UNVERIFIED. The pinned spec declares only a multipart/form-data body with a
// JSON "settings" part, so any other content type (a plain JSON body included) is rejected by
// readParts with 400/10021 (UNVERIFIED: no recording shows the API's answer to a wrong content
// type). A change creates a new version deployed at 100%, like an upload, and the response is
// the settings object (both UNVERIFIED).
func workerSettingsPatch(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route (10007 recorded for GET: 0109, 0143, 0211)
	}
	parts, r := readParts(c)
	if r != nil {
		return *r
	}
	raw, found := parts["settings"]
	if !found {
		return fail(http.StatusBadRequest, 10021, "Missing settings part.") // UNVERIFIED
	}
	var md workerMetadata
	if r := decodeMetadata(raw, &md); r != nil {
		return *r
	}
	if md.Bindings != nil {
		// 400/10180 for an unknown VPC service is recorded for upload (0106); UNVERIFIED here.
		if r := validateBindings(c.account, *md.Bindings); r != nil {
			return *r
		}
		if r := validateAssetsBinding(*md.Bindings, w.Assets != nil); r != nil {
			return *r
		}
		w.Bindings = *md.Bindings
	}
	if md.CompatibilityDate != nil {
		w.CompatDate = *md.CompatibilityDate
	}
	if md.CompatibilityFlags != nil {
		w.CompatFlags = *md.CompatibilityFlags
	}
	if md.UsageModel != nil && *md.UsageModel != "" {
		w.UsageModel = *md.UsageModel
	}
	if md.Observability != nil {
		w.Observability = md.Observability
	}
	if md.Logpush != nil {
		w.Logpush = *md.Logpush
	}
	if md.Tags != nil {
		w.Tags = *md.Tags
	}
	if md.TailConsumers != nil {
		w.TailConsumers = *md.TailConsumers
	}
	if md.Placement != nil {
		w.Placement = *md.Placement
	}
	w.Modified = c.now // UNVERIFIED
	v := c.newVersionAndDeployment(w, workerTriggeredSettings, workerMessageSettings)
	v.res = w.workerResources
	return ok(w.settingsJSON())
}

// workerVersionsList: GET …/versions → {"items": […]} with page/per_page/count/total_count (no
// total_pages), per_page 10 by default, and null errors/messages (0038). Newest-first ordering
// is UNVERIFIED (one version recorded; wrangler sorts client-side and its fixture,
// packages/wrangler/src/__tests__/helpers/msw/handlers/versions.ts#L214, lists oldest first).
func workerVersionsList(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route
	}
	newest := make([]*workerVersion, 0, len(w.Versions))
	for i := len(w.Versions) - 1; i >= 0; i-- {
		newest = append(newest, w.Versions[i])
	}
	// page/per_page query handling is UNVERIFIED (only the default first page is recorded).
	// With deployable=true the API ignores pagination. SOURCED (a statement about the API):
	// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/versions/list.ts#L57. Every emulated
	// version is deployable; the result_info then reported is UNVERIFIED.
	pageItems, page, perPage, _ := paginate(newest, c.intQuery("page", 1), c.intQuery("per_page", 10), 10)
	if c.query.Get("deployable") == "true" {
		pageItems, page = newest, 1
	}
	items := make([]any, 0, len(pageItems))
	for _, v := range pageItems {
		items = append(items, v.json())
	}
	return okList(map[string]any{"items": items},
		PageInfo{Page: intp(page), PerPage: intp(perPage), Count: len(items), TotalCount: intp(len(newest))}).
		withStyle(styleNullErrorsMessages)
}

// workerDeploymentsList: GET …/deployments → {"deployments": […]} with a full page-based
// result_info (0039). Newest first: SOURCED (relies) wrangler takes deployments.at(0) as the
// latest, cloudflare/workers-sdk@485cfb3:packages/deploy-helpers/src/deploy/helpers/versions-api.ts#L83.
// The 10-item cap is UNVERIFIED (one deployment recorded).
func workerDeploymentsList(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		// SOURCED (relies): a first deploy tolerates exactly the 10007 not-found error here,
		// cloudflare/workers-sdk@485cfb3:packages/deploy-helpers/src/deploy/helpers/confirm-latest-deployment-overwrite.ts#L43-L72.
		return workerNotFound()
	}
	const perPage = 10
	out := []any{}
	for i := len(w.Deployments) - 1; i >= 0 && len(out) < perPage; i-- {
		out = append(out, w.Deployments[i].json())
	}
	total := len(w.Deployments)
	return okList(map[string]any{"deployments": out},
		PageInfo{Page: intp(1), PerPage: intp(perPage), Count: len(out), TotalCount: intp(total), TotalPages: intp((total + perPage - 1) / perPage)})
}

// workerSubdomainPost: POST …/subdomain {enabled, previews_enabled} echoes the new state (0037,
// 0063, 0184). An omitted previews_enabled follows enabled. SOURCED:
// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/__tests__/helpers/mock-workers-subdomain.ts#L92
// (fixture, commented "Mimics API behavior"). An omitted enabled keeps its current value
// (UNVERIFIED; wrangler always sends it).
func workerSubdomainPost(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route
	}
	var req struct {
		Enabled         *bool `json:"enabled"`
		PreviewsEnabled *bool `json:"previews_enabled"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.Enabled != nil {
		w.Subdomain.Enabled = *req.Enabled
	}
	switch {
	case req.PreviewsEnabled != nil:
		w.Subdomain.PreviewsEnabled = *req.PreviewsEnabled
	case req.Enabled != nil:
		w.Subdomain.PreviewsEnabled = *req.Enabled
	}
	return ok(w.Subdomain)
}

// workerSubdomainGet is UNVERIFIED (not recorded): same shape as the POST result, as wrangler's
// fixture for the sibling services/{name}/environments/{env}/subdomain route shows
// (packages/wrangler/src/__tests__/deploy/helpers.ts#L735-L758, a mock of another route). A new
// script starts with workers.dev disabled (UNVERIFIED).
func workerSubdomainGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	return ok(w.Subdomain)
}

// workerSubdomainDelete is UNVERIFIED (not recorded): disables both and returns the new state.
func workerSubdomainDelete(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	w.Subdomain = workerScriptSubdomain{}
	return ok(w.Subdomain)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
