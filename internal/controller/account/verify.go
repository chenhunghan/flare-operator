package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// TokenInfo is the verified token.
type TokenInfo struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	ExpiresOn *time.Time `json:"expires_on,omitempty"`
	NotBefore *time.Time `json:"not_before,omitempty"`
	Type      string     `json:"-"`
}

// Verify checks that token-bearing client cf can act on accountID.
//
//  1. GET /accounts/{id}/tokens/verify (account-owned tokens).
//  2. If that fails with a 4xx, the token may be a user token:
//     GET /user/tokens/verify, then GET /accounts/{id} to prove it can access the account.
//
// Permanent failures are *reconcile.AccountError with reason TokenInvalid or AccountMismatch;
// anything else (429, 5xx, transport) is returned as-is for a retry.
//
// UNVERIFIED against the live API: which status/code an invalid token or a token of another
// account gets on step 1 (flarefake: 401/1000 and 403/9109).
func Verify(ctx context.Context, cf cfclient.Client, accountID string) (*TokenInfo, error) {
	ctx = cfclient.WithoutCache(ctx)
	info, acctErr := verifyAt(ctx, cf, "/accounts/"+accountID+"/tokens/verify")
	if acctErr == nil {
		info.Type = cloudflarev1alpha1.TokenTypeAccount
		return info, nil
	}
	if !permanent(acctErr) {
		return nil, acctErr
	}
	info, userErr := verifyAt(ctx, cf, "/user/tokens/verify")
	if userErr != nil {
		if !permanent(userErr) {
			return nil, userErr
		}
		if ae, ok := cfclient.AsAPIError(acctErr); ok && (ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound) {
			return nil, &reconcile.AccountError{Reason: cloudflarev1alpha1.ReasonAccountMismatch,
				Message: fmt.Sprintf("token is not valid for account %s: %v", accountID, acctErr)}
		}
		return nil, &reconcile.AccountError{Reason: cloudflarev1alpha1.ReasonTokenInvalid,
			Message: fmt.Sprintf("token verification failed: %v", acctErr)}
	}
	info.Type = cloudflarev1alpha1.TokenTypeUser
	if info.Status != "active" {
		return info, nil // the caller reports disabled/expired
	}
	if _, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: "/accounts/" + accountID}); err != nil {
		if !permanent(err) {
			return nil, err
		}
		return nil, &reconcile.AccountError{Reason: cloudflarev1alpha1.ReasonAccountMismatch,
			Message: fmt.Sprintf("user token cannot access account %s: %v", accountID, err)}
	}
	return info, nil
}

func verifyAt(ctx context.Context, cf cfclient.Client, p string) (*TokenInfo, error) {
	resp, err := cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: p})
	if err != nil {
		return nil, err
	}
	var info TokenInfo
	if err := json.Unmarshal(resp.Result, &info); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &info, nil
}

// permanent reports a 4xx other than 429: retrying with the same token will not help.
func permanent(err error) bool {
	ae, ok := cfclient.AsAPIError(err)
	return ok && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
}
