// Package workerscript implements the WorkerScript controller: a Cloudflare Workers script
// uploaded from inline modules or a ConfigMap, with bindings that may reference KVNamespaces,
// Queues, D1Databases, VPCServices and other WorkerScripts by name. It completes the
// private-backend flow Worker → vpc_service binding → VPCService → Tunnel → Service
// (docs/spike-results-2026-09-29.md §2).
//
// Behavior (recordings cited in api.go):
//
//   - The script name is the external ID: the external-id annotation, else
//     forProvider.script_name, else metadata.name.
//   - Observe: GET …/settings (404/10007 = missing), GET …/deployments (the version the active
//     deployment serves), GET …/subdomain when forProvider.workersDev is set, and the script
//     list only when the script's tag is not known yet. A reconcile of an in-sync object makes
//     no Cloudflare write.
//   - Upload (multipart PUT: a "metadata" part and one part per module; every upload creates a
//     version deployed at 100%) when the script is missing, when the hash of main_module and
//     the modules differs from status.contentHash (Cloudflare does not return the content; the
//     content GET is not emulated), or when the active deployment serves a version this object
//     did not deploy (someone else uploaded). Settings-only changes (compatibility date/flags,
//     bindings, observability, logpush, secret values) are a multipart PATCH …/settings instead:
//     a change of forProvider (status.settingsHash, status.writeOnlyHash for secret values) or
//     a difference GET …/settings shows.
//   - References resolve to Cloudflare IDs (queues bind by queue name, services by script
//     name). A referenced object that is missing, deleting, on another account or not Ready,
//     a missing Secret or ConfigMap: Synced=False, reason DependencyNotReady, nothing uploaded.
//     Changes of referenced objects, Secrets and ConfigMaps requeue the WorkerScript.
//   - Ownership: a script this object did not create is managed only when the external-id
//     annotation pins it or its flare.dev/owner tag (Resource Tagging resource_type worker,
//     resource_id = the script tag; UNVERIFIED) names this object; otherwise Synced=False,
//     reason NameConflict, and nothing is written (an upload would replace someone's code).
//     A create writes the ownership record (reconcile.RecordCreated) and the owner tag.
//   - Deletion (default policy Delete: a script is code, not data) deletes the script when
//     ownership is proven (reconcile.MayDeleteExternal) and no other WorkerScript still binds
//     it through serviceRef (DependencyNotReady until then). Orphan releases the owner tag.
//     This package also registers WorkerScripts as referrers of KVNamespace, Queue,
//     D1Database and VPCService (referrers.go), so those wait for the bindings to go away
//     before their own Cloudflare delete.
package workerscript

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	d1v1alpha1 "flare.dev/operator/api/d1/v1alpha1"
	kvv1alpha1 "flare.dev/operator/api/kv/v1alpha1"
	queuesv1alpha1 "flare.dev/operator/api/queues/v1alpha1"
	workersv1alpha1 "flare.dev/operator/api/workers/v1alpha1"
	workersvpcv1alpha1 "flare.dev/operator/api/workersvpc/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "workerscript"

// Defaults of the Reconciler's intervals.
const (
	DefaultResyncInterval  = 10 * time.Minute
	DefaultDependencyRetry = 30 * time.Second
)

// TagResourceType is the Resource Tagging resource_type of Workers scripts (pinned spec enum
// resource-tagging_account_resource_type). That its resource_id is the script tag (not the
// name) is UNVERIFIED.
const TagResourceType = "worker"

// Condition reasons of this controller.
const (
	ReasonNameConflict      = reconcile.ReasonNameConflict
	ReasonInvalidScriptName = "InvalidScriptName"
)

// scriptNameRe is the pinned spec's workers_script_name pattern.
var scriptNameRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9-_]*$`)

// indexRefs indexes WorkerScripts by the objects they reference ("<kind>/<name>").
const indexRefs = "workerscript.refs"

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
	for _, add := range []func(*runtime.Scheme) error{workersv1alpha1.AddToScheme, kvv1alpha1.AddToScheme,
		queuesv1alpha1.AddToScheme, d1v1alpha1.AddToScheme, workersvpcv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			return err
		}
	}
	return nil
}

// Reconciler reconciles WorkerScript objects.
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
	// ResyncInterval re-reads the script periodically (default DefaultResyncInterval).
	ResyncInterval time.Duration
	// DependencyRetry re-checks unresolved references and name conflicts (default
	// DefaultDependencyRetry).
	DependencyRetry time.Duration

	// createLocks serializes the existence check and first upload per "<account>/<script>"
	// (a *sync.Mutex per key): the upload is an upsert, so two workers must not both create.
	createLocks sync.Map

	// applied remembers what was last uploaded per object: the next reconcile may read the object
	// from a cache that does not have the status patch yet and must not upload again because of
	// that. Entries are dropped when the object is finalized or the script is found missing.
	applied sync.Map
}

// appliedState is what an object last applied to script name.
type appliedState struct {
	uid                                 types.UID
	name, content, settings, secrets, v string
}

// +kubebuilder:rbac:groups=workers.cloudflare.flare.dev,resources=workerscripts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workers.cloudflare.flare.dev,resources=workerscripts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workers.cloudflare.flare.dev,resources=workerscripts/finalizers,verbs=update
// +kubebuilder:rbac:groups=kv.cloudflare.flare.dev,resources=kvnamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=queues.cloudflare.flare.dev,resources=queues,verbs=get;list;watch
// +kubebuilder:rbac:groups=d1.cloudflare.flare.dev,resources=d1databases,verbs=get;list;watch
// +kubebuilder:rbac:groups=workersvpc.cloudflare.flare.dev,resources=vpcservices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// lockCreate locks the create of script key ("<account>/<script>") and returns the unlock.
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

func (r *Reconciler) owner(ws *workersv1alpha1.WorkerScript) string {
	return reconcile.OwnerValue(r.ClusterName, ws.Namespace, ws.Name)
}

func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Index keys of refKeys.
const (
	keyKV        = "kv/"
	keyQueue     = "queue/"
	keyD1        = "d1/"
	keyVPC       = "vpc/"
	keyScript    = "script/"
	keySecret    = "secret/"
	keyConfigMap = "configmap/"
)

// refKeys is the indexRefs extractor.
func refKeys(o client.Object) []string {
	ws, ok := o.(*workersv1alpha1.WorkerScript)
	if !ok || ws.Spec.ForProvider == nil {
		return nil
	}
	var keys []string
	add := func(prefix string, ref *commonv1alpha1.LocalRef) {
		if ref != nil && ref.Name != "" {
			keys = append(keys, prefix+ref.Name)
		}
	}
	for _, b := range ws.Bindings() {
		add(keyKV, b.KVNamespaceRef)
		add(keyQueue, b.QueueRef)
		add(keyD1, b.D1DatabaseRef)
		add(keyVPC, b.VPCServiceRef)
		add(keyScript, b.ServiceRef)
		if b.SecretKeyRef != nil {
			keys = append(keys, keySecret+b.SecretKeyRef.Name)
		}
	}
	if sr := ws.Spec.ForProvider.SourceRef; sr != nil {
		keys = append(keys, keyConfigMap+sr.Name)
	}
	return keys
}

// SetupWithManager registers the controller. Changes of referenced objects (a new ID, readiness,
// deletion), Secrets and ConfigMaps requeue the WorkerScripts that reference them.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &workersv1alpha1.WorkerScript{}, indexRefs, refKeys); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&workersv1alpha1.WorkerScript{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&kvv1alpha1.KVNamespace{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyKV)), builder.WithPredicates(managedChanged())).
		Watches(&queuesv1alpha1.Queue{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyQueue)), builder.WithPredicates(managedChanged())).
		Watches(&d1v1alpha1.D1Database{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyD1)), builder.WithPredicates(managedChanged())).
		Watches(&workersvpcv1alpha1.VPCService{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyVPC)), builder.WithPredicates(managedChanged())).
		Watches(&workersv1alpha1.WorkerScript{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyScript)), builder.WithPredicates(managedChanged())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keySecret)), builder.WithPredicates(dataChanged())).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyConfigMap)), builder.WithPredicates(dataChanged()))
	// WorkerScripts bound through serviceRef wait for their referrers (referrers.go).
	for _, ref := range generic.ReferrersOf(WorkerScriptKind) {
		b = b.Watches(ref.Object, generic.ReferrerWatch(ref))
	}
	return b.Complete(r)
}

// managedChanged passes the changes of a referenced managed object that can change what a
// WorkerScript binds: its ID, readiness, generation, account or deletion.
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

// dataChanged passes Secret and ConfigMap changes of their data.
func dataChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			switch o := e.ObjectOld.(type) {
			case *corev1.Secret:
				n, ok := e.ObjectNew.(*corev1.Secret)
				return !ok || !reflect.DeepEqual(o.Data, n.Data)
			case *corev1.ConfigMap:
				n, ok := e.ObjectNew.(*corev1.ConfigMap)
				return !ok || !reflect.DeepEqual(o.Data, n.Data) || !reflect.DeepEqual(o.BinaryData, n.BinaryData)
			}
			return true
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// referencing maps an object to the WorkerScripts in its namespace that reference it.
func (r *Reconciler) referencing(prefix string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []ctrlreconcile.Request {
		var l workersv1alpha1.WorkerScriptList
		if err := r.List(ctx, &l, client.InNamespace(o.GetNamespace()), client.MatchingFields{indexRefs: prefix + o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "list WorkerScripts referencing", "object", prefix+o.GetName())
			return nil
		}
		out := make([]ctrlreconcile.Request, 0, len(l.Items))
		for _, ws := range l.Items {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}})
		}
		return out
	}
}

// Reconcile syncs one WorkerScript.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws workersv1alpha1.WorkerScript
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			r.applied.Delete(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := ws.DeepCopy()
	var (
		res  ctrl.Result
		err  error
		gone bool
	)
	if !ws.DeletionTimestamp.IsZero() {
		res, gone, err = r.finalize(ctx, &ws)
	} else {
		res, err = r.sync(ctx, &ws)
	}
	if gone {
		return ctrl.Result{}, err
	}
	reconcile.SetObservedGeneration(&ws)
	if !equality.Semantic.DeepEqual(base.Status, ws.Status) {
		if perr := r.Status().Patch(ctx, &ws, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
			return ctrl.Result{}, errors.Join(err, perr)
		}
	}
	return res, err
}

// last returns what ws last applied to name: this process's record, else the status.
func (r *Reconciler) last(ws *workersv1alpha1.WorkerScript, name string) appliedState {
	if v, ok := r.applied.Load(client.ObjectKeyFromObject(ws)); ok {
		if a := v.(appliedState); a.uid == ws.UID && a.name == name {
			return a
		}
	}
	st := ws.Status
	if st.ID != "" && st.ID != name {
		return appliedState{uid: ws.UID, name: name}
	}
	return appliedState{uid: ws.UID, name: name, content: st.ContentHash, settings: st.SettingsHash, secrets: st.WriteOnlyHash, v: st.AtProvider.VersionID}
}

// remember records a as applied, in status and in memory.
func (r *Reconciler) remember(ws *workersv1alpha1.WorkerScript, a appliedState) {
	a.uid = ws.UID
	ws.Status.ContentHash, ws.Status.SettingsHash, ws.Status.WriteOnlyHash = a.content, a.settings, a.secrets
	ws.Status.AtProvider.VersionID = a.v
	r.applied.Store(client.ObjectKeyFromObject(ws), a)
}

func (r *Reconciler) forget(ws *workersv1alpha1.WorkerScript) {
	r.applied.Delete(client.ObjectKeyFromObject(ws))
	ws.Status.ContentHash, ws.Status.SettingsHash, ws.Status.WriteOnlyHash = "", "", ""
}

// observeScript records an upload result or list item.
func observeScript(ws *workersv1alpha1.WorkerScript, s *apiScript) {
	a := &ws.Status.AtProvider
	a.ID, a.Tag, a.Etag, a.CreatedOn, a.ModifiedOn = s.ID, s.Tag, s.Etag, s.CreatedOn, s.ModifiedOn
	a.Handlers = nil
	if len(s.Handlers) > 0 {
		a.Handlers = append([]string(nil), s.Handlers...)
	}
}

// observeSettings records GET …/settings.
func observeSettings(ws *workersv1alpha1.WorkerScript, name string, s *apiSettings) {
	a := &ws.Status.AtProvider
	a.ID = name
	a.CompatibilityDate, a.UsageModel, a.Logpush = s.CompatibilityDate, s.UsageModel, s.Logpush
	a.CompatibilityFlags = nil
	if len(s.CompatibilityFlags) > 0 {
		a.CompatibilityFlags = append([]string(nil), s.CompatibilityFlags...)
	}
	a.Bindings = nil
	for _, b := range s.Bindings {
		n, _ := b["name"].(string)
		t, _ := b["type"].(string)
		a.Bindings = append(a.Bindings, workersv1alpha1.WorkerBindingObservation{Name: n, Type: t})
	}
	sort.Slice(a.Bindings, func(i, j int) bool { return a.Bindings[i].Name < a.Bindings[j].Name })
}

func observeDeployment(ws *workersv1alpha1.WorkerScript, d *apiDeployment) {
	a := &ws.Status.AtProvider
	a.DeploymentID, a.VersionID = "", ""
	if d != nil {
		a.DeploymentID, a.VersionID = d.ID, d.servedVersion()
	}
}

// fail reports a Cloudflare error. A permanent one (a 4xx such as 400/10180 for an unknown VPC
// service, 0106) waits for a spec change or the resync instead of a rate-limited retry, which
// would repeat the failing write.
func (r *Reconciler) fail(ws *workersv1alpha1.WorkerScript, err error) (ctrl.Result, error) {
	reconcile.MarkSyncError(ws, "", err)
	if reconcile.IsPermanent(err) {
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	if wait, ok := reconcile.Throttled(err); ok {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, err
}

func (r *Reconciler) sync(ctx context.Context, ws *workersv1alpha1.WorkerScript) (ctrl.Result, error) {
	acct, err := r.Accounts.Resolve(ctx, ws)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(ws, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, ws); err != nil {
		return ctrl.Result{}, err
	}
	pol := reconcile.PoliciesOf(ws)
	cf, accountID := acct.Client, acct.AccountID
	name := ws.ScriptName()
	if len(name) > 63 || !scriptNameRe.MatchString(name) {
		msg := fmt.Sprintf("%q is not a valid Worker script name (lower-case letters, digits, - and _, at most 63; set forProvider.script_name)", name)
		reconcile.SetReady(ws, metav1.ConditionFalse, ReasonInvalidScriptName, msg)
		reconcile.SetSynced(ws, metav1.ConditionFalse, ReasonInvalidScriptName, msg)
		return ctrl.Result{}, nil
	}

	cur, err := getSettings(ctx, cf, accountID, name)
	if err != nil {
		return r.fail(ws, err)
	}
	if cur == nil {
		r.forget(ws)
	}
	fp := ws.Spec.ForProvider
	if fp == nil || (!pol.CanCreate() && !pol.CanUpdate()) {
		return r.observeOnly(ctx, ws, cf, accountID, name, cur)
	}

	des, prob, err := r.resolve(ctx, ws)
	if err != nil {
		return r.fail(ws, err)
	}
	if prob != nil {
		if cur != nil {
			observeSettings(ws, name, cur)
		}
		reconcile.SetReady(ws, metav1.ConditionFalse, prob.reason, prob.msg)
		reconcile.SetSynced(ws, metav1.ConditionFalse, prob.reason, prob.msg)
		if prob.reason == commonv1alpha1.ReasonDependency {
			return ctrl.Result{RequeueAfter: r.depRetry()}, nil
		}
		return ctrl.Result{}, nil
	}

	if cur == nil && pol.CanCreate() {
		// The upload is a PUT, which replaces a script someone else made in the meantime. Creates
		// of one script name are serialized across this manager's workers, and existence is
		// checked again under the lock: of two objects with the same script_name reconciled at
		// once, the second finds the first's script and goes through claim (NameConflict)
		// instead of overwriting its code.
		unlock := r.lockCreate(accountID + "/" + name)
		defer unlock()
		if cur, err = getSettings(ctx, cf, accountID, name); err != nil {
			return r.fail(ws, err)
		}
	}
	wrote, pending := false, ""
	if cur == nil {
		if !pol.CanCreate() {
			ws.Status.AtProvider, ws.Status.ID = workersv1alpha1.WorkerScriptObservation{}, ""
			msg := fmt.Sprintf("Worker script %q does not exist and managementPolicies do not allow Create", name)
			reconcile.MarkNotFound(ws, msg)
			reconcile.SetSynced(ws, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
			return ctrl.Result{RequeueAfter: r.resync()}, nil
		}
		reconcile.MarkCreating(ws, "")
		// Announce the upload first (pending.go), so a crash before RecordCreated neither turns
		// the object's own script into a NameConflict nor uploads it twice.
		if err := reconcile.MarkCreatePending(ctx, r.Client, ws, pendingKey(name, des)); err != nil {
			return r.fail(ws, fmt.Errorf("record the pending upload of %s: %w", name, err))
		}
		up, err := uploadScript(ctx, cf, accountID, name, des.metadata, des.modules)
		if err != nil {
			if reconcile.IsPermanent(err) {
				// The API refused the upload: nothing was made, so nothing may be adopted later.
				if cerr := reconcile.ClearCreatePending(ctx, r.Client, ws); cerr != nil {
					return r.fail(ws, errors.Join(err, cerr))
				}
			}
			return r.fail(ws, fmt.Errorf("upload: %w", err))
		}
		log.FromContext(ctx).Info("created Worker script", "script", name, "tag", up.Tag)
		observeScript(ws, up)
		r.remember(ws, appliedState{name: name, content: des.contentHash, settings: des.settingsHash, secrets: des.secretsHash})
		// Tag first, then record: either proves ownership on the next reconcile if the other fails.
		tagErr := r.tagger().EnsureOwner(ctx, cf, accountID, reconcile.TagTarget{Type: TagResourceType, ID: up.Tag}, r.owner(ws))
		var oc *reconcile.OwnershipConflictError
		if errors.As(tagErr, &oc) {
			// Another object (another cluster) tagged the script between the existence check and
			// the upload: it is not this object's, so no ownership is recorded and the
			// create-pending record is dropped (its finalizer must not adopt the script).
			if err := reconcile.ClearCreatePending(ctx, r.Client, ws); err != nil {
				return r.fail(ws, err)
			}
			r.forget(ws)
			msg := fmt.Sprintf("the Worker script %q was created concurrently by another object: %v; choose another forProvider.script_name", name, tagErr)
			ws.Status.AtProvider, ws.Status.ID = workersv1alpha1.WorkerScriptObservation{}, ""
			reconcile.SetReady(ws, metav1.ConditionFalse, ReasonNameConflict, msg)
			reconcile.SetSynced(ws, metav1.ConditionFalse, ReasonNameConflict, msg)
			return ctrl.Result{RequeueAfter: r.depRetry()}, nil
		}
		if err := reconcile.RecordCreated(ctx, r.Client, ws, name); err != nil {
			return r.fail(ws, fmt.Errorf("record the new Worker script %s: %w", name, err))
		}
		if tagErr != nil {
			return r.fail(ws, fmt.Errorf("ownership tag: %w", tagErr))
		}
		wrote = true
	} else {
		if ws.Status.AtProvider.Tag == "" || ws.Status.AtProvider.ID != name {
			item, err := findScript(ctx, cf, accountID, name)
			if err != nil {
				return r.fail(ws, err)
			}
			if item != nil {
				observeScript(ws, item)
			}
		}
		lost, isLost := pendingUpload(ws, name)
		isLost = isLost && pol.CanCreate() && !reconcile.HasOwnershipProof(ws, name)
		msg, err := r.claim(ctx, ws, cf, accountID, name, pol, isLost)
		if err != nil {
			return r.fail(ws, err)
		}
		if msg == "" && isLost {
			// The script is this object's interrupted first upload: what it uploaded is what the
			// create-pending record says, so it is not uploaded again.
			log.FromContext(ctx).Info("adopted the Worker script of an interrupted upload", "script", name)
			r.remember(ws, lost)
		}
		if msg != "" {
			ws.Status.AtProvider, ws.Status.ID = workersv1alpha1.WorkerScriptObservation{}, ""
			reconcile.SetReady(ws, metav1.ConditionFalse, ReasonNameConflict, msg)
			reconcile.SetSynced(ws, metav1.ConditionFalse, ReasonNameConflict, msg)
			return ctrl.Result{RequeueAfter: r.depRetry()}, nil
		}
		ws.Status.ID = name

		dep, err := activeDeployment(ctx, cf, accountID, name)
		if err != nil {
			return r.fail(ws, err)
		}
		prev := r.last(ws, name)
		served := dep.servedVersion()
		codeDrift := prev.v != "" && served != prev.v
		needUpload := des.contentHash != prev.content || codeDrift
		var drift []string
		if !needUpload {
			drift = settingsDrift(des.metadata.apiSettingsBody, cur)
		}
		needSettings := !needUpload && (des.settingsHash != prev.settings || des.secretsHash != prev.secrets || len(drift) > 0)
		switch {
		case (needUpload || needSettings) && !pol.CanUpdate():
			pending = "the Worker script differs from forProvider and managementPolicies do not allow Update"
		case needUpload:
			why := "content changed"
			if codeDrift && des.contentHash == prev.content {
				why = fmt.Sprintf("the active deployment serves version %q, not %q", served, prev.v)
			}
			log.FromContext(ctx).Info("uploading Worker script", "script", name, "why", why)
			up, err := uploadScript(ctx, cf, accountID, name, des.metadata, des.modules)
			if err != nil {
				return r.fail(ws, fmt.Errorf("upload: %w", err))
			}
			observeScript(ws, up)
			r.remember(ws, appliedState{name: name, content: des.contentHash, settings: des.settingsHash, secrets: des.secretsHash})
			wrote = true
		case needSettings:
			log.FromContext(ctx).Info("updating Worker script settings", "script", name, "drift", drift)
			if err := patchSettings(ctx, cf, accountID, name, des.metadata.apiSettingsBody); err != nil {
				return r.fail(ws, fmt.Errorf("settings: %w", err))
			}
			r.remember(ws, appliedState{name: name, content: prev.content, settings: des.settingsHash, secrets: des.secretsHash, v: prev.v})
			wrote = true
		}
		if !wrote {
			observeSettings(ws, name, cur)
			if pending == "" {
				observeDeployment(ws, dep)
			}
		}
	}
	if wrote {
		if err := r.readBack(ctx, ws, cf, accountID, name); err != nil {
			return r.fail(ws, err)
		}
	}
	if fp.WorkersDev != nil {
		p, err := r.syncSubdomain(ctx, ws, cf, accountID, name, fp.WorkersDev, pol.CanUpdate() || (wrote && pol.CanCreate()))
		if err != nil {
			return r.fail(ws, fmt.Errorf("workers.dev route: %w", err))
		}
		if p != "" && pending == "" {
			pending = p
		}
	}
	reconcile.MarkAvailable(ws)
	if pending != "" {
		reconcile.SetSynced(ws, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError, pending)
	} else {
		reconcile.MarkSynced(ws)
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// readBack re-reads the settings and the active deployment after a write and records the
// version this object deployed.
func (r *Reconciler) readBack(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string) error {
	cur, err := getSettings(ctx, cf, accountID, name)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("the Worker script %s is gone right after the write", name)
	}
	dep, err := activeDeployment(ctx, cf, accountID, name)
	if err != nil {
		return err
	}
	observeSettings(ws, name, cur)
	observeDeployment(ws, dep)
	a := r.last(ws, name)
	a.v = ws.Status.AtProvider.VersionID
	r.remember(ws, a)
	ws.Status.ID = name
	return nil
}

// claim decides whether ws may manage the existing script name, and records that it does. A
// non-empty message is a NameConflict: nothing is written. lost says that ws's create-pending
// record names this script (pending.go): it is ws's own interrupted first upload, adopted
// unless a readable owner tag names another object.
func (r *Reconciler) claim(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string,
	pol reconcile.Policies, lost bool) (string, error) {
	tag := ws.Status.AtProvider.Tag
	tagging := reconcile.TaggingEnabled(r.tagger()) && tag != ""
	target := reconcile.TagTarget{Type: TagResourceType, ID: tag}
	proven := reconcile.HasOwnershipProof(ws, name)
	pinned := ws.GetAnnotations()[commonv1alpha1.AnnotationExternalID] == name
	conflict := func(why string) string {
		return fmt.Sprintf("a Worker script named %q already exists and %s; set the %s annotation to %q to adopt it "+
			"(its code and settings are then replaced by forProvider), or choose another forProvider.script_name",
			name, why, commonv1alpha1.AnnotationExternalID, name)
	}
	if !proven && !pinned && lost {
		if tagging {
			o, _, err := r.tagger().Owner(ctx, cf, accountID, target)
			switch {
			case err != nil && !reconcile.IsPermanent(err):
				return "", fmt.Errorf("read ownership tag: %w", err)
			case err == nil && o != "" && o != r.owner(ws):
				return conflict(fmt.Sprintf("it is owned by %q (tag %s)", o, reconcile.OwnerTagKey)), nil
			}
		}
		if err := reconcile.RecordCreated(ctx, r.Client, ws, name); err != nil {
			return "", fmt.Errorf("record the Worker script %s uploaded before a restart: %w", name, err)
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
		case o != r.owner(ws):
			return conflict(fmt.Sprintf("it is owned by %q (tag %s)", o, reconcile.OwnerTagKey)), nil
		}
	}
	if !pol.CanWrite() || !tagging {
		return "", nil
	}
	if err := r.tagger().EnsureOwner(ctx, cf, accountID, target, r.owner(ws)); err != nil {
		var oc *reconcile.OwnershipConflictError
		if errors.As(err, &oc) {
			return fmt.Sprintf("%v; if that object no longer manages it, remove the %s tag from the script (Resource Tagging API) to allow adoption",
				err, reconcile.OwnerTagKey), nil
		}
		return "", fmt.Errorf("ownership tag: %w", err)
	}
	if err := reconcile.RecordOwnership(ctx, r.Client, ws, name); err != nil {
		return "", fmt.Errorf("record ownership of Worker script %s: %w", name, err)
	}
	return "", nil
}

// syncSubdomain makes the workers.dev route match want (previews_enabled unset: left as is). A
// non-empty result reports a difference that may not be written.
func (r *Reconciler) syncSubdomain(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string,
	want *workersv1alpha1.WorkersDev, canWrite bool) (string, error) {
	cur, err := getSubdomain(ctx, cf, accountID, name)
	if err != nil {
		return "", err
	}
	desired := apiSubdomain{Enabled: want.Enabled, PreviewsEnabled: cur.PreviewsEnabled}
	if want.PreviewsEnabled != nil {
		desired.PreviewsEnabled = *want.PreviewsEnabled
	}
	pending := ""
	if *cur != desired {
		if canWrite {
			if cur, err = setSubdomain(ctx, cf, accountID, name, desired); err != nil {
				return "", err
			}
		} else {
			pending = "the workers.dev route differs from forProvider.workersDev and managementPolicies do not allow Update"
		}
	}
	return pending, r.observeSubdomain(ctx, ws, cf, accountID, name, cur)
}

// observeSubdomain records the route and, while it is enabled, the workers.dev URL (the account
// subdomain is read once and kept in status.atProvider.url).
func (r *Reconciler) observeSubdomain(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string, s *apiSubdomain) error {
	a := &ws.Status.AtProvider
	a.Subdomain = &workersv1alpha1.WorkerSubdomainObservation{Enabled: s.Enabled, PreviewsEnabled: s.PreviewsEnabled}
	if !s.Enabled {
		a.URL = ""
		return nil
	}
	if strings.HasPrefix(a.URL, "https://"+name+".") {
		return nil
	}
	sub, err := accountSubdomain(ctx, cf, accountID)
	if err != nil {
		return err
	}
	a.URL = ""
	if sub != "" {
		a.URL = fmt.Sprintf("https://%s.%s.workers.dev", name, sub)
	}
	return nil
}

// observeOnly reads the script without writing (no forProvider, or managementPolicies allow
// neither Create nor Update). With Delete allowed, ownership is still claimed (tag and record)
// when this object can prove it, so that deletion may delete.
func (r *Reconciler) observeOnly(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string, cur *apiSettings) (ctrl.Result, error) {
	pol := reconcile.PoliciesOf(ws)
	if cur == nil {
		ws.Status.AtProvider, ws.Status.ID = workersv1alpha1.WorkerScriptObservation{}, ""
		msg := fmt.Sprintf("Worker script %q does not exist", name)
		if ws.Spec.ForProvider != nil {
			msg += " and managementPolicies do not allow Create"
		}
		reconcile.MarkNotFound(ws, msg)
		reconcile.MarkSynced(ws)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	item, err := findScript(ctx, cf, accountID, name)
	if err != nil {
		return r.fail(ws, err)
	}
	if item != nil {
		observeScript(ws, item)
	}
	conflict := ""
	if pol.CanWrite() {
		if conflict, err = r.claim(ctx, ws, cf, accountID, name, pol, false); err != nil {
			return r.fail(ws, err)
		}
	}
	dep, err := activeDeployment(ctx, cf, accountID, name)
	if err != nil {
		return r.fail(ws, err)
	}
	sub, err := getSubdomain(ctx, cf, accountID, name)
	if err != nil {
		return r.fail(ws, err)
	}
	observeSettings(ws, name, cur)
	observeDeployment(ws, dep)
	if err := r.observeSubdomain(ctx, ws, cf, accountID, name, sub); err != nil {
		return r.fail(ws, err)
	}
	ws.Status.ID = name
	reconcile.MarkAvailable(ws)
	if conflict != "" {
		reconcile.SetSynced(ws, metav1.ConditionFalse, ReasonNameConflict, conflict)
	} else {
		reconcile.MarkSynced(ws)
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// finalize runs the deletion flow (package doc). gone reports that the finalizer was removed.
func (r *Reconciler) finalize(ctx context.Context, ws *workersv1alpha1.WorkerScript) (ctrl.Result, bool, error) {
	if !controllerutil.ContainsFinalizer(ws, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, true, nil
	}
	logger := log.FromContext(ctx)
	if p := pendingScript(ws); p != "" && reconcile.ShouldDeleteExternal(ws, commonv1alpha1.DeletionDelete) {
		// An upload was announced and its result never recorded: the manager may have died
		// between the upload and RecordCreated. With no ID, or with the ID (script name) of a
		// script that is gone while the record names another one, record that script, so it
		// is deleted rather than leaked. (A recreated script normally keeps its name, which is
		// its ID, so the old ID already names it.)
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, ws, "Delete",
			fmt.Sprintf("the Worker script %s this object may have uploaded before a restart was not looked up and may be left in Cloudflare", p))
		if err == nil && acct != nil {
			gone := func(ctx context.Context, name string) (bool, error) {
				if name == p {
					return false, nil // the record names the known script: nothing else to find
				}
				s, err := getSettings(ctx, acct.Client, acct.AccountID, name)
				return s == nil && err == nil, err
			}
			_, err = reconcile.AdoptPendingCreateReplacing(ctx, r.Client, r.Recorder, ws, "Worker script", gone, func(ctx context.Context, key string) (string, error) {
				s, err := getSettings(ctx, acct.Client, acct.AccountID, p)
				if s == nil || err != nil {
					return "", err
				}
				return p, nil
			})
		}
		if err != nil {
			res, err := reconcile.DeletionResult(ws, err)
			return res, false, err
		}
	}
	name := reconcile.ExternalID(ws)
	var del func(context.Context, string) error
	switch {
	case name == "":
	case reconcile.ShouldDeleteExternal(ws, commonv1alpha1.DeletionDelete):
		refs, err := r.blockingReferrers(ctx, ws)
		if err != nil {
			res, err := reconcile.DeletionResult(ws, err)
			return res, false, err
		}
		if len(refs) > 0 {
			reconcile.SetReady(ws, metav1.ConditionFalse, commonv1alpha1.ReasonDependency, generic.ReferrerWaitMessage(refs))
			return ctrl.Result{RequeueAfter: generic.ReferrerRetry}, false, nil
		}
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, ws, "Delete",
			fmt.Sprintf("Worker script %s was left in Cloudflare despite deletionPolicy Delete", name))
		if err != nil {
			res, err := reconcile.DeletionResult(ws, err)
			return res, false, err
		}
		if acct == nil { // the account is gone: the script is kept
			break
		}
		tagger, target, err := r.deleteTarget(ctx, ws, acct.Client, acct.AccountID, name)
		if err != nil {
			res, err := reconcile.DeletionResult(ws, err)
			return res, false, err
		}
		exists := func(ctx context.Context) (bool, error) {
			s, err := getSettings(ctx, acct.Client, acct.AccountID, name)
			return s != nil, err
		}
		dec, err := reconcile.MayDeleteExternal(ctx, tagger, acct.Client, acct.AccountID, target, r.owner(ws), ws, name, exists)
		if err != nil {
			res, err := reconcile.DeletionResult(ws, fmt.Errorf("read ownership tag: %w", err))
			return res, false, err
		}
		switch {
		case dec.Gone:
			logger.Info("the Worker script is already gone", "script", name)
		case !dec.Delete:
			logger.Info("not deleting a Worker script whose ownership is not proven: "+dec.Why, "script", name)
			reconcile.WarnExternalKept(r.Recorder, ws, "Delete",
				fmt.Sprintf("Worker script %s was left in Cloudflare despite deletionPolicy Delete: %s", name, dec.Why))
		default:
			del = func(ctx context.Context, id string) error {
				err := deleteScript(ctx, acct.Client, acct.AccountID, id)
				if err == nil {
					log.FromContext(ctx).Info("deleted Worker script", "script", id)
				}
				return err
			}
		}
	case reconcile.PoliciesOf(ws).CanWrite() && reconcile.TaggingEnabled(r.tagger()) && ws.Status.AtProvider.Tag != "":
		// Orphan: release the ownership tag so the script can be adopted elsewhere (best effort).
		if acct, err := r.Accounts.Resolve(ctx, ws); err == nil {
			if err := r.tagger().RemoveOwner(ctx, acct.Client, acct.AccountID,
				reconcile.TagTarget{Type: TagResourceType, ID: ws.Status.AtProvider.Tag}, r.owner(ws)); err != nil {
				logger.Info("could not remove the ownership tag of an orphaned Worker script", "script", name, "error", err.Error())
			}
		}
	}
	reconcile.MarkDeleting(ws, "")
	res, err := reconcile.Finalize(ctx, r.Client, ws, commonv1alpha1.DeletionDelete, del)
	gone := !controllerutil.ContainsFinalizer(ws, commonv1alpha1.Finalizer)
	if gone {
		r.applied.Delete(client.ObjectKeyFromObject(ws))
	}
	return res, gone, err
}

// blockingReferrers lists the other WorkerScripts that bind ws through serviceRef. Referrers
// that are being deleted themselves do not block (their scripts are going away, and two scripts
// bound to each other would otherwise wait for each other forever), nor does ws itself.
func (r *Reconciler) blockingReferrers(ctx context.Context, ws *workersv1alpha1.WorkerScript) ([]string, error) {
	var l workersv1alpha1.WorkerScriptList
	if err := r.List(ctx, &l, client.InNamespace(ws.Namespace), client.MatchingFields{indexRefs: keyScript + ws.Name}); err != nil {
		return nil, err
	}
	var out []string
	for i := range l.Items {
		o := &l.Items[i]
		if o.Name != ws.Name && o.DeletionTimestamp.IsZero() && slices.Contains(refNames(o, pickService), ws.Name) {
			out = append(out, "WorkerScript "+o.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// deleteTarget returns the tagger and tag target for the deletion decision. The tag comes from
// status, else from the script list; when it cannot be found (the script is gone, or not
// listed) no tag can be read and ownership rests on the record or the pin.
func (r *Reconciler) deleteTarget(ctx context.Context, ws *workersv1alpha1.WorkerScript, cf cfclient.Client, accountID, name string) (reconcile.Tagger, reconcile.TagTarget, error) {
	tagger := r.tagger()
	tag := ""
	if ws.Status.AtProvider.ID == name {
		tag = ws.Status.AtProvider.Tag
	}
	if tag == "" && reconcile.TaggingEnabled(tagger) {
		item, err := findScript(ctx, cf, accountID, name)
		if err != nil {
			return nil, reconcile.TagTarget{}, err
		}
		if item != nil {
			tag = item.Tag
		}
	}
	if tag == "" {
		tagger = reconcile.NoopTagger{}
	}
	return tagger, reconcile.TagTarget{Type: TagResourceType, ID: tag}, nil
}
