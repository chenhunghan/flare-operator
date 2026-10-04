package account

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/chenhunghan/flare-operator/api/cloudflare/v1alpha1"
)

// Token Secret protection.
//
// A namespace deletion deletes the token Secret together with the CloudflareAccount and its
// managed objects. The account survives (AccountInUseFinalizer) until its users have cleaned
// up in Cloudflare, but they need the token for that. So every Secret referenced by an account
// that has not been released carries AccountTokenFinalizer:
//
//   - The account controller adds it to the Secret its account references (on every reconcile,
//     including while a deletion is blocked) and removes it from that Secret when it releases
//     the account (removes AccountInUseFinalizer).
//   - The token controller (TokenControllerName) removes it from every Secret no unreleased
//     account references any more: after an account went away, or its spec.tokenSecretRef now
//     names another Secret, or at start-up for leftovers. It is woken by Secrets carrying the
//     finalizer and by account changes that affect which Secrets are held.
//
// An account holds its Secret while it is not being deleted, or while it still carries
// AccountInUseFinalizer. A deleted Secret that is still held stays (Terminating, readable) until
// it is released; the finalizer is not added to a Secret that is already being deleted.

// TokenControllerName is the name of the controller that releases token Secrets.
const TokenControllerName = Name + "-token"

// secretFinalizerIndex indexes Secrets by whether they carry AccountTokenFinalizer ("true").
const secretFinalizerIndex = ".metadata.finalizers.accountToken"

// holdsSecret reports whether acct still needs the Secret its spec references.
func holdsSecret(acct *cloudflarev1alpha1.CloudflareAccount) bool {
	return acct.DeletionTimestamp.IsZero() || controllerutil.ContainsFinalizer(acct, cloudflarev1alpha1.AccountInUseFinalizer)
}

// secretHeld reports whether an account in ns holds the Secret name. self, when set, is an
// in-memory copy newer than the cache (the account being reconciled) and replaces its cached
// copy.
func (r *Reconciler) secretHeld(ctx context.Context, ns, name string, self *cloudflarev1alpha1.CloudflareAccount) (bool, error) {
	if self != nil && self.Spec.TokenSecretRef.Name == name && holdsSecret(self) {
		return true, nil
	}
	var list cloudflarev1alpha1.CloudflareAccountList
	if err := r.List(ctx, &list, client.InNamespace(ns), client.MatchingFields{secretNameIndex: name}); err != nil {
		return false, err
	}
	for i := range list.Items {
		a := &list.Items[i]
		if self != nil && a.Name == self.Name {
			continue
		}
		if a.Spec.TokenSecretRef.Name == name && holdsSecret(a) {
			return true, nil
		}
	}
	return false, nil
}

// syncSecret adds AccountTokenFinalizer to Secret ns/name when an account holds it and removes
// it when none does. It uses strategic-merge patches of the finalizer list, which add or
// remove one entry without replacing the others, so no optimistic lock is needed; a decision
// taken on a stale cache is corrected by the next event (both directions are woken by it).
func (r *Reconciler) syncSecret(ctx context.Context, ns, name string, self *cloudflarev1alpha1.CloudflareAccount) error {
	if name == "" {
		return nil
	}
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		return client.IgnoreNotFound(err)
	}
	held, err := r.secretHeld(ctx, ns, name, self)
	if err != nil {
		return err
	}
	has := controllerutil.ContainsFinalizer(&s, cloudflarev1alpha1.AccountTokenFinalizer)
	var patch map[string]any
	switch {
	case held && !has && s.DeletionTimestamp.IsZero():
		patch = map[string]any{"metadata": map[string]any{"finalizers": []string{cloudflarev1alpha1.AccountTokenFinalizer}}}
	case !held && has:
		patch = map[string]any{"metadata": map[string]any{"$deleteFromPrimitiveList/finalizers": []string{cloudflarev1alpha1.AccountTokenFinalizer}}}
	default:
		return nil
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if err := r.Patch(ctx, &s, client.RawPatch(types.StrategicMergePatchType, data)); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		return fmt.Errorf("update finalizer %s of token Secret %s/%s: %w", cloudflarev1alpha1.AccountTokenFinalizer, ns, name, err)
	}
	log.FromContext(ctx).V(1).Info("token Secret finalizer updated", "secret", name, "held", held)
	return nil
}

// tokenReconciler releases (and, for Secrets named by an account, adds) AccountTokenFinalizer.
type tokenReconciler struct{ r *Reconciler }

func (t tokenReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, t.r.syncSecret(ctx, req.Namespace, req.Name, nil)
}

// setupTokenController registers the token controller and the Secret finalizer index.
func (r *Reconciler) setupTokenController(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Secret{}, secretFinalizerIndex,
		func(o client.Object) []string {
			if controllerutil.ContainsFinalizer(o, cloudflarev1alpha1.AccountTokenFinalizer) {
				return []string{"true"}
			}
			return nil
		}); err != nil {
		return err
	}
	hasFinalizer := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return controllerutil.ContainsFinalizer(o, cloudflarev1alpha1.AccountTokenFinalizer)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named(TokenControllerName).
		For(&corev1.Secret{}, builder.WithPredicates(hasFinalizer)).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.tokenSecretsFor),
			builder.WithPredicates(holdChanged{})).
		Complete(tokenReconciler{r})
}

// tokenSecretsFor maps an account event to the Secrets whose hold it may change: the Secret it
// references and every Secret in its namespace carrying AccountTokenFinalizer (one of them may
// be the Secret it referenced before a spec change).
func (r *Reconciler) tokenSecretsFor(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	acct, ok := o.(*cloudflarev1alpha1.CloudflareAccount)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []ctrlreconcile.Request
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: acct.Namespace, Name: name}})
		}
	}
	add(acct.Spec.TokenSecretRef.Name)
	var list corev1.SecretList
	if err := r.List(ctx, &list, client.InNamespace(acct.Namespace), client.MatchingFields{secretFinalizerIndex: "true"}); err != nil {
		log.FromContext(ctx).Error(err, "list token Secrets", "namespace", acct.Namespace)
		return out
	}
	for _, s := range list.Items {
		add(s.Name)
	}
	return out
}

// holdChanged passes account events that can change which Secrets are held: creation,
// deletion, a new spec.tokenSecretRef.name, the start of a deletion, and AccountInUseFinalizer
// being added or removed.
type holdChanged struct{ predicate.Funcs }

func (holdChanged) Update(e event.UpdateEvent) bool {
	o, ok1 := e.ObjectOld.(*cloudflarev1alpha1.CloudflareAccount)
	n, ok2 := e.ObjectNew.(*cloudflarev1alpha1.CloudflareAccount)
	if !ok1 || !ok2 {
		return true
	}
	return o.Spec.TokenSecretRef.Name != n.Spec.TokenSecretRef.Name || holdsSecret(o) != holdsSecret(n)
}
