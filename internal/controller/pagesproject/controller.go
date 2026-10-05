// Package pagesproject implements the PagesProject controller: a Cloudflare Pages project with
// its build config, deployment configs (environment variables and bindings, which may
// reference KVNamespaces, D1Databases, R2Buckets, Queues and WorkerScripts by name) and an optional Git
// source passed through. Deployments are PagesDeployments (internal/controller/pagesdeployment).
//
// Behavior (API calls in api.go):
//
//   - The project name is the external ID: the external-id annotation, else forProvider.name,
//     else metadata.name.
//   - Observe: GET …/pages/projects/{name} (404/8000007 = missing). A reconcile of an in-sync
//     object makes no Cloudflare write.
//   - Create: POST …/pages/projects with the whole forProvider. Update: one PATCH when the
//     settings hash (status.settingsHash), the secret values (status.writeOnlyHash) or what
//     GET reports differs; a key of a managed map (env_vars and the bindings) that Cloudflare
//     has and forProvider lacks is sent as null, which removes it.
//   - References resolve to Cloudflare IDs (queues by queue name, services by script name). A
//     referenced object that is missing, deleting, on another account or not Ready, or a Secret
//     that is missing or not opted in: Synced=False, reason DependencyNotReady, nothing written.
//   - Ownership: a project this object did not create is managed only when the external-id
//     annotation pins it, its flare.dev/owner tag (resource_type pages_project, resource_id the
//     project's UUID; UNVERIFIED) names this object, or the create-pending record shows it is
//     this object's own lost create; otherwise Synced=False, reason NameConflict.
//   - Deletion (default Delete) waits for this namespace's PagesDeployments of the project
//     (DependencyNotReady), then deletes the project when ownership is proven
//     (reconcile.MayDeleteExternal). Orphan releases the owner tag. This package registers
//     PagesProjects as referrers of KVNamespace, D1Database, R2Bucket, Queue and WorkerScript, which wait
//     for the bindings to go away before their own Cloudflare delete.
package pagesproject

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	d1v1alpha1 "github.com/chenhunghan/flare-operator/api/d1/v1alpha1"
	kvv1alpha1 "github.com/chenhunghan/flare-operator/api/kv/v1alpha1"
	pagesv1alpha1 "github.com/chenhunghan/flare-operator/api/pages/v1alpha1"
	queuesv1alpha1 "github.com/chenhunghan/flare-operator/api/queues/v1alpha1"
	r2v1alpha1 "github.com/chenhunghan/flare-operator/api/r2/v1alpha1"
	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/controller"
	"github.com/chenhunghan/flare-operator/internal/generic"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "pagesproject"

// Defaults of the Reconciler's intervals.
const (
	DefaultResyncInterval  = 10 * time.Minute
	DefaultDependencyRetry = 30 * time.Second
)

// TagResourceType is the Resource Tagging resource_type of Pages projects (pinned spec enum
// resource-tagging_account_resource_type). That its resource_id is the project's UUID is
// UNVERIFIED.
const TagResourceType = "pages_project"

// PagesProjectKind is the group kind of PagesProject (the referrers registry key).
var PagesProjectKind = schema.GroupKind{Group: pagesv1alpha1.GroupVersion.Group, Kind: "PagesProject"}

// nameRe is the pinned spec's pages_project_name pattern with the CRD's 58-character limit.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,57}$`)

// indexRefs indexes PagesProjects by the objects they reference ("<kind>/<name>").
const indexRefs = "pagesproject.refs"

func init() {
	controller.Register(controller.Registration{
		Name:        Name,
		AddToScheme: AddToScheme,
		Setup: func(mgr ctrl.Manager, d controller.Deps) error {
			return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Tagger: d.Tagger, ClusterName: d.ClusterName,
				Recorder: mgr.GetEventRecorder(Name), APIReader: mgr.GetAPIReader(), ResyncInterval: d.PollInterval}).SetupWithManager(mgr)
		},
	})
}

// AddToScheme registers the API groups this controller reads.
func AddToScheme(s *runtime.Scheme) error {
	for _, add := range []func(*runtime.Scheme) error{pagesv1alpha1.AddToScheme, kvv1alpha1.AddToScheme, queuesv1alpha1.AddToScheme,
		d1v1alpha1.AddToScheme, workersv1alpha1.AddToScheme, r2v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			return err
		}
	}
	return nil
}

// Reconciler reconciles PagesProject objects.
type Reconciler struct {
	client.Client
	Accounts *reconcile.Accounts
	// Tagger maintains the ownership tag (nil or reconcile.NoopTagger disables it).
	Tagger reconcile.Tagger
	// ClusterName is the <cluster> of the owner tag value.
	ClusterName string
	// Recorder records Events (optional).
	Recorder events.EventRecorder
	// APIReader confirms, uncached, that a CloudflareAccount is gone (default: Client).
	APIReader client.Reader
	// ResyncInterval re-reads the project periodically (default DefaultResyncInterval).
	ResyncInterval time.Duration
	// DependencyRetry re-checks unresolved references and name conflicts (default
	// DefaultDependencyRetry).
	DependencyRetry time.Duration

	// createLocks serializes the existence check and the create per "<account>/<project>".
	createLocks sync.Map
	// applied remembers what each object last applied (see workerscript: a stale cache must
	// not make the next reconcile write again).
	applied sync.Map
}

type appliedState struct {
	uid                     types.UID
	name, settings, secrets string
}

// +kubebuilder:rbac:groups=flare.dev,resources=pagesprojects,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=flare.dev,resources=pagesprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=flare.dev,resources=pagesprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=flare.dev,resources=pagesdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=kvnamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=queues,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=d1databases,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=r2buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=workerscripts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=flare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *Reconciler) lockCreate(key string) func() {
	v, _ := r.createLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (r *Reconciler) resync() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

func (r *Reconciler) depRetry() time.Duration {
	if r.DependencyRetry > 0 {
		return r.DependencyRetry
	}
	return DefaultDependencyRetry
}

func (r *Reconciler) tagger() reconcile.Tagger {
	if r.Tagger == nil {
		return reconcile.NoopTagger{}
	}
	return r.Tagger
}

func (r *Reconciler) owner(pp *pagesv1alpha1.PagesProject) string {
	return reconcile.OwnerValue(r.ClusterName, pp.Namespace, pp.Name)
}

func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Index keys of refKeys.
const (
	keyKV     = "kv/"
	keyQueue  = "queue/"
	keyD1     = "d1/"
	keyScript = "script/"
	keySecret = "secret/"
	keyR2     = "r2/"
)

func configs(pp *pagesv1alpha1.PagesProject) []*pagesv1alpha1.PagesDeploymentConfig {
	fp := pp.Spec.ForProvider
	if fp == nil || fp.DeploymentConfigs == nil {
		return nil
	}
	var out []*pagesv1alpha1.PagesDeploymentConfig
	for _, c := range []*pagesv1alpha1.PagesDeploymentConfig{fp.DeploymentConfigs.Production, fp.DeploymentConfigs.Preview} {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// refKeys is the indexRefs extractor.
func refKeys(o client.Object) []string {
	pp, ok := o.(*pagesv1alpha1.PagesProject)
	if !ok {
		return nil
	}
	var keys []string
	add := func(prefix string, ref *commonv1alpha1.LocalRef) {
		if ref != nil && ref.Name != "" {
			keys = append(keys, prefix+ref.Name)
		}
	}
	for _, c := range configs(pp) {
		for _, b := range c.KVNamespaces {
			add(keyKV, b.KVNamespaceRef)
		}
		for _, b := range c.D1Databases {
			add(keyD1, b.D1DatabaseRef)
		}
		for _, b := range c.R2Buckets {
			add(keyR2, b.R2BucketRef)
		}
		for _, b := range c.QueueProducers {
			add(keyQueue, b.QueueRef)
		}
		for _, b := range c.Services {
			add(keyScript, b.ServiceRef)
		}
		for _, ev := range c.EnvVars {
			if ev.SecretKeyRef != nil {
				keys = append(keys, keySecret+ev.SecretKeyRef.Name)
			}
		}
	}
	return keys
}

// SetupWithManager registers the controller. Changes of referenced objects (a new ID,
// readiness, deletion) and Secrets requeue the PagesProjects that reference them; a
// PagesDeployment that goes away requeues its (deleting) project.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &pagesv1alpha1.PagesProject{}, indexRefs, refKeys); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&pagesv1alpha1.PagesProject{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&kvv1alpha1.KVNamespace{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyKV)), builder.WithPredicates(managedChanged())).
		Watches(&queuesv1alpha1.Queue{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyQueue)), builder.WithPredicates(managedChanged())).
		Watches(&d1v1alpha1.D1Database{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyD1)), builder.WithPredicates(managedChanged())).
		Watches(&r2v1alpha1.R2Bucket{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyR2)), builder.WithPredicates(managedChanged())).
		Watches(&workersv1alpha1.WorkerScript{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyScript)), builder.WithPredicates(managedChanged())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keySecret)), builder.WithPredicates(secretDataChanged()))
	for _, ref := range generic.ReferrersOf(PagesProjectKind) {
		b = b.Watches(ref.Object, generic.ReferrerWatch(ref))
	}
	return b.Complete(r)
}

func managedChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(commonv1alpha1.Managed)
			n, ok2 := e.ObjectNew.(commonv1alpha1.Managed)
			if !ok1 || !ok2 {
				return true
			}
			return o.GetResourceStatus().ID != n.GetResourceStatus().ID || reconcile.IsReady(o) != reconcile.IsReady(n) ||
				o.GetGeneration() != n.GetGeneration() || o.GetDeletionTimestamp().IsZero() != n.GetDeletionTimestamp().IsZero() ||
				o.GetResourceSpec().AccountRef != n.GetResourceSpec().AccountRef
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func secretDataChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*corev1.Secret)
			n, ok2 := e.ObjectNew.(*corev1.Secret)
			return !ok1 || !ok2 || !reflect.DeepEqual(o.Data, n.Data) || !reflect.DeepEqual(o.Labels, n.Labels)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (r *Reconciler) referencing(prefix string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []ctrlreconcile.Request {
		var l pagesv1alpha1.PagesProjectList
		if err := r.List(ctx, &l, client.InNamespace(o.GetNamespace()), client.MatchingFields{indexRefs: prefix + o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "list PagesProjects referencing", "object", prefix+o.GetName())
			return nil
		}
		out := make([]ctrlreconcile.Request, 0, len(l.Items))
		for _, pp := range l.Items {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: pp.Namespace, Name: pp.Name}})
		}
		return out
	}
}

// Reconcile syncs one PagesProject.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pp pagesv1alpha1.PagesProject
	if err := r.Get(ctx, req.NamespacedName, &pp); err != nil {
		if apierrors.IsNotFound(err) {
			r.applied.Delete(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := pp.DeepCopy()
	var (
		res  ctrl.Result
		err  error
		gone bool
	)
	if !pp.DeletionTimestamp.IsZero() {
		res, gone, err = r.finalize(ctx, &pp)
	} else {
		res, err = r.sync(ctx, &pp)
	}
	if gone {
		return ctrl.Result{}, err
	}
	reconcile.SetObservedGeneration(&pp)
	var perr error
	if !equality.Semantic.DeepEqual(base.Status, pp.Status) {
		perr = reconcile.PatchStatus(ctx, r.Client, &pp, base)
	}
	return reconcile.StatusWritten(ctx, res, err, perr)
}

func (r *Reconciler) last(pp *pagesv1alpha1.PagesProject, name string) appliedState {
	if v, ok := r.applied.Load(client.ObjectKeyFromObject(pp)); ok {
		if a := v.(appliedState); a.uid == pp.UID && a.name == name {
			return a
		}
	}
	st := pp.Status
	if st.ID != "" && st.ID != name {
		return appliedState{uid: pp.UID, name: name}
	}
	return appliedState{uid: pp.UID, name: name, settings: st.SettingsHash, secrets: st.WriteOnlyHash}
}

func (r *Reconciler) remember(pp *pagesv1alpha1.PagesProject, a appliedState) {
	a.uid = pp.UID
	pp.Status.SettingsHash, pp.Status.WriteOnlyHash = a.settings, a.secrets
	r.applied.Store(client.ObjectKeyFromObject(pp), a)
}

func (r *Reconciler) forget(pp *pagesv1alpha1.PagesProject) {
	r.applied.Delete(client.ObjectKeyFromObject(pp))
	pp.Status.SettingsHash, pp.Status.WriteOnlyHash = "", ""
}

// observe records a project as read from Cloudflare.
func observe(pp *pagesv1alpha1.PagesProject, p *APIProject) {
	a := pagesv1alpha1.PagesProjectObservation{ID: p.ID, Name: p.Name, Subdomain: p.Subdomain, ProductionBranch: p.ProductionBranch,
		CreatedOn: p.CreatedOn}
	if len(p.Domains) > 0 {
		a.Domains = append([]string(nil), p.Domains...)
	}
	if p.Subdomain != "" {
		a.URL = "https://" + p.Subdomain
	}
	if t, ok := p.Source["type"].(string); ok {
		a.SourceType = t
	}
	if p.LatestDeployment != nil {
		a.LatestDeploymentID = p.LatestDeployment.ID
	}
	if p.CanonicalDeployment != nil {
		a.CanonicalDeploymentID = p.CanonicalDeployment.ID
	}
	names := func(env string) []string {
		evs, _ := p.DeploymentConfigs[env]["env_vars"].(map[string]any)
		var out []string
		for n := range evs {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	a.ProductionEnvVars, a.PreviewEnvVars = names("production"), names("preview")
	pp.Status.AtProvider = a
}

// fail reports a Cloudflare error; a permanent one waits for a spec change or the resync.
func (r *Reconciler) fail(pp *pagesv1alpha1.PagesProject, err error) (ctrl.Result, error) {
	reconcile.MarkSyncError(pp, "", err)
	if reconcile.IsPermanent(err) {
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	if wait, ok := reconcile.Throttled(err); ok {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, err
}

func (r *Reconciler) conflict(pp *pagesv1alpha1.PagesProject, msg string) (ctrl.Result, error) {
	pp.Status.AtProvider, pp.Status.ID = pagesv1alpha1.PagesProjectObservation{}, ""
	reconcile.SetReady(pp, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
	reconcile.SetSynced(pp, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
	return ctrl.Result{RequeueAfter: r.depRetry()}, nil
}

func (r *Reconciler) sync(ctx context.Context, pp *pagesv1alpha1.PagesProject) (ctrl.Result, error) {
	acct, err := r.Accounts.Resolve(ctx, pp)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(pp, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, pp); err != nil {
		return ctrl.Result{}, err
	}
	pol := reconcile.PoliciesOf(pp)
	cf, accountID := acct.Client, acct.AccountID
	name := pp.ProjectName()
	if !nameRe.MatchString(name) {
		msg := fmt.Sprintf("%q is not a valid Pages project name (lower-case letters, digits and -, starting with a letter or digit, at most 58; set forProvider.name)", name)
		reconcile.SetReady(pp, metav1.ConditionFalse, ReasonInvalidSpec, msg)
		reconcile.SetSynced(pp, metav1.ConditionFalse, ReasonInvalidSpec, msg)
		return ctrl.Result{}, nil
	}
	cur, err := GetProject(ctx, cf, accountID, name)
	if err != nil {
		return r.fail(pp, err)
	}
	if cur == nil {
		r.forget(pp)
	}
	fp := pp.Spec.ForProvider
	if fp == nil || (!pol.CanCreate() && !pol.CanUpdate()) {
		return r.observeOnly(ctx, pp, cf, accountID, name, cur)
	}
	des, prob, err := r.resolve(ctx, pp, name)
	if err != nil {
		return r.fail(pp, err)
	}
	if prob != nil {
		if cur != nil {
			observe(pp, cur)
		}
		reconcile.SetReady(pp, metav1.ConditionFalse, prob.reason, prob.msg)
		reconcile.SetSynced(pp, metav1.ConditionFalse, prob.reason, prob.msg)
		if prob.reason == commonv1alpha1.ReasonDependency {
			return ctrl.Result{RequeueAfter: r.depRetry()}, nil
		}
		return ctrl.Result{}, nil
	}
	if cur == nil && pol.CanCreate() {
		// Creates of one project name are serialized across this manager's workers, and
		// existence is checked again under the lock: of two objects with the same name
		// reconciled at once, the second finds the first's project (NameConflict).
		unlock := r.lockCreate(accountID + "/" + name)
		defer unlock()
		if cur, err = GetProject(ctx, cf, accountID, name); err != nil {
			return r.fail(pp, err)
		}
	}
	pending := ""
	if cur == nil {
		if !pol.CanCreate() {
			pp.Status.AtProvider, pp.Status.ID = pagesv1alpha1.PagesProjectObservation{}, ""
			msg := fmt.Sprintf("Pages project %q does not exist and managementPolicies do not allow Create", name)
			reconcile.MarkNotFound(pp, msg)
			reconcile.SetSynced(pp, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
			return ctrl.Result{RequeueAfter: r.resync()}, nil
		}
		reconcile.MarkCreating(pp, "")
		// Announce the create first, so a crash before RecordCreated neither turns the
		// object's own project into a NameConflict nor loses what was applied.
		// MarkCreatePending writes the record only when it is not there: read it uncached
		// first, as the cache may still show one that a refused create dropped
		// (reconcile.FreshCreatePending).
		if _, err := reconcile.FreshCreatePending(ctx, r.apiReader(), pp); err != nil {
			return r.fail(pp, err)
		}
		if err := reconcile.MarkCreatePending(ctx, r.Client, pp, pendingKey(name, des)); err != nil {
			return r.fail(pp, fmt.Errorf("record the pending create of %s: %w", name, err))
		}
		created, err := createProject(ctx, cf, accountID, des.body)
		if err != nil {
			if reconcile.IsPermanent(err) {
				// The API refused the create: nothing was made, so nothing may be adopted later.
				if cerr := reconcile.ClearCreatePending(ctx, r.Client, pp); cerr != nil {
					return r.fail(pp, errors.Join(err, cerr))
				}
			}
			return r.fail(pp, fmt.Errorf("create: %w", err))
		}
		log.FromContext(ctx).Info("created Pages project", "project", name, "id", created.ID)
		observe(pp, created)
		r.remember(pp, appliedState{name: name, settings: des.settingsHash, secrets: des.secretsHash})
		// Tag first, then record: either proves ownership on the next reconcile if the other fails.
		tagErr := r.tagger().EnsureOwner(ctx, cf, accountID, reconcile.TagTarget{Type: TagResourceType, ID: created.ID}, r.owner(pp))
		var oc *reconcile.OwnershipConflictError
		if errors.As(tagErr, &oc) {
			if err := reconcile.ClearCreatePending(ctx, r.Client, pp); err != nil {
				return r.fail(pp, err)
			}
			r.forget(pp)
			return r.conflict(pp, fmt.Sprintf("the Pages project %q was created concurrently by another object: %v; choose another forProvider.name", name, tagErr))
		}
		if err := reconcile.RecordCreated(ctx, r.Client, pp, name); err != nil {
			return r.fail(pp, fmt.Errorf("record the new Pages project %s: %w", name, err))
		}
		if tagErr != nil {
			return r.fail(pp, fmt.Errorf("ownership tag: %w", tagErr))
		}
	} else {
		if pol.CanCreate() && !reconcile.HasOwnershipProof(pp, name) && pp.GetAnnotations()[commonv1alpha1.AnnotationExternalID] != name {
			// Nothing in the cached copy proves that pp manages the project: the create-pending
			// record may (its own interrupted create), and so may a RecordCreated the cache does
			// not show yet. Both are read uncached, as a record that a refused create dropped
			// would make pp adopt, and under deletionPolicy Delete delete, someone else's project.
			if _, err := reconcile.FreshCreatePending(ctx, r.apiReader(), pp); err != nil {
				return r.fail(pp, err)
			}
		}
		lost, isLost := pendingCreate(pp, name)
		isLost = isLost && pol.CanCreate() && !reconcile.HasOwnershipProof(pp, name)
		msg, err := r.claim(ctx, pp, cf, accountID, name, cur.ID, pol, isLost)
		if err != nil {
			return r.fail(pp, err)
		}
		if msg != "" {
			return r.conflict(pp, msg)
		}
		if isLost {
			// This object's interrupted create: what it applied is what the record says.
			log.FromContext(ctx).Info("adopted the Pages project of an interrupted create", "project", name)
			r.remember(pp, lost)
		}
		pp.Status.ID = name
		prev := r.last(pp, name)
		// The status may not show what was applied yet (its write conflicted).
		pp.Status.SettingsHash, pp.Status.WriteOnlyHash = prev.settings, prev.secrets
		dr := drift(des, cur)
		need := des.settingsHash != prev.settings || des.secretsHash != prev.secrets || len(dr) > 0
		switch {
		case need && !pol.CanUpdate():
			pending = "the Pages project differs from forProvider and managementPolicies do not allow Update"
			observe(pp, cur)
		case need:
			log.FromContext(ctx).Info("updating Pages project", "project", name, "drift", dr)
			updated, err := patchProject(ctx, cf, accountID, name, patchBody(des, cur))
			if err != nil {
				return r.fail(pp, fmt.Errorf("update: %w", err))
			}
			observe(pp, updated)
			r.remember(pp, appliedState{name: name, settings: des.settingsHash, secrets: des.secretsHash})
		default:
			observe(pp, cur)
		}
	}
	pp.Status.ID = name
	reconcile.MarkAvailable(pp)
	if pending != "" {
		reconcile.SetSynced(pp, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError, pending)
	} else {
		reconcile.MarkSynced(pp)
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// claim decides whether pp may manage the existing project name (UUID id), and records that
// it does. A non-empty message is a NameConflict: nothing is written. lost says that pp's
// create-pending record names this project: it is pp's own interrupted create, adopted unless a
// readable owner tag names another object.
func (r *Reconciler) claim(ctx context.Context, pp *pagesv1alpha1.PagesProject, cf cfclient.Client, accountID, name, id string,
	pol reconcile.Policies, lost bool) (string, error) {
	tagging := reconcile.TaggingEnabled(r.tagger()) && id != ""
	target := reconcile.TagTarget{Type: TagResourceType, ID: id}
	proven := reconcile.HasOwnershipProof(pp, name)
	pinned := pp.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == name
	conflict := func(why string) string {
		return fmt.Sprintf("a Pages project named %q already exists and %s; set the %s annotation to %q to adopt it "+
			"(its settings are then replaced by forProvider), or choose another forProvider.name", name, why, commonv1alpha1.AnnotationExternalID, name)
	}
	if !proven && !pinned && lost {
		if tagging {
			o, _, err := r.tagger().Owner(ctx, cf, accountID, target)
			switch {
			case err != nil && !reconcile.IsPermanent(err):
				return "", fmt.Errorf("read ownership tag: %w", err)
			case err == nil && o != "" && o != r.owner(pp):
				return conflict(fmt.Sprintf("it is owned by %q (tag %s)", o, reconcile.OwnerTagKey)), nil
			}
		}
		if err := reconcile.RecordCreated(ctx, r.Client, pp, name); err != nil {
			return "", fmt.Errorf("record the Pages project %s created before a restart: %w", name, err)
		}
		proven = true
	}
	if !proven && !pinned {
		if !tagging {
			return conflict("this object did not create it"), nil
		}
		o, _, err := r.tagger().Owner(ctx, cf, accountID, target)
		switch {
		case err != nil && !reconcile.IsPermanent(err):
			return "", fmt.Errorf("read ownership tag: %w", err)
		case err != nil:
			return conflict(fmt.Sprintf("its %s tag cannot be read (%v)", reconcile.OwnerTagKey, err)), nil
		case o == "":
			return conflict("it has no " + reconcile.OwnerTagKey + " tag naming this object"), nil
		case o != r.owner(pp):
			return conflict(fmt.Sprintf("it is owned by %q (tag %s)", o, reconcile.OwnerTagKey)), nil
		}
	}
	if !pol.CanWrite() || !tagging {
		return "", nil
	}
	if err := r.tagger().EnsureOwner(ctx, cf, accountID, target, r.owner(pp)); err != nil {
		var oc *reconcile.OwnershipConflictError
		if errors.As(err, &oc) {
			return fmt.Sprintf("%v; if that object no longer manages it, remove the %s tag from the project (Resource Tagging API) to allow adoption",
				err, reconcile.OwnerTagKey), nil
		}
		return "", fmt.Errorf("ownership tag: %w", err)
	}
	if err := reconcile.RecordOwnership(ctx, r.Client, pp, name); err != nil {
		return "", fmt.Errorf("record ownership of Pages project %s: %w", name, err)
	}
	return "", nil
}

// observeOnly reads the project without writing. With Delete allowed, ownership is still
// claimed (tag and record) when this object can prove it, so that deletion may delete.
func (r *Reconciler) observeOnly(ctx context.Context, pp *pagesv1alpha1.PagesProject, cf cfclient.Client, accountID, name string, cur *APIProject) (ctrl.Result, error) {
	pol := reconcile.PoliciesOf(pp)
	if cur == nil {
		pp.Status.AtProvider, pp.Status.ID = pagesv1alpha1.PagesProjectObservation{}, ""
		msg := fmt.Sprintf("Pages project %q does not exist", name)
		if pp.Spec.ForProvider != nil {
			msg += " and managementPolicies do not allow Create"
		}
		reconcile.MarkNotFound(pp, msg)
		reconcile.MarkSynced(pp)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	conflict := ""
	if pol.CanWrite() {
		var err error
		if conflict, err = r.claim(ctx, pp, cf, accountID, name, cur.ID, pol, false); err != nil {
			return r.fail(pp, err)
		}
	}
	observe(pp, cur)
	pp.Status.ID = name
	reconcile.MarkAvailable(pp)
	if conflict != "" {
		reconcile.SetSynced(pp, metav1.ConditionFalse, reconcile.ReasonNameConflict, conflict)
	} else {
		reconcile.MarkSynced(pp)
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// finalize runs the deletion flow (package doc). gone reports that the finalizer was removed.
func (r *Reconciler) finalize(ctx context.Context, pp *pagesv1alpha1.PagesProject) (ctrl.Result, bool, error) {
	if !controllerutil.ContainsFinalizer(pp, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, true, nil
	}
	logger := log.FromContext(ctx)
	deleteExternal := reconcile.ShouldDeleteExternal(pp, commonv1alpha1.DeletionDelete)
	if pendingName(pp) != "" && deleteExternal {
		// The record is confirmed uncached first: one that the cache still shows after a
		// refused create dropped it would delete a project someone else made.
		if _, err := reconcile.FreshCreatePending(ctx, r.apiReader(), pp); err != nil {
			res, err := reconcile.DeletionResult(pp, err)
			return res, false, err
		}
	}
	if p := pendingName(pp); p != "" && deleteExternal {
		// A create was announced and its result never recorded: find the project the record
		// names and record it, so it is deleted rather than leaked.
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, pp, "Delete",
			fmt.Sprintf("the Pages project %s this object may have created before a restart was not looked up and may be left in Cloudflare", p))
		if err == nil && acct != nil {
			_, err = reconcile.AdoptPendingCreate(ctx, r.Client, r.Recorder, pp, "Pages project", func(ctx context.Context, _ string) (string, error) {
				cur, err := GetProject(ctx, acct.Client, acct.AccountID, p)
				if cur == nil || err != nil {
					return "", err
				}
				return p, nil
			})
		}
		if err != nil {
			res, err := reconcile.DeletionResult(pp, err)
			return res, false, err
		}
	}
	name := reconcile.ExternalID(pp)
	var del func(context.Context, string) error
	switch {
	case name == "":
	case deleteExternal:
		// Deleting the project deletes its deployments: wait until this namespace's
		// PagesDeployments of it are gone (their finalizers delete what they made).
		refs, err := generic.BlockingReferrers(ctx, r.Client, PagesProjectKind, pp)
		if err != nil {
			res, err := reconcile.DeletionResult(pp, err)
			return res, false, err
		}
		if len(refs) > 0 {
			reconcile.SetReady(pp, metav1.ConditionFalse, commonv1alpha1.ReasonDependency, generic.ReferrerWaitMessage(refs))
			return ctrl.Result{RequeueAfter: generic.ReferrerRetry}, false, nil
		}
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, pp, "Delete",
			fmt.Sprintf("Pages project %s was left in Cloudflare despite deletionPolicy Delete", name))
		if err != nil {
			res, err := reconcile.DeletionResult(pp, err)
			return res, false, err
		}
		if acct == nil {
			break
		}
		tagger, target, err := r.deleteTarget(ctx, pp, acct.Client, acct.AccountID, name)
		if err != nil {
			res, err := reconcile.DeletionResult(pp, err)
			return res, false, err
		}
		exists := func(ctx context.Context) (bool, error) {
			cur, err := GetProject(ctx, acct.Client, acct.AccountID, name)
			return cur != nil, err
		}
		dec, err := reconcile.MayDeleteExternal(ctx, tagger, acct.Client, acct.AccountID, target, r.owner(pp), pp, name, exists)
		if err != nil {
			res, err := reconcile.DeletionResult(pp, fmt.Errorf("read ownership tag: %w", err))
			return res, false, err
		}
		switch {
		case dec.Gone:
			logger.Info("the Pages project is already gone", "project", name)
		case !dec.Delete:
			logger.Info("not deleting a Pages project whose ownership is not proven: "+dec.Why, "project", name)
			reconcile.WarnExternalKept(r.Recorder, pp, "Delete",
				fmt.Sprintf("Pages project %s was left in Cloudflare despite deletionPolicy Delete: %s", name, dec.Why))
		default:
			del = func(ctx context.Context, id string) error {
				err := deleteProject(ctx, acct.Client, acct.AccountID, id)
				if err == nil {
					log.FromContext(ctx).Info("deleted Pages project", "project", id)
				}
				return err
			}
		}
	case reconcile.PoliciesOf(pp).CanWrite() && reconcile.TaggingEnabled(r.tagger()) && pp.Status.AtProvider.ID != "":
		// Orphan: release the ownership tag so the project can be adopted elsewhere (best effort).
		if acct, err := r.Accounts.Resolve(ctx, pp); err == nil {
			if err := r.tagger().RemoveOwner(ctx, acct.Client, acct.AccountID,
				reconcile.TagTarget{Type: TagResourceType, ID: pp.Status.AtProvider.ID}, r.owner(pp)); err != nil {
				logger.Info("could not remove the ownership tag of an orphaned Pages project", "project", name, "error", err.Error())
			}
		}
	}
	reconcile.MarkDeleting(pp, "")
	res, err := reconcile.Finalize(ctx, r.Client, pp, commonv1alpha1.DeletionDelete, del)
	gone := !controllerutil.ContainsFinalizer(pp, commonv1alpha1.Finalizer)
	if gone {
		r.applied.Delete(client.ObjectKeyFromObject(pp))
	}
	return res, gone, err
}

// deleteTarget returns the tagger and tag target for the deletion decision. The project's UUID
// comes from status, else from GET; when it cannot be found no tag can be read and ownership
// rests on the record or the pin.
func (r *Reconciler) deleteTarget(ctx context.Context, pp *pagesv1alpha1.PagesProject, cf cfclient.Client, accountID, name string) (reconcile.Tagger, reconcile.TagTarget, error) {
	tagger := r.tagger()
	id := ""
	if pp.Status.AtProvider.Name == name {
		id = pp.Status.AtProvider.ID
	}
	if id == "" && reconcile.TaggingEnabled(tagger) {
		cur, err := GetProject(ctx, cf, accountID, name)
		if err != nil {
			return nil, reconcile.TagTarget{}, err
		}
		if cur != nil {
			id = cur.ID
		}
	}
	if id == "" {
		tagger = reconcile.NoopTagger{}
	}
	return tagger, reconcile.TagTarget{Type: TagResourceType, ID: id}, nil
}

// pendingKey is the create-pending key of a create of d as project name.
func pendingKey(name string, d *desired) string {
	return fmt.Sprintf("%s;s=%s;w=%s", name, d.settingsHash, d.secretsHash)
}

// pendingName is the project name of pp's create-pending record ("" without one).
func pendingName(pp *pagesv1alpha1.PagesProject) string {
	key, ok := reconcile.PendingCreate(pp)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(key, ";")
	return name
}

// pendingCreate returns what pp's create-pending record says it applied to name.
func pendingCreate(pp *pagesv1alpha1.PagesProject, name string) (appliedState, bool) {
	key, ok := reconcile.PendingCreate(pp)
	if !ok {
		return appliedState{}, false
	}
	parts := strings.Split(key, ";")
	if len(parts) != 3 || parts[0] != name {
		return appliedState{}, false
	}
	a := appliedState{name: name}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "s":
			a.settings = v
		case "w":
			a.secrets = v
		default:
			return appliedState{}, false
		}
	}
	return a, a.settings != ""
}
