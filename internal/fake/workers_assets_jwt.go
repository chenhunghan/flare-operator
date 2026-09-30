package fake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// JWTs of the Workers assets upload flow (workers_assets.go). The API issues two tokens: the
// upload session's JWT, which authenticates POST …/workers/assets/upload as a bearer token,
// and a "completion" JWT, which the script or version upload redeems in metadata.assets.jwt.
// DOCS https://developers.cloudflare.com/workers/static-assets/direct-upload/: both are valid
// for one hour. Their format beyond "a JWT" is not documented: the claims below are flarefake's
// own (UNVERIFIED), signed with HS256 under a per-server key, so a token is accepted only by the
// emulator that issued it.
//
// What clients read from them: wrangler decodes the payload (the second dot-separated segment,
// base64) and reads exp (to explain an expired upload) and the optional claims
// wrangler_single_asset_uploads and edge_kv_upload_concurrency (single-file upload mode, not
// emulated: flarefake never sets them, so wrangler uses the bulk multipart upload). SOURCED
// (relies): cloudflare/workers-sdk@3bdcd0d:packages/deploy-helpers/src/deploy/helpers/jwt.ts#L1-L26,
// packages/deploy-helpers/src/deploy/helpers/assets.ts#L365-L380.

// assetTokenTTL is how long both tokens are valid. DOCS (direct-upload, above): "The JWT is
// valid for one hour", "This completion token is valid for 1 hour".
const assetTokenTTL = time.Hour

// Kinds of asset tokens (the typ claim).
const (
	assetTokenUpload     = "upload"
	assetTokenCompletion = "completion"
)

// assetClaims are the claims of an asset token.
type assetClaims struct {
	Issuer    string `json:"iss"`
	Session   string `json:"sub"`
	AccountID string `json:"account_id"`
	Script    string `json:"script_name"`
	Kind      string `json:"typ"`
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

var jwtB64 = base64.RawURLEncoding

// signAssetToken issues a token of kind for session. Callers hold s.mu.
func (s *Server) signAssetToken(kind string, sess *assetSession, now time.Time) string {
	head := jwtB64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(assetClaims{Issuer: "flarefake", Session: sess.ID, AccountID: sess.AccountID, Script: sess.Script,
		Kind: kind, IssuedAt: now.Unix(), Expires: now.Add(assetTokenTTL).Unix()})
	msg := head + "." + jwtB64.EncodeToString(body)
	mac := hmac.New(sha256.New, s.assetKey)
	mac.Write([]byte(msg))
	return msg + "." + jwtB64.EncodeToString(mac.Sum(nil))
}

// Reasons an asset token is refused.
var (
	errAssetTokenMalformed = errors.New("malformed")
	errAssetTokenSignature = errors.New("bad signature")
	errAssetTokenExpired   = errors.New("expired")
	errAssetTokenKind      = errors.New("wrong kind of token")
)

// parseAssetToken verifies tok (signature, expiry at now, kind) and returns its claims.
func (s *Server) parseAssetToken(tok, kind string, now time.Time) (*assetClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errAssetTokenMalformed
	}
	mac := hmac.New(sha256.New, s.assetKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := jwtB64.DecodeString(parts[2])
	if err != nil {
		return nil, errAssetTokenMalformed
	}
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errAssetTokenSignature
	}
	raw, err := jwtB64.DecodeString(parts[1])
	if err != nil {
		return nil, errAssetTokenMalformed
	}
	var c assetClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errAssetTokenMalformed
	}
	if now.Unix() >= c.Expires { // wrangler's isJwtExpired: exp <= now (jwt.ts#L20)
		return nil, errAssetTokenExpired
	}
	if c.Kind != kind {
		return nil, errAssetTokenKind
	}
	return &c, nil
}
