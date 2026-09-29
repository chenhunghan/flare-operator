package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// API token verification and account lookup:
//
//	GET /accounts/{account_id}/tokens/verify   (account-owned tokens)
//	GET /user/tokens/verify                    (user tokens)
//	GET /accounts/{account_id}
//
// UNVERIFIED: no recording of these endpoints exists yet. Shapes follow the pinned spec
// (operationIds account-api-tokens-verify-token, user-api-tokens-verify-token); status codes
// and error codes for invalid tokens are Cloudflare's commonly documented ones (1000 "Invalid
// API Token", 9109 "Unauthorized to access requested resource").
//
// Two modes:
//   - open (no token registered, the default): every token verifies as an active token that can
//     access every account, so existing tests that send arbitrary tokens keep working.
//   - strict (at least one token registered with AddToken or POST /_fake/tokens): only
//     registered tokens verify, and account-owned tokens only for their own account.
//
// Token checks apply to these three endpoints only; other routes accept any token.

// Token is a registered API token.
type Token struct {
	Value string `json:"token"`
	// ID is the token's identifier (result.id); derived from Value when empty.
	ID string `json:"id,omitempty"`
	// Kind is "account" (account-owned, the default) or "user".
	Kind string `json:"kind,omitempty"`
	// AccountID: for an account-owned token, the owning account (required); for a user token,
	// the only account it may access ("" = every account).
	AccountID string `json:"account_id,omitempty"`
	// Status is "active" (default), "disabled" or "expired". A token whose ExpiresOn has passed
	// (by the emulator clock) reports "expired".
	Status    string     `json:"status,omitempty"`
	ExpiresOn *time.Time `json:"expires_on,omitempty"`
	NotBefore *time.Time `json:"not_before,omitempty"`
}

// AddToken registers a token and switches token checks to strict mode.
func (s *Server) AddToken(t Token) {
	if t.ID == "" {
		t.ID = tokenID(t.Value)
	}
	if t.Kind == "" {
		t.Kind = "account"
	}
	if t.Status == "" {
		t.Status = "active"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]*Token{}
	}
	s.tokens[t.Value] = &t
}

// ClearTokens removes all registered tokens (back to open mode).
func (s *Server) ClearTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = nil
}

func tokenID(value string) string {
	sum := sha256.Sum256([]byte("flarefake-token:" + value))
	return hex.EncodeToString(sum[:16])
}

func (s *Server) registerTokens() {
	s.handle(http.MethodGet, "/accounts/{account_id}/tokens/verify", tokenVerifyAccount)
	s.handle(http.MethodGet, "/user/tokens/verify", tokenVerifyUser)
	s.handle(http.MethodGet, "/accounts/{account_id}", accountGet)
}

func (c *reqCtx) bearer() string {
	return strings.TrimPrefix(c.r.Header.Get("Authorization"), "Bearer ")
}

// lookupToken returns the registered token, a synthetic open-mode token, or nil (strict mode,
// unknown token). Callers hold s.mu.
func (c *reqCtx) lookupToken() *Token {
	v := c.bearer()
	if c.s.tokens == nil {
		return &Token{Value: v, ID: tokenID(v), Kind: "any", Status: "active"}
	}
	return c.s.tokens[v]
}

func invalidToken() response {
	return fail(http.StatusUnauthorized, 1000, "Invalid API Token") // UNVERIFIED
}

func unauthorizedForResource() response {
	return fail(http.StatusForbidden, 9109, "Unauthorized to access requested resource") // UNVERIFIED
}

func (t *Token) verifyResult(now time.Time) map[string]any {
	status := t.Status
	if status == "active" && t.ExpiresOn != nil && !now.Before(*t.ExpiresOn) {
		status = "expired"
	}
	m := map[string]any{"id": t.ID, "status": status}
	if t.ExpiresOn != nil {
		m["expires_on"] = t.ExpiresOn.UTC().Format(time.RFC3339)
	}
	if t.NotBefore != nil {
		m["not_before"] = t.NotBefore.UTC().Format(time.RFC3339)
	}
	return m
}

func tokenVerifyAccount(c *reqCtx) response {
	t := c.lookupToken()
	switch {
	case t == nil || t.Kind == "user":
		return invalidToken()
	case t.Kind == "account" && t.AccountID != c.params["account_id"]:
		return unauthorizedForResource()
	}
	return ok(t.verifyResult(c.now))
}

func tokenVerifyUser(c *reqCtx) response {
	t := c.lookupToken()
	if t == nil || t.Kind == "account" {
		return invalidToken()
	}
	return ok(t.verifyResult(c.now))
}

func accountGet(c *reqCtx) response {
	id := c.params["account_id"]
	t := c.lookupToken()
	switch {
	case t == nil:
		return invalidToken()
	case t.Kind != "any" && t.AccountID != "" && t.AccountID != id:
		return unauthorizedForResource()
	}
	// Shape per spec (accounts_account); values are placeholders. UNVERIFIED.
	return ok(map[string]any{
		"id":         id,
		"name":       "flarefake account",
		"type":       "standard",
		"created_on": "2026-01-01T00:00:00Z",
		"settings": map[string]any{
			"enforce_twofactor":   false,
			"abuse_contact_email": nil,
		},
	})
}
