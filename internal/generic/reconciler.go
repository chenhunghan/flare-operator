package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//     whose NameField equals forProvider's is adopted from ListPath, but only where an owner
//     tag decides ownership (a tagged kind with tagging on; EnsureOwner refuses another
//     object's resource) or the resource is the object's own lost create (its create-pending
//     record). Otherwise (an untaggable kind, or tagging off) a same-named resource is a
//     NameConflict until the external-id annotation pins it: nothing else could prove that
//     the object owns it, and deletion would delete it.
//   - Missing resource: created from forProvider ∩ CreateFields (unless the policies forbid
//     Create); set fields of UpdateFields \ CreateFields are applied by an update right after.
//     The new ID and the ownership record (reconcile.RecordCreated) are written to the
//     annotations at once, so a crash cannot orphan it; a create-pending record written just
//     before the create lets the finalizer find a resource whose ID a crash did lose.
//   - Existing resource: GET → status.atProvider/status.id; the ownership tag is ensured and
//     recorded (a resource owned by another object is not touched; Observe-only objects neither
//     tag, record nor pin an adopted ID); a change of an Immutable field sets
//     Synced=False/Immutable and writes nothing; set UpdateFields that differ from the observed
//     object (write-only paths, top-level or nested: from status.writeOnlyHash) are updated
//     with UpdateMethod — PATCH sends the changed fields, PUT the full body (unset fields keep
//     their observed values).
//   - A 404 on the known ID recreates the resource (Observe-only: Ready=False/ExternalNotFound).
//   - Deletion follows deletionPolicy (DefaultDeletionPolicy when unset) and the management
//     policies (reconcile.Finalize); DELETE sends no body. Only a resource the object provably
//     owns is deleted (reconcile.MayDeleteExternal; status.id is not proof). An orphaned resource
//     loses its owner tag. Without the CloudflareAccount (deleted) the resource is left as is.
//     Both cases emit a Warning event. Singletons are never created or deleted. Before a delete,
//     objects of registered referrer kinds (referrers.go, e.g. WorkerScript bindings) that still
//     name the object block it: Ready=False, reason DependencyNotReady, until they are gone.
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

	// PollInterval defaults to DefaultPollInterval.
	PollInterval time.Duration

	// Recorder emits Warning events (e.g. a resource deliberately left in Cloudflare on
	// deletion). SetupWithManager sets it; nil disables events.
	Recorder events.EventRecorder
	// APIReader confirms, uncached, that a CloudflareAccount is gone before a finalizer gives
	// up on its Cloudflare resource (SetupWithManager sets it; default: Client).
	APIReader client.Reader

	// applied remembers the write-only hash last applied per object (namespace/name →
	// appliedWriteOnly of that object's UID): the next reconcile may read the object from a
	// cache that does not have the status patch yet, and must not re-apply write-only fields
	// because of that. Entries are dropped when the object is finalized or found gone.
	applied sync.Map
}

type appliedWriteOnly struct {
	uid      types.UID
	id, hash string
}

// recordWriteOnly sets status.writeOnlyHash after the write-only fields were applied to id.
func (r *Reconciler) recordWriteOnly(obj reconcile.ManagedObject, id, hash string) {
	obj.GetResourceStatus().WriteOnlyHash = hash
	r.applied.Store(client.ObjectKeyFromObject(obj), appliedWriteOnly{uid: obj.GetUID(), id: id, hash: hash})
}

// lastWriteOnly returns the write-only hash last applied to id: this process's record, else
// status.writeOnlyHash.
func (r *Reconciler) lastWriteOnly(obj reconcile.ManagedObject, id string) string {
	if v, ok := r.applied.Load(client.ObjectKeyFromObject(obj)); ok {
		if a := v.(appliedWriteOnly); a.uid == obj.GetUID() && a.id == id {
			return a.hash
		}
	}
	return obj.GetResourceStatus().WriteOnlyHash
}

// Name is the controller name of the kind (lower-case kind).
func (r *Reconciler) Name() string { return strings.ToLower(r.Descriptor.Kind) }

// poll is the next drift-poll delay: PollInterval (DefaultPollInterval) plus up to 10% random
// jitter, so objects created together (a GitOps sync, a restart) spread their polls instead of
// hitting the account's rate limit in lockstep every interval.
func (r *Reconciler) poll() time.Duration {
	d := r.PollInterval
	if d <= 0 {
		d = DefaultPollInterval
	}
	return d + time.Duration(rand.Int64N(int64(d/10)+1))
}

func (r *Reconciler) tagger() reconcile.Tagger {
	if r.Tagger == nil || r.Descriptor.TagResourceType == "" {
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
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder(name)
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
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
	// Kinds that reference this one (referrers.go): dropping a reference wakes a blocked deletion.
	for _, ref := range ReferrersOf(r.groupKind()) {
		b = b.Watches(ref.Object, ReferrerWatch(ref))
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
		if apierrors.IsNotFound(err) {
			r.applied.Delete(req.NamespacedName)
		}
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
	return reconcile.TagTarget{Type: r.Descriptor.TagResourceType, ID: id}
}

// errResult reports err as Synced=False and returns it for a rate-limited retry. A Cloudflare
// 429 (the token is backing off) is requeued after its Retry-After instead, without an error,
// so throttled objects do not spin through the controller's short retry backoff.
func errResult(obj reconcile.ManagedObject, err error) (ctrl.Result, error) {
	reconcile.MarkSyncError(obj, "", err)
	if wait, ok := reconcile.Throttled(err); ok {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
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
		if foundID == "" {
			if foundID, err = r.findByClientID(ctx, sc, desired, id); err != nil {
				return errResult(obj, err)
			}
		}
		if foundID != "" && pol.CanWrite() && !r.tagging() && !r.ownLostCreate(obj, desired) {
			// No ownership tag can prove that this object owns a same-named resource (the kind
			// cannot be tagged, or tagging is off): it may be another object's or another
			// tool's, and managing it would let this object's deletion delete it. Adoption is
			// explicit, through the external-id annotation (as for VPCService and Tunnel).
			return r.nameConflict(obj, desired, foundID)
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

// ownLostCreate reports whether obj's create-pending record names the resource desired
// describes (pendingKey): obj announced that create and never recorded its result, so a
// resource found by that name or client-chosen ID is obj's own lost create
// (docs/resilience.md).
func (r *Reconciler) ownLostCreate(obj reconcile.ManagedObject, desired map[string]any) bool {
	key := r.pendingKey(desired)
	pending, ok := reconcile.PendingCreate(obj)
	return ok && key != "" && pending == key
}

// nameConflict reports that a resource with obj's name (or client-chosen ID) exists and obj
// cannot prove that it owns it: Ready=False and Synced=False with reason NameConflict. Nothing
// is written to Cloudflare and no ID is pinned.
func (r *Reconciler) nameConflict(obj reconcile.ManagedObject, desired map[string]any, foundID string) (ctrl.Result, error) {
	d := r.Descriptor
	field := d.NameField
	if field == "" {
		field = d.IDField
	}
	why := "ownership tagging is disabled"
	if d.TagResourceType == "" {
		why = "this resource type cannot carry an ownership tag"
	}
	msg := fmt.Sprintf("a %s with %s %v already exists (id %s) and this object cannot prove that it owns it (%s); "+
		"set the %s annotation to that ID to adopt it, or choose another forProvider.%s",
		d.Kind, field, desired[field], foundID, why, commonv1alpha1.AnnotationExternalID, field)
	ClearAtProvider(obj)
	obj.GetResourceStatus().ID = ""
	reconcile.SetReady(obj, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
	reconcile.SetSynced(obj, metav1.ConditionFalse, reconcile.ReasonNameConflict, msg)
	return ctrl.Result{RequeueAfter: r.poll()}, nil
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
		return "", fmt.Errorf("%d resources have %s %q (%s); pin one with the %s annotation: %w",
			len(ids), d.NameField, want, strings.Join(ids, ", "), commonv1alpha1.AnnotationExternalID, reconcile.ErrAmbiguousName)
	}
}

// findByClientID handles kinds whose create body carries the ID (IDField in CreateFields, e.g.
// AIGateway's id) but that have no NameField to adopt by: a resource with the desired ID is
// looked up directly, so a create whose ID was lost (the manager died before RecordCreated) is
// found and adopted instead of being re-sent, which the API would refuse as a duplicate and
// leave the object failing forever. known is the ID already found missing ("" if none). It
// returns "" when there is nothing to look up or the resource does not exist.
func (r *Reconciler) findByClientID(ctx context.Context, sc scope, desired map[string]any, known string) (string, error) {
	d := r.Descriptor
	want, ok := desired[d.IDField].(string)
	if d.NameField != "" || !has(d.CreateFields, d.IDField) || !ok || want == "" || want == known {
		return "", nil
	}
	if _, err := r.get(ctx, sc, want); err != nil {
		if cfclient.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("look up %s %q for adoption: %w", d.IDField, want, err)
	}
	return want, nil
}

// pendingKey is the create-pending key of a create of desired: the NameField value (adoption by
// name) or the client-chosen ID (findByClientID); "" when the kind can find a lost create by
// neither.
func (r *Reconciler) pendingKey(desired map[string]any) string {
	d := r.Descriptor
	if d.NameField != "" {
		if v, ok := desired[d.NameField].(string); ok && d.ListPath != "" {
			return v
		}
		return ""
	}
	if v, ok := desired[d.IDField].(string); ok && has(d.CreateFields, d.IDField) {
		return v
	}
	return ""
}

// findPending looks up the resource a create-pending record names (pendingKey): by name, or by
// the client-chosen ID. It returns "" when there is none.
func (r *Reconciler) findPending(ctx context.Context, sc scope, key string) (string, error) {
	d := r.Descriptor
	if d.NameField != "" {
		return r.findByName(ctx, sc, map[string]any{d.NameField: key})
	}
	return r.findByClientID(ctx, sc, map[string]any{d.IDField: key}, "")
}

func (r *Reconciler) create(ctx context.Context, obj reconcile.ManagedObject, sc scope, desired map[string]any) (ctrl.Result, error) {
	d := r.Descriptor
	reconcile.MarkCreating(obj, "")
	// Announce the create (reconcile.MarkCreatePending): the next reconcile adopts a lost create
	// by name or client ID anyway, but an object deleted before that has no ID, and its
	// finalizer finds the resource through this record (reconcile.AdoptPendingCreate). The
	// record is kept when the API refuses the create: this reconciler takes any resource with
	// the name (or ID) for the object's own anyway, so the record claims nothing more, and a
	// duplicate-name refusal may be the answer to a lost create that a lagging list missed.
	if key := r.pendingKey(desired); key != "" {
		if err := reconcile.MarkCreatePending(ctx, r.Client, obj, key); err != nil {
			return errResult(obj, fmt.Errorf("record the pending create: %w", err))
		}
	}
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
	// This object created the resource: that is proof of ownership (for deletion). The record
	// is written without an optimistic lock, so a concurrent change of the object cannot lose
	// the new ID.
	if err := reconcile.RecordCreated(ctx, r.Client, obj, id); err != nil {
		// The resource exists; the next reconcile adopts it by name (when the kind has one).
		return errResult(obj, fmt.Errorf("record external ID %s: %w", id, err))
	}
	log.FromContext(ctx).Info("created", "id", id)
	if err := r.tagger().EnsureOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj)); err != nil {
		return errResult(obj, fmt.Errorf("ownership tag: %w", err))
	}
	// Fields only the update body accepts (e.g. Queue settings) are applied right after create.
	written := append([]string(nil), d.CreateFields...)
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
			if d.UpdateMethod == http.MethodPut {
				written = append(written, d.UpdateFields...) // PUT carries every set UpdateField
			} else {
				written = append(written, extra...)
			}
		}
	}
	// Only the write-only paths actually sent are recorded: the rest (e.g. an update-only field
	// with Update not allowed) stays pending, so sync reports it instead of claiming Synced.
	r.recordWriteOnly(obj, id, WriteOnlyHash(desired, writeOnlyUnder(d.WriteOnly, written)))
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
		// With tagging, our owner tag is now on the resource: record that proof of ownership
		// (and pin the ID) durably. Without tagging, observe adopts only this object's own lost
		// create (ownLostCreate), which is recorded as created by it; an ID the object already
		// knows (the external-id annotation, or its own create) needs no write.
		var persist func(context.Context, client.Client, reconcile.ManagedObject, string) error
		switch {
		case r.tagging():
			persist = reconcile.RecordOwnership
		case adopted:
			persist = reconcile.RecordCreated
		}
		if persist != nil {
			if err := persist(ctx, r.Client, obj, id); err != nil {
				return errResult(obj, fmt.Errorf("record external ID %s: %w", id, err))
			}
		}
		if adopted {
			log.FromContext(ctx).Info("adopted by name", "id", id, d.NameField, desired[d.NameField])
		}
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
//   - deletionPolicy Delete: the resource is deleted only when the object provably owns it
//     (reconcile.MayDeleteExternal: its ownership record, its owner tag, or with tagging off the
//     external-id annotation). Otherwise it is left alone with a Warning event: deleting must
//     not destroy what someone else manages, and an unreadable tag (500) proves nothing. A tag
//     read refused for good (a 4xx such as 403) without an ownership record keeps the resource
//     too, so the finalizer never waits forever. A resource that is already gone needs neither.
//   - Orphan: the owner tag is released, so another object may adopt the resource. A tags GET
//     that answers 500 is checked against the tag index (reconcile.ResourceTagger), so a
//     transient 500 on a tagged resource releases the tag or fails and is retried; a permanent
//     4xx is logged and skipped.
//   - Both need the account (reconcile.FinalizeAccount). While it exists but is not Ready, the
//     finalizer stays and the object is retried. Once the CloudflareAccount is gone, nothing can
//     reach Cloudflare: the resource is left in place (owner tag included), a Warning event
//     ExternalResourceKept says so (policy Delete, or an owner tag that Orphan could not
//     release), and the finalizer is removed, so finalization never hangs.
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
	if _, pending := reconcile.PendingCreate(obj); deleteExternal && pending {
		// A create was announced and its result never recorded: the manager may have died
		// between the create and RecordCreated. With no ID, or with the ID of a resource that
		// was found gone (the create recreated it), find the resource the record names and
		// record it, so it is deleted rather than leaked.
		var err error
		if id, err = r.adoptPendingCreate(ctx, obj); err != nil {
			return reconcile.DeletionResult(obj, err)
		}
	}
	release := id != "" && !deleteExternal && reconcile.PoliciesOf(obj).CanWrite() && r.tagging()
	deleteExternal = deleteExternal && id != ""

	// Cloudflare does not check dependencies on delete: wait while registered referrers (e.g.
	// WorkerScript bindings) still use the resource.
	if deleteExternal {
		refs, err := BlockingReferrers(ctx, r.Client, r.groupKind(), obj)
		if err != nil {
			return reconcile.DeletionResult(obj, err)
		}
		if len(refs) > 0 {
			reconcile.SetReady(obj, metav1.ConditionFalse, commonv1alpha1.ReasonDependency, ReferrerWaitMessage(refs))
			return ctrl.Result{RequeueAfter: ReferrerRetry}, nil
		}
	}
	var acct *reconcile.Resolved
	if deleteExternal || release {
		action, note := "Delete", ""
		switch {
		case deleteExternal:
			note = fmt.Sprintf("%s %s was left in Cloudflare despite deletionPolicy Delete", d.Kind, id)
		case reconcile.HasOwnershipProof(obj, id):
			action = "Orphan"
			note = fmt.Sprintf("%s %s was orphaned but keeps its %s tag, which cannot be released (remove it by hand to allow adoption)",
				d.Kind, id, reconcile.OwnerTagKey)
		default:
			action = "Orphan"
		}
		var err error
		if acct, err = reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, obj, action, note); err != nil {
			return reconcile.DeletionResult(obj, err)
		}
		if acct == nil { // the account is gone: nothing can reach Cloudflare
			deleteExternal, release = false, false
		}
	}
	var sc scope
	if acct != nil {
		// sc carries the client and account ID even when the zone is unresolvable.
		var err error
		if sc, err = r.scopeFor(obj, acct); err != nil && deleteExternal {
			return reconcile.DeletionResult(obj, err)
		}
	}
	if deleteExternal {
		exists := func(ctx context.Context) (bool, error) {
			_, err := r.get(ctx, sc, id)
			if cfclient.IsNotFound(err) {
				return false, nil
			}
			return err == nil, err
		}
		dec, err := reconcile.MayDeleteExternal(ctx, r.tagger(), sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj), obj, id, exists)
		if err != nil {
			return reconcile.DeletionResult(obj, fmt.Errorf("read ownership tag: %w", err))
		}
		switch {
		case dec.Gone:
			logger.Info("the Cloudflare resource is already gone", "id", id)
			deleteExternal = false
		case !dec.Delete:
			logger.Info("not deleting the Cloudflare resource: "+dec.Why, "id", id)
			reconcile.WarnExternalKept(r.Recorder, obj, "Delete",
				fmt.Sprintf("%s %s was left in Cloudflare despite deletionPolicy Delete: %s", d.Kind, id, dec.Why))
			deleteExternal = false
		}
	}
	if release {
		// Orphaned: release ownership so another object (or cluster) may adopt it.
		if err := r.tagger().RemoveOwner(ctx, sc.cf, sc.accountID, r.tagTarget(id), r.owner(obj)); err != nil {
			if !reconcile.IsPermanent(err) {
				return reconcile.DeletionResult(obj, fmt.Errorf("release ownership tag: %w", err))
			}
			logger.Error(err, "cannot remove the ownership tag of the orphaned resource", "id", id)
		}
	}
	var del func(ctx context.Context, id string) error
	if deleteExternal {
		del = func(ctx context.Context, id string) error {
			_, err := sc.cf.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: sc.path(d.ItemPath, id)})
			if err == nil {
				log.FromContext(ctx).Info("deleted", "id", id)
			}
			return err
		}
	}
	reconcile.MarkDeleting(obj, "")
	res, err := reconcile.Finalize(ctx, r.Client, obj, kindDefault, del)
	if err == nil && !controllerutil.ContainsFinalizer(obj, commonv1alpha1.Finalizer) {
		r.applied.Delete(client.ObjectKeyFromObject(obj))
	}
	return res, err
}

// adoptPendingCreate resolves the resource of obj's create-pending record for its finalizer
// (reconcile.AdoptPendingCreateReplacing): with no external ID, or with one whose resource is
// gone, the record's resource is recorded and its ID returned; otherwise obj's ID. Without a
// usable account nothing can be looked up: a gone account leaves it with a Warning event (and
// obj's ID for the finalizer's own account step), one that is not Ready yet is waited for.
func (r *Reconciler) adoptPendingCreate(ctx context.Context, obj reconcile.ManagedObject) (string, error) {
	d := r.Descriptor
	key, _ := reconcile.PendingCreate(obj)
	acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, obj, "Delete",
		fmt.Sprintf("the %s this object may have created before a restart (create-pending %q) was not looked up and may be left in Cloudflare", d.Kind, key))
	if err != nil {
		return "", err
	}
	if acct == nil {
		return reconcile.ExternalID(obj), nil
	}
	sc, err := r.scopeFor(obj, acct)
	if err != nil {
		return "", err
	}
	gone := func(ctx context.Context, id string) (bool, error) {
		_, err := r.get(ctx, sc, id)
		if cfclient.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return reconcile.AdoptPendingCreateReplacing(ctx, r.Client, r.Recorder, obj, d.Kind, gone, func(ctx context.Context, key string) (string, error) {
		return r.findPending(ctx, sc, key)
	})
}

// groupKind is the kind's API group and kind.
func (r *Reconciler) groupKind() schema.GroupKind {
	return schema.GroupKind{Group: r.Descriptor.Group, Kind: r.Descriptor.Kind}
}

// tagging reports whether ownership tags are maintained for the kind.
func (r *Reconciler) tagging() bool { return reconcile.TaggingEnabled(r.tagger()) }

// apiReader reads CloudflareAccounts uncached when a finalizer checks that one is gone.
func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
