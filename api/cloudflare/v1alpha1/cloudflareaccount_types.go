package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeySelector selects a key of a Secret in the same namespace.
type SecretKeySelector struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key within the Secret's data. Defaults to "token".
	// +optional
	// +kubebuilder:default=token
	Key string `json:"key,omitempty"`
}

// RateLimitSpec tunes the client-side limiter shared by every CloudflareAccount that uses the
// same token. Cloudflare's global limit is 1200 requests per 300 s per token; the operator
// ignores the API's Ratelimit response header because it does not track usage.
type RateLimitSpec struct {
	// RequestsPerFiveMinutes is the sustained budget (default 1080, 90% of Cloudflare's 1200).
	// +optional
	// +kubebuilder:validation:Minimum=1
	RequestsPerFiveMinutes int32 `json:"requestsPerFiveMinutes,omitempty"`
	// Burst is the number of requests that may be sent at once (default 20).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst int32 `json:"burst,omitempty"`
	// MaxRetries bounds retries of 5xx and transport errors for idempotent calls (default 4).
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRetries *int32 `json:"maxRetries,omitempty"`
	// ListCacheTTL caches collection GETs for this long (default: disabled). Writes under the
	// same path invalidate cached entries.
	// +optional
	ListCacheTTL *metav1.Duration `json:"listCacheTTL,omitempty"`
}

// CloudflareAccountSpec binds a Cloudflare account ID to an API token.
type CloudflareAccountSpec struct {
	// AccountID is the 32-character Cloudflare account identifier. It is immutable: the objects
	// that reference this CloudflareAccount manage resources in that account, and pointing it at
	// another account would make them create new resources there and leave the old ones
	// unmanaged. Create another CloudflareAccount for another account.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountID is immutable: create another CloudflareAccount for another account"
	AccountID string `json:"accountID"`
	// TokenSecretRef names the Secret (same namespace) holding the API token.
	TokenSecretRef SecretKeySelector `json:"tokenSecretRef"`
	// BaseURL overrides the API base URL, e.g. http://flarefake:8787/client/v4 in tests. The
	// operator ignores overrides unless it runs with --allow-base-url-override or lists this URL
	// in --allowed-base-url; otherwise the account reports Ready=False, reason BaseURLNotAllowed.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	BaseURL string `json:"baseURL,omitempty"`
	// RateLimit tunes the client-side rate limiter.
	// +optional
	RateLimit *RateLimitSpec `json:"rateLimit,omitempty"`
}

// Token types reported in status.tokenType.
const (
	TokenTypeAccount = "account" // account-owned token, verified via /accounts/{id}/tokens/verify
	TokenTypeUser    = "user"    // user token, verified via /user/tokens/verify + GET /accounts/{id}
)

// Reasons for the Ready condition of a CloudflareAccount (in addition to api/common reasons).
const (
	ReasonSecretNotFound   = "SecretNotFound"
	ReasonSecretKeyMissing = "SecretKeyMissing"
	ReasonTokenInvalid     = "TokenInvalid"
	ReasonTokenDisabled    = "TokenDisabled"
	ReasonTokenExpired     = "TokenExpired"
	ReasonTokenNotYetValid = "TokenNotYetValid"
	ReasonAccountMismatch  = "AccountMismatch"
	ReasonClientError      = "ClientError"
	// ReasonBaseURLNotAllowed: spec.baseURL is set but the operator does not allow that override.
	ReasonBaseURLNotAllowed = "BaseURLNotAllowed"
)

// Labels and finalizers of the account usage protection.
const (
	// AccountLabel is set on every managed object to the name of the CloudflareAccount it uses
	// (see AccountLabelValue in internal/reconcile for names longer than a label value allows).
	AccountLabel = "cloudflare.flare.dev/account"
	// AccountInUseFinalizer keeps a CloudflareAccount until no managed object in its namespace
	// carries AccountLabel for it.
	AccountInUseFinalizer = "cloudflare.flare.dev/account-in-use"
	// AccountTokenFinalizer keeps a token Secret while a CloudflareAccount references it and
	// has not been released (it is not being deleted, or still has AccountInUseFinalizer), so
	// a namespace deletion cannot remove the token before the account's users have cleaned up
	// in Cloudflare.
	AccountTokenFinalizer = "cloudflare.flare.dev/account-token"
)

// CloudflareAccountObservation is the API token as last returned by
// GET /accounts/{account_id}/tokens/verify (account-owned tokens) or GET /user/tokens/verify
// (user tokens).
type CloudflareAccountObservation struct {
	// ID of the token.
	// +optional
	ID string `json:"id,omitempty"`
	// Status of the token: active, disabled or expired.
	// +optional
	Status string `json:"status,omitempty"`
	// ExpiresOn is the token's expiry, when it has one.
	// +optional
	ExpiresOn *metav1.Time `json:"expires_on,omitempty"`
	// NotBefore is the time before which the token is not valid, when it has one.
	// +optional
	NotBefore *metav1.Time `json:"not_before,omitempty"`
}

// CloudflareAccountStatus reports the result of the last token verification.
type CloudflareAccountStatus struct {
	// ID is the Cloudflare account ID the token was last verified against (spec.accountID while
	// the account is Ready; empty when verification failed).
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AtProvider is the token as last returned by the verify endpoint.
	// +optional
	AtProvider CloudflareAccountObservation `json:"atProvider,omitempty"`
	// TokenID is the verified token's identifier.
	//
	// Deprecated: use atProvider.id. Removed in v1beta1 (docs/api-versioning.md).
	// +optional
	TokenID string `json:"tokenID,omitempty"`
	// TokenStatus is the token status reported by Cloudflare: active, disabled or expired.
	//
	// Deprecated: use atProvider.status. Removed in v1beta1 (docs/api-versioning.md).
	// +optional
	TokenStatus string `json:"tokenStatus,omitempty"`
	// TokenType is "account" (account-owned token) or "user".
	// +optional
	TokenType string `json:"tokenType,omitempty"`
	// TokenExpiresOn is the token's expiry, when it has one.
	//
	// Deprecated: use atProvider.expires_on. Removed in v1beta1 (docs/api-versioning.md).
	// +optional
	TokenExpiresOn *metav1.Time `json:"tokenExpiresOn,omitempty"`
	// LastVerifiedTime is when the token was last verified successfully.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CloudflareAccount holds the credentials for one Cloudflare account. Managed resources in the
// same namespace refer to it through spec.accountRef.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// CloudflareAccount is in the cloudflare category (kubectl get cloudflare) but not in managed:
// like a Crossplane ProviderConfig it holds credentials and manages no Cloudflare resource.
//
// +kubebuilder:resource:scope=Namespaced,shortName=cfaccount;cfacct,categories=cloudflare
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-ID",type="string",JSONPath=".status.id",description="The verified Cloudflare account ID"
// +kubebuilder:printcolumn:name="TOKEN",type="string",JSONPath=".status.atProvider.status"
// +kubebuilder:printcolumn:name="REASON",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type CloudflareAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareAccountSpec   `json:"spec"`
	Status CloudflareAccountStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CloudflareAccountList contains a list of CloudflareAccount.
type CloudflareAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareAccount `json:"items"`
}
