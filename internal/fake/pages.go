package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Cloudflare Pages: projects (/accounts/{account_id}/pages/projects), the Direct Upload asset
// API (pages_assets.go) and deployments (pages_deployments.go).
//
// No real-API recording of Pages exists. Evidence, strongest first:
//   - wrangler, the official client, at the pinned release wrangler@4.143.0 (source files cited
//     at cloudflare/workers-sdk@485cfb3, whose packages/wrangler/package.json is version
//     4.143.0; the published bundle wrangler@4.143.0:wrangler-dist/cli.js has the same code):
//     `pages project create|list|delete`, `pages deploy` and `pages deployment list|delete`.
//     test/differential runs that wrangler against this profile.
//   - the pinned spec's schemas and defaults (UNVERIFIED as behavior; every response conforms
//     to them, see the response allowlist entries pages-*).
//
// Errors: 8000007 "Project not found" is SOURCED (statement + fixture): wrangler treats code
// 8000007 as "project not found", cloudflare/workers-sdk@485cfb3:packages/wrangler/src/pages/projects.ts#L135-L136
// and packages/wrangler/src/__tests__/pages/deploy.test.ts#L194 (404). Every other Pages error
// code here is UNVERIFIED; none is 8000000, which wrangler retries as UNKNOWN_ERROR
// (packages/wrangler/src/pages/errors.ts#L13).

const (
	codePagesProjectNotFound    = 8000007 // SOURCED (see above)
	codePagesUnauthorized       = 8000013 // SOURCED (statement): ApiErrorCodes.UNAUTHORIZED, packages/wrangler/src/pages/errors.ts#L14
	codePagesInvalid            = 8000011 // UNVERIFIED
	codePagesProjectExists      = 8000002 // UNVERIFIED
	codePagesDeploymentNotFound = 8000009 // UNVERIFIED
	codePagesMissingAssets      = 8000084 // UNVERIFIED
	codePagesLiveDeployment     = 8000034 // UNVERIFIED
	codePagesAliasedDeployment  = 8000035 // UNVERIFIED
)

// pagesNameRe is the pinned spec's pages_project_name pattern; the 58-character limit is
// UNVERIFIED (the spec gives none; the <name>.pages.dev host label must stay below 63).
var pagesNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,57}$`)

// pagesDeploymentConfigMaps are the deployment-config fields that map a binding (or variable)
// name to an object: a PATCH merges them key by key, and a null value removes the key (the
// pinned spec marks every such request value nullable; the merge itself is UNVERIFIED).
var pagesDeploymentConfigMaps = map[string]bool{
	"env_vars": true, "kv_namespaces": true, "d1_databases": true, "r2_buckets": true, "queue_producers": true,
	"services": true, "durable_object_namespaces": true, "ai_bindings": true, "analytics_engine_datasets": true,
	"browsers": true, "hyperdrive_bindings": true, "mtls_certificates": true, "vectorize_bindings": true,
}

type pagesProject struct {
	ID, Name         string
	Created          time.Time
	Seq              int64
	ProductionBranch string
	BuildConfig      map[string]any
	// Configs are deployment_configs.production and .preview as stored (secret values kept).
	Configs map[string]map[string]any
	Source  map[string]any // nil for a Direct Upload project
	N       int            // numbers the script names (pages-worker--<n>-…)

	Deployments []*pagesDeployment // oldest first
	Assets      map[string]*pagesAsset
}

type pagesAsset struct {
	Content     []byte
	ContentType string
	Upserted    bool
}

func (s *Server) registerPages() {
	base := "/accounts/{account_id}/pages/projects"
	s.handle(http.MethodPost, base, pagesProjectCreate)
	s.handle(http.MethodGet, base, pagesProjectList)
	s.handle(http.MethodGet, base+"/{project_name}", pagesProjectGet)
	s.handle(http.MethodPatch, base+"/{project_name}", pagesProjectPatch)
	s.handle(http.MethodDelete, base+"/{project_name}", pagesProjectDelete)
	s.handle(http.MethodGet, base+"/{project_name}/upload-token", pagesUploadToken)
	s.handle(http.MethodPost, "/pages/assets/check-missing", pagesCheckMissing)
	s.handle(http.MethodPost, "/pages/assets/upload", pagesAssetsUpload)
	s.handle(http.MethodPost, "/pages/assets/upsert-hashes", pagesUpsertHashes)
	s.handle(http.MethodPost, base+"/{project_name}/deployments", pagesDeploymentCreate)
	s.handle(http.MethodGet, base+"/{project_name}/deployments", pagesDeploymentList)
	s.handle(http.MethodGet, base+"/{project_name}/deployments/{deployment_id}", pagesDeploymentGet)
	s.handle(http.MethodDelete, base+"/{project_name}/deployments/{deployment_id}", pagesDeploymentDelete)
}

func (c *reqCtx) pagesProjects() map[string]*pagesProject {
	if c.account.pages == nil {
		c.account.pages = map[string]*pagesProject{}
	}
	return c.account.pages
}

func pagesNotFound() response {
	return fail(http.StatusNotFound, codePagesProjectNotFound, "Project not found") // SOURCED fixture text, deploy.test.ts#L194
}

func pagesInvalid(msg string) response { return fail(http.StatusBadRequest, codePagesInvalid, msg) }

// project returns the path's project or a 404.
func (c *reqCtx) project() (*pagesProject, *response) {
	p := c.pagesProjects()[c.params["project_name"]]
	if p == nil {
		r := pagesNotFound()
		return nil, &r
	}
	return p, nil
}

// pagesDefaultConfig is a new project's deployment config: the pinned spec's defaults
// (fail_open true, always_use_latest_compatibility_date false, build_image_major_version 3,
// usage_model standard) and the creation date as compatibility date. UNVERIFIED: the spec
// requires all of them in responses; whether the API fills them this way is not recorded.
func pagesDefaultConfig(now time.Time) map[string]any {
	return map[string]any{
		"compatibility_date": now.UTC().Format("2006-01-02"), "compatibility_flags": []any{},
		"fail_open": true, "always_use_latest_compatibility_date": false, "build_image_major_version": 3,
		"usage_model": "standard", "env_vars": nil,
	}
}

// pagesBuildConfigFields are the build_config fields; the spec requires web_analytics_tag and
// web_analytics_token in responses (both nullable), and the others are nullable too.
var pagesBuildConfigFields = []string{"build_caching", "build_command", "destination_dir", "root_dir", "web_analytics_tag", "web_analytics_token"}

// mergeConfig applies a deployment_configs.<env> request object to cfg (PATCH semantics; a
// create merges into the defaults the same way).
func mergeConfig(cfg map[string]any, in map[string]any) *response {
	for k, v := range in {
		if !pagesDeploymentConfigMaps[k] {
			cfg[k] = v
			continue
		}
		if v == nil {
			clearConfigMap(cfg, k) // UNVERIFIED: null for the whole map clears it
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			r := pagesInvalid(fmt.Sprintf("deployment_configs: %s must be an object", k))
			return &r
		}
		cur, _ := cfg[k].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
		}
		for name, val := range m {
			if val == nil {
				delete(cur, name)
				continue
			}
			obj, ok := val.(map[string]any)
			if !ok {
				r := pagesInvalid(fmt.Sprintf("deployment_configs: %s.%s must be an object", k, name))
				return &r
			}
			switch k {
			case "env_vars":
				t, _ := obj["type"].(string)
				if _, isStr := obj["value"].(string); (t != "plain_text" && t != "secret_text") || !isStr {
					r := pagesInvalid(fmt.Sprintf("deployment_configs: env var %s needs a type plain_text or secret_text and a string value", name))
					return &r
				}
			case "services":
				// The pinned spec's response requires environment on every service binding;
				// "production" when the request has none is UNVERIFIED.
				if _, has := obj["environment"]; !has {
					obj["environment"] = "production"
				}
			}
			cur[name] = obj
		}
		if len(cur) == 0 {
			clearConfigMap(cfg, k)
		} else {
			cfg[k] = cur
		}
	}
	return nil
}

// clearConfigMap removes every key of a config map. env_vars stays, as null: the pinned spec
// requires it in responses (nullable); the other maps are optional and dropped.
func clearConfigMap(cfg map[string]any, k string) {
	if k == "env_vars" {
		cfg[k] = nil
		return
	}
	delete(cfg, k)
}

type pagesProjectBody struct {
	Name              *string                   `json:"name"`
	ProductionBranch  *string                   `json:"production_branch"`
	BuildConfig       map[string]any            `json:"build_config"`
	DeploymentConfigs map[string]map[string]any `json:"deployment_configs"`
	Source            map[string]any            `json:"source"`
}

// apply writes a create or PATCH body into p.
func (b *pagesProjectBody) apply(p *pagesProject) *response {
	if b.ProductionBranch != nil {
		if *b.ProductionBranch == "" {
			r := pagesInvalid("production_branch must not be empty")
			return &r
		}
		p.ProductionBranch = *b.ProductionBranch
	}
	for k, v := range b.BuildConfig {
		p.BuildConfig[k] = v
	}
	for env, cfg := range b.DeploymentConfigs {
		if env != "production" && env != "preview" {
			r := pagesInvalid("deployment_configs: unknown environment " + env)
			return &r
		}
		if cfg == nil {
			continue
		}
		if r := mergeConfig(p.Configs[env], cfg); r != nil {
			return r
		}
	}
	if b.Source != nil {
		p.Source = b.Source // pass-through; connecting a repository is not emulated (UNVERIFIED)
	}
	return nil
}

func pagesProjectCreate(c *reqCtx) response {
	var b pagesProjectBody
	if r := c.decodeJSON(&b); r != nil {
		return *r
	}
	// Both are required by the pinned spec's create body.
	if b.Name == nil || !pagesNameRe.MatchString(*b.Name) {
		return pagesInvalid("name must begin with a lowercase letter or digit and contain only lowercase letters, digits, and hyphens (at most 58)")
	}
	if b.ProductionBranch == nil {
		return pagesInvalid("production_branch is required")
	}
	projects := c.pagesProjects()
	if projects[*b.Name] != nil {
		return fail(http.StatusConflict, codePagesProjectExists, "A project with this name already exists. Choose a different project name.") // UNVERIFIED
	}
	seq := c.s.nextSeq()
	p := &pagesProject{
		ID: c.s.ids.next(uuidV4), Name: *b.Name, Created: c.now, Seq: seq, N: int(seq),
		BuildConfig: map[string]any{},
		Configs:     map[string]map[string]any{"production": pagesDefaultConfig(c.now), "preview": pagesDefaultConfig(c.now)},
		Assets:      map[string]*pagesAsset{},
	}
	for _, f := range pagesBuildConfigFields {
		p.BuildConfig[f] = nil
	}
	if r := b.apply(p); r != nil {
		return *r
	}
	projects[p.Name] = p
	return ok(p.json(c.s, c.now))
}

func pagesProjectList(c *reqCtx) response {
	var all []*pagesProject
	for _, p := range c.pagesProjects() {
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq }) // creation order: UNVERIFIED
	// page/per_page (the spec's parameters; wrangler pages with per_page=10 until a short page,
	// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/pages/projects.ts#L82-L102). The
	// default page size is UNVERIFIED.
	page, perPage, total := c.intQuery("page", 1), c.intQuery("per_page", 10), len(all)
	items, page, perPage, totalPages := paginate(all, page, perPage, 10)
	out := make([]any, 0, len(items))
	for _, p := range items {
		out = append(out, p.json(c.s, c.now))
	}
	return okList(out, PageInfo{Page: intp(page), PerPage: intp(perPage), Count: len(out), TotalCount: intp(total), TotalPages: intp(totalPages)})
}

func pagesProjectGet(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	return ok(p.json(c.s, c.now))
}

func pagesProjectPatch(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	var b pagesProjectBody
	if r := c.decodeJSON(&b); r != nil {
		return *r
	}
	if b.Name != nil && *b.Name != p.Name {
		return pagesInvalid("a project cannot be renamed") // UNVERIFIED
	}
	// Apply to a copy first, so a refused body changes nothing.
	cp := p.clone()
	if r := b.apply(cp); r != nil {
		return *r
	}
	*p = *cp
	return ok(p.json(c.s, c.now))
}

// pagesProjectDelete deletes the project with its deployments and assets. Whether the API
// refuses a project that still has deployments is UNVERIFIED; wrangler's `pages project delete`
// sends a bare DELETE (packages/wrangler/src/pages/projects.ts#L374-L378).
func pagesProjectDelete(c *reqCtx) response {
	if _, r := c.project(); r != nil {
		return *r
	}
	delete(c.pagesProjects(), c.params["project_name"])
	return ok(nil)
}

func (p *pagesProject) clone() *pagesProject {
	cp := *p
	b, _ := json.Marshal(map[string]any{"b": p.BuildConfig, "c": p.Configs, "s": p.Source})
	var m struct {
		B map[string]any            `json:"b"`
		C map[string]map[string]any `json:"c"`
		S map[string]any            `json:"s"`
	}
	_ = json.Unmarshal(b, &m)
	cp.BuildConfig, cp.Configs, cp.Source = m.B, m.C, m.S
	return &cp
}

func (p *pagesProject) subdomain() string { return p.Name + ".pages.dev" } // UNVERIFIED: no collision suffix

// configJSON renders a stored deployment config: secret_text values are never returned (the
// spec marks them x-sensitive; its example shows "" — UNVERIFIED).
func configJSON(cfg map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range cfg {
		out[k] = v
	}
	if evs, ok := cfg["env_vars"].(map[string]any); ok {
		masked := map[string]any{}
		for n, v := range evs {
			ev, _ := v.(map[string]any)
			if ev != nil && ev["type"] == "secret_text" {
				masked[n] = map[string]any{"type": "secret_text", "value": ""}
				continue
			}
			masked[n] = v
		}
		out["env_vars"] = masked
	}
	return out
}

// json renders the project (the pinned spec's pages_project; every required field is sent).
func (p *pagesProject) json(s *Server, now time.Time) map[string]any {
	var latest, canonical any
	if d := p.latest(); d != nil {
		latest = d.json(s, p, now)
	}
	if d := p.canonical(now); d != nil {
		canonical = d.json(s, p, now)
	}
	usesFunctions := any(nil)
	for _, d := range p.Deployments {
		if d.Worker != nil {
			usesFunctions = true
		}
	}
	m := map[string]any{
		"id": p.ID, "name": p.Name, "subdomain": p.subdomain(), "domains": []any{p.subdomain()},
		"created_on": tsMicro(p.Created), "production_branch": p.ProductionBranch,
		// Script names: the spec's example form (UNVERIFIED).
		"production_script_name": fmt.Sprintf("pages-worker--%d-production", p.N),
		"preview_script_name":    fmt.Sprintf("pages-worker--%d-preview", p.N),
		"uses_functions":         usesFunctions, "framework": "", "framework_version": "",
		"build_config":       p.BuildConfig,
		"deployment_configs": map[string]any{"production": configJSON(p.Configs["production"]), "preview": configJSON(p.Configs["preview"])},
		// null for a project without deployments: see the allowlist entry
		// pages-no-deployment-unsatisfiable.
		"latest_deployment": latest, "canonical_deployment": canonical,
	}
	if p.Source != nil {
		m["source"] = p.Source
	}
	return m
}

// branchAlias is the <alias> of a preview deployment's https://<alias>.<project>.pages.dev URL:
// the branch lower-cased, runs of other characters as "-", at most 28 characters. DOCS:
// https://developers.cloudflare.com/pages/configuration/preview-deployments/#preview-aliases
// describes branch aliases; the exact normalization is UNVERIFIED.
func branchAlias(branch string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(branch) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 28 {
		out = strings.TrimRight(out[:28], "-")
	}
	return out
}
