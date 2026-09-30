package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"lukechampine.com/blake3"
)

// Workers static assets: the three-step upload flow of
// DOCS https://developers.cloudflare.com/workers/static-assets/direct-upload/ and of wrangler's
// syncAssets, SOURCED cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/assets.ts#L72-L363:
//
//  1. POST …/workers/scripts/{script_name}/assets-upload-session {"manifest": {path: {hash,
//     size}}} answers {jwt, buckets}: the hashes still to upload, grouped into requests.
//  2. POST …/workers/assets/upload?base64=true, authenticated with the session JWT, uploads one
//     bucket as multipart/form-data (part name = hash, content = base64, part Content-Type =
//     the Content-Type to serve). 202 {} until the last bucket; that one answers 201 {jwt} with
//     the completion token.
//  3. The script upload (PUT …/scripts/{name}) or version upload (POST …/versions) redeems the
//     completion token in metadata.assets.jwt (workers_versions.go parseAssets).
//
// No recording covers any of it: the shapes follow the pinned spec, the docs and what wrangler
// sends and reads (cited per behavior); everything else is UNVERIFIED.

// Asset limits. SOURCED (statement): MAX_ASSET_SIZE = 25 MiB and MAX_ASSET_COUNT = 100000,
// cloudflare/workers-sdk@3bdcd0d:packages/workers-shared/utils/constants.ts#L30-L32 (wrangler
// refuses larger files client-side; that the API refuses them too is UNVERIFIED).
const (
	assetMaxFileBytes = 25 << 20
	assetMaxFiles     = 100000
)

// Bucket sizing of the session answer. wrangler sends each bucket as one request and reads
// the whole bucket into memory, "limited to 3 concurrent uploads at 50 MiB per bucket", SOURCED
// (statement) assets.ts#L218; the API's actual grouping (a file-count limit per bucket in
// particular) is UNVERIFIED. SetAssetBuckets changes both for tests.
const (
	defaultAssetBucketBytes = 50 << 20
	defaultAssetBucketFiles = 1000
)

// assetHashRe: DOCS (direct-upload) "hash represents a 32 hexadecimal character hash of the
// file".
var assetHashRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

type assetEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// AssetFile is an uploaded asset, as stored: its decoded bytes and the Content-Type of its
// upload part ("application/null" means none: DOCS direct-upload).
type AssetFile struct {
	Content     []byte
	ContentType string
}

// assetStore holds what was uploaded for one script name (before and after the script exists:
// a first deploy uploads its assets before the script). UNVERIFIED: whether the API scopes
// "already uploaded" to the script, the account or neither; the docs say unchanged files are
// skipped "if they have recently been uploaded in previous versions of your Worker".
type assetStore struct {
	files map[string]AssetFile // by hash
}

type assetSession struct {
	ID, AccountID, Script string
	Manifest              map[string]assetEntry
	pending               map[string]bool // hashes not uploaded yet
	Created               time.Time
}

// workerAssets are a version's assets: the manifest of the completion token it redeemed and
// its metadata.assets.config.
type workerAssets struct {
	Manifest map[string]assetEntry
	Config   map[string]any
}

func (a *account) assetStoreFor(script string) *assetStore {
	if a.assets == nil {
		a.assets = map[string]*assetStore{}
	}
	st, ok := a.assets[script]
	if !ok {
		st = &assetStore{files: map[string]AssetFile{}}
		a.assets[script] = st
	}
	return st
}

// SetAssetBuckets sets how many files and bytes (the manifest sizes) one bucket of an upload
// session holds at most; zero restores the defaults.
func (s *Server) SetAssetBuckets(maxFiles int, maxBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assetBucketFiles, s.assetBucketBytes = maxFiles, maxBytes
}

// WorkerAssets returns the asset manifest (path → hash) and assets config of the deployed
// version of script, and whether it has assets.
func (s *Server) WorkerAssets(accountID, script string) (map[string]string, map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[accountID]
	if !ok {
		return nil, nil, false
	}
	w, ok := a.scripts[script]
	if !ok || w.deployedVersion().res.Assets == nil {
		return nil, nil, false
	}
	as := w.deployedVersion().res.Assets
	m := make(map[string]string, len(as.Manifest))
	for p, e := range as.Manifest {
		m[p] = e.Hash
	}
	return m, as.Config, true
}

// UploadedAsset returns an asset uploaded for script by its hash.
func (s *Server) UploadedAsset(accountID, script, hash string) (AssetFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[accountID]
	if !ok || a.assets[script] == nil {
		return AssetFile{}, false
	}
	f, ok := a.assets[script].files[hash]
	return f, ok
}

func (s *Server) registerWorkerAssets() {
	s.handle(http.MethodPost, "/accounts/{account_id}/workers/scripts/{script_name}/assets-upload-session", workerAssetsSession)
	s.handle(http.MethodPost, "/accounts/{account_id}/workers/assets/upload", workerAssetsUpload)
}

// Error codes of the assets flow. None is recorded or documented: UNVERIFIED. 10021 is the
// emulator's code for invalid Workers uploads (workers.go readParts); 10000 is Cloudflare's
// usual "Authentication error".
const (
	assetInvalidCode = 10021
	assetAuthCode    = 10000
)

func assetInvalid(msg string) response { return fail(http.StatusBadRequest, assetInvalidCode, msg) } // UNVERIFIED

// workerAssetsSession: POST …/scripts/{name}/assets-upload-session. The script need not exist:
// wrangler starts the session of a first deploy before the script upload, SOURCED (relies)
// cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/deploy.ts#L282-L293.
func workerAssetsSession(c *reqCtx) response {
	var req struct {
		Manifest *map[string]assetEntry `json:"manifest"`
	}
	if r := c.decodeJSON(&req); r != nil {
		return *r
	}
	if req.Manifest == nil {
		return assetInvalid("The request body must have a manifest.") // spec: manifest is required; the error UNVERIFIED
	}
	manifest := *req.Manifest
	if len(manifest) > assetMaxFiles {
		return assetInvalid(fmt.Sprintf("The manifest has %d files, more than the maximum of %d.", len(manifest), assetMaxFiles)) // UNVERIFIED
	}
	for p, e := range manifest {
		switch {
		case !strings.HasPrefix(p, "/"):
			// DOCS (direct-upload): keys are paths such as "/filea.html"; wrangler prefixes "/",
			// SOURCED packages/workers-shared/utils/helpers.ts#L13-L18. Refusing others: UNVERIFIED.
			return assetInvalid(fmt.Sprintf("Manifest path %q must start with /.", p))
		case !assetHashRe.MatchString(e.Hash):
			return assetInvalid(fmt.Sprintf("Manifest entry %q has an invalid hash %q.", p, e.Hash)) // UNVERIFIED
		case e.Size < 0 || e.Size > assetMaxFileBytes:
			return assetInvalid(fmt.Sprintf("Manifest entry %q has size %d; assets are at most %d bytes.", p, e.Size, assetMaxFileBytes)) // UNVERIFIED
		}
	}
	name := c.params["script_name"]
	st := c.account.assetStoreFor(name)
	sess := &assetSession{ID: hex.EncodeToString(randBytes(16)), AccountID: c.account.id, Script: name,
		Manifest: manifest, pending: map[string]bool{}, Created: c.now}
	sizes := map[string]int64{}
	for _, e := range manifest {
		if _, done := st.files[e.Hash]; !done {
			sess.pending[e.Hash] = true
			sizes[e.Hash] = e.Size
		}
	}
	if c.account.assetSessions == nil {
		c.account.assetSessions = map[string]*assetSession{}
	}
	// Drop sessions no token can name any more, so a long-running emulator does not keep every
	// deploy's manifest: the upload token expires assetTokenTTL after the session, and a
	// completion token (issued by an upload, so before that) at most assetTokenTTL later.
	// Emulator housekeeping, not API behavior.
	for id, old := range c.account.assetSessions {
		if !c.now.Before(old.Created.Add(2 * assetTokenTTL)) {
			delete(c.account.assetSessions, id)
		}
	}
	c.account.assetSessions[sess.ID] = sess
	if len(sess.pending) == 0 {
		// DOCS (direct-upload): "If all assets have been previously uploaded, buckets will be
		// empty, and jwt will contain a completion token." wrangler then redeems that jwt at
		// once, SOURCED (relies) assets.ts#L114-L134.
		return ok(map[string]any{"buckets": []any{}, "jwt": c.s.signAssetToken(assetTokenCompletion, sess, c.now)})
	}
	return ok(map[string]any{"buckets": c.s.assetBuckets(sess.pending, sizes), "jwt": c.s.signAssetToken(assetTokenUpload, sess, c.now)})
}

// assetBuckets groups the pending hashes (sorted, so answers are deterministic) into buckets of
// at most the configured files and bytes (a larger single file gets a bucket of its own).
func (s *Server) assetBuckets(pending map[string]bool, sizes map[string]int64) [][]string {
	maxFiles, maxBytes := s.assetBucketFiles, s.assetBucketBytes
	if maxFiles <= 0 {
		maxFiles = defaultAssetBucketFiles
	}
	if maxBytes <= 0 {
		maxBytes = defaultAssetBucketBytes
	}
	hashes := make([]string, 0, len(pending))
	for h := range pending {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	var (
		out   [][]string
		cur   []string
		bytes int64
	)
	for _, h := range hashes {
		if len(cur) > 0 && (len(cur) >= maxFiles || bytes+sizes[h] > maxBytes) {
			out, cur, bytes = append(out, cur), nil, 0
		}
		cur = append(cur, h)
		bytes += sizes[h]
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// assetAuthFailed answers an upload whose bearer token is not a valid session JWT. The status
// and code are UNVERIFIED (not recorded; the spec only declares 4XX).
func assetAuthFailed(why string) response {
	return fail(http.StatusUnauthorized, assetAuthCode, "Authentication error: the assets upload JWT is "+why+".")
}

// workerAssetsUpload: POST /accounts/{account_id}/workers/assets/upload?base64=true.
func workerAssetsUpload(c *reqCtx) response {
	// Bearer <session JWT>: the spec's assets_jwt security scheme, DOCS (direct-upload) "The
	// Authorization header must be provided as a bearer token, using the JWT (upload token)";
	// wrangler sends exactly that, SOURCED assets.ts#L239-L249.
	tok := strings.TrimPrefix(c.r.Header.Get("Authorization"), "Bearer ")
	claims, err := c.s.parseAssetToken(tok, assetTokenUpload, c.now)
	if err != nil {
		return assetAuthFailed(err.Error())
	}
	sess := c.account.assetSessions[claims.Session]
	if claims.AccountID != c.account.id || sess == nil {
		return assetAuthFailed("for another account") // UNVERIFIED
	}
	// The spec requires base64=true ("Must be `true`"); request validation reports its absence,
	// and the API's answer is UNVERIFIED.
	if c.query.Get("base64") != "true" {
		return assetInvalid("Uploads must be base64-encoded: set base64=true.")
	}
	mt, params, perr := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if perr != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return assetInvalid("Expected a multipart/form-data request body.") // UNVERIFIED
	}
	type upload struct {
		hash string
		file AssetFile
	}
	var got []upload
	mr := multipart.NewReader(bytes.NewReader(c.body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return assetInvalid("Malformed multipart body: " + err.Error()) // UNVERIFIED
		}
		text, err := io.ReadAll(p)
		if err != nil {
			return assetInvalid("Malformed multipart body: " + err.Error()) // UNVERIFIED
		}
		h := p.FormName()
		paths := sess.pathsOf(h)
		if len(paths) == 0 {
			return assetInvalid(fmt.Sprintf("Hash %q is not in the manifest of this upload session.", h)) // UNVERIFIED
		}
		content, err := base64.StdEncoding.DecodeString(string(text))
		if err != nil {
			return assetInvalid(fmt.Sprintf("The content of %s is not base64.", h)) // UNVERIFIED
		}
		// The hash is the one wrangler computes (assetHash), over the base64 text as sent. That
		// the API checks it, and the size, is UNVERIFIED; flarefake does, so a client with a
		// different hash fails here instead of serving mismatched files.
		if want := AssetHash(string(text), paths[0]); want != h {
			return assetInvalid(fmt.Sprintf("The content uploaded for %s (%s) hashes to %s.", h, paths[0], want))
		}
		if int64(len(content)) != sess.Manifest[paths[0]].Size {
			return assetInvalid(fmt.Sprintf("The content uploaded for %s is %d bytes; the manifest says %d.", h, len(content), sess.Manifest[paths[0]].Size))
		}
		ct := p.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream" // a part without one (multipart's default)
		}
		got = append(got, upload{hash: h, file: AssetFile{Content: content, ContentType: ct}})
	}
	if len(got) == 0 {
		return assetInvalid("The upload has no files.") // UNVERIFIED
	}
	st := c.account.assetStoreFor(sess.Script)
	for _, u := range got {
		st.files[u.hash] = u.file
		delete(sess.pending, u.hash)
	}
	if len(sess.pending) > 0 {
		// Spec: 202 with an empty result. wrangler keeps the jwt of whichever response has one,
		// SOURCED (relies) assets.ts#L318-L342.
		return response{status: http.StatusAccepted, result: map[string]any{}}
	}
	// DOCS (direct-upload): "Once every file in the manifest has been uploaded, a status code of
	// 201 will be returned, with the jwt field present."
	return response{status: http.StatusCreated, result: map[string]any{"jwt": c.s.signAssetToken(assetTokenCompletion, sess, c.now)}}
}

// parseAssets resolves the assets of a script or version upload (metadata.assets,
// metadata.keep_assets). The spec (workers_multipart-script): assets.jwt is the completion
// token, keep_assets "Retain assets which exist for a previously uploaded Worker version; used
// in lieu of providing a completion token. An explicit assets upload takes precedence over
// keep_assets." DOCS (direct-upload) shows both forms. The error answers are UNVERIFIED.
func parseAssets(c *reqCtx, prev *workerScript, md *workerMetadata) (*workerAssets, *response) {
	bad := func(msg string) (*workerAssets, *response) {
		r := assetInvalid(msg)
		return nil, &r
	}
	if md.Assets != nil && md.Assets.JWT != nil && *md.Assets.JWT != "" {
		claims, err := c.s.parseAssetToken(*md.Assets.JWT, assetTokenCompletion, c.now)
		if err != nil {
			return bad("The assets completion token (metadata.assets.jwt) is " + err.Error() + ".")
		}
		sess := c.account.assetSessions[claims.Session]
		// The token is bound to the account and the script its session was made for (UNVERIFIED).
		if sess == nil || claims.AccountID != c.account.id || claims.Script != c.params["script_name"] {
			return bad("The assets completion token (metadata.assets.jwt) was issued for another Worker.")
		}
		return &workerAssets{Manifest: sess.Manifest, Config: md.Assets.Config}, nil
	}
	if md.KeepAssets != nil && *md.KeepAssets {
		// The deployed version's assets; its config unless the upload sends one (UNVERIFIED). With
		// nothing to keep the version has no assets (UNVERIFIED).
		if prev == nil || prev.Assets == nil {
			return nil, nil
		}
		keep := *prev.Assets
		if md.Assets != nil && md.Assets.Config != nil {
			keep.Config = md.Assets.Config
		}
		return &keep, nil
	}
	if md.Assets != nil {
		return bad("metadata.assets needs a completion token (jwt), or set keep_assets.") // UNVERIFIED
	}
	return nil, nil
}

// validateAssetsBinding refuses a binding of type assets on a version without assets
// (UNVERIFIED: the API's answer is not recorded; wrangler only sends one with assets).
func validateAssetsBinding(bindings []map[string]any, hasAssets bool) *response {
	for _, b := range bindings {
		if b["type"] == "assets" && !hasAssets {
			r := assetInvalid(fmt.Sprintf("The assets binding %v needs static assets (metadata.assets or keep_assets).", b["name"]))
			return &r
		}
	}
	return nil
}

// pathsOf returns the manifest paths whose file has hash h, sorted.
func (s *assetSession) pathsOf(h string) []string {
	var out []string
	for p, e := range s.Manifest {
		if e.Hash == h {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// AssetHash is the hash of an asset whose content, base64-encoded, is b64, at path p: the first
// 16 bytes (32 hex digits) of BLAKE3 over the base64 text followed by the file extension
// without its dot. SOURCED: cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/hash.ts#L5-L13
// (blake3-wasm; the extension is node's path.extname, see nodeExtname).
func AssetHash(b64, p string) string {
	sum := blake3.Sum256([]byte(b64 + strings.TrimPrefix(nodeExtname(p), ".")))
	return hex.EncodeToString(sum[:16])
}

// nodeExtname is node's path.posix.extname: the last "." of the last segment and what
// follows, except for a name that starts with its only dot (".bashrc" → "") and "..".
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
