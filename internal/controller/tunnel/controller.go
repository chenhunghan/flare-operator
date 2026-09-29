// Package tunnel implements the Tunnel controller: a remotely managed Cloudflare Tunnel
// (config_src=cloudflare) plus the Kubernetes objects that connect it — an owned Secret with
// the connector token, an owned cloudflared Deployment and an owned egress NetworkPolicy
// generated from the VPCServices that reference the Tunnel.
//
// Behavior follows docs/spike-results-2026-09-29.md §2–§3 and the recordings cited in api.go:
//
//   - Create/adopt: the external-id annotation pins a tunnel; otherwise a live tunnel with the
//     same name is adopted once the ownership tag was written (reconcile.RecordOwnership then
//     pins it and records the proof). With tagging disabled nothing can prove that a
//     same-named tunnel is ours, so it is not adopted by name: Ready/Synced=False, reason
//     NameConflict, no connectors (as VPCService); pin it to adopt it. A tunnel known only
//     through status.id (an untagged adoption by an older build) is treated the same way.
//     Otherwise one is created and the ownership record is written at once
//     (reconcile.RecordCreated, which a concurrent change of the object cannot make fail). An
//     older build's created-by-uid annotation is honoured and migrated. An observe-only object
//     that finds a tunnel by name keeps its ID only in status.atProvider. A pinned tunnel that
//     is gone (404, or soft-deleted: GET answers 200 with deleted_at) is reported as
//     ExternalNotFound and never silently recreated.
//   - Ready = the Cloudflare tunnel exists AND the cloudflared Deployment has a ready replica
//     (or zero replicas are wanted). A Tunnel whose managementPolicies allow no write (Observe,
//     alone or with LateInitialize) only reads the tunnel: no ownership tag is checked or set,
//     so no token Secret, connector Deployment or NetworkPolicy is created.
//   - Deletion waits (Ready=False, reason DependencyNotReady) while any VPCService references
//     the Tunnel, because Cloudflare lets a referenced tunnel be deleted (0099). With the Delete
//     policy it then scales cloudflared to zero, waits until the pods are gone and the tunnel
//     reports no active connections (a connected tunnel refuses deletion with 400/1022, 0095),
//     and soft-deletes it; each wait is a reconcile.WaitError (requeue, no error). Only a
//     tunnel whose ownership is proven is deleted (reconcile.MayDeleteExternal: the ownership
//     record, the owner tag naming this object, or with tagging disabled the pin); any other is
//     kept with a Warning Event (ForeignOwnerTunnelKept, ExternalResourceKept). If the
//     CloudflareAccount no longer exists the tunnel is kept (ExternalResourceKept) and the
//     finalizer removed; an account that exists but is not Ready is waited for
//     (reconcile.FinalizeAccount).
//   - Owned objects (Deployment, NetworkPolicy) are rewritten when the desired spec changes or
//     a field the controller sets drifted (see drifted).
//   - A reconcile without spec changes makes no Cloudflare writes (only GETs).
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/tunnelnet"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "tunnel"

// Defaults of the Reconciler's intervals.
const (
	DefaultResyncInterval = 10 * time.Minute
	DefaultDrainInterval  = 5 * time.Second
	DependencyRetry       = 30 * time.Second
)

// tagResourceType is the Resource Tagging resource_type of a tunnel.
const tagResourceType = "cloudflared_tunnel"

func init() {
	controller.Register(controller.Registration{
		Name:        Name,
		AddToScheme: AddToScheme,
		Setup: func(mgr ctrl.Manager, d controller.Deps) error {
			return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				Recorder: mgr.GetEventRecorder(Name), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr)
		},
	})
}

// AddToScheme registers the API groups this controller reads (Tunnels and VPCServices).
func AddToScheme(s *runtime.Scheme) error {
	if err := tunnelsv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	return workersvpcv1alpha1.AddToScheme(s)
}

// Reconciler reconciles Tunnel objects.
type Reconciler struct {
	client.Client
	Accounts    *reconcile.Accounts
	Tagger      reconcile.Tagger
	ClusterName string
	// DNS locates cluster DNS (default kube-system/kube-dns, cluster.local).
	DNS tunnelnet.ClusterDNS
	// ResyncInterval re-reads the tunnel periodically (default DefaultResyncInterval).
	ResyncInterval time.Duration
	// DrainInterval is the poll interval while deleting (default DefaultDrainInterval).
	DrainInterval time.Duration
	// Recorder records Events on Tunnels (optional; nil records none).
	Recorder events.EventRecorder
	// APIReader confirms, uncached, that a CloudflareAccount is gone before a finalizer gives
	// up on its Cloudflare resource (default: Client).
	APIReader client.Reader
}

// EventReasonTunnelKept is the reason of the Warning Event recorded when a Tunnel with the
// Delete policy is deleted but its Cloudflare tunnel is kept because another owner holds it.
const EventReasonTunnelKept = "ForeignOwnerTunnelKept"

// +kubebuilder:rbac:groups=tunnels.cloudflare.flare.dev,resources=tunnels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=tunnels.cloudflare.flare.dev,resources=tunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnels.cloudflare.flare.dev,resources=tunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=workersvpc.cloudflare.flare.dev,resources=vpcservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *Reconciler) resync() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

func (r *Reconciler) drain() time.Duration {
	if r.DrainInterval > 0 {
		return r.DrainInterval
	}
	return DefaultDrainInterval
}

func (r *Reconciler) tagger() reconcile.Tagger {
	if r.Tagger == nil {
		return reconcile.NoopTagger{}
	}
	return r.Tagger
}

// SetupWithManager registers the controller and its watches: owned Deployments, Secrets and
// NetworkPolicies; VPCServices (their tunnelRef, both old and new); Services that back a
// referencing VPCService (and the cluster DNS Service).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.DNS = r.DNS.WithDefaults()
	if err := tunnelnet.RegisterIndexes(context.Background(), mgr.GetFieldIndexer(), r.DNS.Domain); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&tunnelsv1alpha1.Tunnel{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&workersvpcv1alpha1.VPCService{}, handler.EnqueueRequestsFromMapFunc(tunnelForVPCService),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.tunnelsForService),
			builder.WithPredicates(serviceChanged())).
		Complete(r)
}

func tunnelForVPCService(_ context.Context, o client.Object) []ctrlreconcile.Request {
	vs, ok := o.(*workersvpcv1alpha1.VPCService)
	if !ok || vs.TunnelRefName() == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: vs.Namespace, Name: vs.TunnelRefName()}}}
}

// serviceChanged passes Service creates/deletes and updates of the fields the policy uses.
func serviceChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*corev1.Service)
			n, ok2 := e.ObjectNew.(*corev1.Service)
			if !ok1 || !ok2 {
				return true
			}
			return !equality.Semantic.DeepEqual(o.Spec.Selector, n.Spec.Selector) ||
				!equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports) ||
				!equality.Semantic.DeepEqual(o.Spec.ClusterIPs, n.Spec.ClusterIPs) ||
				o.Spec.Type != n.Spec.Type
		},
	}
}

func (r *Reconciler) tunnelsForService(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	svc, ok := o.(*corev1.Service)
	if !ok {
		return nil
	}
	seen := map[types.NamespacedName]bool{}
	var out []ctrlreconcile.Request
	add := func(nn types.NamespacedName) {
		if !seen[nn] {
			seen[nn] = true
			out = append(out, ctrlreconcile.Request{NamespacedName: nn})
		}
	}
	dns := r.DNS.WithDefaults()
	if svc.Namespace == dns.Namespace && svc.Name == dns.Name {
		var tl tunnelsv1alpha1.TunnelList
		if err := r.List(ctx, &tl); err != nil {
			log.FromContext(ctx).Error(err, "list Tunnels for the DNS Service")
			return nil
		}
		for _, t := range tl.Items {
			add(types.NamespacedName{Namespace: t.Namespace, Name: t.Name})
		}
		return out
	}
	keys := []string{tunnelnet.ServiceKey(svc.Namespace, svc.Name)}
	for _, ip := range tunnelnet.ServiceClusterIPs(svc) {
		keys = append(keys, tunnelnet.IPKey(ip))
	}
	for _, k := range keys {
		var vl workersvpcv1alpha1.VPCServiceList
		if err := r.List(ctx, &vl, client.MatchingFields{tunnelnet.IndexBackend: k}); err != nil {
			log.FromContext(ctx).Error(err, "list VPCServices for Service", "service", svc.Namespace+"/"+svc.Name)
			continue
		}
		for _, vs := range vl.Items {
			if n := vs.TunnelRefName(); n != "" {
				add(types.NamespacedName{Namespace: vs.Namespace, Name: n})
			}
		}
	}
	return out
}

// Reconcile syncs one Tunnel.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t tunnelsv1alpha1.Tunnel
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := t.DeepCopy()
	var (
		res  ctrl.Result
		err  error
		gone bool
	)
	if !t.DeletionTimestamp.IsZero() {
		res, gone, err = r.finalize(ctx, &t)
	} else {
		res, err = r.sync(ctx, &t)
	}
	if gone {
		return ctrl.Result{}, err
	}
	reconcile.SetObservedGeneration(&t)
	if !equality.Semantic.DeepEqual(base.Status, t.Status) {
		if perr := r.Status().Patch(ctx, &t, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
			return ctrl.Result{}, errors.Join(err, perr)
		}
	}
	return res, err
}

// referencing lists the VPCServices in t's namespace whose tunnelRef names t.
func (r *Reconciler) referencing(ctx context.Context, t *tunnelsv1alpha1.Tunnel) ([]workersvpcv1alpha1.VPCService, error) {
	var vl workersvpcv1alpha1.VPCServiceList
	if err := r.List(ctx, &vl, client.InNamespace(t.Namespace), client.MatchingFields{tunnelnet.IndexTunnelRef: t.Name}); err != nil {
		return nil, err
	}
	return vl.Items, nil
}

func (r *Reconciler) owner(t *tunnelsv1alpha1.Tunnel) string {
	return reconcile.OwnerValue(r.ClusterName, t.Namespace, t.Name)
}

// sync reconciles a live Tunnel.
func (r *Reconciler) sync(ctx context.Context, t *tunnelsv1alpha1.Tunnel) (ctrl.Result, error) {
	acct, err := r.Accounts.Resolve(ctx, t)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(t, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, t); err != nil {
		return ctrl.Result{}, err
	}
	pol := reconcile.PoliciesOf(t)
	cf, accountID := acct.Client, acct.AccountID
	syncErr := func(err error) (ctrl.Result, error) {
		reconcile.MarkSyncError(t, "", err)
		return ctrl.Result{}, err
	}

	// Observe (and adopt or create).
	var tun *apiTunnel
	tagging := reconcile.TaggingEnabled(r.tagger())
	if id := reconcile.ExternalID(t); id != "" && !tagging && pol.CanWrite() && !reconcile.HasExternalIDAnnotation(t) && !reconcile.HasOwnershipProof(t, id) {
		// Without tagging, a tunnel known through status.id alone (an untagged adoption by an
		// older build) is not proven to be ours: look it up by name again, which reports
		// NameConflict instead of running connectors on it.
		t.Status.ID = ""
	}
	if id := reconcile.ExternalID(t); id != "" {
		if tun, err = getTunnel(ctx, cf, accountID, id); err != nil {
			return syncErr(err)
		}
		if tun == nil {
			if reconcile.HasExternalIDAnnotation(t) {
				msg := fmt.Sprintf("tunnel %s does not exist or was deleted; remove the %s annotation to create a new one", id, commonv1alpha1.AnnotationExternalID)
				t.Status.AtProvider = tunnelsv1alpha1.TunnelObservation{}
				reconcile.MarkNotFound(t, msg)
				reconcile.SetSynced(t, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
				return ctrl.Result{RequeueAfter: r.resync()}, nil
			}
			t.Status.ID = "" // status.id only: the tunnel went away, look it up again
		}
	}
	token, tagged := "", false
	if tun == nil {
		if tun, err = findTunnelByName(ctx, cf, accountID, t.TunnelName()); err != nil {
			return syncErr(err)
		}
		switch {
		case tun != nil && !pol.CanWrite():
			// Observe-only: a name match is only observed. Its ID goes to status.atProvider,
			// never to the external-id annotation or status.id (ExternalID would return
			// either), so switching the object to management re-runs the ownership check.
			t.Status.ID = ""
			t.Status.AtProvider = tun.observation()
			reconcile.MarkAvailable(t)
			reconcile.MarkSynced(t)
			return ctrl.Result{RequeueAfter: r.resync()}, nil
		case tun != nil && !tagging:
			// Without an ownership tag nothing proves that this object owns a same-named
			// tunnel: it may be another cluster's, and running connectors on it would take a
			// share of that tunnel's traffic. Adoption is explicit, through the external-id
			// annotation (as for VPCService).
			msg := fmt.Sprintf("a tunnel named %q already exists (id %s) and ownership tagging is disabled, so this object cannot prove "+
				"that it owns it; set the %s annotation to that ID to adopt it, or choose another forProvider.name",
				t.TunnelName(), tun.ID, commonv1alpha1.AnnotationExternalID)
			t.Status.ID = ""
			t.Status.AtProvider = tunnelsv1alpha1.TunnelObservation{}
			reconcile.SetReady(t, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
			reconcile.SetSynced(t, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
			return ctrl.Result{RequeueAfter: DependencyRetry}, nil
		case tun != nil:
			// Adopt by name once the owner tag is ours (recorded below).
			if err := r.tagger().EnsureOwner(ctx, cf, accountID, reconcile.TagTarget{Type: tagResourceType, ID: tun.ID}, r.owner(t)); err != nil {
				return r.ownershipError(t, err)
			}
			tagged = true
		case pol.CanCreate():
			if tun, err = createTunnel(ctx, cf, accountID, t.TunnelName()); err != nil {
				return syncErr(err)
			}
			token = tun.Token
			// Created by this object: proof of ownership, written so a Conflict cannot lose it.
			if err := reconcile.RecordCreated(ctx, r.Client, t, tun.ID); err != nil {
				return syncErr(fmt.Errorf("record the new tunnel %s: %w", tun.ID, err))
			}
			reconcile.MarkCreating(t, "tunnel created")
		default:
			msg := fmt.Sprintf("no live tunnel named %q and managementPolicies do not allow Create", t.TunnelName())
			t.Status.AtProvider = tunnelsv1alpha1.TunnelObservation{}
			reconcile.MarkNotFound(t, msg)
			reconcile.MarkSynced(t)
			return ctrl.Result{RequeueAfter: r.resync()}, nil
		}
	}
	t.Status.ID = tun.ID
	t.Status.AtProvider = tun.observation()

	// Without a write action (Observe alone, or with LateInitialize) the tunnel is never
	// tagged, so its ownership is unverified: running connectors on it could join a tunnel
	// another cluster owns and take a share of its traffic. Only observe it.
	if !pol.CanWrite() {
		reconcile.MarkAvailable(t)
		reconcile.MarkSynced(t)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	if !tagged {
		if err := r.tagger().EnsureOwner(ctx, cf, accountID, reconcile.TagTarget{Type: tagResourceType, ID: tun.ID}, r.owner(t)); err != nil {
			return r.ownershipError(t, err)
		}
	}
	// With tagging, our owner tag is on the tunnel now: record that proof (and pin the ID)
	// durably. Without tagging the object created or pins it; only an older build's
	// created-by-uid record is rewritten.
	record := reconcile.MigrateLegacyOwnership
	if tagging {
		record = reconcile.RecordOwnership
	}
	if err := record(ctx, r.Client, t, tun.ID); err != nil {
		return syncErr(fmt.Errorf("record ownership of tunnel %s: %w", tun.ID, err))
	}

	// Connector.
	if err := r.ensureTokenSecret(ctx, t, cf, accountID, tun.ID, token); err != nil {
		return syncErr(err)
	}
	dep, err := r.ensureDeployment(ctx, t, tun.ID)
	if err != nil {
		return syncErr(err)
	}
	if err := r.ensureNetworkPolicy(ctx, t); err != nil {
		return syncErr(err)
	}

	want := desiredReplicas(t)
	t.Status.Connector = tunnelsv1alpha1.ConnectorStatus{
		DeploymentName: dep.Name, TokenSecretName: TokenSecretName(t), Replicas: want, ReadyReplicas: dep.Status.ReadyReplicas,
	}
	switch {
	case want == 0 || dep.Status.ReadyReplicas > 0:
		reconcile.MarkAvailable(t)
	case tun.Status == "inactive" || tun.Status == "":
		reconcile.MarkCreating(t, fmt.Sprintf("waiting for cloudflared: %d/%d replicas ready", dep.Status.ReadyReplicas, want))
	default:
		reconcile.MarkUnavailable(t, fmt.Sprintf("cloudflared: %d/%d replicas ready, tunnel status %s", dep.Status.ReadyReplicas, want, tun.Status))
	}
	if tun.Name != t.TunnelName() {
		reconcile.MarkImmutable(t, fmt.Sprintf("forProvider.name %q differs from the tunnel's name %q; renaming is not supported", t.TunnelName(), tun.Name))
	} else {
		reconcile.MarkSynced(t)
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

func (r *Reconciler) ownershipError(t *tunnelsv1alpha1.Tunnel, err error) (ctrl.Result, error) {
	var conflict *reconcile.OwnershipConflictError
	if errors.As(err, &conflict) {
		reconcile.MarkUnavailable(t, err.Error())
		reconcile.MarkSyncError(t, "", err)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	reconcile.MarkSyncError(t, "", err)
	return ctrl.Result{}, err
}

// controlled checks that an existing owned-name object is controlled by t.
func controlled(t *tunnelsv1alpha1.Tunnel, o client.Object) error {
	if o.GetResourceVersion() != "" && !metav1.IsControlledBy(o, t) {
		return fmt.Errorf("%s/%s already exists and is not controlled by Tunnel %s", o.GetNamespace(), o.GetName(), t.Name)
	}
	return nil
}

// ensureTokenSecret keeps the token Secret for tunnel id. The token is fetched (GET …/token)
// only when the Secret is missing or belongs to another tunnel ID; token is the create
// response's token, if any.
func (r *Reconciler) ensureTokenSecret(ctx context.Context, t *tunnelsv1alpha1.Tunnel, cf cfclient.Client, accountID, id, token string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: TokenSecretName(t)}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := controlled(t, s); err != nil {
		return err
	}
	current := s.ResourceVersion != "" && s.Annotations[AnnotationTunnelID] == id && len(s.Data[TokenKey]) > 0
	if current && equality.Semantic.DeepEqual(s.Labels, mergeLabels(s.Labels, objectLabels(t))) {
		return nil
	}
	if !current && token == "" {
		var err error
		if token, err = getToken(ctx, cf, accountID, id); err != nil {
			return err
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		s.Labels = mergeLabels(s.Labels, objectLabels(t))
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations[AnnotationTunnelID] = id
		s.Type = corev1.SecretTypeOpaque
		if token != "" {
			s.Data = map[string][]byte{TokenKey: []byte(token)}
		}
		return controllerutil.SetControllerReference(t, s, r.Scheme())
	})
	return err
}

func mergeLabels(have, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range have {
		out[k] = v
	}
	for k, v := range want {
		out[k] = v
	}
	return out
}

// ensureDeployment keeps the cloudflared Deployment. The spec is rewritten when the desired
// spec's hash differs from the stored one or the live spec drifted from it.
func (r *Reconciler) ensureDeployment(ctx context.Context, t *tunnelsv1alpha1.Tunnel, id string) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: DeploymentName(t)}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(dep), dep); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := controlled(t, dep); err != nil {
		return nil, err
	}
	spec := deploymentSpec(t, id)
	hash := specHash(spec)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = mergeLabels(dep.Labels, objectLabels(t))
		if dep.Annotations[AnnotationSpecHash] != hash || drifted(spec, dep.Spec) {
			if dep.Annotations == nil {
				dep.Annotations = map[string]string{}
			}
			dep.Annotations[AnnotationSpecHash] = hash
			dep.Spec = spec
		}
		return controllerutil.SetControllerReference(t, dep, r.Scheme())
	})
	return dep, err
}

// ensureNetworkPolicy keeps (or, when disabled, removes) the egress NetworkPolicy.
func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, t *tunnelsv1alpha1.Tunnel) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: NetworkPolicyName(t)}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := controlled(t, np); err != nil {
		return err
	}
	if t.Spec.NetworkPolicy.Disabled {
		t.Status.NetworkPolicy = tunnelsv1alpha1.NetworkPolicyStatus{}
		if np.ResourceVersion == "" {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, np))
	}
	vpcs, err := r.referencing(ctx, t)
	if err != nil {
		return err
	}
	spec, backends, warnings, err := r.networkPolicySpec(ctx, t, vpcs)
	if err != nil {
		return err
	}
	hash := specHash(spec)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = mergeLabels(np.Labels, objectLabels(t))
		if np.Annotations[AnnotationSpecHash] != hash || drifted(spec, np.Spec) {
			if np.Annotations == nil {
				np.Annotations = map[string]string{}
			}
			np.Annotations[AnnotationSpecHash] = hash
			np.Spec = spec
		}
		return controllerutil.SetControllerReference(t, np, r.Scheme())
	}); err != nil {
		return err
	}
	t.Status.NetworkPolicy = tunnelsv1alpha1.NetworkPolicyStatus{Name: np.Name, Backends: backends, Warnings: warnings}
	return nil
}

// finalize runs the deletion flow. gone reports that the finalizer was removed.
func (r *Reconciler) finalize(ctx context.Context, t *tunnelsv1alpha1.Tunnel) (ctrl.Result, bool, error) {
	if !controllerutil.ContainsFinalizer(t, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, true, nil
	}
	refs, err := r.referencing(ctx, t)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, vs := range refs {
			names = append(names, vs.Name)
		}
		sort.Strings(names)
		reconcile.SetReady(t, metav1.ConditionFalse, commonv1alpha1.ReasonDependency,
			fmt.Sprintf("waiting for %d VPCService(s) that reference this Tunnel to be deleted: %s", len(names), strings.Join(names, ", ")))
		return ctrl.Result{RequeueAfter: DependencyRetry}, false, nil
	}
	var del func(context.Context, string) error
	switch id := reconcile.ExternalID(t); {
	case id == "":
	case reconcile.ShouldDeleteExternal(t, commonv1alpha1.DeletionDelete):
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, t, "Delete",
			fmt.Sprintf("Tunnel %s was left in Cloudflare despite deletionPolicy Delete", id))
		if err != nil {
			res, err := reconcile.DeletionResult(t, err)
			return res, false, err
		}
		if acct != nil { // nil: the account is gone, the tunnel is kept
			del = func(ctx context.Context, id string) error {
				return r.deleteExternal(ctx, t, acct.Client, acct.AccountID, id)
			}
		}
	case reconcile.PoliciesOf(t).CanWrite():
		// Orphan: release the ownership tag so the tunnel can be adopted elsewhere (best effort).
		if acct, err := r.Accounts.Resolve(ctx, t); err == nil {
			if err := r.tagger().RemoveOwner(ctx, acct.Client, acct.AccountID, reconcile.TagTarget{Type: tagResourceType, ID: id}, r.owner(t)); err != nil {
				log.FromContext(ctx).Info("could not remove the ownership tag of an orphaned tunnel", "tunnel", id, "error", err.Error())
			}
		}
	}
	res, err := reconcile.Finalize(ctx, r.Client, t, commonv1alpha1.DeletionDelete, del)
	return res, !controllerutil.ContainsFinalizer(t, commonv1alpha1.Finalizer), err
}

func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// warn records a Warning Event on t (when a Recorder is set).
func (r *Reconciler) warn(t *tunnelsv1alpha1.Tunnel, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(t, nil, corev1.EventTypeWarning, reason, "Delete", format, args...)
	}
}

// deleteExternal is the deleteExternal step of reconcile.Finalize for tunnel id. It deletes the
// tunnel only when t provably owns it (reconcile.MayDeleteExternal): it scales cloudflared to
// zero, waits for the pods and the tunnel's connections to go away (each wait is a
// *reconcile.WaitError: requeue after DrainInterval, no error), then deletes the tunnel. A
// tunnel whose ownership is not proven, or that another owner's tag names, is kept with a
// Warning Event and nil is returned (the finalizer is removed).
func (r *Reconciler) deleteExternal(ctx context.Context, t *tunnelsv1alpha1.Tunnel, cf cfclient.Client, accountID, id string) error {
	wait := func(msg string) error { return &reconcile.WaitError{After: r.drain(), Reason: msg} }
	tun, err := getTunnel(ctx, cf, accountID, id)
	if err != nil {
		return err
	}
	if tun == nil { // 404 or already soft-deleted (0101)
		return nil
	}
	t.Status.AtProvider = tun.observation()
	// sync refuses to manage a (pinned) tunnel tagged for another owner; deleting it would
	// break that owner, and waiting for its connectors to drain would never end. Such a
	// tunnel, like one whose ownership is not proven, is released as with Orphan. The tunnel
	// was just read, so no existence check is needed.
	dec, err := reconcile.MayDeleteExternal(ctx, r.tagger(), cf, accountID, reconcile.TagTarget{Type: tagResourceType, ID: id}, r.owner(t), t, id, nil)
	switch {
	case err != nil:
		return fmt.Errorf("cannot verify the tunnel's ownership tag: %w", err)
	case dec.Foreign != "":
		log.FromContext(ctx).Info("not deleting a tunnel owned by someone else", "tunnel", id, "owner", dec.Foreign)
		r.warn(t, EventReasonTunnelKept,
			"deletionPolicy is Delete, but tunnel %s is owned by %s; it was kept in Cloudflare (released as with Orphan)", id, dec.Foreign)
		return nil
	case !dec.Delete:
		log.FromContext(ctx).Info("not deleting a tunnel whose ownership is not proven: "+dec.Why, "tunnel", id)
		reconcile.WarnExternalKept(r.Recorder, t, "Delete", fmt.Sprintf("Tunnel %s was left in Cloudflare despite deletionPolicy Delete: %s", id, dec.Why))
		return nil
	}

	var dep appsv1.Deployment
	err = r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: DeploymentName(t)}, &dep)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case metav1.IsControlledBy(&dep, t):
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
			base := dep.DeepCopy()
			zero := int32(0)
			dep.Spec.Replicas = &zero
			if err := r.Patch(ctx, &dep, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			return wait("scaling cloudflared to zero before deleting the tunnel")
		}
		if dep.Status.ObservedGeneration < dep.Generation || dep.Status.Replicas > 0 {
			return wait(fmt.Sprintf("waiting for cloudflared to scale down (%d pods left)", dep.Status.Replicas))
		}
	}
	if tun.connected() {
		return wait(fmt.Sprintf("waiting for the tunnel's connections to drain (status %s)", tun.Status))
	}
	if err := deleteTunnel(ctx, cf, accountID, id); err != nil {
		if cfclient.HasCode(err, codeActiveConnections) { // 0095
			return wait("the tunnel still has active connections (1022); waiting for them to close")
		}
		return err // a 404 counts as deleted (reconcile.Finalize)
	}
	return nil
}
