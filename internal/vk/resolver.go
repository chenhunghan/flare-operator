package vk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workersv1alpha1 "github.com/chenhunghan/flare-operator/api/workers/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/reconcile"
	"github.com/chenhunghan/flare-operator/internal/workerlogs"
)

// ReadOnly hides every method of r but those of client.Reader. reconcile.Accounts labels the
// objects it resolves when its reader is also a client.Writer; the virtual kubelet must never
// write WorkerScripts, so it hands Accounts a ReadOnly reader (Resolve then labels in memory
// only, on a copy).
func ReadOnly(r client.Reader) client.Reader { return readOnly{r} }

type readOnly struct{ r client.Reader }

func (o readOnly) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return o.r.Get(ctx, key, obj, opts...)
}

func (o readOnly) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return o.r.List(ctx, list, opts...)
}

// BudgetClientFactory returns a reconcile.ClientFactory that builds clients with the virtual
// kubelet's own Cloudflare budget: budget requests per 5 minutes per token (in this process;
// the manager's limiter is another process), whatever the account's spec.rateLimit says. The
// account's retry settings are kept.
func BudgetClientFactory(budget int, next reconcile.ClientFactory) reconcile.ClientFactory {
	if next == nil {
		next = cfclient.New
	}
	return func(o cfclient.Options) (cfclient.Client, error) {
		o.RPS = float64(budget) / 300
		o.Burst = min(cfclient.DefaultBurst, budget)
		return next(o)
	}
}

// Resolver is the PodResolver over the manager's cache: Pods (restricted to the node),
// WorkerScripts and CloudflareAccounts come from it; accounts are resolved by a
// reconcile.Accounts built on ReadOnly(reader), whose token Secrets are read uncached.
type Resolver struct {
	reader   client.Reader
	accounts *reconcile.Accounts
	nodeName string
	// namespaces limits the namespaces served (nil: all).
	namespaces labels.Selector
}

// errNamespaceNotServed: the Pod's namespace does not match the namespace selector.
var errNamespaceNotServed = fmt.Errorf("%w: the namespace is outside the virtual kubelet's namespace selector", ErrPodNotFound)

// WithNamespaceSelector limits the Resolver to namespaces whose labels match sel (the stand-in
// controller's selector): a Pod elsewhere is not served, even if it looks like a stand-in. A nil
// or empty selector serves every namespace.
func (r *Resolver) WithNamespaceSelector(sel labels.Selector) *Resolver {
	if sel != nil && !sel.Empty() {
		r.namespaces = sel
	}
	return r
}

var _ PodResolver = (*Resolver)(nil)

// NewResolver returns a Resolver. accounts must have been built over ReadOnly(...) (it must not
// label WorkerScripts); NewResolver checks nothing about it.
func NewResolver(reader client.Reader, accounts *reconcile.Accounts, nodeName string) *Resolver {
	return &Resolver{reader: reader, accounts: accounts, nodeName: nodeName}
}

// Resolve implements PodResolver.
func (r *Resolver) Resolve(ctx context.Context, namespace, name, container string) (workerlogs.Target, error) {
	if r.namespaces != nil {
		var ns corev1.Namespace
		if err := r.reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			if apierrors.IsNotFound(err) {
				return workerlogs.Target{}, ErrPodNotFound
			}
			return workerlogs.Target{}, err
		}
		if !r.namespaces.Matches(labels.Set(ns.Labels)) {
			return workerlogs.Target{}, errNamespaceNotServed
		}
	}
	var pod corev1.Pod
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return workerlogs.Target{}, ErrPodNotFound
		}
		return workerlogs.Target{}, err
	}
	if pod.Spec.NodeName != r.nodeName {
		return workerlogs.Target{}, ErrPodNotFound
	}
	cached, err := standInWorkerScript(ctx, r.reader, &pod)
	if err != nil {
		if errors.Is(err, errWorkerScriptMissing) {
			return workerlogs.Target{}, fmt.Errorf("%w (%s)", ErrNotStandIn, err.Error())
		}
		return workerlogs.Target{}, err
	}
	if container != workersv1alpha1.StandInContainerName {
		return workerlogs.Target{}, ErrContainerNotFound
	}
	// Resolve sets the account label on its argument (in memory: the reader cannot write), so
	// hand it a copy, never the cache's object.
	ws := cached.DeepCopy()
	res, err := r.accounts.Resolve(ctx, ws)
	if err != nil {
		return workerlogs.Target{}, err
	}
	return workerlogs.Target{
		Client:            res.Client,
		AccountID:         res.AccountID,
		Script:            ws.ScriptName(),
		WorkerScript:      types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name},
		AllowInsecureTail: baseURLOverridden(res.Account.Spec.BaseURL),
	}, nil
}

// baseURLOverridden reports whether an (allowed: Resolve refuses the others) spec.baseURL points
// somewhere other than Cloudflare, e.g. flarefake, whose tail URLs may be ws://.
func baseURLOverridden(base string) bool {
	b := strings.TrimRight(base, "/")
	return b != "" && b != cfclient.DefaultBaseURL
}
