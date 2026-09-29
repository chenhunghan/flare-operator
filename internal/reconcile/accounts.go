package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
)

// AccountError explains why a CloudflareAccount cannot be used. Reason is a condition reason
// (cloudflarev1alpha1.Reason* for the account itself, commonv1alpha1.ReasonAccountNotReady
// for managed objects that reference it).
type AccountError struct {
	Reason  string
	Message string
}

func (e *AccountError) Error() string { return e.Message }

// IsAccountNotReady reports whether err is an *AccountError.
func IsAccountNotReady(err error) bool {
	var ae *AccountError
	return errors.As(err, &ae)
}

// ClientFactory builds a Cloudflare client; cfclient.New by default.
type ClientFactory func(cfclient.Options) (cfclient.Client, error)

// Accounts caches one cfclient.Client per CloudflareAccount and resolves accountRefs. It is
// shared by the CloudflareAccount controller and every managed-resource controller.
type Accounts struct {
	kube       client.Reader
	writer     client.Writer // labels managed objects; nil → labels are set in memory only
	newClient  ClientFactory
	userAgent  string
	httpClient *http.Client
	baseURLs   BaseURLPolicy

	mu      sync.Mutex
	clients map[types.NamespacedName]cachedClient
}

type cachedClient struct {
	key    string
	client cfclient.Client
}

// AccountsOption configures Accounts.
type AccountsOption func(*Accounts)

// WithClientFactory replaces cfclient.New (tests).
func WithClientFactory(f ClientFactory) AccountsOption { return func(a *Accounts) { a.newClient = f } }

// WithUserAgent sets the User-Agent of built clients.
func WithUserAgent(ua string) AccountsOption { return func(a *Accounts) { a.userAgent = ua } }

// WithHTTPClient sets the HTTP client of built clients.
func WithHTTPClient(h *http.Client) AccountsOption { return func(a *Accounts) { a.httpClient = h } }

// WithWriter sets the client that writes the account label on managed objects (default: kube,
// when it is also a client.Writer).
func WithWriter(w client.Writer) AccountsOption { return func(a *Accounts) { a.writer = w } }

// BaseURLPolicy says which CloudflareAccount spec.baseURL overrides are honoured. The zero
// value allows none: an account with a baseURL other than cfclient.DefaultBaseURL is not
// Ready (reason BaseURLNotAllowed). Overrides let whoever can create a CloudflareAccount make
// the operator send that account's token to an arbitrary endpoint, so they are opt-in.
type BaseURLPolicy struct {
	// AllowAny allows every http(s) base URL (--allow-base-url-override; tests, flarefake).
	AllowAny bool
	// Allowed lists base URLs allowed verbatim (compared without trailing slashes).
	Allowed []string
}

// Allows reports whether base (a spec.baseURL value) may be used.
func (p BaseURLPolicy) Allows(base string) bool {
	b := strings.TrimRight(base, "/")
	if b == "" || b == cfclient.DefaultBaseURL || p.AllowAny {
		return true
	}
	for _, a := range p.Allowed {
		if strings.TrimRight(strings.TrimSpace(a), "/") == b {
			return true
		}
	}
	return false
}

// WithBaseURLPolicy sets which spec.baseURL overrides are allowed (default: none).
func WithBaseURLPolicy(p BaseURLPolicy) AccountsOption { return func(a *Accounts) { a.baseURLs = p } }

// NewAccounts returns an Accounts reading CloudflareAccounts and Secrets through kube
// (normally the manager's cached client). When kube is also a client.Writer it labels managed
// objects in Resolve (see AccountLabel).
func NewAccounts(kube client.Reader, opts ...AccountsOption) *Accounts {
	a := &Accounts{kube: kube, newClient: cfclient.New, clients: map[types.NamespacedName]cachedClient{}}
	if w, ok := kube.(client.Writer); ok {
		a.writer = w
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Token reads the API token from the account's Secret. Missing Secrets or keys yield an
// *AccountError with reason SecretNotFound / SecretKeyMissing.
func (a *Accounts) Token(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (string, error) {
	ref := acct.Spec.TokenSecretRef
	key := ref.Key
	if key == "" {
		key = "token"
	}
	var s corev1.Secret
	if err := a.kube.Get(ctx, types.NamespacedName{Namespace: acct.Namespace, Name: ref.Name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &AccountError{Reason: cloudflarev1alpha1.ReasonSecretNotFound,
				Message: fmt.Sprintf("token Secret %s/%s not found", acct.Namespace, ref.Name)}
		}
		return "", err
	}
	tok := strings.TrimSpace(string(s.Data[key]))
	if tok == "" {
		return "", &AccountError{Reason: cloudflarev1alpha1.ReasonSecretKeyMissing,
			Message: fmt.Sprintf("token Secret %s/%s has no (or an empty) key %q", acct.Namespace, ref.Name, key)}
	}
	return tok, nil
}

// Options derives cfclient.Options from the account spec and token.
func (a *Accounts) Options(acct *cloudflarev1alpha1.CloudflareAccount, token string) cfclient.Options {
	o := cfclient.Options{Token: token, BaseURL: acct.Spec.BaseURL, UserAgent: a.userAgent, HTTPClient: a.httpClient}
	if rl := acct.Spec.RateLimit; rl != nil {
		if rl.RequestsPerFiveMinutes > 0 {
			o.RPS = float64(rl.RequestsPerFiveMinutes) / 300
		}
		o.Burst = int(rl.Burst)
		if rl.MaxRetries != nil {
			o.MaxRetries = int(*rl.MaxRetries)
			if o.MaxRetries == 0 {
				o.MaxRetries = -1 // explicit 0 = no retries (cfclient treats 0 as "default")
			}
		}
		if rl.ListCacheTTL != nil {
			o.ListTTL = rl.ListCacheTTL.Duration
		}
	}
	return o
}

func optionsKey(o cfclient.Options) string {
	sum := sha256.Sum256([]byte(o.Token))
	return fmt.Sprintf("%s|%s|%g|%d|%d|%s", hex.EncodeToString(sum[:8]), o.BaseURL, o.RPS, o.Burst, o.MaxRetries, o.ListTTL)
}

// ClientFor returns the cached client for acct, building a new one whenever the token or a
// client-relevant spec field changed. It does not check the account's Ready condition.
func (a *Accounts) ClientFor(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (cfclient.Client, error) {
	if !a.baseURLs.Allows(acct.Spec.BaseURL) {
		return nil, &AccountError{Reason: cloudflarev1alpha1.ReasonBaseURLNotAllowed,
			Message: "spec.baseURL overrides are disabled in this operator (start it with --allow-base-url-override or --allowed-base-url)"}
	}
	nn := types.NamespacedName{Namespace: acct.Namespace, Name: acct.Name}
	tok, err := a.Token(ctx, acct)
	if err != nil {
		// A namespace deletion removes the token Secret at once while the account waits for its
		// managed objects (finalizer); keep serving them the cached client so they can clean up.
		if IsAccountNotReady(err) && !acct.DeletionTimestamp.IsZero() {
			a.mu.Lock()
			cc, ok := a.clients[nn]
			a.mu.Unlock()
			if ok {
				return cc.client, nil
			}
		}
		return nil, err
	}
	o := a.Options(acct, tok)
	key := optionsKey(o)
	a.mu.Lock()
	defer a.mu.Unlock()
	if cc, ok := a.clients[nn]; ok && cc.key == key {
		return cc.client, nil
	}
	c, err := a.newClient(o)
	if err != nil {
		return nil, &AccountError{Reason: cloudflarev1alpha1.ReasonClientError, Message: err.Error()}
	}
	a.clients[nn] = cachedClient{key: key, client: c}
	return c, nil
}

// Forget drops the cached client of a deleted account.
func (a *Accounts) Forget(nn types.NamespacedName) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.clients, nn)
}

// Resolved is a usable account.
type Resolved struct {
	Client    cfclient.Client
	AccountID string
	Account   *cloudflarev1alpha1.CloudflareAccount
}

// AccountReady reports whether acct's Ready condition is True for its current generation.
func AccountReady(acct *cloudflarev1alpha1.CloudflareAccount) bool {
	c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == acct.Generation
}

// AccountLabel is the label Resolve puts on every managed object: the name of the
// CloudflareAccount it uses (AccountLabelValue). The CloudflareAccount controller keeps an
// account (finalizer) while objects in its namespace carry it.
const AccountLabel = cloudflarev1alpha1.AccountLabel

// AccountLabelValue is the AccountLabel value for an account name: the name itself, or, when
// the name is not a valid label value (longer than 63 characters), "h-" plus 40 hex digits of
// its SHA-256.
func AccountLabelValue(name string) string {
	if len(validation.IsValidLabelValue(name)) == 0 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "h-" + hex.EncodeToString(sum[:20])
}

// ensureAccountLabel sets AccountLabel on mg (a metadata-only merge patch) unless it already has
// the right value. mg is updated in place (labels and resourceVersion only). An object that no
// longer exists needs no protection, so NotFound is ignored.
func (a *Accounts) ensureAccountLabel(ctx context.Context, mg commonv1alpha1.Managed, name string) error {
	want := AccountLabelValue(name)
	if mg.GetLabels()[AccountLabel] == want {
		return nil
	}
	setLabel := func(o metav1.Object) {
		l := o.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l[AccountLabel] = want
		o.SetLabels(l)
	}
	obj, isObj := mg.(client.Object)
	if a.writer == nil || !isObj {
		setLabel(mg)
		return nil
	}
	cp, _ := obj.DeepCopyObject().(client.Object)
	base, _ := cp.DeepCopyObject().(client.Object)
	setLabel(cp)
	if err := a.writer.Patch(ctx, cp, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			setLabel(mg)
			return nil
		}
		return fmt.Errorf("label %s/%s with %s: %w", mg.GetNamespace(), mg.GetName(), AccountLabel, err)
	}
	mg.SetLabels(cp.GetLabels())
	mg.SetResourceVersion(cp.GetResourceVersion())
	return nil
}

// Resolve maps mg's spec.accountRef to a Ready CloudflareAccount in mg's namespace. When the
// account is missing or not Ready it returns an *AccountError with reason AccountNotReady
// (use MarkAccountNotReady and requeue); API-server errors are returned unchanged.
//
// Before looking the account up it labels mg with AccountLabel=<accountRef.name> (see
// AccountLabel); every managed kind must resolve its account through Resolve so that the
// account cannot be deleted while mg still needs it.
func (a *Accounts) Resolve(ctx context.Context, mg commonv1alpha1.Managed) (*Resolved, error) {
	name := mg.GetResourceSpec().AccountRef.Name
	notReady := func(format string, args ...any) error {
		return &AccountError{Reason: commonv1alpha1.ReasonAccountNotReady, Message: fmt.Sprintf(format, args...)}
	}
	if name == "" {
		return nil, notReady("spec.accountRef.name is empty")
	}
	if err := a.ensureAccountLabel(ctx, mg, name); err != nil {
		return nil, err
	}
	var acct cloudflarev1alpha1.CloudflareAccount
	if err := a.kube.Get(ctx, types.NamespacedName{Namespace: mg.GetNamespace(), Name: name}, &acct); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notReady("CloudflareAccount %s/%s not found", mg.GetNamespace(), name)
		}
		return nil, err
	}
	if !AccountReady(&acct) {
		msg := "not verified yet"
		if c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady); c != nil && c.Status != metav1.ConditionTrue {
			msg = c.Reason
			if c.Message != "" {
				msg += ": " + c.Message
			}
		}
		return nil, notReady("CloudflareAccount %s/%s is not Ready (%s)", acct.Namespace, acct.Name, msg)
	}
	c, err := a.ClientFor(ctx, &acct)
	if err != nil {
		var ae *AccountError
		if errors.As(err, &ae) {
			return nil, notReady("CloudflareAccount %s/%s: %s", acct.Namespace, acct.Name, ae.Message)
		}
		return nil, err
	}
	return &Resolved{Client: c, AccountID: acct.Spec.AccountID, Account: &acct}, nil
}

// AccountRetryInterval is how long callers should wait before retrying after AccountNotReady
// (they are not woken by account changes unless they watch CloudflareAccounts).
const AccountRetryInterval = 15 * time.Second
