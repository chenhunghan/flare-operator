package fake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Pages Direct Upload assets. The flow is wrangler's `pages deploy`
// (SOURCED, relies: cloudflare/workers-sdk@485cfb3:packages/wrangler/src/api/pages/deploy.ts#L254-L269
// and packages/wrangler/src/pages/upload.ts#L96-L395):
//
//  1. GET /accounts/{account_id}/pages/projects/{project_name}/upload-token → {jwt}.
//  2. POST /pages/assets/check-missing {hashes} with "Authorization: Bearer <jwt>" → the hashes
//     the asset store lacks.
//  3. POST /pages/assets/upload, a JSON array of {key, value (base64), metadata {contentType},
//     base64: true}, in buckets.
//  4. POST /pages/assets/upsert-hashes {hashes} (every hash of the deployment).
//  5. POST …/deployments with the manifest (path → hash), pages_deployments.go.
//
// The upload JWT is a real JWT here, because wrangler decodes its payload: it reads the
// max_file_count_allowed claim (SOURCED, statement: upload.ts#L397-L425; 20000 when absent,
// packages/wrangler/src/pages/constants.ts#L15) and exp (SOURCED, relies: an expired token is
// fetched again, packages/deploy-helpers/src/deploy/helpers/jwt.ts#L5-L25). Its other claims,
// lifetime and signature are UNVERIFIED. The asset store is scoped to the token's project
// (UNVERIFIED: the real store's scope is not documented).

// pagesJWTLifetime is how long an upload token is valid (UNVERIFIED).
const pagesJWTLifetime = 5 * time.Minute

// pagesMaxFileCount is the max_file_count_allowed claim (wrangler's default, constants.ts#L15).
const pagesMaxFileCount = 20000

type pagesClaims struct {
	AccountID           string `json:"account_id"`
	ProjectName         string `json:"project_name"`
	Exp                 int64  `json:"exp"`
	Iat                 int64  `json:"iat"`
	MaxFileCountAllowed int    `json:"max_file_count_allowed"`
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *Server) signPagesJWT(cl pagesClaims) string {
	head := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(cl)
	signing := head + "." + b64url(body)
	mac := hmac.New(sha256.New, s.pagesJWTKey)
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil))
}

// pagesClaimsOf verifies the request's bearer JWT and returns its project, or a 401.
func (c *reqCtx) pagesJWTProject() (*pagesProject, *response) {
	unauthorized := func() (*pagesProject, *response) {
		// Status UNVERIFIED (wrangler's fixture answers 200 with success false,
		// packages/wrangler/src/__tests__/pages/deploy.test.ts#L1240-L1256; the spec has 4XX).
		r := fail(http.StatusUnauthorized, codePagesUnauthorized, "Authorization failed")
		return nil, &r
	}
	tok := strings.TrimPrefix(c.r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return unauthorized()
	}
	mac := hmac.New(sha256.New, c.s.pagesJWTKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return unauthorized()
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	var cl pagesClaims
	if err != nil || json.Unmarshal(raw, &cl) != nil || cl.Exp <= c.now.Unix() {
		return unauthorized()
	}
	a := c.s.accountLocked(cl.AccountID)
	p := a.pages[cl.ProjectName]
	if p == nil {
		return unauthorized() // the project was deleted: its token is worthless (UNVERIFIED)
	}
	return p, nil
}

func pagesUploadToken(c *reqCtx) response {
	p, r := c.project()
	if r != nil {
		return *r
	}
	jwt := c.s.signPagesJWT(pagesClaims{AccountID: c.account.id, ProjectName: p.Name, Iat: c.now.Unix(),
		Exp: c.now.Add(pagesJWTLifetime).Unix(), MaxFileCountAllowed: pagesMaxFileCount})
	return ok(map[string]any{"jwt": jwt})
}

type pagesHashesBody struct {
	Hashes []string `json:"hashes"`
}

// pagesHashRe: wrangler's hashes are 32 lowercase hex characters (the first 128 bits of a
// BLAKE3 hex digest, packages/deploy-helpers/src/deploy/helpers/hash.ts#L5-L13). Whether the API
// checks the format is UNVERIFIED.
var pagesHashRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func pagesCheckMissing(c *reqCtx) response {
	p, r := c.pagesJWTProject()
	if r != nil {
		return *r
	}
	var b pagesHashesBody
	if r := c.decodeJSON(&b); r != nil {
		return *r
	}
	missing := []string{}
	seen := map[string]bool{}
	for _, h := range b.Hashes {
		if !pagesHashRe.MatchString(h) {
			return pagesInvalid("invalid hash " + h)
		}
		if p.Assets[h] == nil && !seen[h] {
			missing = append(missing, h)
		}
		seen[h] = true
	}
	// SOURCED (fixture): the result is the list of missing hashes,
	// packages/wrangler/src/__tests__/pages/deploy.test.ts#L480-L503.
	return ok(missing)
}

type pagesUploadItem struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Metadata struct {
		ContentType string `json:"contentType"`
	} `json:"metadata"`
	Base64 bool `json:"base64"`
}

// pagesMaxAssetSize is wrangler's per-file limit (constants.ts#L16); the API's is UNVERIFIED.
const pagesMaxAssetSize = 25 << 20

func pagesAssetsUpload(c *reqCtx) response {
	p, r := c.pagesJWTProject()
	if r != nil {
		return *r
	}
	var items []pagesUploadItem
	if r := c.decodeJSON(&items); r != nil {
		return *r
	}
	decoded := make([][]byte, len(items))
	for i, it := range items {
		if !pagesHashRe.MatchString(it.Key) {
			return pagesInvalid("invalid asset key " + it.Key)
		}
		v := []byte(it.Value)
		if it.Base64 {
			var err error
			if v, err = base64.StdEncoding.DecodeString(it.Value); err != nil {
				return pagesInvalid("asset " + it.Key + ": value is not base64")
			}
		}
		if len(v) > pagesMaxAssetSize {
			return pagesInvalid("asset " + it.Key + " is larger than 25 MiB")
		}
		decoded[i] = v
	}
	// All or nothing (UNVERIFIED). The key is not checked against the content: the hash covers
	// the file extension, which the API never sees (hash.ts#L5-L13).
	for i, it := range items {
		p.Assets[it.Key] = &pagesAsset{Content: decoded[i], ContentType: it.Metadata.ContentType}
	}
	// SOURCED (fixture): result null, deploy.test.ts#L520-L523.
	return ok(nil)
}

func pagesUpsertHashes(c *reqCtx) response {
	p, r := c.pagesJWTProject()
	if r != nil {
		return *r
	}
	var b pagesHashesBody
	if r := c.decodeJSON(&b); r != nil {
		return *r
	}
	for _, h := range b.Hashes {
		if a := p.Assets[h]; a != nil {
			a.Upserted = true // unknown hashes are ignored (UNVERIFIED)
		}
	}
	return ok(nil)
}

// PagesAsset returns an uploaded asset of a project (tests).
func (s *Server) PagesAsset(accountID, project, hash string) (content []byte, contentType string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accountLocked(accountID)
	p := a.pages[project]
	if p == nil || p.Assets[hash] == nil {
		return nil, "", false
	}
	as := p.Assets[hash]
	return append([]byte(nil), as.Content...), as.ContentType, true
}

// PagesProjectConfig returns a project's stored deployment config of env ("production" or
// "preview"), secret values included (tests; the API never returns them).
func (s *Server) PagesProjectConfig(accountID, project, env string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.accountLocked(accountID).pages[project]
	if p == nil {
		return nil, false
	}
	return p.clone().Configs[env], true
}
