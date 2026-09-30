package pagesdeployment

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

	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller/pagesproject"
)

// Pages deployment API calls (x-fern-sdk-group-name "pages.deployments"). No real-API
// recording of Pages exists; the calls are wrangler@4.143.0's `pages deploy` and
// `pages deployment list|delete` (cited per call; internal/fake/pages_deployments.go emulates
// them).

func deploymentsPath(accountID, project string) string {
	return pagesproject.ProjectPath(accountID, project) + "/deployments"
}

func deploymentPath(accountID, project, id string) string {
	return deploymentsPath(accountID, project) + "/" + url.PathEscape(id)
}

type apiStage struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	StartedOn *string `json:"started_on"`
	EndedOn   *string `json:"ended_on"`
}

// apiDeployment is a deployment (the pinned spec's pages_deployment; the fields read here).
type apiDeployment struct {
	ID                string    `json:"id"`
	ShortID           string    `json:"short_id"`
	ProjectName       string    `json:"project_name"`
	Environment       string    `json:"environment"`
	URL               string    `json:"url"`
	Aliases           []string  `json:"aliases"`
	CreatedOn         string    `json:"created_on"`
	ModifiedOn        string    `json:"modified_on"`
	LatestStage       *apiStage `json:"latest_stage"`
	DeploymentTrigger struct {
		Metadata struct {
			Branch     string `json:"branch"`
			CommitHash string `json:"commit_hash"`
		} `json:"metadata"`
	} `json:"deployment_trigger"`
}

// deployed reports the deploy stage's success; failed its failure (or cancellation). wrangler
// waits for exactly these (SOURCED, relies: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/pages/deploy.ts#L535-L590).
func (d *apiDeployment) deployed() bool {
	return d.LatestStage != nil && d.LatestStage.Name == "deploy" && d.LatestStage.Status == "success"
}

func (d *apiDeployment) failed() bool {
	return d.LatestStage != nil && (d.LatestStage.Status == "failure" || d.LatestStage.Status == "canceled")
}

// getDeployment returns the deployment, or nil when it (or its project) does not exist.
func getDeployment(ctx context.Context, cf cfclient.Client, accountID, project, id string) (*apiDeployment, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: deploymentPath(accountID, project, id)})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var d apiDeployment
	if err := json.Unmarshal(resp.Result, &d); err != nil {
		return nil, fmt.Errorf("decode Pages deployment: %w", err)
	}
	return &d, nil
}

// listDeployments returns every deployment of project (all pages; env "production" or
// "preview" filters, as wrangler's `pages deployment list --environment` does,
// packages/wrangler/src/pages/deployments.ts#L72-L79). A missing project lists nothing.
func listDeployments(ctx context.Context, cf cfclient.Client, accountID, project, env string) ([]apiDeployment, error) {
	q := url.Values{}
	if env != "" {
		q.Set("env", env)
	}
	items, err := cfclient.ListAllInto[apiDeployment](ctx, cf, cfclient.Request{Path: deploymentsPath(accountID, project), Query: q})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return items, nil
}

// deleteDeployment deletes a deployment with force=true, which lets Cloudflare delete a
// preview deployment that holds its branch alias (the spec's "Allow deletion when a
// non-production deployment has an active alias"; wrangler's `pages deployment delete --force`
// sends it, packages/wrangler/src/pages/deployments.ts#L190-L195). The live production
// deployment is refused regardless.
func deleteDeployment(ctx context.Context, cf cfclient.Client, accountID, project, id string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: deploymentPath(accountID, project, id),
		Query: url.Values{"force": {"true"}}})
	return err
}

// deployForm is the multipart body of POST …/deployments, as wrangler builds it (SOURCED,
// relies: packages/wrangler/src/api/pages/deploy.ts#L271-L312): the fields manifest (JSON path →
// hash), branch, commit_message and commit_hash, then _headers and _redirects as files (undici
// sends a File without a type as application/octet-stream). _routes.json goes as a file too
// (deploy.ts#L401-L404 sends it only with a Worker; Cloudflare's use of it without one is
// UNVERIFIED). _worker.js is the pinned spec's advanced-mode Worker part (mutually exclusive
// with _worker.bundle, which wrangler sends after bundling; UNVERIFIED: wrangler never sends
// _worker.js), with the content type the spec's encoding lists first.
type deployForm struct {
	manifest                map[string]string
	branch, commit, message string
	files                   map[string][]byte // _headers, _redirects, _routes.json, _worker.js
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func (f deployForm) encode() ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	m, err := json.Marshal(f.manifest)
	if err != nil {
		return nil, "", err
	}
	fields := [][2]string{{"manifest", string(m)}}
	if f.branch != "" {
		fields = append(fields, [2]string{"branch", f.branch})
	}
	if f.message != "" {
		fields = append(fields, [2]string{"commit_message", f.message})
	}
	if f.commit != "" {
		fields = append(fields, [2]string{"commit_hash", f.commit})
	}
	for _, kv := range fields {
		if err := w.WriteField(kv[0], kv[1]); err != nil {
			return nil, "", err
		}
	}
	for _, name := range []string{"_headers", "_redirects", "_routes.json", "_worker.js"} {
		b, ok := f.files[name]
		if !ok {
			continue
		}
		ct := "application/octet-stream"
		if name == "_worker.js" {
			ct = "application/javascript+module"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscaper.Replace(name), quoteEscaper.Replace(name)))
		h.Set("Content-Type", ct)
		pw, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := pw.Write(b); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func createDeployment(ctx context.Context, cf cfclient.Client, accountID, project string, f deployForm) (*apiDeployment, error) {
	body, ct, err := f.encode()
	if err != nil {
		return nil, err
	}
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: deploymentsPath(accountID, project), RawBody: body, ContentType: ct})
	if err != nil {
		return nil, err
	}
	var d apiDeployment
	if err := json.Unmarshal(resp.Result, &d); err != nil {
		return nil, fmt.Errorf("decode Pages deployment: %w", err)
	}
	return &d, nil
}
