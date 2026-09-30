package workerscript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"lukechampine.com/blake3"

	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/cfclient"
)

// Workers static assets (forProvider.assets). The manifest and the upload follow wrangler at
// the differential tests' pin, cloudflare/workers-sdk@3bdcd0d (tag wrangler@4.143.0):
//
//   - Manifest (packages/deploy-helpers/src/deploy/helpers/assets.ts#L382-L436,
//     buildAssetManifest): every file of the directory except the root .assetsignore, _headers
//     and _redirects and what .assetsignore ignores (gitignore syntax;
//     packages/workers-shared/utils/helpers.ts#L65-L93), keyed "/" + its relative path
//     (helpers.ts#L12-L18), with {hash, size}. A file over 25 MiB is refused
//     (packages/workers-shared/utils/constants.ts#L32), as is a _worker.js file or directory
//     without an .assetsignore (assets.ts#L487-L516: it would publish server code).
//   - Hash (packages/deploy-helpers/src/deploy/helpers/hash.ts#L5-L13): BLAKE3 of the base64 of
//     the content followed by the extension without its dot, hex, first 32 digits.
//   - Upload (assets.ts#L72-L363): POST …/scripts/{name}/assets-upload-session {manifest} →
//     {jwt, buckets}; each bucket is one POST …/workers/assets/upload?base64=true with
//     "Authorization: Bearer <session jwt>", a multipart part per hash (name and file name the
//     hash, the base64 content, the Content-Type to serve or application/null for none); the
//     response that carries a jwt is the completion token. No bucket: the session's jwt already
//     is the completion token (assets.ts#L114-L134).
//   - Metadata (packages/deploy-helpers/src/deploy/helpers/create-worker-upload-form.ts#L91-L114,
//     #L899-L905): assets: {jwt, config: {html_handling, not_found_handling, run_worker_first,
//     _redirects, _headers}}; the _headers and _redirects files' contents go into the config
//     (assets.ts#L567-L589). An assets-only Worker's metadata has no main_module and no part.
//
// The operator runs the session only when the manifest or config changed (status.assetsHash);
// an upload of changed code with unchanged assets sends keep_assets: true instead (DOCS
// https://developers.cloudflare.com/workers/static-assets/direct-upload/, "Create/Deploy New
// Version"), so unchanged assets cost no asset call.

// Limits of assets (wrangler's MAX_ASSET_SIZE and MAX_ASSET_COUNT, constants.ts#L30-L32).
const (
	maxAssetBytes = 25 << 20
	maxAssetFiles = 100000
)

// Root files wrangler treats specially (constants.ts#L34-L36, assets.ts#L487).
const (
	assetsIgnoreFile     = ".assetsignore"
	assetsHeadersFile    = "_headers"
	assetsRedirectsFile  = "_redirects"
	legacyPagesWorkerJS  = "_worker.js"
	assetNullContentType = "application/null" // "serve without a Content-Type" (assets.ts#L229-L233)
)

// apiAssetsConfig is metadata.assets.config (the pinned spec's workers_assets-2).
type apiAssetsConfig struct {
	HTMLHandling     string `json:"html_handling,omitempty"`
	NotFoundHandling string `json:"not_found_handling,omitempty"`
	// RunWorkerFirst is a bool or a list of route rules.
	RunWorkerFirst any    `json:"run_worker_first,omitempty"`
	BasePath       string `json:"base_path,omitempty"`
	Headers        string `json:"_headers,omitempty"`
	Redirects      string `json:"_redirects,omitempty"`
}

// apiAssets is metadata.assets.
type apiAssets struct {
	JWT    string           `json:"jwt,omitempty"`
	Config *apiAssetsConfig `json:"config,omitempty"`
}

// assetFile is one manifest entry and the file behind it.
type assetFile struct {
	path        string // manifest key, "/" + the artifact path
	hash        string
	size        int64
	contentType string
	content     []byte
}

// assetManifest is the manifest of an artifact tree (without the config).
type assetManifest struct {
	files     []assetFile // by path
	byHash    map[string]*assetFile
	headers   string // content of _headers ("" without)
	redirects string // content of _redirects
}

// assetPlan is what forProvider.assets resolved to.
type assetPlan struct {
	*assetManifest
	config apiAssetsConfig
	// hash covers the manifest and the config: what status.assetsHash records.
	hash string
}

// manifestJSON is the session request's manifest.
func (m *assetManifest) manifestJSON() map[string]map[string]any {
	out := make(map[string]map[string]any, len(m.files))
	for _, f := range m.files {
		out[f.path] = map[string]any{"hash": f.hash, "size": f.size}
	}
	return out
}

// newAssetPlan combines a manifest with the spec's config.
func newAssetPlan(m *assetManifest, c *workersv1alpha1.WorkerAssetsConfig) *assetPlan {
	p := &assetPlan{assetManifest: m}
	if c != nil {
		p.config = apiAssetsConfig{HTMLHandling: c.HTMLHandling, NotFoundHandling: c.NotFoundHandling, BasePath: c.BasePath}
		switch {
		case c.RunWorkerFirst != nil:
			p.config.RunWorkerFirst = *c.RunWorkerFirst
		case len(c.RunWorkerFirstPaths) > 0:
			p.config.RunWorkerFirst = append([]string(nil), c.RunWorkerFirstPaths...)
		}
	}
	p.config.Headers, p.config.Redirects = m.headers, m.redirects
	h := sha256.New()
	fmt.Fprintf(h, "assets-v1\x00")
	for _, f := range m.files {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00", f.path, f.hash, f.size, f.contentType)
	}
	cfg, _ := json.Marshal(p.config)
	h.Write(cfg)
	p.hash = hex.EncodeToString(h.Sum(nil))
	return p
}

// assetHash is wrangler's hashFile for content at path p (hash.ts#L5-L13).
func assetHash(content []byte, p string) string {
	sum := blake3.Sum256([]byte(base64.StdEncoding.EncodeToString(content) + strings.TrimPrefix(nodeExtname(p), ".")))
	return hex.EncodeToString(sum[:16])
}

// nodeExtname is node's path.posix.extname, which hashFile uses: the last "." of the last
// segment and what follows, except for a name that starts with its only dot (".bashrc" → "")
// and "..".
func nodeExtname(p string) string {
	startDot, startPart, end := -1, 0, -1
	matchedSlash := true
	preDotState := 0
	for i := len(p) - 1; i >= 0; i-- {
		ch := p[i]
		if ch == '/' {
			if !matchedSlash {
				startPart = i + 1
				break
			}
			continue
		}
		if end == -1 {
			matchedSlash = false
			end = i + 1
		}
		if ch == '.' {
			if startDot == -1 {
				startDot = i
			} else if preDotState != 1 {
				preDotState = 1
			}
		} else if startDot != -1 {
			preDotState = -1
		}
	}
	if startDot == -1 || end == -1 || preDotState == 0 ||
		(preDotState == 1 && startDot == end-1 && startDot == startPart+1) {
		return ""
	}
	return p[startDot:end]
}

// assetContentType is the Content-Type an asset is served with: the artifact loader's table by
// extension, and application/null (no Content-Type) for an extension it does not know, as
// wrangler sends for one its mime table does not know (helpers.ts#L20-L30, assets.ts#L229-L233).
// wrangler's table is the npm mime package's, so an extension outside this table may differ.
func assetContentType(p string) string {
	if ct := artifact.ContentType(p); ct != artifact.DefaultContentType {
		return ct
	}
	return assetNullContentType
}

// buildAssetManifest builds the manifest of an artifact's files (in path order; package doc).
func buildAssetManifest(files []artifact.File) (*assetManifest, *problem) {
	root := map[string][]byte{}
	for _, f := range files {
		switch f.Path {
		case assetsIgnoreFile, assetsHeadersFile, assetsRedirectsFile:
			root[f.Path] = f.Content
		}
	}
	patterns := []string{"/" + assetsIgnoreFile, "/" + assetsRedirectsFile, "/" + assetsHeadersFile}
	ignoreFile, hasIgnore := root[assetsIgnoreFile]
	if hasIgnore {
		patterns = append(patterns, strings.Split(string(ignoreFile), "\n")...)
	}
	ign := newIgnoreMatcher(patterns)
	m := &assetManifest{byHash: map[string]*assetFile{}, headers: string(root[assetsHeadersFile]), redirects: string(root[assetsRedirectsFile])}
	for _, f := range files {
		if ign.ignored(f.Path) {
			continue
		}
		if !hasIgnore && (f.Path == legacyPagesWorkerJS || strings.HasPrefix(f.Path, legacyPagesWorkerJS)) {
			return nil, invalid("assets: %s would publish a Pages %s as a static asset (its server code); remove it, or add an %s file "+
				"to the root of the assets (listing %s to leave it out, or empty to publish it)", f.Path, legacyPagesWorkerJS, assetsIgnoreFile, legacyPagesWorkerJS)
		}
		if len(f.Content) > maxAssetBytes {
			return nil, invalid("assets: %s is %d bytes; a static asset may be at most %d (25 MiB)", f.Path, len(f.Content), maxAssetBytes)
		}
		m.files = append(m.files, assetFile{path: "/" + f.Path, hash: assetHash(f.Content, f.Path), size: int64(len(f.Content)),
			contentType: assetContentType(f.Path), content: f.Content})
	}
	if len(m.files) > maxAssetFiles {
		return nil, invalid("assets: %d files; a Worker may have at most %d static assets", len(m.files), maxAssetFiles)
	}
	for i := range m.files {
		if _, dup := m.byHash[m.files[i].hash]; !dup {
			m.byHash[m.files[i].hash] = &m.files[i]
		}
	}
	return m, nil
}

// manifestCache keeps the manifests of recently loaded trees by tree digest, so that a resync
// of unchanged assets does not hash every file again.
type manifestCache struct {
	mu sync.Mutex
	m  map[string]*assetManifest
}

const manifestCacheSize = 32

func (c *manifestCache) get(tree *artifact.Tree) (*assetManifest, *problem) {
	c.mu.Lock()
	m, ok := c.m[tree.Digest]
	c.mu.Unlock()
	if ok {
		return m, nil
	}
	m, p := buildAssetManifest(tree.Files())
	if p != nil {
		return nil, p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= manifestCacheSize {
		c.m = map[string]*assetManifest{} // small and rarely full: dropping all is enough
	}
	c.m[tree.Digest] = m
	return m, nil
}

// ignoreMatcher is the subset of gitignore that wrangler's "ignore" package implements for
// .assetsignore: blank lines and # comments are skipped, ! negates, a pattern with a / other
// than a trailing one is anchored at the root (a leading / is dropped), a trailing / matches
// directories only, * and ? do not cross /, ** matches any number of directories, [...] is a
// class. A file is ignored when the last pattern matching it or one of its directories is not
// negated; a file in an ignored directory cannot be re-included.
type ignoreMatcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	base    bool // matches the last segment only (no / in the pattern)
}

func newIgnoreMatcher(patterns []string) *ignoreMatcher {
	m := &ignoreMatcher{}
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "\r")
		p = trimTrailingSpaces(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(p, "!") {
			r.negate, p = true, p[1:]
		} else if strings.HasPrefix(p, `\!`) || strings.HasPrefix(p, `\#`) {
			p = p[1:]
		}
		if strings.HasSuffix(p, "/") {
			r.dirOnly, p = true, strings.TrimRight(p, "/")
		}
		if p == "" {
			continue
		}
		r.base = !strings.Contains(p, "/")
		p = strings.TrimPrefix(p, "/")
		re, err := regexp.Compile("^" + globRegexp(p) + "$")
		if err != nil {
			continue // an unparsable pattern matches nothing
		}
		r.re = re
		m.rules = append(m.rules, r)
	}
	return m
}

// trimTrailingSpaces drops trailing spaces not escaped with a backslash.
func trimTrailingSpaces(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}

// globRegexp translates a gitignore glob into a regular expression over a slash path.
func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "/**") && i+3 == len(g):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case c == '\\' && i+1 < len(g):
			i++
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// ignored reports whether the file at p (relative, slash-separated) is ignored.
func (m *ignoreMatcher) ignored(p string) bool {
	segs := strings.Split(p, "/")
	for i := range segs {
		prefix := strings.Join(segs[:i+1], "/")
		isDir := i < len(segs)-1
		state := false
		for _, r := range m.rules {
			if r.dirOnly && !isDir {
				continue
			}
			subject := prefix
			if r.base {
				subject = segs[i]
			}
			if r.re.MatchString(subject) {
				state = !r.negate
			}
		}
		if state {
			return true // an ignored directory's files cannot be re-included
		}
	}
	return false
}

// uploadAssets runs the upload session for plan and returns the completion token and the
// number of buckets uploaded.
func uploadAssets(ctx context.Context, cf cfclient.Client, accountID, name string, plan *assetPlan) (string, int, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: scriptPath(accountID, name) + "/assets-upload-session",
		Body: map[string]any{"manifest": plan.manifestJSON()}})
	if err != nil {
		return "", 0, fmt.Errorf("start the assets upload session: %w", err)
	}
	var sess struct {
		JWT     string     `json:"jwt"`
		Buckets [][]string `json:"buckets"`
	}
	// wrangler guards against a null result too (assets.ts#L98-L107).
	if err := json.Unmarshal(resp.Result, &sess); err != nil || sess.JWT == "" {
		return "", 0, fmt.Errorf("the assets upload session answered no token (result %.200s)", resp.Result)
	}
	completion, n := "", 0
	for _, bucket := range sess.Buckets {
		if len(bucket) == 0 {
			continue
		}
		body, ct, err := assetBucketBody(plan, bucket)
		if err != nil {
			return "", n, err
		}
		resp, err := cf.Do(cfclient.WithBearerToken(ctx, sess.JWT), cfclient.Request{Method: http.MethodPost,
			Path: "/accounts/" + accountID + "/workers/assets/upload", Query: url.Values{"base64": {"true"}}, RawBody: body, ContentType: ct})
		if err != nil {
			if ae, ok := cfclient.AsAPIError(err); ok && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
				// The session token expired or was refused: a new session (the next attempt)
				// skips what was uploaded already, so this is not a permanent failure.
				return "", n, fmt.Errorf("upload assets bucket %d of %d: the session token was refused (%v); a new session is started", n+1, len(sess.Buckets), err)
			}
			return "", n, fmt.Errorf("upload assets bucket %d of %d: %w", n+1, len(sess.Buckets), err)
		}
		n++
		var up struct {
			JWT string `json:"jwt"`
		}
		if json.Unmarshal(resp.Result, &up) == nil && up.JWT != "" {
			completion = up.JWT // the last bucket's answer (assets.ts#L318-L342)
		}
	}
	if n == 0 {
		return sess.JWT, 0, nil // nothing to upload: the session's jwt is the completion token
	}
	if completion == "" {
		return "", n, fmt.Errorf("the assets upload finished without a completion token")
	}
	return completion, n, nil
}

var assetQuote = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// assetBucketBody is one bucket's multipart body (assets.ts#L218-L249).
func assetBucketBody(plan *assetPlan, bucket []string) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	sorted := append([]string(nil), bucket...)
	sort.Strings(sorted)
	for _, h := range sorted {
		f, ok := plan.byHash[h]
		if !ok {
			// wrangler's "A file was requested that does not appear to exist." (assets.ts#L153-L162)
			return nil, "", fmt.Errorf("the assets upload session asked for %q, which is not in the manifest", h)
		}
		hd := textproto.MIMEHeader{}
		hd.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, assetQuote.Replace(h), assetQuote.Replace(h)))
		hd.Set("Content-Type", f.contentType)
		pw, err := w.CreatePart(hd)
		if err != nil {
			return nil, "", err
		}
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		if _, err := enc.Write(f.content); err != nil {
			return nil, "", err
		}
		if err := enc.Close(); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
