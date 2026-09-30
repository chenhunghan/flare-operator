package pagesdeployment

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"lukechampine.com/blake3"

	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller/pagesproject"
)

// The Pages Direct Upload flow, as wrangler's `pages deploy` runs it (SOURCED, relies:
// cloudflare/workers-sdk@485cfb3:packages/wrangler/src/api/pages/deploy.ts#L109-L510 and
// packages/wrangler/src/pages/upload.ts#L96-L395; checked against the published bundle the
// differential tests run, wrangler@4.143.0:wrangler-dist/cli.js: the hash at #L162160-L162165,
// the ignore list at #L298546-L298556, the bucket limits at #L181544-L181545, the manifest at
// #L299080; test/differential/pages_test.go compares wrangler's hashes with HashFile):
//
//  1. GET …/pages/projects/{name}/upload-token → a JWT, whose max_file_count_allowed claim
//     bounds the file count (upload.ts#L397-L425; 20000 without it).
//  2. Hash every file (HashFile below) and POST /pages/assets/check-missing with all hashes.
//  3. POST /pages/assets/upload the missing files, base64, in buckets of at most 40 MiB and
//     2000 files, largest first (upload.ts#L173-L212, constants.ts#L18-L21).
//  4. POST /pages/assets/upsert-hashes with all hashes (upload.ts#L337-L387; a failure there
//     only warns in wrangler, and does here).
//  5. POST …/deployments with the manifest ("/<path>" → hash) and the routing files.
//
// The asset calls authenticate with "Authorization: Bearer <jwt>", not the account's token.
// cfclient owns the Authorization header (frozen contract), so they go through a cfclient
// built for the JWT with the account's other client options (reconcile.Accounts.NewClient:
// HTTP client and timeout, User-Agent, rate limit), cached per project until shortly before
// the token expires.

// Limits of wrangler's upload (packages/wrangler/src/pages/constants.ts#L15-L21).
const (
	maxAssetCountDefault = 20000
	maxAssetSize         = 25 << 20
	maxBucketSize        = 40 << 20
	maxBucketFileCount   = 2000
)

// codeUnauthorized is Pages' "Authorization failed" of an expired or refused upload JWT
// (SOURCED, statement: ApiErrorCodes.UNAUTHORIZED, packages/wrangler/src/pages/errors.ts#L14).
const codeUnauthorized = 8000013

// HashFile is wrangler's asset hash: the first 32 hex digits of BLAKE3 over the file's
// base64 encoding followed by its extension without the dot (SOURCED, relies:
// packages/deploy-helpers/src/deploy/helpers/hash.ts#L5-L13; test vector: "foobar" as
// logo.png → 2082190357cfd3617ccfe04f340c6247, packages/wrangler/src/__tests__/pages/deploy.test.ts#L469-L491).
func HashFile(p string, content []byte) string {
	sum := blake3.Sum256([]byte(base64.StdEncoding.EncodeToString(content) + nodeExt(p)))
	return hex.EncodeToString(sum[:])[:32]
}

// nodeExt is Node's path.extname(p).substring(1): the text after the last "." of the base
// name, unless that dot is its first character ("" for ".bashrc" and "README").
func nodeExt(p string) string {
	base := p[strings.LastIndex(p, "/")+1:]
	i := strings.LastIndex(base, ".")
	if i < 1 {
		return ""
	}
	return base[i+1:]
}

// Special files: sent as deployment parts, not uploaded as assets. wrangler's ignore list
// (packages/wrangler/src/pages/validate.ts#L60-L70) also drops a top-level functions directory
// and .wrangler, and .DS_Store, node_modules and .git at any depth.
var routingFiles = map[string]bool{"_headers": true, "_redirects": true, "_routes.json": true, "_worker.js": true}

// ignored reports whether wrangler's validate would skip path p.
func ignored(p string) bool {
	segs := strings.Split(p, "/")
	if routingFiles[segs[0]] || segs[0] == "functions" || segs[0] == ".wrangler" {
		return true
	}
	for i, s := range segs {
		if s == "node_modules" || s == ".git" || (s == ".DS_Store" && i == len(segs)-1) {
			return true
		}
	}
	return false
}

// assetFile is one file to upload.
type assetFile struct {
	path, hash, contentType string
	content                 []byte
}

// plan is what a deployment of a tree sends.
type plan struct {
	assets []assetFile
	form   deployForm
}

// planTree splits t into assets and routing files. A _worker.js directory (which wrangler
// bundles) cannot be deployed as is. Neither can a top-level functions directory without a
// _worker.js file: wrangler compiles Pages Functions into the deployment's Worker unless
// _worker.js is given, and only then ignores the directory (SOURCED, relies:
// wrangler@4.143.0:wrangler-dist/cli.js#L299030-L299058, `if (!_workerJS &&
// fs.existsSync(functionsDirectory))` → buildFunctions). It is refused rather than dropped,
// which would deploy the site without its Functions.
func planTree(t *artifact.Tree, maxFiles int) (*plan, error) {
	p := &plan{form: deployForm{manifest: map[string]string{}, files: map[string][]byte{}}}
	_, workerJS := t.File("_worker.js")
	for _, f := range t.Files() {
		if strings.HasPrefix(f.Path, "_worker.js/") {
			return nil, fmt.Errorf("the artifact has a _worker.js directory, which needs bundling (wrangler pages deploy); provide a single bundled _worker.js file instead")
		}
		if strings.HasPrefix(f.Path, "functions/") && !workerJS {
			return nil, fmt.Errorf("the artifact has a functions directory (Pages Functions), which needs compiling (wrangler pages functions build); provide the compiled Worker as a single _worker.js file instead (with it, the functions directory is ignored)")
		}
		if routingFiles[f.Path] {
			p.form.files[f.Path] = f.Content
			continue
		}
		if ignored(f.Path) {
			continue
		}
		if len(f.Content) > maxAssetSize {
			return nil, fmt.Errorf("%s is %d bytes: Pages takes files up to 25 MiB", f.Path, len(f.Content))
		}
		h := HashFile(f.Path, f.Content)
		p.assets = append(p.assets, assetFile{path: f.Path, hash: h, contentType: ContentType(f.Path), content: f.Content})
		p.form.manifest["/"+f.Path] = h
	}
	if len(p.assets) > maxFiles {
		return nil, fmt.Errorf("the artifact has %d files; Pages takes at most %d in a deployment", len(p.assets), maxFiles)
	}
	return p, nil
}

// ContentType is the contentType metadata of the asset at p (a "/"-separated path relative to
// the artifact's root): mime@3.0.0's getType(p), else application/octet-stream, as wrangler's
// validate walk computes it (SOURCED, relies: validate.ts#L136-L141;
// wrangler@4.143.0:wrangler-dist/cli.js#L298603, getType at #L298453-L298459; mime_table.go is
// generated from the bundled mime by hack/pages-mime-gen.mjs). Like getType, the extension is
// the lower-cased text after the last "." of the base name, and a base name without a dot
// (after its first character) counts as an extension only when p has no directory: "html" is
// text/html, "a/html" and "a/.html" are not. "\" separates directories as "/" does.
func ContentType(p string) string {
	hasPath, last := false, p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		hasPath, last = true, p[i+1:]
	}
	last = strings.ToLower(last)
	if dot := strings.LastIndex(last, "."); dot > 0 || !hasPath {
		if t, ok := mimeTypes[last[dot+1:]]; ok {
			return t
		}
	}
	return "application/octet-stream"
}

// jwtClients caches a cfclient per upload JWT.
type jwtClients struct {
	mu sync.Mutex
	m  map[string]jwtClient
}

type jwtClient struct {
	cf       cfclient.Client
	maxFiles int
	exp      time.Time
}

// jwtRefreshMargin: a cached upload JWT is replaced this long before it expires.
const jwtRefreshMargin = time.Minute

// uploader runs one deployment's upload for a project.
type uploader struct {
	cf               cfclient.Client // the account's client
	accountID        string
	project, baseURL string
	cache            *jwtClients
	// newClient builds the client of an upload JWT: the account's client options (HTTP
	// client and timeout, User-Agent, rate limit) with the JWT as the token.
	newClient func(token string) (cfclient.Client, error)
}

func (u *uploader) key() string { return u.baseURL + "|" + u.accountID + "|" + u.project }

// jwt returns a client authenticated with the project's upload JWT (fresh when refresh).
func (u *uploader) jwt(ctx context.Context, refresh bool) (jwtClient, error) {
	u.cache.mu.Lock()
	if u.cache.m == nil {
		u.cache.m = map[string]jwtClient{}
	}
	c, ok := u.cache.m[u.key()]
	u.cache.mu.Unlock()
	if ok && !refresh && time.Now().Add(jwtRefreshMargin).Before(c.exp) {
		return c, nil
	}
	resp, err := u.cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: pagesproject.ProjectPath(u.accountID, u.project) + "/upload-token"})
	if err != nil {
		return jwtClient{}, fmt.Errorf("upload token: %w", err)
	}
	var tok struct {
		JWT string `json:"jwt"`
	}
	if err := json.Unmarshal(resp.Result, &tok); err != nil || tok.JWT == "" {
		return jwtClient{}, fmt.Errorf("decode upload token: %v", err)
	}
	claims := decodeClaims(tok.JWT)
	c = jwtClient{maxFiles: maxAssetCountDefault, exp: time.Now().Add(2 * jwtRefreshMargin)}
	if claims.MaxFileCountAllowed > 0 {
		c.maxFiles = claims.MaxFileCountAllowed
	}
	if claims.Exp > 0 {
		c.exp = time.Unix(claims.Exp, 0)
	}
	if c.cf, err = u.newClient(tok.JWT); err != nil {
		return jwtClient{}, err
	}
	u.cache.mu.Lock()
	u.cache.m[u.key()] = c
	u.cache.mu.Unlock()
	return c, nil
}

type jwtClaims struct {
	Exp                 int64 `json:"exp"`
	MaxFileCountAllowed int   `json:"max_file_count_allowed"`
}

// decodeClaims reads the JWT's payload without verifying it (as wrangler does,
// upload.ts#L407-L411: an invalid token fails the uploads anyway).
func decodeClaims(tok string) jwtClaims {
	var c jwtClaims
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return c
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return c
		}
	}
	_ = json.Unmarshal(raw, &c)
	return c
}

// withJWT calls fn with the upload client, once more with a fresh token when the first one is
// refused as unauthorized (wrangler refreshes on 8000013 or an expired token, upload.ts#L158-L164).
func (u *uploader) withJWT(ctx context.Context, fn func(jwtClient) error) error {
	c, err := u.jwt(ctx, false)
	if err != nil {
		return err
	}
	err = fn(c)
	if ae, ok := cfclient.AsAPIError(err); ok && (ae.HasCode(codeUnauthorized) || ae.Status == http.StatusUnauthorized) {
		if c, err = u.jwt(ctx, true); err != nil {
			return err
		}
		return fn(c)
	}
	return err
}

// maxFiles is the upload JWT's file limit.
func (u *uploader) maxFiles(ctx context.Context) (int, error) {
	c, err := u.jwt(ctx, false)
	return c.maxFiles, err
}

// upload makes every asset of p present in the project's asset store.
func (u *uploader) upload(ctx context.Context, p *plan) error {
	hashes := make([]string, 0, len(p.assets))
	byHash := map[string]assetFile{}
	for _, a := range p.assets {
		if _, dup := byHash[a.hash]; !dup {
			hashes = append(hashes, a.hash)
		}
		byHash[a.hash] = a
	}
	sort.Strings(hashes)
	var missing []string
	err := u.withJWT(ctx, func(c jwtClient) error {
		resp, err := c.cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: "/pages/assets/check-missing", Body: map[string]any{"hashes": hashes}})
		if err != nil {
			return err
		}
		return json.Unmarshal(resp.Result, &missing)
	})
	if err != nil {
		return fmt.Errorf("check missing assets: %w", err)
	}
	files := make([]assetFile, 0, len(missing))
	for _, h := range missing {
		if a, ok := byHash[h]; ok {
			files = append(files, a)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return len(files[i].content) > len(files[j].content) })
	for _, bucket := range buckets(files) {
		payload := make([]map[string]any, 0, len(bucket))
		for _, a := range bucket {
			payload = append(payload, map[string]any{"key": a.hash, "value": base64.StdEncoding.EncodeToString(a.content),
				"metadata": map[string]any{"contentType": a.contentType}, "base64": true})
		}
		if err := u.withJWT(ctx, func(c jwtClient) error {
			_, err := c.cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: "/pages/assets/upload", Body: payload})
			return err
		}); err != nil {
			return fmt.Errorf("upload assets: %w", err)
		}
	}
	if len(hashes) > 0 {
		// wrangler only warns when this fails ("you might need to re-upload for future
		// deployments", upload.ts#L381-L387); a failure here is returned, so it is retried.
		if err := u.withJWT(ctx, func(c jwtClient) error {
			_, err := c.cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: "/pages/assets/upsert-hashes", Body: map[string]any{"hashes": hashes}})
			return err
		}); err != nil {
			return fmt.Errorf("upsert asset hashes: %w", err)
		}
	}
	return nil
}

// buckets groups files (largest first) into upload requests of at most maxBucketSize bytes and
// maxBucketFileCount files; a file larger than a bucket goes alone.
func buckets(files []assetFile) [][]assetFile {
	var out [][]assetFile
	var cur []assetFile
	size := 0
	for _, f := range files {
		if len(cur) > 0 && (size+len(f.content) > maxBucketSize || len(cur) >= maxBucketFileCount) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += len(f.content)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
