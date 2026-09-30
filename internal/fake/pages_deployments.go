package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Pages deployments: POST /accounts/{account_id}/pages/projects/{project_name}/deployments (a
// Direct Upload, multipart), list, get and delete.
//
// The create body is wrangler's (SOURCED, relies: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/api/pages/deploy.ts#L271-L476):
// form fields manifest (JSON: "/path" → hash), branch, commit_message, commit_hash,
// commit_dirty, and files _headers, _redirects, _routes.json, _worker.bundle,
// functions-filepath-routing-config.json. The pinned spec also allows a _worker.js file
// (mutually exclusive with _worker.bundle); wrangler never sends one (it bundles).
//
// Stages: a deployment starts queued/active, is deploy/active after half of
// Options.PagesDeployDelay and deploy/success (or deploy/failure, FailPagesDeployments) after
// all of it, on the emulated clock. wrangler polls GET …/deployments/{id} until latest_stage is
// deploy/success or failure (SOURCED, relies: packages/wrangler/src/pages/deploy.ts#L530-L590);
// the real timing and the stages a Direct Upload goes through are UNVERIFIED.
//
// Deletion: the live production deployment (the project's canonical_deployment) cannot be
// deleted, and a preview deployment holding a branch alias only with ?force=true. SOURCED
// (statement): the spec's force parameter "Allow deletion when a non-production deployment has
// an active alias" and wrangler's --force "Delete even if the deployment has an active alias"
// (packages/wrangler/src/pages/deployments.ts#L160-L165, sent as force=true|false). The error
// codes and messages of both refusals are UNVERIFIED.

// DefaultPagesDeployDelay is the default Options.PagesDeployDelay. It is one second so that
// wrangler's first status poll (after 1s) finds the deployment done.
const DefaultPagesDeployDelay = time.Second

type pagesDeployment struct {
	ID, ShortID                string
	Created                    time.Time
	Environment, Branch        string
	CommitHash, CommitMessage  string
	CommitDirty                bool
	Manifest                   map[string]string
	Headers, Redirects, Routes []byte
	Worker                     []byte // _worker.js or _worker.bundle
	Failed                     bool
	deployedDelay, halfway     time.Duration
}

// FailPagesDeployments makes the next n Pages deployments end in deploy/failure (tests).
func (s *Server) FailPagesDeployments(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pagesFailNext = n
}

// PagesDeploymentFiles returns a deployment's manifest and routing files (tests).
func (s *Server) PagesDeploymentFiles(accountID, project, id string) (manifest map[string]string, files map[string][]byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.accountLocked(accountID).pages[project]
	if p == nil {
		return nil, nil, false
	}
	for _, d := range p.Deployments {
		if d.ID == id {
			files = map[string][]byte{}
			for n, b := range map[string][]byte{"_headers": d.Headers, "_redirects": d.Redirects, "_routes.json": d.Routes, "_worker.js": d.Worker} {
				if b != nil {
					files[n] = append([]byte(nil), b...)
				}
			}
			m := map[string]string{}
			for k, v := range d.Manifest {
				m[k] = v
			}
			return m, files, true
		}
	}
	return nil, nil, false
}

// done reports whether d's deploy stage has ended at now.
func (d *pagesDeployment) done(now time.Time) bool {
	return !now.Before(d.Created.Add(d.deployedDelay))
}

func (d *pagesDeployment) succeeded(now time.Time) bool { return d.done(now) && !d.Failed }

// latest returns the newest deployment (any environment), or nil.
func (p *pagesProject) latest() *pagesDeployment {
	if len(p.Deployments) == 0 {
		return nil
	}
	return p.Deployments[len(p.Deployments)-1]
}

// canonical is the live production deployment: the newest production deployment that deployed
// successfully (UNVERIFIED: a failed newer one keeps the older one live).
func (p *pagesProject) canonical(now time.Time) *pagesDeployment {
	for i := len(p.Deployments) - 1; i >= 0; i-- {
		d := p.Deployments[i]
		if d.Environment == "production" && d.succeeded(now) {
			return d
		}
	}
	return nil
}

// aliased reports whether d is the newest deployment of its preview branch, which holds the
// branch alias (UNVERIFIED: the alias moves when a newer deployment is created).
func (p *pagesProject) aliased(d *pagesDeployment) bool {
	if d.Environment != "preview" {
		return false
	}
	for i := len(p.Deployments) - 1; i >= 0; i-- {
		if o := p.Deployments[i]; o.Environment == "preview" && o.Branch == d.Branch {
			return o == d
		}
	}
	return false
}

func stage(name, status string, started, ended *time.Time) map[string]any {
	ts := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return tsMicro(*t)
	}
	return map[string]any{"name": name, "status": status, "started_on": ts(started), "ended_on": ts(ended)}
}

// json renders the deployment (the pinned spec's pages_deployment).
func (d *pagesDeployment) json(s *Server, p *pagesProject, now time.Time) map[string]any {
	created := d.Created
	half := created.Add(d.halfway)
	end := created.Add(d.deployedDelay)
	var stages []any
	var latest map[string]any
	switch {
	case now.Before(half):
		stages = []any{stage("queued", "active", &created, nil), stage("initialize", "idle", nil, nil),
			stage("clone_repo", "idle", nil, nil), stage("build", "idle", nil, nil), stage("deploy", "idle", nil, nil)}
		latest = stage("queued", "active", &created, nil)
	case now.Before(end):
		stages = []any{stage("queued", "success", &created, &half), stage("initialize", "success", &half, &half),
			stage("clone_repo", "success", &half, &half), stage("build", "success", &half, &half), stage("deploy", "active", &half, nil)}
		latest = stage("deploy", "active", &half, nil)
	default:
		st := "success"
		if d.Failed {
			st = "failure"
		}
		stages = []any{stage("queued", "success", &created, &half), stage("initialize", "success", &half, &half),
			stage("clone_repo", "success", &half, &half), stage("build", "success", &half, &half), stage("deploy", st, &half, &end)}
		latest = stage("deploy", st, &half, &end)
	}
	modified := created
	if d.done(now) {
		modified = end
	} else if !now.Before(half) {
		modified = half
	}
	var aliases any // production: custom domains, of which the emulator has none (UNVERIFIED: null vs [])
	if p.aliased(d) {
		if a := branchAlias(d.Branch); a != "" {
			aliases = []any{"https://" + a + "." + p.subdomain()}
		}
	}
	env := p.Configs[d.Environment]
	var envVars any
	if ev := configJSON(env)["env_vars"]; ev != nil {
		envVars = ev
	}
	uses := any(nil)
	if d.Worker != nil {
		uses = true
	}
	return map[string]any{
		"id": d.ID, "short_id": d.ShortID, "project_id": p.ID, "project_name": p.Name,
		"environment": d.Environment, "url": "https://" + d.ShortID + "." + p.subdomain(), "aliases": aliases,
		"created_on": tsMicro(created), "modified_on": tsMicro(modified),
		"is_skipped": false, "skip_reason": nil, "latest_stage": latest, "stages": stages,
		"deployment_trigger": map[string]any{"type": "ad_hoc", "metadata": map[string]any{
			"branch": d.Branch, "commit_hash": d.CommitHash, "commit_message": d.CommitMessage, "commit_dirty": d.CommitDirty}},
		"build_config": p.BuildConfig, "env_vars": envVars, "uses_functions": uses,
		// A Direct Upload has no Git source: null, see the allowlist entry
		// pages-direct-upload-source-unsatisfiable.
		"source": nil,
	}
}

// readPagesForm parses the multipart create body into fields (no filename) and files.
func readPagesForm(c *reqCtx) (fields map[string]string, files map[string][]byte, _ *response) {
	bad := func(msg string) (map[string]string, map[string][]byte, *response) {
		r := pagesInvalid(msg)
		return nil, nil, &r
	}
	mt, params, err := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return bad("Expected a multipart/form-data request body.")
	}
	mr := multipart.NewReader(bytes.NewReader(c.body), params["boundary"])
	fields, files = map[string]string{}, map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return fields, files, nil
		}
		if err != nil {
			return bad("Malformed multipart body: " + err.Error())
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return bad("Malformed multipart body: " + err.Error())
		}
		if p.FileName() != "" {
			files[p.FormName()] = data
		} else {
			fields[p.FormName()] = string(data)
		}
	}
}

func pagesDeploymentCreate(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	fields, files, r := readPagesForm(c)
	if r != nil {
		return *r
	}
	// Fields may also arrive as files (and files as fields) depending on the client's encoding.
	get := func(name string) ([]byte, bool) {
		if b, ok := files[name]; ok {
			return b, true
		}
		if v, ok := fields[name]; ok {
			return []byte(v), true
		}
		return nil, false
	}
	raw, found := get("manifest")
	if !found {
		// Required for Direct Upload (the pinned spec's field description); Git-triggered
		// deployments of a connected project are not emulated.
		return pagesInvalid("manifest is required for a Direct Upload deployment")
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return pagesInvalid("manifest must be a JSON object of paths to hashes")
	}
	if len(manifest) > pagesMaxFileCount { // the spec: "Maximum 20,000 entries"
		return pagesInvalid("the manifest has more than 20000 entries")
	}
	var missing []string
	for path, h := range manifest {
		if !strings.HasPrefix(path, "/") || !pagesHashRe.MatchString(h) {
			return pagesInvalid("manifest entry " + path + " is invalid")
		}
		if p.Assets[h] == nil {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return pagesInvalid2(codePagesMissingAssets, "the manifest references files that were not uploaded: "+strings.Join(missing, ", "))
	}
	_, hasJS := get("_worker.js")
	_, hasBundle := get("_worker.bundle")
	if hasJS && hasBundle { // the spec: mutually exclusive
		return pagesInvalid("_worker.js and _worker.bundle cannot both be sent")
	}
	branch := fields["branch"]
	env := "preview"
	if branch == "" || branch == p.ProductionBranch {
		env = "production" // "defaults to the project's production branch" (the spec's branch field)
		if branch == "" {
			branch = p.ProductionBranch
		}
	}
	id := c.s.ids.next(uuidV4)
	d := &pagesDeployment{
		ID: id, ShortID: strings.ReplaceAll(id, "-", "")[:8], Created: c.now, Environment: env, Branch: branch,
		CommitHash: fields["commit_hash"], CommitMessage: fields["commit_message"], CommitDirty: fields["commit_dirty"] == "true",
		Manifest: manifest, deployedDelay: c.s.opts.PagesDeployDelay, halfway: c.s.opts.PagesDeployDelay / 2,
	}
	d.Headers, _ = get("_headers")
	d.Redirects, _ = get("_redirects")
	d.Routes, _ = get("_routes.json")
	if b, has := get("_worker.js"); has {
		d.Worker = b
	} else if b, has := get("_worker.bundle"); has {
		d.Worker = b
	}
	if c.s.pagesFailNext > 0 {
		c.s.pagesFailNext--
		d.Failed = true
	}
	p.Deployments = append(p.Deployments, d)
	return ok(d.json(c.s, p, c.now))
}

func pagesInvalid2(code int, msg string) response { return fail(http.StatusBadRequest, code, msg) }

func pagesDeploymentList(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	envFilter := c.query.Get("env") // the spec's env parameter (wrangler sends it, deployments.ts#L72-L79)
	var all []*pagesDeployment
	for i := len(p.Deployments) - 1; i >= 0; i-- { // newest first: UNVERIFIED
		if d := p.Deployments[i]; envFilter == "" || d.Environment == envFilter {
			all = append(all, d)
		}
	}
	page, perPage, total := c.intQuery("page", 1), c.intQuery("per_page", 25), len(all)
	items, page, perPage, totalPages := paginate(all, page, perPage, 25) // default page size UNVERIFIED
	out := make([]any, 0, len(items))
	for _, d := range items {
		out = append(out, d.json(c.s, p, c.now))
	}
	return okList(out, PageInfo{Page: intp(page), PerPage: intp(perPage), Count: len(out), TotalCount: intp(total), TotalPages: intp(totalPages)})
}

func (p *pagesProject) deployment(id string) (int, *pagesDeployment) {
	for i, d := range p.Deployments {
		if d.ID == id {
			return i, d
		}
	}
	return -1, nil
}

func pagesDeploymentGet(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	_, d := p.deployment(c.params["deployment_id"])
	if d == nil {
		return fail(http.StatusNotFound, codePagesDeploymentNotFound, "Deployment not found") // UNVERIFIED
	}
	return ok(d.json(c.s, p, c.now))
}

func pagesDeploymentDelete(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	i, d := p.deployment(c.params["deployment_id"])
	if d == nil {
		return fail(http.StatusNotFound, codePagesDeploymentNotFound, "Deployment not found") // UNVERIFIED
	}
	if p.canonical(c.now) == d {
		return fail(http.StatusBadRequest, codePagesLiveDeployment,
			"You cannot delete the active production deployment. Deploy or roll back to another deployment first.") // UNVERIFIED
	}
	if p.aliased(d) && c.query.Get("force") != "true" {
		return fail(http.StatusBadRequest, codePagesAliasedDeployment,
			"This deployment has an active alias. Delete it with force=true to remove the alias as well.") // UNVERIFIED
	}
	p.Deployments = append(p.Deployments[:i], p.Deployments[i+1:]...)
	return ok(nil)
}
