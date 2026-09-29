// Package vpcservice implements the VPCService controller: a Workers VPC connectivity service
// whose tunnel_id comes from a Tunnel (forProvider.tunnelRef) or is given directly.
//
// Behavior follows docs/spike-results-2026-09-29.md §2–§3 and the recordings cited in api.go:
//
//   - Hostnames are FQDN-only (CRD validation; cloudflared never applies search domains). An
//     unset resolver_ips defaults to the cluster DNS Service's ClusterIP, discovered at runtime.
//   - Adoption: the external-id annotation pins a service. VPC services carry no ownership
//     tag (Resource Tagging has no resource_type for them), so a managing object never adopts
//     a same-named service: it reports Synced=False, reason NameConflict. Only observe-only
//     objects look a service up by name. Known gap: if the create succeeds but persisting the
//     new ID fails (API server unavailable), the next reconcile reports NameConflict for the
//     object's own service and the annotation must be set by hand.
//   - The desired body is compared with the observed service; only a difference sends a PUT,
//     which is always the full body (PUT is a full replace).
//   - Cloudflare does not check dependencies on delete (0099), so the delete order is enforced
//     here: the Tunnel's finalizer waits for every VPCService that references it. A VPCService
//     whose Tunnel is missing, being deleted or has no ID yet is DependencyNotReady.
package vpcservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	tunnelsv1alpha1 "flare.dev/operator/api/tunnels/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/tunnelnet"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "vpcservice"

// Defaults of the Reconciler's intervals.
const (
	DefaultResyncInterval = 10 * time.Minute
	DependencyRetry       = 30 * time.Second
)

// Condition reasons of this controller.
const (
	// ReasonInvalidHostname marks a hostname that looks like a short in-cluster name.
	ReasonInvalidHostname = "InvalidHostname"
	// ReasonNameConflict marks a managing object whose service name is taken by a service it
	// does not manage (it is adopted only through the external-id annotation).
	ReasonNameConflict = "NameConflict"
)

func init() {
	controller.Register(controller.Registration{
		Name:        Name,
		AddToScheme: AddToScheme,
		Setup: func(mgr ctrl.Manager, d controller.Deps) error {
			return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts}).SetupWithManager(mgr)
		},
	})
}

// AddToScheme registers the API groups this controller reads (VPCServices and Tunnels).
func AddToScheme(s *runtime.Scheme) error {
	if err := workersvpcv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	return tunnelsv1alpha1.AddToScheme(s)
}

// Reconciler reconciles VPCService objects.
type Reconciler struct {
	client.Client
	Accounts *reconcile.Accounts
	// DNS locates cluster DNS (default kube-system/kube-dns, cluster.local).
	DNS tunnelnet.ClusterDNS
	// ResyncInterval re-reads the service periodically (default DefaultResyncInterval).
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=workersvpc.cloudflare.flare.dev,resources=vpcservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workersvpc.cloudflare.flare.dev,resources=vpcservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workersvpc.cloudflare.flare.dev,resources=vpcservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=tunnels.cloudflare.flare.dev,resources=tunnels,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *Reconciler) resync() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

// SetupWithManager registers the controller. Tunnel changes that matter to a VPCService (a new
// status.id, deletion, account change) requeue every VPCService that references the Tunnel.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.DNS = r.DNS.WithDefaults()
	if err := tunnelnet.RegisterIndexes(context.Background(), mgr.GetFieldIndexer(), r.DNS.Domain); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&workersvpcv1alpha1.VPCService{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&tunnelsv1alpha1.Tunnel{}, handler.EnqueueRequestsFromMapFunc(r.servicesForTunnel), builder.WithPredicates(tunnelChanged())).
		Complete(r)
}

func tunnelChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*tunnelsv1alpha1.Tunnel)
			n, ok2 := e.ObjectNew.(*tunnelsv1alpha1.Tunnel)
			if !ok1 || !ok2 {
				return true
			}
			return o.Status.ID != n.Status.ID || o.DeletionTimestamp.IsZero() != n.DeletionTimestamp.IsZero() ||
				o.Spec.AccountRef != n.Spec.AccountRef
		},
	}
}

func (r *Reconciler) servicesForTunnel(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	var vl workersvpcv1alpha1.VPCServiceList
	if err := r.List(ctx, &vl, client.InNamespace(o.GetNamespace()), client.MatchingFields{tunnelnet.IndexTunnelRef: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list VPCServices for Tunnel", "tunnel", o.GetName())
		return nil
	}
	out := make([]ctrlreconcile.Request, 0, len(vl.Items))
	for _, vs := range vl.Items {
		out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: vs.Namespace, Name: vs.Name}})
	}
	return out
}

// Reconcile syncs one VPCService.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vs workersvpcv1alpha1.VPCService
	if err := r.Get(ctx, req.NamespacedName, &vs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := vs.DeepCopy()
	var (
		res  ctrl.Result
		err  error
		gone bool
	)
	if !vs.DeletionTimestamp.IsZero() {
		res, gone, err = r.finalize(ctx, &vs)
	} else {
		res, err = r.sync(ctx, &vs)
	}
	if gone {
		return ctrl.Result{}, err
	}
	reconcile.SetObservedGeneration(&vs)
	if !equality.Semantic.DeepEqual(base.Status, vs.Status) {
		if perr := r.Status().Patch(ctx, &vs, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
			return ctrl.Result{}, errors.Join(err, perr)
		}
	}
	return res, err
}

func (r *Reconciler) finalize(ctx context.Context, vs *workersvpcv1alpha1.VPCService) (ctrl.Result, bool, error) {
	if !controllerutil.ContainsFinalizer(vs, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, true, nil
	}
	var acct *reconcile.Resolved
	if reconcile.ShouldDeleteExternal(vs, commonv1alpha1.DeletionDelete) && reconcile.ExternalID(vs) != "" {
		var err error
		if acct, err = r.Accounts.Resolve(ctx, vs); err != nil {
			if reconcile.IsAccountNotReady(err) {
				reconcile.MarkAccountNotReady(vs, err)
				return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, false, nil
			}
			return ctrl.Result{}, false, err
		}
	}
	_, err := reconcile.Finalize(ctx, r.Client, vs, commonv1alpha1.DeletionDelete, func(ctx context.Context, id string) error {
		return deleteService(ctx, acct.Client, acct.AccountID, id)
	})
	if err != nil {
		return ctrl.Result{}, false, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, true, nil
}

// dependencyError marks vs DependencyNotReady.
func dependencyError(vs *workersvpcv1alpha1.VPCService, msg string) (ctrl.Result, error) {
	reconcile.SetReady(vs, metav1.ConditionFalse, commonv1alpha1.ReasonDependency, msg)
	reconcile.SetSynced(vs, metav1.ConditionFalse, commonv1alpha1.ReasonDependency, msg)
	return ctrl.Result{RequeueAfter: DependencyRetry}, nil
}

// tunnelID resolves the tunnel_id: from tunnelRef (the Tunnel's status.id) or as given.
// ok=false carries a DependencyNotReady message.
func (r *Reconciler) tunnelID(ctx context.Context, vs *workersvpcv1alpha1.VPCService) (id, msg string, err error) {
	fp := vs.Spec.ForProvider
	if fp.TunnelRef == nil {
		h := fp.Host
		switch {
		case h.Network != nil && h.Network.TunnelID != nil:
			return *h.Network.TunnelID, "", nil
		case h.ResolverNetwork != nil && h.ResolverNetwork.TunnelID != nil:
			return *h.ResolverNetwork.TunnelID, "", nil
		}
		return "", "no tunnelRef and no tunnel_id", nil
	}
	var t tunnelsv1alpha1.Tunnel
	if err := r.Get(ctx, client.ObjectKey{Namespace: vs.Namespace, Name: fp.TunnelRef.Name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("Tunnel %s not found", fp.TunnelRef.Name), nil
		}
		return "", "", err
	}
	switch {
	case !t.DeletionTimestamp.IsZero():
		return "", fmt.Sprintf("Tunnel %s is being deleted", t.Name), nil
	case t.Spec.AccountRef.Name != vs.Spec.AccountRef.Name:
		return "", fmt.Sprintf("Tunnel %s uses CloudflareAccount %s, not %s", t.Name, t.Spec.AccountRef.Name, vs.Spec.AccountRef.Name), nil
	case t.Status.ID == "":
		return "", fmt.Sprintf("Tunnel %s has no Cloudflare ID yet", t.Name), nil
	}
	return t.Status.ID, "", nil
}

// shortNameError rejects "<service>.<namespace>" hostnames that name an existing Service: they
// look fully qualified to the CRD pattern, but cloudflared never applies search domains, so they
// fail with dns_error (spike §2.1).
func (r *Reconciler) shortNameError(ctx context.Context, host string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(labels) != 2 {
		return "", nil
	}
	var svc corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: labels[1], Name: labels[0]}, &svc)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", err
	}
	return fmt.Sprintf("hostname %q is the short name of Service %s/%s; search domains never apply, use %s.%s.svc.%s",
		host, labels[1], labels[0], labels[0], labels[1], r.DNS.WithDefaults().Domain), nil
}

// desired builds the full create/replace body.
func (r *Reconciler) desired(ctx context.Context, vs *workersvpcv1alpha1.VPCService, tunnelID string) (*apiService, error) {
	fp := vs.Spec.ForProvider
	body := &apiService{
		Name: vs.ServiceName(), Type: fp.Type,
		HTTPPort: fp.HTTPPort, HTTPSPort: fp.HTTPSPort, TCPPort: fp.TCPPort, AppProtocol: fp.AppProtocol,
		Host: apiHost{IPv4: fp.Host.IPv4, IPv6: fp.Host.IPv6, Hostname: fp.Host.Hostname},
	}
	if fp.TLSSettings != nil {
		body.TLSSettings = &apiTLS{CertVerificationMode: fp.TLSSettings.CertVerificationMode}
	}
	if fp.Host.Hostname != nil {
		n := &apiNetwork{TunnelID: tunnelID}
		if fp.Host.ResolverNetwork != nil && len(fp.Host.ResolverNetwork.ResolverIPs) > 0 {
			n.ResolverIPs = append([]string(nil), fp.Host.ResolverNetwork.ResolverIPs...)
		} else {
			ip, err := r.DNS.ResolverIP(ctx, r.Client)
			if err != nil {
				return nil, err
			}
			if ip != "" { // 0176: resolver_ips [kube-dns ClusterIP]
				n.ResolverIPs = []string{ip}
			}
		}
		body.Host.ResolverNetwork = n
	} else {
		body.Host.Network = &apiNetwork{TunnelID: tunnelID}
	}
	return body, nil
}

func (r *Reconciler) sync(ctx context.Context, vs *workersvpcv1alpha1.VPCService) (ctrl.Result, error) {
	acct, err := r.Accounts.Resolve(ctx, vs)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(vs, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, vs); err != nil {
		return ctrl.Result{}, err
	}
	pol := reconcile.PoliciesOf(vs)
	cf, accountID := acct.Client, acct.AccountID
	fp := vs.Spec.ForProvider
	syncErr := func(err error) (ctrl.Result, error) {
		reconcile.MarkSyncError(vs, "", err)
		return ctrl.Result{}, err
	}
	notFound := func(msg string) (ctrl.Result, error) {
		vs.Status.AtProvider = workersvpcv1alpha1.VPCServiceObservation{}
		reconcile.MarkNotFound(vs, msg)
		reconcile.SetSynced(vs, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}

	// Observe.
	var cur *apiService
	if id := reconcile.ExternalID(vs); id != "" {
		if cur, err = getService(ctx, cf, accountID, id); err != nil {
			return syncErr(err)
		}
		if cur == nil {
			if reconcile.HasExternalIDAnnotation(vs) {
				return notFound(fmt.Sprintf("VPC service %s does not exist; remove the %s annotation to create a new one", id, commonv1alpha1.AnnotationExternalID))
			}
			vs.Status.ID = ""
		}
	}
	if fp == nil || pol.ObserveOnly() {
		if cur == nil {
			if fp == nil {
				return notFound("no external-id annotation and no forProvider to find the service by name")
			}
			if cur, err = findServiceByName(ctx, cf, accountID, vs.ServiceName()); err != nil {
				return syncErr(err)
			}
			if cur == nil {
				return notFound(fmt.Sprintf("no VPC service named %q", vs.ServiceName()))
			}
			if err := reconcile.PersistExternalID(ctx, r.Client, vs, cur.ServiceID); err != nil {
				return ctrl.Result{}, err
			}
		}
		vs.Status.ID = cur.ServiceID
		vs.Status.AtProvider = cur.observation()
		reconcile.MarkAvailable(vs)
		reconcile.MarkSynced(vs)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}

	// Desired state.
	if fp.Host.Hostname != nil {
		msg, err := r.shortNameError(ctx, *fp.Host.Hostname)
		if err != nil {
			return syncErr(err)
		}
		if msg != "" {
			reconcile.SetReady(vs, metav1.ConditionFalse, ReasonInvalidHostname, msg)
			reconcile.SetSynced(vs, metav1.ConditionFalse, ReasonInvalidHostname, msg)
			return ctrl.Result{}, nil
		}
	}
	tunnelID, depMsg, err := r.tunnelID(ctx, vs)
	if err != nil {
		return syncErr(err)
	}
	if depMsg != "" {
		if cur != nil {
			vs.Status.AtProvider = cur.observation()
		}
		return dependencyError(vs, depMsg)
	}
	body, err := r.desired(ctx, vs, tunnelID)
	if err != nil {
		return syncErr(err)
	}

	if cur == nil {
		existing, err := findServiceByName(ctx, cf, accountID, body.Name)
		if err != nil {
			return syncErr(err)
		}
		switch {
		case existing != nil:
			// Never adopt by name: nothing marks which object owns a VPC service, so a
			// same-named object (another namespace, or the same forProvider.name) would
			// manage — and on deletion delete — a service someone else uses. Adoption is
			// explicit, through the external-id annotation.
			msg := fmt.Sprintf("a VPC service named %q already exists (service_id %s) and this object does not manage it; "+
				"set the %s annotation to that ID to adopt it, or choose another forProvider.name",
				body.Name, existing.ServiceID, commonv1alpha1.AnnotationExternalID)
			vs.Status.AtProvider = workersvpcv1alpha1.VPCServiceObservation{}
			reconcile.SetReady(vs, metav1.ConditionFalse, ReasonNameConflict, msg)
			reconcile.SetSynced(vs, metav1.ConditionFalse, ReasonNameConflict, msg)
			return ctrl.Result{RequeueAfter: DependencyRetry}, nil
		case pol.CanCreate():
			if cur, err = createService(ctx, cf, accountID, body); err != nil {
				return syncErr(err)
			}
			if err := reconcile.PersistExternalID(ctx, r.Client, vs, cur.ServiceID); err != nil {
				return ctrl.Result{}, err
			}
		default:
			return notFound(fmt.Sprintf("no VPC service named %q and managementPolicies do not allow Create", body.Name))
		}
	}
	vs.Status.ID = cur.ServiceID
	inSync := matches(body, cur)
	if !inSync && pol.CanUpdate() {
		updated, err := replaceService(ctx, cf, accountID, cur.ServiceID, body)
		if err != nil {
			vs.Status.AtProvider = cur.observation()
			return syncErr(err)
		}
		cur, inSync = updated, true
	}
	vs.Status.AtProvider = cur.observation()
	vs.Status.TunnelID = tunnelID
	reconcile.MarkAvailable(vs)
	if inSync {
		reconcile.MarkSynced(vs)
	} else {
		reconcile.SetSynced(vs, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError,
			"the service differs from forProvider and managementPolicies do not allow Update")
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}
