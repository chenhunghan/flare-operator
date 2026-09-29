// Package account implements the CloudflareAccount controller: it reads the token Secret,
// builds (and caches, through reconcile.Accounts) the account's Cloudflare client, verifies the
// token against the account, and reports Ready/Synced plus token status and expiry.
//
// Verification schedule: the token is verified with Cloudflare when this process has not
// verified it yet (start-up), when the spec (generation) or the token in the Secret changed,
// after a local problem (missing Secret, ...), and otherwise every VerifyInterval (or at token
// expiry). Other events, such as this controller's own finalizer patch of the token Secret, do
// not call Cloudflare. A failure to add or remove the token Secret's finalizer does not block
// verification; it is reported as Synced=False, reason TokenSecretUpdateFailed. A held token
// Secret that was deleted (Terminating) is named in the Ready message.
//
// Usage protection: every account carries the finalizer AccountInUseFinalizer. When it is
// deleted, the controller keeps verifying it (so managed objects can still use it to clean up
// in Cloudflare) and keeps the finalizer, with Synced=False reason DependencyNotReady, while any
// object of an API group ending in ".cloudflare.flare.dev" in the same namespace carries the
// label cloudflare.flare.dev/account=<name> (set by reconcile.Accounts.Resolve). While blocked it
// re-lists users every DependencyRequeue but re-verifies with Cloudflare only on the normal
// schedule and writes status only on change. An account nothing uses is released at once,
// whatever its Ready condition says; one that is used but not Ready (token expired, Secret gone,
// base URL not allowed, ...) stays blocked, and its condition messages say how to unblock it
// (reconcile.DeletingAccountHint).
//
// Token protection (secret.go): while an account has not been released, the Secret it
// references carries AccountTokenFinalizer, so a namespace deletion cannot remove the token
// before the account's users have cleaned up. Known limit: should the Secret go away anyway (it
// was deleted before it was protected, or its finalizer was removed by hand), the client kept
// for the deleting account lives in process memory only; after an operator restart the account
// is Ready=False (SecretNotFound) and its users cannot clean up until the Secret is restored or
// they are removed by hand.
package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "cloudflareaccount"

// DefaultVerifyInterval is how often a Ready token is re-verified.
const DefaultVerifyInterval = 10 * time.Minute

// DefaultDependencyRequeue is how often a deletion blocked by managed objects is re-checked.
const DefaultDependencyRequeue = 10 * time.Second

// cacheRetry is how soon a reconcile that found the cache behind its own last status write
// looks again.
const cacheRetry = time.Second

// cacheSettle bounds how long a cache that does not show the last verify's Ready condition is
// taken to be merely behind. After that the status is taken to have changed some other way (a
// lost write, an edit by someone else), and the account is verified and its status written
// again, instead of waiting for the next scheduled verify.
const cacheSettle = 5 * time.Second

// ManagedGroupSuffix selects the API groups whose objects can use an account.
const ManagedGroupSuffix = ".cloudflare.flare.dev"

// maxListedUsers bounds the objects named in the DependencyNotReady message.
const maxListedUsers = 5

const secretNameIndex = ".spec.tokenSecretRef.name"

func init() {
	controller.Register(controller.Registration{
		Name:        Name,
		AddToScheme: cloudflarev1alpha1.AddToScheme,
		Setup: func(mgr ctrl.Manager, d controller.Deps) error {
			return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts}).SetupWithManager(mgr)
		},
	})
}

// Reconciler reconciles CloudflareAccount objects.
type Reconciler struct {
	client.Client
	Accounts *reconcile.Accounts
	// VerifyInterval defaults to DefaultVerifyInterval.
	VerifyInterval time.Duration
	// DependencyRequeue defaults to DefaultDependencyRequeue.
	DependencyRequeue time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Discovery finds the managed kinds and Metadata lists them (metadata only). Both default
	// to clients built from the manager's config in SetupWithManager.
	Discovery discovery.DiscoveryInterface
	Metadata  metadata.Interface

	mu         sync.Mutex
	nextVerify map[types.NamespacedName]verifySchedule // when an account next re-verifies
}

// verifySchedule records when an account's verification is next due (from the last verify's
// requeue policy), for which generation and token it was computed, and the Ready condition it
// produced.
type verifySchedule struct {
	generation int64
	tokenKey   string
	due        time.Time
	// at is when the verify ran (cacheSettle counts from it).
	at time.Time
	// readyStatus and readyReason are the Ready condition the verify set; a cached account
	// that shows another one has not caught up with that status write yet.
	readyStatus metav1.ConditionStatus
	readyReason string
}

// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch
//
// Usage protection lists every kind of the *.cloudflare.flare.dev groups (metadata only). RBAC
// cannot match a group suffix, so each group is listed here; a group missing from this list is
// skipped (logged) because the operator could not manage its objects either.
// +kubebuilder:rbac:groups=kv.cloudflare.flare.dev;queues.cloudflare.flare.dev;d1.cloudflare.flare.dev;tunnels.cloudflare.flare.dev;workersvpc.cloudflare.flare.dev;workers.cloudflare.flare.dev;vectorize.cloudflare.flare.dev;secretsstore.cloudflare.flare.dev;aigateway.cloudflare.flare.dev,resources=*,verbs=get;list;watch

// SetupWithManager registers the controller. It watches CloudflareAccounts (spec changes only,
// so its own status writes do not loop) and the Secrets they reference. It also registers the
// token controller (TokenControllerName), which releases token Secrets (see secret.go).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Discovery == nil {
		d, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
		if err != nil {
			return err
		}
		r.Discovery = d
	}
	if r.Metadata == nil {
		m, err := metadata.NewForConfig(mgr.GetConfig())
		if err != nil {
			return err
		}
		r.Metadata = m
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &cloudflarev1alpha1.CloudflareAccount{}, secretNameIndex,
		func(o client.Object) []string {
			return []string{o.(*cloudflarev1alpha1.CloudflareAccount).Spec.TokenSecretRef.Name}
		}); err != nil {
		return err
	}
	if err := r.setupTokenController(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&cloudflarev1alpha1.CloudflareAccount{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, deletionStarted{}))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.accountsForSecret)).
		Complete(r)
}

func (r *Reconciler) accountsForSecret(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	var list cloudflarev1alpha1.CloudflareAccountList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{secretNameIndex: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list CloudflareAccounts for Secret", "secret", o.GetName())
		return nil
	}
	out := make([]ctrlreconcile.Request, 0, len(list.Items))
	for _, a := range list.Items {
		out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return out
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) dependencyRequeue() time.Duration {
	if r.DependencyRequeue > 0 {
		return r.DependencyRequeue
	}
	return DefaultDependencyRequeue
}

// deletionStarted passes the update that sets deletionTimestamp (the API server also bumps the
// generation then, but that is not guaranteed for every storage path).
type deletionStarted struct{ predicate.Funcs }

func (deletionStarted) Update(e event.UpdateEvent) bool {
	return e.ObjectOld != nil && e.ObjectNew != nil &&
		e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero()
}

func (r *Reconciler) interval() time.Duration {
	if r.VerifyInterval > 0 {
		return r.VerifyInterval
	}
	return DefaultVerifyInterval
}

// Reconcile verifies one CloudflareAccount.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var acct cloudflarev1alpha1.CloudflareAccount
	if err := r.Get(ctx, req.NamespacedName, &acct); err != nil {
		if apierrors.IsNotFound(err) {
			r.Accounts.Forget(req.NamespacedName)
			r.clearSchedule(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !acct.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &acct)
	}
	if !controllerutil.ContainsFinalizer(&acct, cloudflarev1alpha1.AccountInUseFinalizer) {
		base := acct.DeepCopy()
		controllerutil.AddFinalizer(&acct, cloudflarev1alpha1.AccountInUseFinalizer)
		if err := r.Patch(ctx, &acct, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	// A failure to protect the token Secret does not stop verification (the token may be fine,
	// and dependents need Ready); it is reported on Synced and retried.
	serr := r.syncSecret(ctx, acct.Namespace, acct.Spec.TokenSecretRef.Name, &acct)
	base := acct.DeepCopy()
	var (
		res  ctrl.Result
		verr error
	)
	// Verify only when due: events that change neither the spec nor the token (this
	// controller's own finalizer patch of the Secret, another account's) must not call
	// Cloudflare again.
	due, wait := r.verifyDue(ctx, &acct)
	behind, settled := r.cacheBehind(&acct)
	switch {
	case behind && !settled && (!due || r.lagging(&acct)):
		// The cache has not caught up with the last verify's status write: a status patch built
		// from it could overwrite newer conditions. Look again shortly. This holds even when a
		// verify is due (its schedule elapsed, the token changed): verifying on the stale object
		// would build the status from it, and a transient error, which keeps the Ready it finds,
		// would find none and write Ready=False over the last verify's Ready=True.
		if serr != nil {
			return ctrl.Result{}, serr
		}
		return ctrl.Result{RequeueAfter: cacheRetry}, nil
	case due || settled:
		// settled: the cache still does not show the last verify's status well after that
		// write; verify and write the status again rather than wait for the schedule.
		res, verr = r.verify(ctx, &acct)
		r.schedule(ctx, &acct, res, verr)
	default:
		res.RequeueAfter = wait
	}
	acct.Status.ObservedGeneration = acct.Generation
	r.noteHeldSecret(ctx, &acct)
	r.markSecretSync(&acct, serr)
	if !equality.Semantic.DeepEqual(base.Status, acct.Status) {
		if err := r.Status().Patch(ctx, &acct, client.MergeFrom(base)); err != nil {
			// The verify's result was not written: the retry must verify (and write) again,
			// not take the schedule as done.
			r.clearSchedule(req.NamespacedName)
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if serr != nil {
		return ctrl.Result{}, serr
	}
	return res, verr
}

// ReasonTokenSecretUpdateFailed marks Synced=False because the finalizer that protects the
// token Secret (AccountTokenFinalizer) could not be added or removed.
const ReasonTokenSecretUpdateFailed = "TokenSecretUpdateFailed"

// markSecretSync reports a failed syncSecret on Synced (and clears that report once it works).
func (r *Reconciler) markSecretSync(acct *cloudflarev1alpha1.CloudflareAccount, serr error) {
	c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionSynced)
	switch {
	case serr != nil:
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, ReasonTokenSecretUpdateFailed,
			fmt.Sprintf("cannot protect the token Secret %q: %v (the operator needs update/patch on Secrets)", acct.Spec.TokenSecretRef.Name, serr))
	case c != nil && c.Reason == ReasonTokenSecretUpdateFailed:
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonReconcileOK, "")
	}
}

// heldSecretNote explains that the token Secret was deleted but is kept (AccountTokenFinalizer)
// while this account holds it ("" when the Secret is not being deleted).
func (r *Reconciler) heldSecretNote(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) string {
	name := acct.Spec.TokenSecretRef.Name
	var s corev1.Secret
	if name == "" || r.Get(ctx, types.NamespacedName{Namespace: acct.Namespace, Name: name}, &s) != nil ||
		s.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(&s, cloudflarev1alpha1.AccountTokenFinalizer) {
		return ""
	}
	return fmt.Sprintf("; the token Secret %q was deleted but is kept (Terminating, finalizer %s) while this account uses it; "+
		"to rotate the token, update the Secret's data in place or point spec.tokenSecretRef at a new Secret "+
		"(a Secret recreated under the same name fails with AlreadyExists until this one is released)",
		name, cloudflarev1alpha1.AccountTokenFinalizer)
}

// noteHeldSecret appends heldSecretNote to a Ready=True message, which is rebuilt from the
// token type each time so the note appears and disappears with the Secret's state.
func (r *Reconciler) noteHeldSecret(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) {
	c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		return
	}
	r.setCond(acct, commonv1alpha1.ConditionReady, metav1.ConditionTrue, c.Reason, readyMessage(acct.Status.TokenType)+r.heldSecretNote(ctx, acct))
}

func readyMessage(tokenType string) string { return "token verified (" + tokenType + " token)" }

// reconcileDelete keeps the account (verified, so its users can still clean up) until no
// managed object uses it, then removes the finalizer.
//
// While the deletion is blocked it re-checks the users every DependencyRequeue, but it
// re-verifies the token with Cloudflare only when a verification is due (the normal
// VerifyInterval / token-expiry schedule, a spec change, or a local problem such as a missing
// Secret that may have been fixed), and it writes status only when the status changed.
func (r *Reconciler) reconcileDelete(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (ctrl.Result, error) {
	nn := types.NamespacedName{Namespace: acct.Namespace, Name: acct.Name}
	if !controllerutil.ContainsFinalizer(acct, cloudflarev1alpha1.AccountInUseFinalizer) {
		r.Accounts.Forget(nn)
		r.clearSchedule(nn)
		return ctrl.Result{}, r.syncSecret(ctx, acct.Namespace, acct.Spec.TokenSecretRef.Name, acct)
	}
	// Released whatever Ready says: with no users left nothing needs the token any more.
	users, more, uerr := r.usersOf(ctx, acct)
	if uerr == nil && len(users) == 0 {
		base := acct.DeepCopy()
		controllerutil.RemoveFinalizer(acct, cloudflarev1alpha1.AccountInUseFinalizer)
		if err := r.Patch(ctx, acct, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.Accounts.Forget(nn)
		r.clearSchedule(nn)
		// Release the token Secret now (acct no longer holds it); the token controller also
		// does it on the account's update or delete event.
		return ctrl.Result{}, r.syncSecret(ctx, acct.Namespace, acct.Spec.TokenSecretRef.Name, acct)
	}
	// Still used: the token must outlive the users' cleanup (this also protects a Secret that
	// predates the protection). A failure is reported on Synced and retried.
	serr := r.syncSecret(ctx, acct.Namespace, acct.Spec.TokenSecretRef.Name, acct)

	base := acct.DeepCopy()
	var (
		res  ctrl.Result
		verr error
	)
	if due, wait := r.verifyDue(ctx, acct); due || behindOnly(r.cacheBehind(acct)) {
		res, verr = r.verify(ctx, acct)
		r.schedule(ctx, acct, res, verr)
	} else {
		res.RequeueAfter = wait
	}
	acct.Status.ObservedGeneration = acct.Generation
	if uerr != nil {
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError,
			"deletion blocked: cannot check for managed objects using this account: "+uerr.Error())
	} else {
		count := fmt.Sprintf("%d managed object(s)", len(users))
		if more {
			count = fmt.Sprintf("more than %d managed objects", len(users))
		}
		msg := fmt.Sprintf("deletion blocked: %s in namespace %s still use this account (label %s=%s)",
			count, acct.Namespace, reconcile.AccountLabel, reconcile.AccountLabelValue(acct.Name))
		// Not Ready (for any reason): the users cannot clean up, so say why and how to unblock.
		// The hint goes before the (long) user list so the message bound does not cut it off.
		if c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady); c != nil && c.Status != metav1.ConditionTrue {
			msg += reconcile.DeletingAccountHint(acct, c.Reason)
		}
		msg += "; used by: " + strings.Join(users, ", ")
		if more {
			msg += ", ..."
		}
		reason := commonv1alpha1.ReasonDependency
		if serr != nil {
			reason = ReasonTokenSecretUpdateFailed
			msg = fmt.Sprintf("cannot protect the token Secret %q: %v; %s", acct.Spec.TokenSecretRef.Name, serr, msg)
		}
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reason, msg)
	}
	if !equality.Semantic.DeepEqual(base.Status, acct.Status) {
		if err := r.Status().Patch(ctx, acct, client.MergeFrom(base)); err != nil {
			r.clearSchedule(nn) // not written: verify again on the retry
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if uerr != nil {
		return ctrl.Result{}, uerr
	}
	if serr != nil {
		return ctrl.Result{}, serr
	}
	if verr != nil {
		return ctrl.Result{}, verr
	}
	return ctrl.Result{RequeueAfter: min(res.RequeueAfter, r.dependencyRequeue())}, nil
}

// localReasons are Ready=False reasons found without calling Cloudflare; they may be fixed at
// any time (the Secret is restored, ...), so an account showing one is always re-verified.
var localReasons = map[string]bool{
	cloudflarev1alpha1.ReasonSecretNotFound: true, cloudflarev1alpha1.ReasonSecretKeyMissing: true,
	cloudflarev1alpha1.ReasonBaseURLNotAllowed: true, cloudflarev1alpha1.ReasonClientError: true,
}

// verifyDue reports whether acct must be re-verified with Cloudflare now; if not, wait is how
// long until it must. It is due when this process has no schedule for acct (e.g. after a
// restart), the spec (generation) or the token changed since the last verify, the last verify
// found a local problem (a missing Secret, ...) that may be fixed at any time, or the schedule
// (VerifyInterval, token expiry) says so.
func (r *Reconciler) verifyDue(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (due bool, wait time.Duration) {
	if _, err := r.Accounts.ClientFor(ctx, acct); err != nil {
		return true, 0 // verify stops at ClientFor too: no Cloudflare call, just the reason
	}
	sch, ok := r.scheduleOf(acct)
	if !ok || sch.generation != acct.Generation || sch.tokenKey != r.tokenKey(ctx, acct) || localReasons[sch.readyReason] {
		return true, 0
	}
	wait = sch.due.Sub(r.now())
	return wait <= 0, wait
}

func (r *Reconciler) scheduleOf(acct *cloudflarev1alpha1.CloudflareAccount) (verifySchedule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sch, ok := r.nextVerify[types.NamespacedName{Namespace: acct.Namespace, Name: acct.Name}]
	return sch, ok
}

// cacheBehind reports whether acct (normally read from the cache) does not show the status the
// last verify wrote yet: a status patch computed from it could overwrite newer conditions.
// settled reports a mismatch that is no longer taken for cache lag: there is no schedule, or
// the verify ran more than cacheSettle ago.
func (r *Reconciler) cacheBehind(acct *cloudflarev1alpha1.CloudflareAccount) (behind, settled bool) {
	sch, ok := r.scheduleOf(acct)
	c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady)
	behind = !ok || c == nil || c.ObservedGeneration != acct.Generation || acct.Status.ObservedGeneration != acct.Generation ||
		c.Status != sch.readyStatus || c.Reason != sch.readyReason
	return behind, behind && (!ok || r.now().Sub(sch.at) > cacheSettle)
}

func behindOnly(behind, _ bool) bool { return behind }

// lagging reports whether this process's last verify of acct was for acct's current generation,
// so that a cached acct that is behind (cacheBehind) lags that verify's status write rather
// than showing a spec change the verify has not seen.
func (r *Reconciler) lagging(acct *cloudflarev1alpha1.CloudflareAccount) bool {
	sch, ok := r.scheduleOf(acct)
	return ok && sch.generation == acct.Generation
}

// tokenKey fingerprints the token acct's Secret holds now ("" when it cannot be read).
func (r *Reconciler) tokenKey(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) string {
	tok, err := r.Accounts.Token(ctx, acct)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:16])
}

// schedule records when acct is next due for verification after a verify that returned res
// and err (an error: retry with the controller's backoff, so it is due at once).
func (r *Reconciler) schedule(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount, res ctrl.Result, err error) {
	nn := types.NamespacedName{Namespace: acct.Namespace, Name: acct.Name}
	key := r.tokenKey(ctx, acct)
	now := r.now()
	sch := verifySchedule{generation: acct.Generation, tokenKey: key, due: now.Add(res.RequeueAfter), at: now}
	if c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady); c != nil {
		sch.readyStatus, sch.readyReason = c.Status, c.Reason
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil || res.RequeueAfter <= 0 {
		delete(r.nextVerify, nn)
		return
	}
	if r.nextVerify == nil {
		r.nextVerify = map[types.NamespacedName]verifySchedule{}
	}
	r.nextVerify[nn] = sch
}

func (r *Reconciler) clearSchedule(nn types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nextVerify, nn)
}

// usersOf lists (metadata only) the objects of every *.cloudflare.flare.dev kind in acct's
// namespace that carry the account label. It returns up to maxListedUsers "Kind.group/name"
// strings and whether there are more.
func (r *Reconciler) usersOf(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) ([]string, bool, error) {
	lists, err := discovery.ServerPreferredNamespacedResources(r.Discovery)
	if err != nil {
		var gdf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gdf) {
			return nil, false, fmt.Errorf("discovery: %w", err)
		}
		for gv, gerr := range gdf.Groups {
			if strings.HasSuffix(gv.Group, ManagedGroupSuffix) {
				return nil, false, fmt.Errorf("discovery of %s: %w", gv, gerr)
			}
		}
	}
	sel := labels.SelectorFromSet(labels.Set{reconcile.AccountLabel: reconcile.AccountLabelValue(acct.Name)}).String()
	var users []string
	more := false
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil || !strings.HasSuffix(gv.Group, ManagedGroupSuffix) {
			continue
		}
		for _, res := range l.APIResources {
			if strings.Contains(res.Name, "/") || !slices.Contains(res.Verbs, "list") {
				continue
			}
			gvr := gv.WithResource(res.Name)
			got, err := r.Metadata.Resource(gvr).Namespace(acct.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel, Limit: maxListedUsers + 1})
			switch {
			case apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err):
				continue // the CRD went away since discovery
			case apierrors.IsForbidden(err):
				log.FromContext(ctx).Info("cannot list managed kind for account usage protection; skipping it (grant list on it)", "resource", gvr.String())
				continue
			case err != nil:
				return nil, false, fmt.Errorf("list %s: %w", gvr, err)
			}
			for _, it := range got.Items {
				if len(users) == maxListedUsers {
					more = true
					break
				}
				users = append(users, fmt.Sprintf("%s.%s/%s", res.Kind, gv.Group, it.Name))
			}
			if got.Continue != "" {
				more = true
			}
		}
	}
	sort.Strings(users)
	return users, more, nil
}

// verify updates acct.Status in memory and returns the requeue policy.
func (r *Reconciler) verify(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (ctrl.Result, error) {
	notReady := func(reason, msg string) {
		// A deleting account that is not Ready blocks its users' cleanup: say so.
		msg += reconcile.DeletingAccountHint(acct, reason)
		r.setCond(acct, commonv1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonReconcileOK, "")
	}
	transient := func(err error) (ctrl.Result, error) {
		// Keep a previous Ready=True: a blip must not flip every managed resource to AccountNotReady.
		if c := meta.FindStatusCondition(acct.Status.Conditions, commonv1alpha1.ConditionReady); c == nil {
			r.setCond(acct, commonv1alpha1.ConditionReady, metav1.ConditionFalse, commonv1alpha1.ReasonUnavailable, err.Error())
		}
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError, err.Error())
		return ctrl.Result{}, err
	}

	cf, err := r.Accounts.ClientFor(ctx, acct)
	if err != nil {
		var ae *reconcile.AccountError
		if errors.As(err, &ae) {
			clearToken(acct)
			notReady(ae.Reason, ae.Message)
			return ctrl.Result{RequeueAfter: r.interval()}, nil
		}
		return transient(err)
	}
	info, err := Verify(ctx, cf, acct.Spec.AccountID)
	if err != nil {
		var ae *reconcile.AccountError
		if errors.As(err, &ae) {
			clearToken(acct)
			notReady(ae.Reason, ae.Message)
			return ctrl.Result{RequeueAfter: r.interval()}, nil
		}
		return transient(err)
	}

	now := r.now()
	acct.Status.TokenID = info.ID
	acct.Status.TokenStatus = info.Status
	acct.Status.TokenType = info.Type
	acct.Status.TokenExpiresOn = nil
	if info.ExpiresOn != nil {
		t := metav1.NewTime(*info.ExpiresOn)
		acct.Status.TokenExpiresOn = &t
	}
	requeue := r.interval()
	switch {
	case info.Status == "disabled":
		notReady(cloudflarev1alpha1.ReasonTokenDisabled, "the API token is disabled")
	case info.Status == "expired" || (info.ExpiresOn != nil && !now.Before(*info.ExpiresOn)):
		notReady(cloudflarev1alpha1.ReasonTokenExpired, "the API token has expired")
	case info.NotBefore != nil && now.Before(*info.NotBefore):
		notReady(cloudflarev1alpha1.ReasonTokenNotYetValid, "the API token is not valid before "+info.NotBefore.UTC().Format(time.RFC3339))
		requeue = min(requeue, info.NotBefore.Sub(now)+time.Second)
	case info.Status != "active":
		notReady(cloudflarev1alpha1.ReasonTokenInvalid, "unexpected token status "+info.Status)
	default:
		t := metav1.NewTime(now)
		acct.Status.LastVerifiedTime = &t
		r.setCond(acct, commonv1alpha1.ConditionReady, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable, readyMessage(info.Type))
		r.setCond(acct, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, commonv1alpha1.ReasonReconcileOK, "")
		if info.ExpiresOn != nil {
			requeue = min(requeue, info.ExpiresOn.Sub(now)+time.Second)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func clearToken(acct *cloudflarev1alpha1.CloudflareAccount) {
	acct.Status.TokenID, acct.Status.TokenStatus, acct.Status.TokenType = "", "", ""
	acct.Status.TokenExpiresOn = nil
}

// maxConditionMessage bounds condition messages (they may quote API error text).
const maxConditionMessage = reconcile.MaxConditionMessage

func (r *Reconciler) setCond(acct *cloudflarev1alpha1.CloudflareAccount, typ string, st metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&acct.Status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: cfclient.Sanitize(msg, maxConditionMessage), ObservedGeneration: acct.Generation,
	})
}
