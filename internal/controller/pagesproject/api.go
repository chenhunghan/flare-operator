package pagesproject

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// Pages project API calls (x-fern-sdk-group-name "pages"). No real-API recording of Pages
// exists; shapes follow the pinned spec and wrangler@4.143.0's `pages project` commands
// (internal/fake/pages.go cites them). The flarefake profile emulates them.

// CodeProjectNotFound is Pages' "Project not found" (SOURCED, statement: wrangler treats code
// 8000007 as a missing project, cloudflare/workers-sdk@485cfb3:packages/wrangler/src/pages/projects.ts#L135-L136).
const CodeProjectNotFound = 8000007

// ProjectsPath is the account's project collection.
func ProjectsPath(accountID string) string { return "/accounts/" + accountID + "/pages/projects" }

// ProjectPath is one project.
func ProjectPath(accountID, name string) string {
	return ProjectsPath(accountID) + "/" + url.PathEscape(name)
}

// APIDeploymentRef is the part of a project's latest_deployment / canonical_deployment the
// controllers read.
type APIDeploymentRef struct {
	ID string `json:"id"`
}

// APIProject is a project as GET/POST/PATCH return it (the pinned spec's pages_project). The
// deployment configs are kept as decoded JSON: their maps are compared key by key.
type APIProject struct {
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	Subdomain           string                    `json:"subdomain"`
	Domains             []string                  `json:"domains"`
	CreatedOn           string                    `json:"created_on"`
	ProductionBranch    string                    `json:"production_branch"`
	BuildConfig         map[string]any            `json:"build_config"`
	DeploymentConfigs   map[string]map[string]any `json:"deployment_configs"`
	Source              map[string]any            `json:"source"`
	LatestDeployment    *APIDeploymentRef         `json:"latest_deployment"`
	CanonicalDeployment *APIDeploymentRef         `json:"canonical_deployment"`
}

// GetProject returns the project, or nil when it does not exist (404; code 8000007).
func GetProject(ctx context.Context, cf cfclient.Client, accountID, name string) (*APIProject, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: ProjectPath(accountID, name)})
	if err != nil {
		if cfclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return decodeProject(resp)
}

func decodeProject(resp *cfclient.Response) (*APIProject, error) {
	var p APIProject
	if err := json.Unmarshal(resp.Result, &p); err != nil {
		return nil, fmt.Errorf("decode Pages project: %w", err)
	}
	return &p, nil
}

func createProject(ctx context.Context, cf cfclient.Client, accountID string, body map[string]any) (*APIProject, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: ProjectsPath(accountID), Body: body})
	if err != nil {
		return nil, err
	}
	return decodeProject(resp)
}

func patchProject(ctx context.Context, cf cfclient.Client, accountID, name string, body map[string]any) (*APIProject, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPatch, Path: ProjectPath(accountID, name), Body: body})
	if err != nil {
		return nil, err
	}
	return decodeProject(resp)
}

// deleteProject deletes the project and, with it, every deployment (wrangler's `pages project
// delete` sends a bare DELETE, packages/wrangler/src/pages/projects.ts#L374-L378).
func deleteProject(ctx context.Context, cf cfclient.Client, accountID, name string) error {
	_, err := cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: ProjectPath(accountID, name)})
	return err
}
