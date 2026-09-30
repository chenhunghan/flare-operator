// Package pagesdeployment implements the PagesDeployment controller: a Cloudflare Pages Direct
// Upload of an artifact (internal/artifact) to a PagesProject.
//
// Behavior:
//
//   - The project comes from forProvider.projectRef: a PagesProject in the namespace, Ready and
//     on the same CloudflareAccount (else Synced=False, reason DependencyNotReady). Its
//     Cloudflare name is recorded in status.atProvider.project_name, which the finalizer uses.
//   - The artifact is loaded on every sync (cached by digest for OCI and URL sources). A
//     deployment is made (upload.go) when there is none yet, when the hash of the artifact's
//     digest and the branch (status.deployedHash) changes, or when the deployment this object
//     made is gone. Anything else (a commit message edit, a poll) makes no Cloudflare write.
//   - Each deployment is followed until its deploy stage succeeds: until then Ready=False with
//     reason Deploying and a requeue after StagePollInterval; a failed deploy stage is
//     Ready=False/Synced=False with reason DeploymentFailed (not retried until the content or
//     branch changes).
//   - The external ID is the deployment ID, recorded with reconcile.RecordCreated. A create is
//     announced first (create-pending, key "<project>;k=<deployedHash>;c=<commit hash>"): every
//     deployment carries a commit hash (forProvider.commit_hash, else one derived from the
//     object's UID and the deployed hash), by which a deployment made before a crash is found
//     again instead of deploying twice. Only the derived commit hash identifies a deployment: a
//     user-set one is a git commit that other deployments carry too, so with it a lost
//     deployment is not looked up (Warning event ExternalResourceKept; it may be left in
//     Cloudflare, and the object deploys again).
//   - Observe (managementPolicies without Create and Update): the deployment the external-id
//     annotation pins, else the project's live production deployment (no branch, or the
//     production branch) or the newest deployment of the branch. Nothing is written.
//   - Deletion (default Delete) deletes the deployment (force=true, which removes a preview's
//     branch alias with it) when this object made it or pins it. Cloudflare refuses to delete
//     the project's live production deployment: that one is kept, with a Warning event
//     ExternalResourceKept and Ready=False reason LiveDeploymentKept, and the finalizer is
//     removed. PagesDeployments are registered as referrers of their PagesProject, whose
//     deletion waits for them.
package pagesdeployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	pagesv1alpha1 "flare.dev/operator/api/pages/v1alpha1"
	sharedv1alpha1 "flare.dev/operator/api/shared/v1alpha1"
	"flare.dev/operator/internal/artifact"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/controller/pagesproject"
	"flare.dev/operator/internal/reconcile"
)

// Name is the registration name of this controller.
const Name = "pagesdeployment"

// Defaults of the Reconciler's intervals.
const (
	DefaultResyncInterval    = 10 * time.Minute
	DefaultDependencyRetry   = 30 * time.Second
	DefaultStagePollInterval = 5 * time.Second
)

// Condition reasons of this controller.
const (
	// ReasonDeploying: the deployment was made and its deploy stage has not finished.
	ReasonDeploying = "Deploying"
	// ReasonDeploymentFailed: the deployment's deploy stage failed.
	ReasonDeploymentFailed = "DeploymentFailed"
	// ReasonLiveDeploymentKept: deleting the object kept the project's live production
	// deployment, which Cloudflare refuses to delete.
	ReasonLiveDeploymentKept = "LiveDeploymentKept"
	// ReasonArtifactUnavailable: the artifact source cannot be loaded (a registry or server
	// failure; retried with backoff).
	ReasonArtifactUnavailable = "ArtifactUnavailable"
	// ReasonInvalidArtifact: the artifact source or content cannot be deployed (an invalid
	// reference, a limit or safety rule, a file Pages does not take).
	ReasonInvalidArtifact = "InvalidArtifact"
)

// indexRefs indexes PagesDeployments by the objects they reference ("<kind>/<name>").
const indexRefs = "pagesdeployment.refs"

const (
	keyProject   = "project/"
	keyConfigMap = "configmap/"
	keySecret    = "secret/"
)

func init() {
	controller.Register(controller.Registration{
		Name:        Name,
		AddToScheme: pagesv1alpha1.AddToScheme,
		Setup: func(mgr ctrl.Manager, d controller.Deps) error {
			return (&Reconciler{Client: mgr.GetClient(), Accounts: d.Accounts, Artifacts: d.Artifacts,
				Recorder: mgr.GetEventRecorder(Name), APIReader: mgr.GetAPIReader(), ResyncInterval: d.PollInterval}).SetupWithManager(mgr)
		},
	})
}

// Reconciler reconciles PagesDeployment objects.
type Reconciler struct {
	client.Client
	Accounts  *reconcile.Accounts
	Artifacts *artifact.Loader
	// Recorder records Events (optional).
	Recorder events.EventRecorder
	// APIReader confirms, uncached, that a CloudflareAccount is gone (default: Client).
	APIReader client.Reader
	// ResyncInterval re-reads the deployment periodically (default DefaultResyncInterval).
	ResyncInterval time.Duration
	// DependencyRetry re-checks an unready project or artifact dependency (default
	// DefaultDependencyRetry).
	DependencyRetry time.Duration
	// StagePollInterval re-reads a deployment whose deploy stage has not finished (default
	// DefaultStagePollInterval).
	StagePollInterval time.Duration

	jwts jwtClients
	// applied remembers what each object last deployed (object key → appliedState): a reconcile
	// that reads a stale cache must not deploy twice.
	applied sync.Map
}

type appliedState struct {
	uid      types.UID
	id, hash string
	digest   *sharedv1alpha1.ArtifactStatus
}

// +kubebuilder:rbac:groups=pages.cloudflare.flare.dev,resources=pagesdeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=pages.cloudflare.flare.dev,resources=pagesdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pages.cloudflare.flare.dev,resources=pagesdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=pages.cloudflare.flare.dev,resources=pagesprojects,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudflare.flare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

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

func (r *Reconciler) stagePoll() time.Duration {
	if r.StagePollInterval > 0 {
		return r.StagePollInterval
	}
	return DefaultStagePollInterval
}

func (r *Reconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// refKeys is the indexRefs extractor.
func refKeys(o client.Object) []string {
	pd, ok := o.(*pagesv1alpha1.PagesDeployment)
	if !ok {
		return nil
	}
	fp := pd.Spec.ForProvider
	keys := []string{keyProject + fp.ProjectRef.Name}
	if src := fp.Source; src != nil {
		if src.ConfigMapRef != nil {
			for _, cm := range src.ConfigMapRef.ConfigMaps {
				keys = append(keys, keyConfigMap+cm.Name)
			}
		}
		if src.OCIRef != nil && src.OCIRef.PullSecretRef != nil {
			keys = append(keys, keySecret+src.OCIRef.PullSecretRef.Name)
		}
	}
	return keys
}

// SetupWithManager registers the controller. Changes of the project (readiness, deletion),
// the ConfigMaps and the pull Secret requeue the PagesDeployments that use them.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &pagesv1alpha1.PagesDeployment{}, indexRefs, refKeys); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&pagesv1alpha1.PagesDeployment{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&pagesv1alpha1.PagesProject{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyProject)), builder.WithPredicates(projectChanged())).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keyConfigMap)), builder.WithPredicates(dataChanged())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.referencing(keySecret)), builder.WithPredicates(dataChanged())).
		Complete(r)
}

func projectChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*pagesv1alpha1.PagesProject)
			n, ok2 := e.ObjectNew.(*pagesv1alpha1.PagesProject)
			if !ok1 || !ok2 {
				return true
			}
			return o.Status.ID != n.Status.ID || reconcile.IsReady(o) != reconcile.IsReady(n) ||
				o.DeletionTimestamp.IsZero() != n.DeletionTimestamp.IsZero() || o.Spec.AccountRef != n.Spec.AccountRef ||
				o.Status.AtProvider.ProductionBranch != n.Status.AtProvider.ProductionBranch
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func dataChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			switch o := e.ObjectOld.(type) {
			case *corev1.Secret:
				n, ok := e.ObjectNew.(*corev1.Secret)
				return !ok || !reflect.DeepEqual(o.Data, n.Data) || !reflect.DeepEqual(o.Labels, n.Labels)
			case *corev1.ConfigMap:
				n, ok := e.ObjectNew.(*corev1.ConfigMap)
				return !ok || !reflect.DeepEqual(o.Data, n.Data) || !reflect.DeepEqual(o.BinaryData, n.BinaryData) ||
					!reflect.DeepEqual(o.Labels, n.Labels)
			}
			return true
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (r *Reconciler) referencing(prefix string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []ctrlreconcile.Request {
		var l pagesv1alpha1.PagesDeploymentList
		if err := r.List(ctx, &l, client.InNamespace(o.GetNamespace()), client.MatchingFields{indexRefs: prefix + o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "list PagesDeployments referencing", "object", prefix+o.GetName())
			return nil
		}
		out := make([]ctrlreconcile.Request, 0, len(l.Items))
		for _, pd := range l.Items {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: pd.Namespace, Name: pd.Name}})
		}
		return out
	}
}

// Reconcile syncs one PagesDeployment.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pd pagesv1alpha1.PagesDeployment
	if err := r.Get(ctx, req.NamespacedName, &pd); err != nil {
		if apierrors.IsNotFound(err) {
			r.applied.Delete(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := pd.DeepCopy()
	var (
		res  ctrl.Result
		err  error
		gone bool
	)
	if !pd.DeletionTimestamp.IsZero() {
		res, gone, err = r.finalize(ctx, &pd)
	} else {
		res, err = r.sync(ctx, &pd)
	}
	if gone {
		return ctrl.Result{}, err
	}
	reconcile.SetObservedGeneration(&pd)
	if !equality.Semantic.DeepEqual(base.Status, pd.Status) {
		if perr := r.Status().Patch(ctx, &pd, client.MergeFrom(base)); perr != nil && !apierrors.IsNotFound(perr) {
			return ctrl.Result{}, errors.Join(err, perr)
		}
	}
	return res, err
}

// last returns what pd last deployed: this process's record, else the status.
func (r *Reconciler) last(pd *pagesv1alpha1.PagesDeployment) appliedState {
	if v, ok := r.applied.Load(client.ObjectKeyFromObject(pd)); ok {
		if a := v.(appliedState); a.uid == pd.UID {
			return a
		}
	}
	a := appliedState{uid: pd.UID, id: pd.Status.ID, hash: pd.Status.DeployedHash, digest: pd.Status.Artifact}
	if id := pd.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; a.id == "" && reconcile.HasOwnershipProof(pd, id) {
		// RecordCreated persisted the deployment, the status patch after it did not (a crash).
		a.id = id
	}
	return a
}

func (r *Reconciler) remember(pd *pagesv1alpha1.PagesDeployment, a appliedState) {
	a.uid = pd.UID
	pd.Status.ID, pd.Status.DeployedHash, pd.Status.Artifact = a.id, a.hash, a.digest
	r.applied.Store(client.ObjectKeyFromObject(pd), a)
}

func (r *Reconciler) fail(pd *pagesv1alpha1.PagesDeployment, err error) (ctrl.Result, error) {
	reconcile.MarkSyncError(pd, "", err)
	if reconcile.IsPermanent(err) {
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	if wait, ok := reconcile.Throttled(err); ok {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, err
}

func (r *Reconciler) blocked(pd *pagesv1alpha1.PagesDeployment, reason, msg string) (ctrl.Result, error) {
	reconcile.SetReady(pd, metav1.ConditionFalse, reason, msg)
	reconcile.SetSynced(pd, metav1.ConditionFalse, reason, msg)
	if reason == commonv1alpha1.ReasonDependency {
		return ctrl.Result{RequeueAfter: r.depRetry()}, nil
	}
	return ctrl.Result{}, nil
}

// project resolves projectRef: the PagesProject must exist, not be deleting, use the same
// account and be Ready. It returns the Cloudflare project name, or a message.
func (r *Reconciler) project(ctx context.Context, pd *pagesv1alpha1.PagesDeployment) (string, string, error) {
	ref := pd.Spec.ForProvider.ProjectRef.Name
	var pp pagesv1alpha1.PagesProject
	if err := r.Get(ctx, client.ObjectKey{Namespace: pd.Namespace, Name: ref}, &pp); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("PagesProject %s not found", ref), nil
		}
		return "", "", err
	}
	switch {
	case !pp.DeletionTimestamp.IsZero():
		return "", fmt.Sprintf("PagesProject %s is being deleted", ref), nil
	case pp.Spec.AccountRef.Name != pd.Spec.AccountRef.Name:
		return "", fmt.Sprintf("PagesProject %s uses CloudflareAccount %s, not %s", ref, pp.Spec.AccountRef.Name, pd.Spec.AccountRef.Name), nil
	case !reconcile.IsReady(&pp) || pp.Status.ID == "":
		return "", fmt.Sprintf("PagesProject %s is not Ready", ref), nil
	}
	return pp.Status.ID, "", nil
}

// deployHash identifies what a deployment deploys: the artifact's digest and the branch.
func deployHash(digest, branch string) string {
	s := sha256.Sum256([]byte("pages-deploy-v1\x00" + digest + "\x00" + branch))
	return hex.EncodeToString(s[:])
}

// commitHash is the commit hash recorded on a deployment of hash: forProvider.commit_hash,
// else 40 hex digits derived from the object's UID and hash (unique per object and content).
func commitHash(pd *pagesv1alpha1.PagesDeployment, hash string) string {
	if c := pd.Spec.ForProvider.CommitHash; c != "" {
		return c
	}
	return derivedCommit(pd.UID, hash)
}

func derivedCommit(uid types.UID, hash string) string {
	s := sha256.Sum256([]byte("pages-commit-v1\x00" + string(uid) + "\x00" + hash))
	return hex.EncodeToString(s[:20])
}

func (r *Reconciler) sync(ctx context.Context, pd *pagesv1alpha1.PagesDeployment) (ctrl.Result, error) {
	acct, err := r.Accounts.Resolve(ctx, pd)
	if err != nil {
		if reconcile.IsAccountNotReady(err) {
			reconcile.MarkAccountNotReady(pd, err)
			return ctrl.Result{RequeueAfter: reconcile.AccountRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := reconcile.EnsureFinalizer(ctx, r.Client, pd); err != nil {
		return ctrl.Result{}, err
	}
	pol := reconcile.PoliciesOf(pd)
	cf, accountID := acct.Client, acct.AccountID
	project, msg, err := r.project(ctx, pd)
	if err != nil {
		return ctrl.Result{}, err
	}
	if msg != "" {
		return r.blocked(pd, commonv1alpha1.ReasonDependency, msg)
	}
	pd.Status.AtProvider.ProjectName = project
	proj, err := pagesproject.GetProject(ctx, cf, accountID, project)
	if err != nil {
		return r.fail(pd, err)
	}
	if proj == nil {
		return r.blocked(pd, commonv1alpha1.ReasonDependency, fmt.Sprintf("the Pages project %s does not exist in Cloudflare", project))
	}
	canonical := ""
	if proj.CanonicalDeployment != nil {
		canonical = proj.CanonicalDeployment.ID
	}
	if !pol.CanCreate() && !pol.CanUpdate() {
		return r.observeOnly(ctx, pd, cf, accountID, proj, canonical)
	}
	fp := pd.Spec.ForProvider
	if fp.Source == nil {
		return r.blocked(pd, ReasonInvalidArtifact, "forProvider.source is required unless managementPolicies exclude Create and Update")
	}
	tree, err := r.Artifacts.Load(ctx, r.Client, pd.Namespace, *fp.Source)
	if err != nil {
		switch artifact.KindOf(err) {
		case artifact.KindDependency:
			return r.blocked(pd, commonv1alpha1.ReasonDependency, err.Error())
		case artifact.KindInvalid, artifact.KindRejected:
			return r.blocked(pd, ReasonInvalidArtifact, err.Error())
		case artifact.KindFetch:
			reconcile.SetSynced(pd, metav1.ConditionFalse, ReasonArtifactUnavailable, err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}
	hash := deployHash(tree.Digest, fp.Branch)
	status := &sharedv1alpha1.ArtifactStatus{Digest: tree.Digest, ResolvedDigest: tree.ResolvedDigest, Files: int32(tree.Len()), Bytes: tree.Bytes}

	prev := r.last(pd)
	// A deployment announced before a crash and never recorded: find it by its commit hash,
	// when that is the one derived from this object (a user-set one proves nothing).
	if lost, ok := pendingDeploy(pd); ok && !lost.identifies(pd) {
		reconcile.WarnExternalKept(r.Recorder, pd, "Create", lost.unidentifiable())
		if err := reconcile.ClearCreatePending(ctx, r.Client, pd); err != nil {
			return r.fail(pd, fmt.Errorf("clear the pending deployment: %w", err))
		}
	} else if ok && lost.project == project {
		d, err := findByCommit(ctx, cf, accountID, project, lost.commit)
		if err != nil {
			return r.fail(pd, err)
		}
		if d != nil && (d.ID != prev.id || !reconcile.HasOwnershipProof(pd, d.ID)) {
			log.FromContext(ctx).Info("adopted the Pages deployment of an interrupted deploy", "project", project, "deployment", d.ID)
			if err := reconcile.RecordCreated(ctx, r.Client, pd, d.ID); err != nil {
				return r.fail(pd, fmt.Errorf("record the Pages deployment %s made before a restart: %w", d.ID, err))
			}
			a := appliedState{id: d.ID, hash: lost.hash}
			if lost.hash == hash {
				a.digest = status
			}
			r.remember(pd, a)
			prev = r.last(pd)
		}
	}
	var cur *apiDeployment
	if prev.id != "" {
		if cur, err = getDeployment(ctx, cf, accountID, project, prev.id); err != nil {
			return r.fail(pd, err)
		}
	}
	if cur != nil && prev.hash != hash && fp.CommitHash == "" && cur.DeploymentTrigger.Metadata.CommitHash == commitHash(pd, hash) {
		// The deployment carries the commit hash derived from this very content and branch: it
		// deploys what forProvider asks (the status lost its hash, e.g. to a crash after
		// RecordCreated). With a user-set commit_hash this cannot be told, and it deploys again.
		r.remember(pd, appliedState{id: cur.ID, hash: hash, digest: status})
		prev = r.last(pd)
	}
	switch {
	case cur != nil && prev.hash == hash:
		// In sync: follow the deploy stage.
	case cur != nil && !pol.CanUpdate():
		observeDeployment(pd, cur, canonical)
		reconcile.MarkAvailable(pd)
		reconcile.SetSynced(pd, metav1.ConditionFalse, commonv1alpha1.ReasonReconcileError,
			"the artifact or branch changed and managementPolicies do not allow Update (a new deployment)")
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	case cur == nil && prev.id != "" && !pol.CanCreate():
		pd.Status.AtProvider = pagesv1alpha1.PagesDeploymentObservation{ProjectName: project}
		msg := fmt.Sprintf("Pages deployment %s no longer exists and managementPolicies do not allow Create", prev.id)
		reconcile.MarkNotFound(pd, msg)
		reconcile.SetSynced(pd, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	case cur == nil && prev.id == "" && !pol.CanCreate():
		msg := "no deployment exists yet and managementPolicies do not allow Create"
		reconcile.MarkNotFound(pd, msg)
		reconcile.SetSynced(pd, metav1.ConditionFalse, commonv1alpha1.ReasonNotFound, msg)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	default:
		why := "first deployment"
		switch {
		case cur == nil && prev.id != "":
			why = "the deployment " + prev.id + " is gone"
		case cur != nil:
			why = "the artifact or branch changed"
		}
		d, res, err := r.deploy(ctx, pd, acct, project, tree, hash, status, why)
		if d == nil {
			return res, err
		}
		cur = d
	}
	observeDeployment(pd, cur, canonical)
	if prev.hash == hash && pd.Status.Artifact == nil {
		pd.Status.Artifact = status
	}
	switch {
	case cur.deployed():
		reconcile.MarkAvailable(pd)
		reconcile.MarkSynced(pd)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	case cur.failed():
		msg := fmt.Sprintf("the deploy stage of deployment %s ended with %s; change the artifact or branch to deploy again", cur.ID, cur.LatestStage.Status)
		reconcile.SetReady(pd, metav1.ConditionFalse, ReasonDeploymentFailed, msg)
		reconcile.SetSynced(pd, metav1.ConditionFalse, ReasonDeploymentFailed, msg)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	stage := "unknown"
	if cur.LatestStage != nil {
		stage = cur.LatestStage.Name + "/" + cur.LatestStage.Status
	}
	reconcile.SetReady(pd, metav1.ConditionFalse, ReasonDeploying, fmt.Sprintf("deployment %s is at stage %s", cur.ID, stage))
	reconcile.MarkSynced(pd)
	wait := r.stagePoll()
	if created, err := time.Parse(time.RFC3339Nano, cur.CreatedOn); err == nil && time.Since(created) > slowStageAfter && wait < slowStagePoll {
		wait = slowStagePoll // a deploy stage that takes this long is polled less often
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// A deployment whose deploy stage has not finished slowStageAfter after its creation is
// re-read every slowStagePoll instead of every StagePollInterval.
const (
	slowStageAfter = 5 * time.Minute
	slowStagePoll  = time.Minute
)

// deploy uploads tree and creates a deployment. A nil deployment comes with the result to
// return.
func (r *Reconciler) deploy(ctx context.Context, pd *pagesv1alpha1.PagesDeployment, acct *reconcile.Resolved, project string,
	tree *artifact.Tree, hash string, status *sharedv1alpha1.ArtifactStatus, why string) (*apiDeployment, ctrl.Result, error) {
	cf, accountID := acct.Client, acct.AccountID
	baseURL := ""
	if acct.Account != nil {
		baseURL = acct.Account.Spec.BaseURL
	}
	up := &uploader{cf: cf, accountID: accountID, project: project, baseURL: baseURL, cache: &r.jwts,
		newClient: func(token string) (cfclient.Client, error) {
			if acct.Account == nil {
				return cfclient.New(cfclient.Options{Token: token, BaseURL: baseURL})
			}
			return r.Accounts.NewClient(acct.Account, token)
		}}
	maxFiles, err := up.maxFiles(ctx)
	if err != nil {
		res, err := r.fail(pd, err)
		return nil, res, err
	}
	p, perr := planTree(tree, maxFiles)
	if perr != nil {
		res, err := r.blocked(pd, ReasonInvalidArtifact, perr.Error())
		return nil, res, err
	}
	commit := commitHash(pd, hash)
	p.form.branch, p.form.commit, p.form.message = pd.Spec.ForProvider.Branch, commit, pd.Spec.ForProvider.CommitMessage
	// The asset upload creates nothing this object owns (assets are content-addressed and
	// shared by the project), so a failure there leaves no record behind.
	if err := up.upload(ctx, p); err != nil {
		res, err := r.fail(pd, err)
		return nil, res, err
	}
	// Announce the deployment, so that one made before a crash is found by its commit hash.
	if err := reconcile.MarkCreatePending(ctx, r.Client, pd, pendingKey(project, hash, commit)); err != nil {
		res, err := r.fail(pd, fmt.Errorf("record the pending deployment: %w", err))
		return nil, res, err
	}
	log.FromContext(ctx).Info("deploying to Pages", "project", project, "why", why, "files", len(p.assets), "digest", tree.Digest)
	d, err := createDeployment(ctx, cf, accountID, project, p.form)
	if err != nil {
		if reconcile.IsPermanent(err) {
			if cerr := reconcile.ClearCreatePending(ctx, r.Client, pd); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
		res, err := r.fail(pd, fmt.Errorf("create deployment: %w", err))
		return nil, res, err
	}
	r.remember(pd, appliedState{id: d.ID, hash: hash, digest: status})
	if err := reconcile.RecordCreated(ctx, r.Client, pd, d.ID); err != nil {
		res, err := r.fail(pd, fmt.Errorf("record the new Pages deployment %s: %w", d.ID, err))
		return nil, res, err
	}
	log.FromContext(ctx).Info("created Pages deployment", "project", project, "deployment", d.ID, "url", d.URL)
	return d, ctrl.Result{}, nil
}

// findByCommit returns the newest deployment of project whose trigger carries commit.
func findByCommit(ctx context.Context, cf cfclient.Client, accountID, project, commit string) (*apiDeployment, error) {
	ds, err := listDeployments(ctx, cf, accountID, project, "")
	if err != nil {
		return nil, err
	}
	var best *apiDeployment
	for i := range ds {
		d := &ds[i]
		if d.DeploymentTrigger.Metadata.CommitHash == commit && (best == nil || d.CreatedOn > best.CreatedOn) {
			best = d
		}
	}
	return best, nil
}

func observeDeployment(pd *pagesv1alpha1.PagesDeployment, d *apiDeployment, canonical string) {
	a := pagesv1alpha1.PagesDeploymentObservation{ID: d.ID, ShortID: d.ShortID, ProjectName: d.ProjectName, Environment: d.Environment,
		Branch: d.DeploymentTrigger.Metadata.Branch, CommitHash: d.DeploymentTrigger.Metadata.CommitHash, URL: d.URL,
		CreatedOn: d.CreatedOn, ModifiedOn: d.ModifiedOn, Production: canonical != "" && canonical == d.ID}
	if a.ProjectName == "" {
		a.ProjectName = pd.Status.AtProvider.ProjectName
	}
	if len(d.Aliases) > 0 {
		a.Aliases = append([]string(nil), d.Aliases...)
	}
	if s := d.LatestStage; s != nil {
		a.LatestStage = &pagesv1alpha1.PagesStageObservation{Name: s.Name, Status: s.Status}
		if s.StartedOn != nil {
			a.LatestStage.StartedOn = *s.StartedOn
		}
		if s.EndedOn != nil {
			a.LatestStage.EndedOn = *s.EndedOn
		}
	}
	pd.Status.AtProvider = a
}

// observeOnly reads a deployment without writing: the pinned one, else the live production
// deployment (no branch or the production branch), else the newest of the branch.
func (r *Reconciler) observeOnly(ctx context.Context, pd *pagesv1alpha1.PagesDeployment, cf cfclient.Client, accountID string,
	proj *pagesproject.APIProject, canonical string) (ctrl.Result, error) {
	project := proj.Name
	branch := pd.Spec.ForProvider.Branch
	var d *apiDeployment
	var err error
	switch id := pd.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; {
	case id != "":
		d, err = getDeployment(ctx, cf, accountID, project, id)
	case branch == "" || branch == proj.ProductionBranch:
		if canonical != "" {
			d, err = getDeployment(ctx, cf, accountID, project, canonical)
		}
	default:
		var ds []apiDeployment
		if ds, err = listDeployments(ctx, cf, accountID, project, "preview"); err == nil {
			for i := range ds {
				if ds[i].DeploymentTrigger.Metadata.Branch == branch && (d == nil || ds[i].CreatedOn > d.CreatedOn) {
					d = &ds[i]
				}
			}
		}
	}
	if err != nil {
		return r.fail(pd, err)
	}
	if d == nil {
		pd.Status.AtProvider, pd.Status.ID = pagesv1alpha1.PagesDeploymentObservation{ProjectName: project}, ""
		reconcile.MarkNotFound(pd, fmt.Sprintf("no such Pages deployment of project %s", project))
		reconcile.MarkSynced(pd)
		return ctrl.Result{RequeueAfter: r.resync()}, nil
	}
	observeDeployment(pd, d, canonical)
	pd.Status.ID = d.ID
	reconcile.MarkAvailable(pd)
	reconcile.MarkSynced(pd)
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// finalize runs the deletion flow (package doc). gone reports that the finalizer was removed.
func (r *Reconciler) finalize(ctx context.Context, pd *pagesv1alpha1.PagesDeployment) (ctrl.Result, bool, error) {
	if !controllerutil.ContainsFinalizer(pd, commonv1alpha1.Finalizer) {
		return ctrl.Result{}, true, nil
	}
	logger := log.FromContext(ctx)
	deleteExternal := reconcile.ShouldDeleteExternal(pd, commonv1alpha1.DeletionDelete)
	project := pd.Status.AtProvider.ProjectName
	if lost, ok := pendingDeploy(pd); ok && deleteExternal && !lost.identifies(pd) {
		// Its commit hash was set by the user and may be any deployment's: nothing to look up.
		reconcile.WarnExternalKept(r.Recorder, pd, "Delete", lost.unidentifiable())
	} else if ok && deleteExternal {
		if project == "" {
			project = lost.project
		}
		// A deployment was announced and its ID never recorded: find it by its commit hash and
		// record it, so it is deleted rather than leaked.
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, pd, "Delete",
			fmt.Sprintf("the Pages deployment this object may have made in project %s before a restart was not looked up and may be left in Cloudflare", lost.project))
		if err == nil && acct != nil {
			gone := func(ctx context.Context, id string) (bool, error) {
				d, err := getDeployment(ctx, acct.Client, acct.AccountID, lost.project, id)
				return d == nil && err == nil, err
			}
			_, err = reconcile.AdoptPendingCreateReplacing(ctx, r.Client, r.Recorder, pd, "Pages deployment", gone, func(ctx context.Context, _ string) (string, error) {
				d, err := findByCommit(ctx, acct.Client, acct.AccountID, lost.project, lost.commit)
				if d == nil || err != nil {
					return "", err
				}
				return d.ID, nil
			})
		}
		if err != nil {
			res, err := reconcile.DeletionResult(pd, err)
			return res, false, err
		}
	}
	id := reconcile.ExternalID(pd)
	var del func(context.Context, string) error
	if id != "" && project != "" && deleteExternal {
		acct, err := reconcile.FinalizeAccount(ctx, r.Accounts, r.apiReader(), r.Recorder, pd, "Delete",
			fmt.Sprintf("Pages deployment %s of project %s was left in Cloudflare despite deletionPolicy Delete", id, project))
		if err != nil {
			res, err := reconcile.DeletionResult(pd, err)
			return res, false, err
		}
		if acct != nil {
			exists := func(ctx context.Context) (bool, error) {
				d, err := getDeployment(ctx, acct.Client, acct.AccountID, project, id)
				return d != nil, err
			}
			// Deployments cannot be tagged: ownership is the create record or the pin.
			dec, err := reconcile.MayDeleteExternal(ctx, reconcile.NoopTagger{}, acct.Client, acct.AccountID,
				reconcile.TagTarget{Type: "pages_deployment", ID: id}, "", pd, id, exists)
			if err != nil {
				res, err := reconcile.DeletionResult(pd, err)
				return res, false, err
			}
			switch {
			case dec.Gone:
				logger.Info("the Pages deployment is already gone", "project", project, "deployment", id)
			case !dec.Delete:
				reconcile.WarnExternalKept(r.Recorder, pd, "Delete",
					fmt.Sprintf("Pages deployment %s was left in Cloudflare despite deletionPolicy Delete: %s", id, dec.Why))
			default:
				live, err := r.isLive(ctx, acct, project, id)
				if err != nil {
					res, err := reconcile.DeletionResult(pd, err)
					return res, false, err
				}
				if live {
					r.keepLive(ctx, pd, project, id)
					break
				}
				del = func(ctx context.Context, id string) error {
					err := deleteDeployment(ctx, acct.Client, acct.AccountID, project, id)
					if err != nil && !cfclient.IsNotFound(err) && reconcile.IsPermanent(err) {
						// Refused: kept when it became the live production deployment meanwhile
						// (the refusal's error code is UNVERIFIED, so the project is re-read).
						if live, lerr := r.isLive(ctx, acct, project, id); lerr == nil && live {
							r.keepLive(ctx, pd, project, id)
							return nil
						}
					}
					if err == nil {
						log.FromContext(ctx).Info("deleted Pages deployment", "project", project, "deployment", id)
					}
					return err
				}
			}
		}
	}
	if c := reconcile.GetCondition(pd, commonv1alpha1.ConditionReady); c == nil || c.Reason != ReasonLiveDeploymentKept {
		reconcile.MarkDeleting(pd, "")
	}
	res, err := reconcile.Finalize(ctx, r.Client, pd, commonv1alpha1.DeletionDelete, del)
	gone := !controllerutil.ContainsFinalizer(pd, commonv1alpha1.Finalizer)
	if gone {
		r.applied.Delete(client.ObjectKeyFromObject(pd))
	}
	return res, gone, err
}

// isLive reports whether id is the project's live production deployment (canonical_deployment).
func (r *Reconciler) isLive(ctx context.Context, acct *reconcile.Resolved, project, id string) (bool, error) {
	p, err := pagesproject.GetProject(ctx, acct.Client, acct.AccountID, project)
	if err != nil || p == nil {
		return false, err
	}
	return p.CanonicalDeployment != nil && p.CanonicalDeployment.ID == id, nil
}

func (r *Reconciler) keepLive(ctx context.Context, pd *pagesv1alpha1.PagesDeployment, project, id string) {
	msg := fmt.Sprintf("Pages deployment %s is the live production deployment of project %s, which Cloudflare does not delete; "+
		"it is kept (deploy another production deployment, or delete the project, to remove it)", id, project)
	log.FromContext(ctx).Info("keeping the live production deployment", "project", project, "deployment", id)
	reconcile.SetReady(pd, metav1.ConditionFalse, ReasonLiveDeploymentKept, msg)
	reconcile.WarnExternalKept(r.Recorder, pd, "Delete", msg)
}

// pending is a create-pending record of a deployment.
type pending struct{ project, hash, commit string }

// identifies reports whether the record's commit hash identifies the deployment it announced:
// only the one derived from pd's UID does. A user-set forProvider.commit_hash is a git commit,
// which other deployments carry too (another PagesDeployment of the commit, a CI run of
// `wrangler pages deploy --commit-hash`), so a deployment found by it is not proven pd's.
func (p pending) identifies(pd *pagesv1alpha1.PagesDeployment) bool {
	return p.commit == derivedCommit(pd.UID, p.hash)
}

// unidentifiable is the Warning of a record that does not identify its deployment.
func (p pending) unidentifiable() string {
	return fmt.Sprintf("a Pages deployment this object may have made in project %s before a restart or failed request carries "+
		"the user-set commit hash %s, which does not identify it among the project's deployments; it is not looked up and "+
		"may be left in Cloudflare (leave forProvider.commit_hash unset to have lost deployments found again)", p.project, p.commit)
}

func pendingKey(project, hash, commit string) string {
	return fmt.Sprintf("%s;k=%s;c=%s", project, hash, commit)
}

func pendingDeploy(pd *pagesv1alpha1.PagesDeployment) (pending, bool) {
	key, ok := reconcile.PendingCreate(pd)
	if !ok {
		return pending{}, false
	}
	parts := strings.Split(key, ";")
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "k=") || !strings.HasPrefix(parts[2], "c=") {
		return pending{}, false
	}
	return pending{project: parts[0], hash: parts[1][2:], commit: parts[2][2:]}, true
}
