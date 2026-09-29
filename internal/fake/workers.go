package fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Workers scripts: /accounts/{account_id}/workers/scripts and the account's workers.dev
// subdomain. Source recordings: test/recordings/2026-09-29/0001, 0002, 0036…0039, 0062, 0063,
// 0065, 0091, 0104, 0106, 0108, 0109, 0114, 0120, 0142…0145, 0183, 0184, 0197, 0211, 0214.
//
// Not emulated yet: tails (0064, 0073…0075, 0139…0141), Workers Observability telemetry and
// live-tail (0047, 0053, 0054, 0077…0098, 0107, 0115…0138), GET of the script content, secrets,
// schedules, script-settings, version upload (POST /versions) and gradual deployments.

type workerScript struct {
	Name     string
	Tag      string
	Created  time.Time
	Modified time.Time
	Seq      int64

	MainModule    string // "" for service-worker syntax (body_part)
	EntryPoint    string
	CompatDate    string
	CompatFlags   []string
	UsageModel    string
	Bindings      []map[string]any
	Observability *workerObservability
	Logpush       bool
	Tags          []string
	TailConsumers []map[string]any
	Placement     map[string]any
	Handlers      []string
	Etag          string
	StartupMs     int

	Versions    []*workerVersion    // oldest first
	Deployments []*workerDeployment // oldest first

	Subdomain workerScriptSubdomain
}

type workerVersion struct {
	ID          string
	Number      int
	Created     time.Time
	TriggeredBy string
}

type workerDeployment struct {
	ID          string
	Created     time.Time
	Message     string
	TriggeredBy string
	VersionID   string
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
}

const (
	// Sanitized placeholders the recordings use for the token owner (0038, 0039). The emulator
	// reports them verbatim so replayed recordings match.
	workerAuthorID    = "USER_ID"
	workerAuthorEmail = "user@example.com"

	workerTriggeredUpload   = "upload"                          // 0038, 0039, 0065
	workerMessageUpload     = "Automatic deployment on upload." // 0039
	workerTriggeredSettings = "settings"                        // UNVERIFIED: settings PATCH not recorded
	workerMessageSettings   = "Automatic deployment on settings update."
	workerDefaultUsageModel = "standard" // 0036
	workerDefaultStartupMs  = 1          // 0036, 0183 (0062 and 0104 report 2)
	workerVPCDocsURL        = "https://developers.cloudflare.com/workers-vpc/get-started/"
)

// workerHandlerRe finds exported handler methods (`async fetch(req) {` at the start of a line)
// without matching calls like `env.X.fetch(url)`. Real handler detection runs the script;
// this heuristic is UNVERIFIED beyond the fetch-only scripts recorded (0036, 0062, 0183).
var workerHandlerRe = regexp.MustCompile(`(?m)^\s*(?:async\s+)?(fetch|scheduled|queue|email|tail|trace|test)\s*\(`)

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
		"has_assets":         false, // assets upload not emulated
		"has_modules":        w.MainModule != "",
		"etag":               w.Etag,
		"handlers":           emptyIfNil(w.Handlers),
		"last_deployed_from": "api",
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
		"annotations":         map[string]any{"workers/triggered_by": w.currentVersion().TriggeredBy},
		"bindings":            emptyIfNil(w.Bindings), // echoed as uploaded (0065)
	}
	if w.Observability != nil {
		m["observability"] = w.Observability.expanded() // UNVERIFIED: not recorded in settings
	}
	return m
}

func (w *workerScript) currentVersion() *workerVersion { return w.Versions[len(w.Versions)-1] }

func (v *workerVersion) json() map[string]any {
	return map[string]any{
		"id": v.ID, "number": v.Number,
		"metadata": map[string]any{
			"created_on": tsMicro(v.Created), "source": "api",
			"author_id": workerAuthorID, "author_email": workerAuthorEmail,
			"has_preview": true, // 0038 (even with previews_enabled false)
		},
		"annotations": map[string]any{"workers/triggered_by": v.TriggeredBy},
	}
}

func (d *workerDeployment) json() map[string]any {
	return map[string]any{
		"id": d.ID, "source": "api", "strategy": "percentage", "author_email": workerAuthorEmail,
		"annotations": map[string]any{"workers/message": d.Message, "workers/triggered_by": d.TriggeredBy},
		"versions":    []any{map[string]any{"version_id": d.VersionID, "percentage": 100}},
		"created_on":  tsMicro(d.Created),
	}
}

// ---- handlers --------------------------------------------------------------------------------

// workerAccountSubdomainGet: GET /accounts/{id}/workers/subdomain (0001). Accounts without a
// workers.dev subdomain are UNVERIFIED (not emulated: every account has one).
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
// malformed uploads are UNVERIFIED (not recorded); 10021 is Workers' script-validation code.
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

// newVersionAndDeployment adds a version and a 100% deployment of it — every upload does this
// implicitly (0038, 0039). IDs are consumed version first, then deployment.
func (c *reqCtx) newVersionAndDeployment(w *workerScript, triggeredBy, message string) {
	n := 1
	if len(w.Versions) > 0 {
		n = w.currentVersion().Number + 1 // UNVERIFIED: only version 1 recorded
	}
	v := &workerVersion{ID: c.s.ids.next(uuidV4), Number: n, Created: c.now, TriggeredBy: triggeredBy}
	d := &workerDeployment{ID: c.s.ids.next(uuidV4), Created: c.now, Message: message, TriggeredBy: triggeredBy, VersionID: v.ID}
	w.Versions = append(w.Versions, v)
	w.Deployments = append(w.Deployments, d)
}

// workerUpload: PUT …/workers/scripts/{name}, multipart with a "metadata" part and one part per
// module (0036, 0062, 0104, 0183). A re-upload keeps tag and created_on and replaces everything
// else (0104). Server-assigned values are consumed from the ID source in this order: tag (new
// scripts only), version ID, deployment ID, etag.
func workerUpload(c *reqCtx) response {
	parts, r := readParts(c)
	if r != nil {
		return *r
	}
	rawMeta, found := parts["metadata"]
	if !found {
		return fail(http.StatusBadRequest, 10021, "Missing metadata part.") // UNVERIFIED
	}
	var md workerMetadata
	if r := decodeMetadata(rawMeta, &md); r != nil {
		return *r
	}
	var mainModule, entry string
	switch {
	case md.MainModule != nil && *md.MainModule != "":
		mainModule, entry = *md.MainModule, *md.MainModule
	case md.BodyPart != nil && *md.BodyPart != "":
		entry = *md.BodyPart // UNVERIFIED: service-worker syntax not recorded
	default:
		return fail(http.StatusBadRequest, 10021, "Metadata must set main_module or body_part.") // UNVERIFIED
	}
	code, found := parts[entry]
	if !found {
		return fail(http.StatusBadRequest, 10021, fmt.Sprintf("No such module %q.", entry)) // UNVERIFIED
	}
	var bindings []map[string]any
	if md.Bindings != nil {
		bindings = *md.Bindings
	}
	if r := validateBindings(c.account, bindings); r != nil {
		return *r
	}

	name := c.params["script_name"]
	w, exists := c.account.scripts[name]
	if !exists {
		w = &workerScript{Name: name, Tag: c.s.ids.next(hex32), Created: c.now, Seq: c.s.nextSeq()}
	}
	w.Modified = c.now
	w.MainModule, w.EntryPoint = mainModule, entry
	w.CompatDate, w.CompatFlags = deref(md.CompatibilityDate), derefSlice(md.CompatibilityFlags)
	w.UsageModel = workerDefaultUsageModel
	if md.UsageModel != nil && *md.UsageModel != "" {
		w.UsageModel = *md.UsageModel // UNVERIFIED
	}
	w.Bindings = bindings
	w.Observability = md.Observability
	w.Logpush = md.Logpush != nil && *md.Logpush
	w.Tags, w.TailConsumers = derefSlice(md.Tags), derefSlice(md.TailConsumers)
	w.Placement = nil
	if md.Placement != nil {
		w.Placement = *md.Placement
	}
	w.Handlers = nil
	for _, m := range workerHandlerRe.FindAllStringSubmatch(string(code), -1) {
		if !containsStr(w.Handlers, m[1]) {
			w.Handlers = append(w.Handlers, m[1])
		}
	}
	w.StartupMs = c.s.workerStartupMs
	c.newVersionAndDeployment(w, workerTriggeredUpload, workerMessageUpload)
	// etag: a 64-hex digest that changes with the content (0036 vs 0104). The real input to the
	// hash is unknown (it is not sha256 of the module or of the body), so this is UNVERIFIED.
	w.Etag = c.s.ids.next(func() string {
		sum := sha256.Sum256(append(append([]byte{}, rawMeta...), code...))
		return hex.EncodeToString(sum[:])
	})
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
	return ok(map[string]any{"id": w.Tag})
}

func workerSettingsGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // 0109, 0143, 0211
	}
	return ok(w.settingsJSON())
}

// workerSettingsPatch: PATCH …/settings merges the given fields. Entirely UNVERIFIED (not
// recorded): the spec's multipart "settings" part is accepted, and so is a plain JSON body; a
// change creates a new version deployed at 100%, like an upload; the response is the settings
// object.
func workerSettingsPatch(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	var md workerMetadata
	raw := c.body
	if mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		parts, r := readParts(c)
		if r != nil {
			return *r
		}
		if raw, found = parts["settings"]; !found {
			return fail(http.StatusBadRequest, 10021, "Missing settings part.")
		}
	}
	if r := decodeMetadata(raw, &md); r != nil {
		return *r
	}
	if md.Bindings != nil {
		if r := validateBindings(c.account, *md.Bindings); r != nil {
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
	w.Modified = c.now
	c.newVersionAndDeployment(w, workerTriggeredSettings, workerMessageSettings)
	return ok(w.settingsJSON())
}

// workerVersionsList: GET …/versions → {"items": […]} with page/per_page/count/total_count (no
// total_pages), per_page 10 by default, and null errors/messages (0038). Newest-first ordering
// is UNVERIFIED (one version recorded).
func workerVersionsList(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route
	}
	newest := make([]*workerVersion, 0, len(w.Versions))
	for i := len(w.Versions) - 1; i >= 0; i-- {
		newest = append(newest, w.Versions[i])
	}
	pageItems, page, perPage, _ := paginate(newest, c.intQuery("page", 1), c.intQuery("per_page", 10), 10)
	items := make([]any, 0, len(pageItems))
	for _, v := range pageItems {
		items = append(items, v.json())
	}
	return okList(map[string]any{"items": items},
		PageInfo{Page: intp(page), PerPage: intp(perPage), Count: len(items), TotalCount: intp(len(newest))}).
		withStyle(styleNullErrorsMessages)
}

// workerDeploymentsList: GET …/deployments → {"deployments": […]} with a full page-based
// result_info (0039). Newest first and the 10-item cap are UNVERIFIED (one deployment recorded).
func workerDeploymentsList(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route
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
// 0063, 0184). An omitted field keeps its current value (UNVERIFIED).
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
	if req.PreviewsEnabled != nil {
		w.Subdomain.PreviewsEnabled = *req.PreviewsEnabled
	}
	return ok(w.Subdomain)
}

// workerSubdomainGet is UNVERIFIED (not recorded): same shape as the POST result. A new script
// starts with workers.dev disabled (UNVERIFIED).
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
