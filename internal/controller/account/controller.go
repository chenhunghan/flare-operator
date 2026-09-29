// Package account implements the CloudflareAccount controller: it reads the token Secret,
// builds (and caches, through reconcile.Accounts) the account's Cloudflare client, verifies the
// token against the account, and reports Ready/Synced plus token status and expiry.
package account

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "cloudflareaccount"

// DefaultVerifyInterval is how often a Ready token is re-verified.
const DefaultVerifyInterval = 10 * time.Minute

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
	// Now defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// SetupWithManager registers the controller. It watches CloudflareAccounts (spec changes only,
// so its own status writes do not loop) and the Secrets they reference.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &cloudflarev1alpha1.CloudflareAccount{}, secretNameIndex,
		func(o client.Object) []string {
			return []string{o.(*cloudflarev1alpha1.CloudflareAccount).Spec.TokenSecretRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&cloudflarev1alpha1.CloudflareAccount{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !acct.DeletionTimestamp.IsZero() {
		r.Accounts.Forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	base := acct.DeepCopy()
	res, verr := r.verify(ctx, &acct)
	acct.Status.ObservedGeneration = acct.Generation
	if err := r.Status().Patch(ctx, &acct, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return res, verr
}

// verify updates acct.Status in memory and returns the requeue policy.
func (r *Reconciler) verify(ctx context.Context, acct *cloudflarev1alpha1.CloudflareAccount) (ctrl.Result, error) {
	notReady := func(reason, msg string) {
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
		r.setCond(acct, commonv1alpha1.ConditionReady, metav1.ConditionTrue, commonv1alpha1.ReasonAvailable, "token verified ("+info.Type+" token)")
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

func (r *Reconciler) setCond(acct *cloudflarev1alpha1.CloudflareAccount, typ string, st metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&acct.Status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: acct.Generation,
	})
}
