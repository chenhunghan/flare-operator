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
	// AccountID is the 32-character Cloudflare account identifier.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
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

// CloudflareAccountStatus reports the result of the last token verification.
type CloudflareAccountStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// TokenID is the verified token's identifier.
	// +optional
	TokenID string `json:"tokenID,omitempty"`
	// TokenStatus is the token status reported by Cloudflare: active, disabled or expired.
	// +optional
	TokenStatus string `json:"tokenStatus,omitempty"`
	// TokenType is "account" (account-owned token) or "user".
	// +optional
	TokenType string `json:"tokenType,omitempty"`
	// TokenExpiresOn is the token's expiry, when it has one.
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
// +kubebuilder:resource:scope=Namespaced,shortName=cfaccount;cfacct,categories=cloudflare
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.accountID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Token",type=string,JSONPath=`.status.tokenStatus`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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

func init() {
	SchemeBuilder.Register(&CloudflareAccount{}, &CloudflareAccountList{})
}
