package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
)

// The Workers versions API and the other routes `wrangler deploy`, `versions view` and
// `delete` use besides the script upload: POST/GET …/scripts/{name}/versions[/{id}], POST
// …/deployments, GET/PATCH …/script-settings, GET …/secrets, GET/DELETE
// …/workers/services/{name} and GET …/workers/workers/{id}. None of them is recorded; the
// shapes follow the pinned spec and what wrangler reads (SOURCED below), the rest is UNVERIFIED.

func (s *Server) registerWorkerVersions() {
	base := "/accounts/{account_id}/workers/scripts/{script_name}"
	s.handle(http.MethodPost, base+"/versions", workerVersionUpload)
	s.handle(http.MethodGet, base+"/versions/{version_id}", workerVersionGet)
	s.handle(http.MethodPost, base+"/deployments", workerDeploymentCreate)
	s.handle(http.MethodGet, base+"/script-settings", workerScriptSettingsGet)
	s.handle(http.MethodPatch, base+"/script-settings", workerScriptSettingsPatch)
	s.handle(http.MethodGet, base+"/secrets", workerSecretsList)
	s.handle(http.MethodGet, "/accounts/{account_id}/workers/services/{service_name}", workerServiceGet)
	s.handle(http.MethodDelete, "/accounts/{account_id}/workers/services/{service_name}", workerServiceDelete)
	s.handle(http.MethodGet, "/accounts/{account_id}/workers/workers/{worker_id}", workerResourceGet)
}

// scriptUpload is a parsed multipart script or version upload.
type scriptUpload struct {
	md      workerMetadata
	res     workerResources // Etag and StartupMs unset
	rawMeta []byte
	code    []byte
}

// etag consumes the upload's etag from the ID source: a 64-hex digest that changes with the
// content (0036 vs 0104). The real input to the hash is unknown (it is not sha256 of the module
// or of the body), so this is UNVERIFIED.
func (u *scriptUpload) etag(c *reqCtx) string {
	return c.s.ids.next(func() string {
		sum := sha256.Sum256(append(append([]byte{}, u.rawMeta...), u.code...))
		return hex.EncodeToString(sum[:])
	})
}

// parseScriptUpload parses the multipart body of PUT …/scripts/{name} and POST …/versions: a
// "metadata" part and one part per module (0036, 0062, 0104, 0183). prev is the existing
// script (nil for a new one); inherit bindings and keep_bindings resolve against its deployed
// version.
func parseScriptUpload(c *reqCtx, prev *workerScript) (*scriptUpload, *response) {
	parts, r := readParts(c)
	if r != nil {
		return nil, r
	}
	rawMeta, found := parts["metadata"]
	if !found {
		r := fail(http.StatusBadRequest, 10021, "Missing metadata part.") // UNVERIFIED
		return nil, &r
	}
	u := &scriptUpload{rawMeta: rawMeta}
	md := &u.md
	if r := decodeMetadata(rawMeta, md); r != nil {
		return nil, r
	}
	res := &u.res
	if res.Assets, r = parseAssets(c, prev, md); r != nil {
		return nil, r
	}
	switch {
	case md.MainModule != nil && *md.MainModule != "":
		res.MainModule, res.EntryPoint = *md.MainModule, *md.MainModule
	case md.BodyPart != nil && *md.BodyPart != "":
		// Service-worker syntax (not recorded): metadata.body_part names the script part.
		// SOURCED: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/__tests__/helpers/mock-upload-worker.ts#L123
		// (the fixture asserts wrangler's request); see has_modules for the response.
		res.EntryPoint = *md.BodyPart
	case res.Assets != nil:
		// An assets-only Worker: metadata with assets and no module, as wrangler uploads a
		// project without main, SOURCED cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/create-worker-upload-form.ts#L99-L114.
		// How the API reports it (entry_point, has_modules false) is UNVERIFIED.
	default:
		r := fail(http.StatusBadRequest, 10021, "Metadata must set main_module or body_part.") // UNVERIFIED
		return nil, &r
	}
	if res.EntryPoint != "" {
		if u.code, found = parts[res.EntryPoint]; !found {
			r := fail(http.StatusBadRequest, 10021, fmt.Sprintf("No such module %q.", res.EntryPoint)) // UNVERIFIED
			return nil, &r
		}
		res.MainCode = append([]byte(nil), u.code...)
	}
	var bindings []map[string]any
	if md.Bindings != nil {
		bindings = *md.Bindings
	}
	bindings, r = resolveBindings(prev, bindings, md.KeepBindings, c.query.Get("bindings_inherit") == "strict")
	if r != nil {
		return nil, r
	}
	if r := validateBindings(c.account, bindings); r != nil {
		return nil, r
	}
	if r := validateAssetsBinding(bindings, res.Assets != nil); r != nil {
		return nil, r
	}
	res.Bindings = bindings
	res.CompatDate, res.CompatFlags = deref(md.CompatibilityDate), derefSlice(md.CompatibilityFlags)
	res.UsageModel = workerDefaultUsageModel
	if md.UsageModel != nil && *md.UsageModel != "" {
		res.UsageModel = *md.UsageModel // UNVERIFIED
	}
	if md.Placement != nil { // reported as {} when omitted (0065)
		res.Placement = *md.Placement
	}
	for _, m := range workerHandlerRe.FindAllStringSubmatch(string(u.code), -1) {
		if !containsStr(res.Handlers, m[1]) {
			res.Handlers = append(res.Handlers, m[1])
		}
	}
	return u, nil
}

// invalidInheritCode is the API's error for an inherit binding that names no binding of the
// previous version. SOURCED (statement): INVALID_INHERIT_BINDING_CODE = 10057, and wrangler
// parses the message "inherit binding '<name>' is invalid",
// wrangler@4.143.0:wrangler-dist/cli.js#L158251 (deploy-helpers/src/deploy/helpers/error-codes.ts)
// and #L170815-L170819. The status (400) is UNVERIFIED.
const invalidInheritCode = 10057

// resolveBindings replaces {"type":"inherit","name":N[,"old_name":O][,"version_id":V]}
// bindings with the named binding of the previous version (the deployed one, or version V),
// renamed to N, and appends the previous bindings of the keep_bindings types that the upload
// does not name. wrangler sends inherit bindings (for example for required secrets) and
// keep_bindings with bindings_inherit=strict on every upload, SOURCED (relies)
// wrangler@4.143.0:wrangler-dist/cli.js#L163098-L163100,L163649-L163683,L170791-L170813,L175373.
// With strict, an unresolvable inherit binding fails the upload; without it, the binding is
// dropped (UNVERIFIED: the spec only says strict "fails" and the default "ignores").
func resolveBindings(prev *workerScript, bindings []map[string]any, keep []string, strict bool) ([]map[string]any, *response) {
	var base []map[string]any
	if prev != nil && len(prev.Versions) > 0 {
		base = prev.Bindings
	}
	byName := func(list []map[string]any, name string) map[string]any {
		for _, b := range list {
			if b["name"] == name {
				return b
			}
		}
		return nil
	}
	out := make([]map[string]any, 0, len(bindings))
	for _, b := range bindings {
		if b["type"] != "inherit" {
			out = append(out, b)
			continue
		}
		name, _ := b["name"].(string)
		from := name
		if o, _ := b["old_name"].(string); o != "" {
			from = o
		}
		src := base
		if vid, _ := b["version_id"].(string); vid != "" && prev != nil {
			src = nil
			for _, v := range prev.Versions {
				if v.ID == vid {
					src = v.res.Bindings
				}
			}
		}
		old := byName(src, from)
		if old == nil {
			if strict {
				r := fail(http.StatusBadRequest, invalidInheritCode, fmt.Sprintf("inherit binding '%s' is invalid: the previous version has no binding named '%s'", name, from))
				return nil, &r
			}
			continue
		}
		cp := make(map[string]any, len(old))
		for k, v := range old {
			cp[k] = v
		}
		cp["name"] = name
		out = append(out, cp)
	}
	for _, old := range base {
		t, _ := old["type"].(string)
		if containsStr(keep, t) && byName(out, fmt.Sprint(old["name"])) == nil {
			out = append(out, old)
		}
	}
	return out, nil
}

// workerVersionUpload: POST …/scripts/{name}/versions?bindings_inherit=strict uploads a version
// without deploying it. wrangler deploy uses it for an existing Worker, then deploys the new
// version at 100% (POST …/deployments) and writes the non-versioned settings (PATCH
// …/script-settings), SOURCED (relies) cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/deploy.ts#L420-L573.
// It reads id, resources.script.etag and startup_time_ms (deploy.ts#L567-L573). The version's
// triggered_by "upload" follows the recorded upload (0038); the rest of the shape follows the
// spec (workers_version-item-full) and is UNVERIFIED.
func workerVersionUpload(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound() // UNVERIFIED for this route (10007 recorded for GET …/settings)
	}
	up, r := parseScriptUpload(c, w)
	if r != nil {
		return *r
	}
	v := c.newVersion(w, workerTriggeredUpload)
	for _, k := range []string{"workers/message", "workers/tag"} {
		if val, set := up.md.Annotations[k]; set {
			if v.Annotations == nil {
				v.Annotations = map[string]string{}
			}
			v.Annotations[k] = val
		}
	}
	v.res = up.res
	v.res.Etag = up.etag(c)
	v.res.StartupMs = c.s.workerStartupMs
	m := v.fullJSON(w)
	m["startup_time_ms"] = v.res.StartupMs
	return ok(m)
}

// fullJSON is a version with its resources (GET …/versions/{id}; workers_version-item-full).
// SOURCED (relies): `wrangler versions view` prints resources.script.handlers,
// resources.script_runtime.compatibility_date/flags and resources.bindings,
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/versions/view.ts#L60-L121, typed as
// ApiVersion (packages/deploy-helpers/src/deploy/helpers/versions-types.ts#L23-L55).
// metadata.modified_on (= created_on) is UNVERIFIED.
func (v *workerVersion) fullJSON(w *workerScript) map[string]any {
	m := v.json()
	m["metadata"].(map[string]any)["modified_on"] = tsMicro(v.Created)
	runtime := map[string]any{
		"compatibility_date": v.res.CompatDate, "compatibility_flags": emptyIfNil(v.res.CompatFlags),
		"usage_model": v.res.UsageModel,
	}
	m["resources"] = map[string]any{
		"bindings": readBindings(v.res.Bindings),
		"script": map[string]any{
			"etag": v.res.Etag, "handlers": emptyIfNil(v.res.Handlers), "last_deployed_from": w.DeployedFrom,
		},
		"script_runtime": runtime,
	}
	return m
}

// workerVersionGet: GET …/versions/{id}. SOURCED (relies): wrangler's fetchVersion,
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/versions-api.ts#L16-L36.
// The error for an unknown version ID is UNVERIFIED.
func workerVersionGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	for _, v := range w.Versions {
		if v.ID == c.params["version_id"] {
			return ok(v.fullJSON(w))
		}
	}
	return fail(http.StatusNotFound, 10222, "Version not found.") // UNVERIFIED code and message
}

// workerDeploymentCreate: POST …/deployments {strategy: "percentage", versions: [{version_id,
// percentage}], annotations: {workers/message}}, what wrangler sends, SOURCED (relies)
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/versions-api.ts#L132-L162;
// it reads only the result's id. The deployment answers like a listed one (0039) but without
// workers/triggered_by (the value for an explicit deployment is UNVERIFIED). Percentages must
// name known versions and add up to 100; those checks' errors are UNVERIFIED. The deployed
// version's code and bindings become the script's (settings, list).
func workerDeploymentCreate(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	var req struct {
		Strategy    string            `json:"strategy"`
		Versions    []workerTraffic   `json:"versions"`
		Annotations map[string]string `json:"annotations"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.Strategy != "percentage" || len(req.Versions) == 0 {
		return fail(http.StatusBadRequest, 10001, "a deployment needs strategy \"percentage\" and at least one version") // UNVERIFIED
	}
	sum := 0.0
	for _, t := range req.Versions {
		known := false
		for _, v := range w.Versions {
			known = known || v.ID == t.VersionID
		}
		if !known {
			return fail(http.StatusBadRequest, 10222, fmt.Sprintf("Version %s not found.", t.VersionID)) // UNVERIFIED
		}
		sum += t.Percentage
	}
	if math.Abs(sum-100) > 1e-9 {
		return fail(http.StatusBadRequest, 10001, fmt.Sprintf("version percentages add up to %g, not 100", sum)) // UNVERIFIED
	}
	d := &workerDeployment{ID: c.s.ids.next(uuidV4), Created: c.now, Message: req.Annotations["workers/message"],
		Source: uploadSource(c), Versions: req.Versions}
	w.Deployments = append(w.Deployments, d)
	w.workerResources = w.deployedVersion().res
	w.Modified = c.now // UNVERIFIED
	w.DeployedFrom = d.Source
	return ok(d.json())
}

// scriptSettingsJSON is the non-versioned settings object (workers_script-settings-item):
// logpush, observability, tags, tail_consumers.
func (w *workerScript) scriptSettingsJSON() map[string]any {
	m := map[string]any{
		"logpush": w.Logpush, "tags": emptyIfNil(w.Tags), "tail_consumers": emptyIfNil(w.TailConsumers),
	}
	if w.Observability != nil {
		m["observability"] = w.Observability.expanded()
	}
	return m
}

// workerScriptSettingsGet: GET …/script-settings (UNVERIFIED: not recorded; the shape is the
// spec's, the values those of GET …/settings, 0065).
func workerScriptSettingsGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	return ok(w.scriptSettingsJSON())
}

// workerScriptSettingsPatch: PATCH …/script-settings merges the given non-versioned settings
// and creates no version. SOURCED (relies): wrangler deploy sends tail_consumers, logpush,
// observability and tags here after deploying a version (versions-api.ts#L164-L191, called
// "patchNonVersionedScriptSettings", deploy.ts#L552-L562). The echo of the merged settings is
// UNVERIFIED.
func workerScriptSettingsPatch(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	var md workerMetadata
	if r := c.decodeJSON(&md); r != nil {
		return *r
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
	return ok(w.scriptSettingsJSON())
}

// workerSecretsList: GET …/secrets lists the secret bindings by name and type (values are never
// returned: the spec marks them writeOnly). SOURCED (relies): wrangler deploy lists them whenever
// the config has bindings and accepts exactly 404/10007 for a new Worker,
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/check-remote-secrets-override.ts#L10,L35-L42.
func workerSecretsList(c *reqCtx) response {
	w, found := c.account.scripts[c.params["script_name"]]
	if !found {
		return workerNotFound()
	}
	out := []any{}
	for _, b := range readBindings(w.Bindings) {
		if t := b["type"]; t == "secret_text" || t == "secret_key" {
			out = append(out, b)
		}
	}
	return ok(out)
}

// workerServiceGet: GET /accounts/{a}/workers/services/{name} (absent from the pinned spec).
// wrangler deploy reads it before every upload: 404 with code 10007 (or 10090) means a new
// Worker, otherwise it uses default_environment.script.tag, .tags and .last_deployed_from and
// default_environment.environment. SOURCED (relies): wrangler@4.143.0:wrangler-dist/cli.js#L174941-L174948,L175009
// and cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/worker-not-found-error.ts#L4-L30.
// The other fields are UNVERIFIED.
func workerServiceGet(c *reqCtx) response {
	w, found := c.account.scripts[c.params["service_name"]]
	if !found {
		return workerNotFound()
	}
	script := map[string]any{
		"id": w.Name, "tag": w.Tag, "etag": w.Etag, "handlers": emptyIfNil(w.Handlers),
		"tags": emptyIfNil(w.Tags), "last_deployed_from": w.DeployedFrom,
		"created_on": tsMicro(w.Created), "modified_on": tsMicro(w.Modified),
	}
	return ok(map[string]any{
		"id": w.Name, "created_on": tsMicro(w.Created), "modified_on": tsMicro(w.Modified),
		"default_environment": map[string]any{
			"environment": "production", "script": script,
			"created_on": tsMicro(w.Created), "modified_on": tsMicro(w.Modified),
		},
	})
}

// workerServiceDelete: DELETE /accounts/{a}/workers/services/{name}?force= (absent from the
// pinned spec) deletes the Worker; `wrangler delete` uses it and ignores the result, SOURCED
// (relies) cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/delete.ts#L154-L159. It answers
// like the script DELETE (0108; UNVERIFIED here).
func workerServiceDelete(c *reqCtx) response {
	c.params["script_name"] = c.params["service_name"]
	return workerDelete(c)
}

// workerResourceGet: GET …/workers/workers/{id} (the Workers resource API) by tag or name.
// wrangler reads subdomain.enabled/previews_enabled from it after every upload, SOURCED (relies)
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/triggers/subdomain.ts#L157-L175.
// The object follows the spec's workers_Worker (id = the script tag, empty references); those
// values are UNVERIFIED.
func workerResourceGet(c *reqCtx) response {
	id := c.params["worker_id"]
	w, found := c.account.scripts[id]
	if !found {
		for _, x := range c.account.scripts {
			if x.Tag == id {
				w, found = x, true
			}
		}
	}
	if !found {
		return workerNotFound() // UNVERIFIED for this route
	}
	obs := w.Observability
	if obs == nil {
		obs = &workerObservability{}
	}
	tails := []any{}
	for _, t := range w.TailConsumers {
		if name, _ := t["service"].(string); name != "" {
			tails = append(tails, map[string]any{"name": name})
		}
	}
	return ok(map[string]any{
		"id": w.Tag, "name": w.Name, "tags": emptyIfNil(w.Tags), "logpush": w.Logpush,
		"subdomain":      map[string]any{"enabled": w.Subdomain.Enabled, "previews_enabled": w.Subdomain.PreviewsEnabled},
		"observability":  obs.expanded(),
		"tail_consumers": tails,
		"created_on":     tsMicro(w.Created), "updated_on": tsMicro(w.Modified),
		"deployed_on": tsMicro(w.Deployments[len(w.Deployments)-1].Created),
		"references": map[string]any{
			"workers": []any{}, "domains": []any{}, "dispatch_namespace_outbounds": []any{},
			"durable_objects": []any{}, "queues": []any{},
		},
	})
}
