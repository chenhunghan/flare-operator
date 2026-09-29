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
		// The account controller keeps the token Secret of an unreleased account with a finalizer
		// (AccountTokenFinalizer), so a namespace deletion does not remove it early. Should the
		// Secret still go away (it was not protected yet, or its finalizer was removed by hand)
		// while a deleting account waits for its managed objects, keep serving them the cached
		// client so they can clean up. The cache is in-process only: after an operator restart
		// such an account cannot serve its users, and its deletion stays blocked until the Secret
		// is restored or the users are removed by hand (see DeletingAccountHint).
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

// DeletingAccountHint explains, for an account that is being deleted and is not usable (reason:
// its Ready=False reason, e.g. an AccountError reason), that the managed objects still using it
// cannot clean up in Cloudflare, so its deletion stays blocked, and how to break that deadlock.
// It returns "" when acct is not being deleted or reason is empty or ReasonAvailable. (An
// account nothing uses is released at once whatever its Ready condition says.)
func DeletingAccountHint(acct *cloudflarev1alpha1.CloudflareAccount, reason string) string {
	if acct.DeletionTimestamp.IsZero() || reason == "" || reason == commonv1alpha1.ReasonAvailable {
		return ""
	}
	secret := acct.Spec.TokenSecretRef.Name
	var fix string
	switch reason {
	case cloudflarev1alpha1.ReasonSecretNotFound, cloudflarev1alpha1.ReasonSecretKeyMissing:
		fix = fmt.Sprintf("restore the token Secret %q", secret)
	case cloudflarev1alpha1.ReasonTokenInvalid, cloudflarev1alpha1.ReasonTokenDisabled, cloudflarev1alpha1.ReasonTokenExpired,
		cloudflarev1alpha1.ReasonTokenNotYetValid, cloudflarev1alpha1.ReasonAccountMismatch:
		fix = fmt.Sprintf("put a valid, active token for account %s into Secret %q", acct.Spec.AccountID, secret)
	case cloudflarev1alpha1.ReasonBaseURLNotAllowed:
		fix = "allow its spec.baseURL (operator flag --allow-base-url-override or --allowed-base-url)"
	default:
		fix = "fix the cause above"
	}
	return fmt.Sprintf("; the account is being deleted but is not usable (%s), so the managed objects that still use it "+
		"cannot clean up in Cloudflare and the deletion stays blocked: %s, or delete those objects and remove their "+
		"finalizers by hand (their Cloudflare resources are then orphaned)", reason, fix)
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
//
// Setting deletionTimestamp makes the API server bump metadata.generation once (graceful
// deletion of an object with finalizers). The deletion changes nothing the verification rests
// on, so for a deleting account a Ready condition observed at the generation just before that
// bump still counts. Without this, every user that reads the account between its deletion and
// the account controller's next status write (a cache easily lags that long) got "not Ready
// (not verified yet)" and backed off, although a deleting account stays Ready for its users.
//
// The allowance holds only for a Ready condition verified before the deletion: its
// status.lastVerifiedTime (the account controller sets it with every Ready=True it writes) is
// not after metadata.deletionTimestamp. A spec change made before the deletion bumps the
// generation twice (the condition is two behind). One made during the deletion bumps it once
// more after the deletion bump: a condition verified before the deletion is then two behind,
// and one verified during the deletion (at the deletion generation, one behind) is refused by
// the time check, because the account controller stamps a verification of a deleting account
// strictly after its deletionTimestamp (whatever its own clock says; see VerifiedAt). Either
// way a fresh verification is required. (An operator clock running ahead of the API server's
// can only make a verification from just before the deletion look later, so the user retries
// after AccountRetryInterval; it never lets a stale condition through.)
func AccountReady(acct *cloudflarev1alpha1.CloudflareAccount) bool {
	c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false
	}
	if c.ObservedGeneration == acct.Generation {
		return true
	}
	del, verified := acct.DeletionTimestamp, acct.Status.LastVerifiedTime
	return !del.IsZero() && c.ObservedGeneration > 0 && c.ObservedGeneration == acct.Generation-1 &&
		verified != nil && !verified.Time.After(del.Time)
}

// VerifiedAt is the status.lastVerifiedTime to record for a successful verification of acct at
// now: now, except that for a deleting account it is strictly after deletionTimestamp at the
// second precision timestamps are stored with (deletionTimestamp plus one second when now is
// not later), so AccountReady can tell a verification made during the deletion from one made
// before it even when the clocks of the operator and the API server disagree.
func VerifiedAt(acct *cloudflarev1alpha1.CloudflareAccount, now time.Time) metav1.Time {
	if del := acct.DeletionTimestamp; !del.IsZero() && !now.Truncate(time.Second).After(del.Time) {
		return metav1.NewTime(del.Truncate(time.Second).Add(time.Second))
	}
	return metav1.NewTime(now)
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
	// Optimistic lock: mg may be a stale cache copy. Taking the fresh resourceVersion of a blind
	// patch onto it would defeat the optimistic lock of every later patchMeta in this reconcile
	// (PersistExternalID, finalizers), e.g. overwrite an external-ID annotation persisted by an
	// earlier reconcile and orphan that Cloudflare resource. A Conflict is returned for requeue.
	if err := a.writer.Patch(ctx, cp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			setLabel(mg)
			return nil
		}
		return fmt.Errorf("label %s/%s with %s: %w", mg.GetNamespace(), mg.GetName(), AccountLabel, err)
	}
	// mg was current (the lock held), so the server's metadata is mg's plus the label.
	mg.SetResourceVersion(cp.GetResourceVersion())
	mg.SetLabels(cp.GetLabels())
	mg.SetAnnotations(cp.GetAnnotations())
	mg.SetFinalizers(cp.GetFinalizers())
	mg.SetGeneration(cp.GetGeneration())
	return nil
}

// Resolve maps mg's spec.accountRef to a Ready CloudflareAccount in mg's namespace. When the
// account is missing or not Ready it returns an *AccountError with reason AccountNotReady
// (use MarkAccountNotReady and requeue); API-server errors are returned unchanged.
//
// It labels mg with AccountLabel=<accountRef.name> (see AccountLabel) before using the account;
// every managed kind must resolve its account through Resolve so that the account cannot be
// deleted while mg still needs it. The label is set even when the account does not exist.
//
// A deleting account only serves objects that already have (or pin) a Cloudflare resource
// (ExternalID) or are being deleted themselves, so they can observe, update or clean it up. A
// new object must not create a resource under an account that is about to go away: it gets
// AccountNotReady and, if it is not labelled yet, no label (it does not hold the account up).
// This narrows, but cannot close, the window between the account controller's last usage
// listing and its finalizer removal (the cache may not show the deletionTimestamp yet).
func (a *Accounts) Resolve(ctx context.Context, mg commonv1alpha1.Managed) (*Resolved, error) {
	name := mg.GetResourceSpec().AccountRef.Name
	notReady := func(format string, args ...any) error {
		return &AccountError{Reason: commonv1alpha1.ReasonAccountNotReady, Message: fmt.Sprintf(format, args...)}
	}
	if name == "" {
		return nil, notReady("spec.accountRef.name is empty")
	}
	var acct cloudflarev1alpha1.CloudflareAccount
	getErr := a.kube.Get(ctx, types.NamespacedName{Namespace: mg.GetNamespace(), Name: name}, &acct)
	if getErr == nil && !acct.DeletionTimestamp.IsZero() && mg.GetDeletionTimestamp().IsZero() && ExternalID(mg) == "" {
		return nil, notReady("CloudflareAccount %s/%s is being deleted; not creating new resources under it", acct.Namespace, acct.Name)
	}
	if err := a.ensureAccountLabel(ctx, mg, name); err != nil {
		return nil, err
	}
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return nil, notReady("CloudflareAccount %s/%s not found", mg.GetNamespace(), name)
		}
		return nil, getErr
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
			return nil, notReady("CloudflareAccount %s/%s: %s%s", acct.Namespace, acct.Name, ae.Message, DeletingAccountHint(&acct, ae.Reason))
		}
		return nil, err
	}
	return &Resolved{Client: c, AccountID: acct.Spec.AccountID, Account: &acct}, nil
}

// AccountRetryInterval is how long callers should wait before retrying after AccountNotReady
// (they are not woken by account changes unless they watch CloudflareAccounts).
const AccountRetryInterval = 15 * time.Second
