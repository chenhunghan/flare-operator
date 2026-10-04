package tunnel

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tunnelsv1alpha1 "github.com/chenhunghan/flare-operator/api/tunnels/v1alpha1"
)

// Namespace teardown. The namespace controller deletes a namespace's objects in no particular
// order, and the garbage collector may delete the Tunnel's owned Secret, Deployment or
// NetworkPolicy before the Tunnel itself gets its deletionTimestamp. A create in a terminating
// namespace is refused (403 Forbidden, status cause NamespaceTerminating). Retrying that on
// error backoff only logs Reconciler errors and, for the token Secret, fetches the connector
// token (GET …/token) on every retry, so both cases requeue quietly instead: the Tunnel's own
// deletion is on its way and starts the finalizer.

// isNamespaceTerminating reports whether err is the API server refusing a create because the
// namespace is being terminated.
func isNamespaceTerminating(err error) bool {
	return apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)
}

// namespaceTerminatingRequeue is the result of a sync that stopped because t's namespace is
// being terminated: no error (nothing to report), polled at DependencyRetry in case the
// namespace deletion is stuck.
func namespaceTerminatingRequeue(ctx context.Context, t *tunnelsv1alpha1.Tunnel) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("namespace is being terminated; not recreating the Tunnel's connector objects", "namespace", t.Namespace)
	return ctrl.Result{RequeueAfter: DependencyRetry}, nil
}

// connectorRemovedByTeardown reports whether t ran a connector (status.connector records its
// token Secret), that Secret is gone and a create of it would be refused because the namespace
// is terminating. It runs before any Cloudflare call, so a Tunnel whose Secret the garbage
// collector removed makes no Cloudflare request (tunnel, tag or token GETs) until it is deleted.
//
// The namespace is probed with a server-side dry-run create of the Secret, which runs the
// same admission (NamespaceLifecycle) as the real create and persists nothing; it needs only
// the create permission on Secrets the controller already has, and it runs only while the
// Secret is missing from the cache.
func (r *Reconciler) connectorRemovedByTeardown(ctx context.Context, t *tunnelsv1alpha1.Tunnel) (bool, error) {
	if t.Status.Connector.TokenSecretName == "" {
		return false, nil
	}
	key := client.ObjectKey{Namespace: t.Namespace, Name: TokenSecretName(t)}
	if err := r.Get(ctx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		return false, client.IgnoreNotFound(err)
	}
	probe := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	err := r.Create(ctx, probe, client.DryRunAll)
	switch {
	case err == nil, apierrors.IsAlreadyExists(err):
		return false, nil
	case isNamespaceTerminating(err):
		return true, nil
	default:
		// Anything else (RBAC, a webhook) is left to the real create, which reports it.
		log.FromContext(ctx).V(1).Info("dry-run create of the token Secret failed", "error", err.Error())
		return false, nil
	}
}
