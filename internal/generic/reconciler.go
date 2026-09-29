package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/reconcile"
)

// DefaultPollInterval is how often an in-sync object is re-observed to detect drift.
const DefaultPollInterval = 5 * time.Minute

// accountRefIndex indexes managed objects by spec.accountRef.name.
const accountRefIndex = ".spec.accountRef.name"

// Reconciler runs one Descriptor over its generated kind.
//
// Per object:
//   - The account is resolved through reconcile.Accounts (AccountNotReady otherwise).
//   - The external ID is the external-id annotation, else status.id. Without one, a resource
//     whose NameField equals forProvider's is adopted from ListPath.
//   - Missing resource: created from forProvider ∩ CreateFields (unless the policies forbid
//     Create); set fields of UpdateFields \ CreateFields are applied by an update right after.
//     The new ID is written to the annotation at once, so a crash cannot orphan it.
//   - Existing resource: GET → status.atProvider/status.id; the ownership tag is ensured (a
//     resource owned by another object is not touched); a change of an Immutable field sets
//     Synced=False/Immutable and writes nothing; set UpdateFields that differ from the observed
//     object (write-only paths, top-level or nested: from status.writeOnlyHash) are updated
//     with UpdateMethod — PATCH sends the changed fields, PUT the full body (unset fields keep
//     their observed values).
//   - A 404 on the known ID recreates the resource (Observe-only: Ready=False/ExternalNotFound).
//   - Deletion follows deletionPolicy (DefaultDeletionPolicy when unset) and the management
//     policies (reconcile.Finalize); DELETE sends no body. A resource whose owner tag names
//     another object is never deleted. An orphaned resource loses its owner tag. Without the
//     CloudflareAccount (deleted) the resource is left as is. Singletons are never created or
//     deleted.
//
// Unchanged objects cost reads only: a reconcile of an in-sync object makes no Cloudflare write.
type Reconciler struct {
	Client   client.Client
	Accounts *reconcile.Accounts
	// Tagger maintains the ownership tag (reconcile.NoopTagger disables it).
	Tagger reconcile.Tagger
	// ClusterName is the <cluster> of the owner tag value.
	ClusterName string

	Descriptor Descriptor
	// New returns an empty object of the kind; NewList an empty list (optional: without it,
	// CloudflareAccount changes do not wake the kind's objects).
	New     func() reconcile.ManagedObject
	NewList func() client.ObjectList
	// TagResourceType is the Resource Tagging resource_type; "" disables ownership tags.
	TagResourceType string

	// PollInterval defaults to DefaultPollInterval.
	PollInterval time.Duration

	// applied remembers the write-only hash last applied per object (UID → appliedWriteOnly):
	// the next reconcile may read the object from a cache that does not have the status patch
	// yet, and must not re-apply write-only fields because of that.
	applied sync.Map
}

type appliedWriteOnly struct{ id, hash string }

// recordWriteOnly sets status.writeOnlyHash after the write-only fields were applied to id.
func (r *Reconciler) recordWriteOnly(obj reconcile.ManagedObject, id, hash string) {
	obj.GetResourceStatus().WriteOnlyHash = hash
	r.applied.Store(obj.GetUID(), appliedWriteOnly{id: id, hash: hash})
}

// lastWriteOnly returns the write-only hash last applied to id: this process's record, else
// status.writeOnlyHash.
func (r *Reconciler) lastWriteOnly(obj reconcile.ManagedObject, id string) string {
	if v, ok := r.applied.Load(obj.GetUID()); ok {
		if a := v.(appliedWriteOnly); a.id == id {
			return a.hash
		}
	}
	return obj.GetResourceStatus().WriteOnlyHash
}

// Name is the controller name of the kind (lower-case kind).
func (r *Reconciler) Name() string { return strings.ToLower(r.Descriptor.Kind) }

func (r *Reconciler) poll() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return DefaultPollInterval
}

func (r *Reconciler) tagger() reconcile.Tagger {
	if r.Tagger == nil || r.TagResourceType == "" {
		return reconcile.NoopTagger{}
	}
	return r.Tagger
}

// SetupWithManager registers the controller under name (Name() when empty). It watches the
// kind (spec and annotation changes) and CloudflareAccounts, so objects waiting for their
// account are reconciled when it becomes Ready.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	if name == "" {
		name = r.Name()
	}
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), r.New(), accountRefIndex, func(o client.Object) []string {
		if m, ok := o.(commonv1alpha1.Managed); ok {
			return []string{m.GetResourceSpec().AccountRef.Name}
		}
		return nil
	}); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(r.New(), builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{})))
	if r.NewList != nil {
		b = b.Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.objectsForAccount),
			builder.WithPredicates(accountReadinessChanged()))
	}
	return b.Complete(r)
}

// accountReadinessChanged passes account events that can unblock (or block) managed objects:
// creation and a change of readiness. Periodic re-verification is filtered out.
func accountReadinessChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*cloudflarev1alpha1.CloudflareAccount)
			n, ok2 := e.ObjectNew.(*cloudflarev1alpha1.CloudflareAccount)
			return !ok1 || !ok2 || reconcile.AccountReady(o) != reconcile.AccountReady(n)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (r *Reconciler) objectsForAccount(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	if r.NewList == nil {
		return nil
	}
	list := r.NewList()
	if err := r.Client.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{accountRefIndex: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list objects for CloudflareAccount", "kind", r.Descriptor.Kind, "account", o.GetName())
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	out := make([]ctrlreconcile.Request, 0, len(items))
	for _, it := range items {
		if m, ok := it.(metav1.Object); ok {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.GetNamespace(), Name: m.GetName()}})
		}
	}
	return out
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.New()
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base, ok := obj.DeepCopyObject().(reconcile.ManagedObject)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("generic: %T DeepCopyObject is not a ManagedObject", obj)
	}
	var res ctrl.Result
	var err error
	if !obj.GetDeletionTimestamp().IsZero() {
		res, err = r.finalize(ctx, obj)
		if !controllerutil.ContainsFinalizer(obj, commonv1alpha1.Finalizer) {
			return res, err // gone (or going): no status to write
		}
	} else {
		res, err = r.observe(ctx, obj)
	}
	if !equality.Semantic.DeepEqual(statusOf(base), statusOf(obj)) {
		if perr := r.Client.Status().Patch(ctx, obj, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
			if err == nil {
				err = perr
			}
		}
	}
	return res, err
}

// scope holds the resolved account and paths of one object.
type scope struct {
	cf        cfclient.Client
	accountID string
	zoneID    string
}

func (s scope) path(p, id string) string {
	return strings.NewReplacer("{account_id}", s.accountID, "{zone_id}", s.zoneID, "{id}", id).Replace(p)
}

func (r *Reconciler) scopeFor(obj reconcile.ManagedObject, acct *reconcile.Resolved) (scope, error) {
	s := scope{cf: acct.Client, accountID: acct.AccountID}
	if r.Descriptor.Scope == "zone" {
		z := obj.GetResourceSpec().ZoneRef
		switch {
		case z == nil || (z.ID == "" && z.Name == ""):
			return s, errors.New("spec.zoneRef is required for a zone-scoped kind")
		case z.ID == "":
			return s, errors.New("spec.zoneRef.name is not supported yet; set spec.zoneRef.id")
		}
		s.zoneID = z.ID
	}
	return s, nil
}

func (r *Reconciler) owner(obj reconcile.ManagedObject) string {
	return reconcile.OwnerValue(r.ClusterName, obj.GetNamespace(), obj.GetName())
}

func (r *Reconciler) tagTarget(id string) reconcile.TagTarget {
	return reconcile.TagTarget{Type: r.TagResourceType, ID: id}
}

// errResult reports err as Synced=False and returns it for a rate-limited retry.
func errResult(obj reconcile.ManagedObject, err error) (ctrl.Result, error) {
	reconcile.MarkSyncError(obj, "", err)
	return ctrl.Result{}, err
}

// observe runs one reconcile of a live object.
func (r *Reconciler) observe(ctx context.Context, obj reconcile.ManagedObject) (ctrl.Result, error) {
	d := r.Descriptor
	pol := reconcile.PoliciesOf(obj)
	acct, err := r.Accounts.Resolve(ctx, obj)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(obj, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, obj); err != nil {
		return ctrl.Result{}, err
	}
	sc, err := r.scopeFor(obj, acct)
	if err != nil {
		reconcile.MarkSyncError(obj, "", err)
		return ctrl.Result{RequeueAfter: r.poll()}, nil
	}
	desired, err := ForProvider(obj)
	if err != nil {
		return errResult(obj, err)
	}
	if d.Singleton {
		return r.sync(ctx, obj, sc, desired, "", nil, false)
	}

	id := reconcile.ExternalID(obj)
	var observed json.RawMessage
	if id != "" {
		observed, err = r.get(ctx, sc, id)
		switch {
		case cfclient.IsNotFound(err):
			log.FromContext(ctx).Info("external resource not found", "id", id)
			observed = nil
		case err != nil:
			return errResult(obj, err)
		}
	}
	adopted := false
	if observed == nil {
		foundID, err := r.findByName(ctx, sc, desired)
		if err != nil {
			return errResult(obj, err)
		}
		if foundID != "" {
			if observed, err = r.get(ctx, sc, foundID); err != nil {
				return errResult(obj, err)
			}
			id, adopted = foundID, true
		}
	}
	if observed == nil {
		if !pol.CanCreate() {
			ClearAtProvider(obj)
			obj.GetResourceStatus().ID = ""
			msg := "the Cloudflare resource does not exist and managementPolicies do not allow Create"
			if id == "" && (d.NameField == "" || desired[d.NameField] == nil) {
				msg = "nothing to observe: set the " + commonv1alpha1.AnnotationExternalID + " annotation"
				if d.NameField != "" {
					msg += " or forProvider." + d.NameField
				}
			}
			reconcile.MarkNotFound(obj, msg)
			reconcile.MarkSynced(obj)
			reconcile.SetObservedGeneration(obj)
			return ctrl.Result{RequeueAfter: r.poll()}, nil
		}
		return r.create(ctx, obj, sc, desired)
	}
	return r.sync(ctx, obj, sc, desired, id, observed, adopted)
}

func (r *Reconciler) get(ctx context.Context, sc scope, id string) (json.RawMessage, error) {
	resp, err := sc.cf.Do(ctx, cfclient.Request{Method: http.MethodGet, Path: sc.path(r.Descriptor.ItemPath, id)})
	if err != nil {
		return nil, err
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil, fmt.Errorf("GET %s: empty result", sc.path(r.Descriptor.ItemPath, id))
	}
	return resp.Result, nil
}

// findByName lists the collection and returns the ID of the item whose NameField equals the
// desired one ("" when none or when adoption by name is not possible).
func (r *Reconciler) findByName(ctx context.Context, sc scope, desired map[string]any) (string, error) {
	d := r.Descriptor
	want, ok := desired[d.NameField].(string)
	if d.NameField == "" || d.ListPath == "" || !ok || want == "" {
		return "", nil
	}
	items, err := cfclient.ListAll(ctx, sc.cf, cfclient.Request{Path: sc.path(d.ListPath, "")})
	if err != nil {
		return "", fmt.Errorf("list for adoption by %s: %w", d.NameField, err)
	}
	var ids []string
	for _, raw := range items {
		var it map[string]any
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		if it[d.NameField] == want {
			if id, _ := it[d.IDField].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%d resources have %s %q (%s); pin one with the %s annotation",
			len(ids), d.NameField, want, strings.Join(ids, ", "), commonv1alpha1.AnnotationExternalID)
	}
}

func (r *Reconciler) create(ctx context.Context, obj reconcile.ManagedObject, sc scope, desired map[string]any) (ctrl.Result, error) {
	d := r.Descriptor
	reconcile.MarkCreating(obj, "")
	resp, err := sc.cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: sc.path(d.CreatePath, ""), Body: pick(desired, d.CreateFields)})
	if err != nil {
		return errResult(obj, fmt.Errorf("create: %w", err))
	}
	created, err := decodeObject(resp.Result)
	if err != nil {
		return errResult(obj, fmt.Errorf("create: %w", err))
	}
	id, _ := created[d.IDField].(string)
	if id == "" {
		return errResult(obj, fmt.Errorf("create: the result has no %s", d.IDField))
	}
	if err := reconcile.PersistExternalID(ctx, r.Client, obj, id); err != nil {
		// The resource exists; the next reconcile adopts it by name (when the kind has one).
		return errResult(obj, fmt.Errorf("record external ID %s: %w", id, err))
	}
	log.FromContext(ctx).Info("created", "id", id)
	if err := r.tagger().EnsureOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj)); err != nil {
		return errResult(obj, fmt.Errorf("ownership tag: %w", err))
	}
	// Fields only the update body accepts (e.g. Queue settings) are applied right after create.
	if pol := reconcile.PoliciesOf(obj); d.UpdateMethod != "" && pol.CanUpdate() {
		var extra []string
		for _, f := range d.UpdateFields {
			if !has(d.CreateFields, f) && desired[f] != nil {
				extra = append(extra, f)
			}
		}
		if len(extra) > 0 {
			if err := r.update(ctx, sc, id, pick(desired, extra), desired, created); err != nil {
				return errResult(obj, fmt.Errorf("update after create: %w", err))
			}
		}
	}
	r.recordWriteOnly(obj, id, WriteOnlyHash(desired, d.WriteOnly))
	observed, err := r.get(ctx, sc, id)
	if err != nil {
		return errResult(obj, err)
	}
	return r.sync(ctx, obj, sc, desired, id, observed, false)
}

// sync records the observed object, checks ownership and immutable fields, and updates drifted
// fields. id is "" for singletons.
func (r *Reconciler) sync(ctx context.Context, obj reconcile.ManagedObject, sc scope, desired map[string]any, id string,
	observed json.RawMessage, adopted bool) (ctrl.Result, error) {
	d := r.Descriptor
	pol := reconcile.PoliciesOf(obj)
	if d.Singleton {
		var err error
		if observed, err = r.get(ctx, sc, ""); err != nil {
			return errResult(obj, err)
		}
	}
	obs, err := decodeObject(observed)
	if err != nil {
		return errResult(obj, err)
	}
	// Ownership: never touch (or pin) a resource another object owns.
	if id != "" && pol.CanWrite() {
		if err := r.tagger().EnsureOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj)); err != nil {
			var conflict *reconcile.OwnershipConflictError
			if errors.As(err, &conflict) {
				reconcile.MarkSyncError(obj, "", fmt.Errorf("%w; if that object no longer manages it, remove the %s tag from the resource (Resource Tagging API) to allow adoption",
					err, reconcile.OwnerTagKey))
				return ctrl.Result{RequeueAfter: r.poll()}, nil
			}
			return errResult(obj, fmt.Errorf("ownership tag: %w", err))
		}
	}
	if adopted && pol.CanWrite() {
		if err := reconcile.PersistExternalID(ctx, r.Client, obj, id); err != nil {
			return errResult(obj, fmt.Errorf("record external ID %s: %w", id, err))
		}
		log.FromContext(ctx).Info("adopted by name", "id", id, d.NameField, desired[d.NameField])
	}
	obj.GetResourceStatus().ID = id
	if err := SetAtProvider(obj, observed); err != nil {
		return errResult(obj, err)
	}
	reconcile.MarkAvailable(obj)

	if !pol.CanWrite() {
		reconcile.MarkSynced(obj)
		reconcile.SetObservedGeneration(obj)
		return ctrl.Result{RequeueAfter: r.poll()}, nil
	}

	prevWO, woKnown := parseWriteOnlyHash(r.lastWriteOnly(obj, id))
	if imm := r.immutableChanges(desired, obs, prevWO, woKnown); len(imm) > 0 {
		reconcile.MarkImmutable(obj, fmt.Sprintf("immutable fields cannot be changed after creation: %s (recreate the object to change them)",
			strings.Join(imm, ", ")))
		return ctrl.Result{RequeueAfter: r.poll()}, nil
	}

	var changed []string
	for _, f := range d.UpdateFields {
		v, set := desired[f]
		if !set || v == nil {
			continue
		}
		// Unknown write-only state (adopted): apply once, then the hash is the record.
		if r.differs(f, v, desired, obs, prevWO, woKnown, true) {
			changed = append(changed, f)
		}
	}
	if len(changed) > 0 {
		if d.UpdateMethod == "" || !pol.CanUpdate() {
			why := "managementPolicies do not allow Update"
			if d.UpdateMethod == "" {
				why = "the API has no update operation"
			}
			reconcile.SetSynced(obj, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError,
				fmt.Sprintf("forProvider differs from Cloudflare in %s, but %s", strings.Join(changed, ", "), why))
			return ctrl.Result{RequeueAfter: r.poll()}, nil
		}
		log.FromContext(ctx).Info("updating", "id", id, "fields", changed)
		if err := r.update(ctx, sc, id, pick(desired, changed), desired, obs); err != nil {
			return errResult(obj, fmt.Errorf("update: %w", err))
		}
		if observed, err = r.get(ctx, sc, id); err != nil {
			return errResult(obj, err)
		}
		if err := SetAtProvider(obj, observed); err != nil {
			return errResult(obj, err)
		}
	}
	r.recordWriteOnly(obj, id, WriteOnlyHash(desired, d.WriteOnly))
	reconcile.MarkSynced(obj)
	reconcile.SetObservedGeneration(obj)
	return ctrl.Result{RequeueAfter: r.poll()}, nil
}

// immutableChanges lists the Immutable fields whose desired value differs from Cloudflare's. A
// write-only immutable field is compared with the hash recorded when it was applied; with no
// record (an adopted resource) the current value becomes the baseline.
func (r *Reconciler) immutableChanges(desired, obs map[string]any, prevWO map[string]string, woKnown bool) []string {
	d := r.Descriptor
	var out []string
	for _, f := range d.Immutable {
		v, set := desired[f]
		if !set || v == nil {
			continue
		}
		if r.differs(f, v, desired, obs, prevWO, woKnown, false) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// differs reports whether the desired top-level field f (value v) differs from Cloudflare's.
// Write-only paths (f itself, or dotted paths below it such as settings.delivery_paused) are
// never read back: they are compared with the hash recorded when last applied, and with no
// record (woKnown=false) unknownChanged decides. The rest of f is compared with Covers.
func (r *Reconciler) differs(f string, v any, desired, obs map[string]any, prevWO map[string]string, woKnown, unknownChanged bool) bool {
	d := r.Descriptor
	woDiffers := func(p string, w any) bool {
		if !woKnown {
			return unknownChanged
		}
		return prevWO[p] != valueHash(w)
	}
	if has(d.WriteOnly, f) {
		return woDiffers(f, v)
	}
	sub := nestedPaths(d.WriteOnly, f)
	for _, p := range sub {
		if w, ok := pathValue(desired, f+"."+p); ok && woDiffers(f+"."+p, w) {
			return true
		}
	}
	return !Covers(without(v, sub), obs[f])
}

// update sends UpdateMethod to the item. PATCH carries only the changed fields; PUT replaces
// the object, so it carries every UpdateField: desired where set, else the observed value.
func (r *Reconciler) update(ctx context.Context, sc scope, id string, changed, desired, observed map[string]any) error {
	d := r.Descriptor
	body := changed
	if d.UpdateMethod == http.MethodPut {
		body = map[string]any{}
		for _, f := range d.UpdateFields {
			if v, ok := desired[f]; ok && v != nil {
				body[f] = v
			} else if v, ok := observed[f]; ok && v != nil && !has(d.WriteOnly, f) {
				body[f] = v
			}
		}
	}
	_, err := sc.cf.Do(ctx, cfclient.Request{Method: d.UpdateMethod, Path: sc.path(d.ItemPath, id), Body: body})
	return err
}

// finalize handles a deleted object.
//
//   - deletionPolicy Delete: the resource is deleted unless its owner tag names another object
//     (then it is left alone: deleting must not destroy what someone else manages).
//   - Orphan: the owner tag is released, so another object may adopt the resource; a failure is
//     retried (a permanent 4xx is logged and skipped).
//   - Both need the account. While it exists but is not Ready, the finalizer stays and the
//     object is retried. Once the CloudflareAccount is gone, nothing can reach Cloudflare: the
//     resource is left in place (owner tag included) and the finalizer is removed, so deleting
//     a namespace does not hang on its managed objects.
func (r *Reconciler) finalize(ctx context.Context, obj reconcile.ManagedObject) (ctrl.Result, error) {
	d := r.Descriptor
	if !controllerutil.ContainsFinalizer(obj, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	kindDefault := commonv1alpha1.DeletionPolicy(d.DefaultDeletionPolicy)
	deleteExternal := !d.Singleton && reconcile.ShouldDeleteExternal(obj, kindDefault)
	id := reconcile.ExternalID(obj)
	if d.Singleton {
		id = ""
	}
	release := id != "" && !deleteExternal && reconcile.PoliciesOf(obj).CanWrite() && r.tagging()
	deleteExternal = deleteExternal && id != ""

	acct, err := r.Accounts.Resolve(ctx, obj)
	if err != nil {
		acct = nil
		if deleteExternal || release {
			gone, gerr := r.accountGone(ctx, obj)
			if gerr != nil {
				return ctrl.Result{}, gerr
			}
			if !gone {
				reconcile.MarkDeleting(obj, err.Error())
				if reconcile.IsAccountNotReady(err) {
					return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
				}
				return ctrl.Result{}, err
			}
			logger.Info("CloudflareAccount is gone: leaving the Cloudflare resource in place", "id", id,
				"account", obj.GetResourceSpec().AccountRef.Name, "deletionPolicy", reconcile.EffectiveDeletionPolicy(obj, kindDefault))
			deleteExternal, release = false, false
		}
	}
	var sc scope
	if acct != nil {
		// sc carries the client and account ID even when the zone is unresolvable.
		if sc, err = r.scopeFor(obj, acct); err != nil && deleteExternal {
			reconcile.MarkDeleting(obj, err.Error())
			return ctrl.Result{}, err
		}
	}
	if acct != nil && deleteExternal {
		other, err := r.foreignOwner(ctx, sc, obj, id)
		if err != nil {
			reconcile.MarkDeleting(obj, fmt.Sprintf("read ownership tag: %v", err))
			return ctrl.Result{}, err
		}
		if other != "" {
			logger.Info("not deleting the Cloudflare resource: another object owns it", "id", id, "owner", other)
			deleteExternal = false
		}
	}
	if acct != nil && release {
		// Orphaned: release ownership so another object (or cluster) may adopt it.
		if err := r.tagger().RemoveOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj)); err != nil {
			if !permanent(err) {
				reconcile.MarkDeleting(obj, fmt.Sprintf("release ownership tag: %v", err))
				return ctrl.Result{}, err
			}
			logger.Error(err, "cannot remove the ownership tag of the orphaned resource", "id", id)
		}
	}
	var del func(ctx context.Context, id string) error
	if deleteExternal && acct != nil {
		del = func(ctx context.Context, id string) error {
			_, err := sc.cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: sc.path(d.ItemPath, id)})
			if err == nil {
				log.FromContext(ctx).Info("deleted", "id", id)
			}
			return err
		}
	}
	reconcile.MarkDeleting(obj, "")
	if _, err := reconcile.Finalize(ctx, r.Client, obj, kindDefault, del); err != nil {
		return ctrl.Result{}, err
	}
	r.applied.Delete(obj.GetUID())
	return ctrl.Result{}, nil
}

// tagging reports whether ownership tags are maintained for the kind.
func (r *Reconciler) tagging() bool {
	_, noop := r.tagger().(reconcile.NoopTagger)
	return !noop
}

// accountGone reports whether obj's CloudflareAccount no longer exists.
func (r *Reconciler) accountGone(ctx context.Context, obj reconcile.ManagedObject) (bool, error) {
	name := obj.GetResourceSpec().AccountRef.Name
	if name == "" {
		return true, nil
	}
	var acct cloudflarev1alpha1.CloudflareAccount
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, &acct)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// ownerReader reads the owner tag without writing (reconcile.ResourceTagger implements it).
type ownerReader interface {
	Owner(ctx context.Context, cf cfclient.Client, accountID string, t reconcile.TagTarget) (string, error)
}

// foreignOwner returns the owner tag of the resource when it names another object ("" when the
// resource is ours, untagged, gone, or tagging is off).
func (r *Reconciler) foreignOwner(ctx context.Context, sc scope, obj reconcile.ManagedObject, id string) (string, error) {
	if !r.tagging() {
		return "", nil
	}
	me := r.owner(obj)
	tg := r.tagger()
	if rd, ok := tg.(ownerReader); ok {
		o, err := rd.Owner(ctx, sc.cf, sc.accountID, r.tagTarget(id))
		switch {
		case cfclient.IsNotFound(err):
			return "", nil
		case err != nil:
			return "", err
		case o != "" && o != me:
			return o, nil
		}
		return "", nil
	}
	// Other taggers: EnsureOwner reports a foreign owner without writing (and claims an
	// untagged resource, which is about to be deleted anyway).
	err := tg.EnsureOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), me)
	var conflict *reconcile.OwnershipConflictError
	switch {
	case errors.As(err, &conflict):
		return conflict.Owner, nil
	case cfclient.IsNotFound(err):
		return "", nil
	}
	return "", err
}

// permanent reports whether a Cloudflare error will not go away by retrying (a 4xx other than
// 408, 409 and 429).
func permanent(err error) bool {
	ae, ok := cfclient.AsAPIError(err)
	return ok && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusRequestTimeout && ae.Status != http.StatusConflict && ae.Status != http.StatusTooManyRequests
}
