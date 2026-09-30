package testenv

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"flare.dev/operator/internal/reconcile"
)

// LaggingClient is a reconciler's client whose Get serves a stale view of objects: the lag
// edits every copy Get returns, as an informer cache that has not seen the reconciler's latest
// writes yet would. Writes and a reconciler's APIReader (the embedded client itself) go to the
// API server. Crash-consistency tests use it to show that a decision is read uncached
// (reconcile.FreshCreatePending).
type LaggingClient struct {
	client.Client
	// Lists, when set, serves List instead (the manager's cached client, for field indexes).
	Lists client.Reader

	mu   sync.Mutex
	lag  func(client.Object)
	seen map[types.UID]map[string]string
}

// SetLag sets (nil: clears) the edit applied to every object Get returns.
func (c *LaggingClient) SetLag(lag func(client.Object)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lag = lag
}

// Get reads through the embedded client and applies the lag.
func (c *LaggingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	c.mu.Lock()
	lag := c.lag
	c.mu.Unlock()
	if lag != nil {
		lag(obj)
	}
	return nil
}

// List lists through Lists when set, else the embedded client.
func (c *LaggingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.Lists != nil {
		return c.Lists.List(ctx, list, opts...)
	}
	return c.Client.List(ctx, list, opts...)
}

// Patch patches through the embedded client and remembers the annotations of the answer.
func (c *LaggingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[types.UID]map[string]string{}
	}
	s := c.seen[obj.GetUID()]
	if s == nil {
		s = map[string]string{}
		c.seen[obj.GetUID()] = s
	}
	for k, v := range obj.GetAnnotations() {
		s[k] = v
	}
	return nil
}

// LastWritten is the last value of annotation k that a Patch of obj (by UID) answered with
// ("" when none did): a value the reconciler wrote even if it dropped it again since.
func (c *LaggingClient) LastWritten(obj client.Object, k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[obj.GetUID()][k]
}

// SetAnnotation sets (v != "") or removes annotation k of o.
func SetAnnotation(o client.Object, k, v string) {
	a := maps.Clone(o.GetAnnotations())
	if a == nil {
		a = map[string]string{}
	}
	if v == "" {
		delete(a, k)
	} else {
		a[k] = v
	}
	o.SetAnnotations(a)
}

// LagAnnotation returns a lag that shows annotation k of obj (matched by UID) as v ("":
// absent); other objects are served as they are.
func LagAnnotation(obj client.Object, k, v string) func(client.Object) {
	uid := obj.GetUID()
	return func(o client.Object) {
		if o.GetUID() == uid {
			SetAnnotation(o, k, v)
		}
	}
}

// DirectAccounts is a reconcile.Accounts for a reconciler driven outside the manager
// (CloudflareAccounts point at the in-process flarefake).
func (e *Env) DirectAccounts(opts ...reconcile.AccountsOption) *reconcile.Accounts {
	return reconcile.NewAccounts(e.Client, append([]reconcile.AccountsOption{reconcile.WithUserAgent("flare-operator-testenv"),
		reconcile.WithBaseURLPolicy(reconcile.BaseURLPolicy{AllowAny: true})}, opts...)...)
}

// ReconcileDirect runs one reconcile of obj with r (a reconciler driven outside the manager)
// and re-reads obj from the API server (an object that is gone keeps its last state); it
// returns the reconcile's error.
func (e *Env) ReconcileDirect(t testing.TB, r crreconcile.Reconciler, obj client.Object) error {
	t.Helper()
	ctx := Context(t, 20*time.Second)
	_, err := r.Reconcile(ctx, crreconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	if gerr := e.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj); client.IgnoreNotFound(gerr) != nil {
		t.Fatal(gerr)
	}
	return err
}
