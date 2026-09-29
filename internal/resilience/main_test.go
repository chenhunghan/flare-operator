package resilience_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/generic/kindsuite"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

// TestMain starts envtest and an in-process flarefake. Outside -short the fake also serves the
// generic-profile kinds (VectorizeIndex, SecretsStore, AIGateway), which need the pinned spec.
func TestMain(m *testing.M) {
	flag.Parse()
	generic.ReferrerRetry = 500 * time.Millisecond
	opts := testenv.Options{}
	if !testing.Short() {
		spec, err := fake.LoadDefaultSpec()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load spec:", err)
			os.Exit(1)
		}
		opts.Fake.Spec = spec
		opts.Fake.Generic = fake.GeneratedGenericKinds()
	}
	testenv.Main(m, &env, opts)
}

// entry returns the generated kind's descriptor entry.
func entry(t testing.TB, kind string) descriptors.Entry {
	t.Helper()
	for _, e := range descriptors.Entries() {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no generated kind %s", kind)
	return descriptors.Entry{}
}

// needsGenericProfile skips kinds the fake serves only with the pinned spec (-short).
func needsGenericProfile(t testing.TB, e descriptors.Entry) {
	t.Helper()
	if testing.Short() && kindsuite.IsGeneric(e) {
		t.Skip("the generic flarefake profile needs the pinned spec (skipped with -short)")
	}
}

var (
	schemaMu sync.Mutex
	schemas  = map[string]*kindsuite.Schema{}
)

// forProvider is a minimal valid forProvider of a generated kind with the unique name
// (kindsuite's fixture when it has one, else synthesized from the CRD schema).
func forProvider(t testing.TB, e descriptors.Entry, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testenv.RepoRoot(), "internal", "generic", "kindsuite", "testdata", e.Kind+".json"))
	if err == nil {
		var fx kindsuite.Fixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		if err := json.Unmarshal([]byte(strings.ReplaceAll(string(fx.Create), "{name}", name)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	schemaMu.Lock()
	s := schemas[e.Kind]
	if s == nil {
		if s, err = kindsuite.LoadSchema(testenv.RepoRoot(), e); err != nil {
			schemaMu.Unlock()
			t.Fatal(err)
		}
		schemas[e.Kind] = s
	}
	schemaMu.Unlock()
	fp, err := s.Synthesize(name)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// newGeneric builds (does not create) an object of a generated kind in ns.
func newGeneric(t testing.TB, e descriptors.Entry, ns, name string, fp map[string]any, policy string) reconcile.ManagedObject {
	t.Helper()
	obj := e.New().(reconcile.ManagedObject)
	spec := map[string]any{"accountRef": map[string]any{"name": "acct"}, "forProvider": fp}
	if policy != "" {
		spec["deletionPolicy"] = policy
	}
	b, _ := json.Marshal(map[string]any{"spec": spec})
	if err := json.Unmarshal(b, obj); err != nil {
		t.Fatal(err)
	}
	obj.SetNamespace(ns)
	obj.SetName(name)
	return obj
}

// apiClient talks to the fake as the account (for assertions; its calls are journaled too).
func apiClient(t testing.TB, e *testenv.Env, acct *testenv.Account) cfclient.Client {
	t.Helper()
	c, err := cfclient.New(cfclient.Options{Token: acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// listField lists path and returns the values of field of every item.
func listField(t testing.TB, c cfclient.Client, path, field string) []string {
	t.Helper()
	items, err := cfclient.ListAllInto[map[string]any](testenv.Context(t, 20*time.Second), c, cfclient.Request{Path: path})
	if err != nil {
		t.Fatalf("list %s: %v", path, err)
	}
	var out []string
	for _, it := range items {
		if v, ok := it[field].(string); ok {
			out = append(out, v)
		}
	}
	return out
}

func count(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}

func condString(obj commonv1alpha1.Managed) string {
	var b strings.Builder
	for _, c := range obj.GetResourceStatus().Conditions {
		fmt.Fprintf(&b, "%s=%s/%s %q (gen %d); ", c.Type, c.Status, c.Reason, c.Message, c.ObservedGeneration)
	}
	return b.String()
}

func condIs(obj commonv1alpha1.Managed, typ string, st metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(obj.GetResourceStatus().Conditions, typ)
	return c != nil && c.Status == st && (reason == "" || c.Reason == reason) && c.ObservedGeneration == obj.GetGeneration()
}

// waitReadySynced waits until obj is Ready and Synced for its generation and returns it fresh.
func waitReadySynced(t testing.TB, e *testenv.Env, obj reconcile.ManagedObject, timeout time.Duration) {
	t.Helper()
	testenv.Eventually(t, timeout, func() (bool, string) {
		if err := e.Client.Get(testenv.Context(t, 10*time.Second), client.ObjectKeyFromObject(obj), obj); err != nil {
			return false, err.Error()
		}
		ok := condIs(obj, commonv1alpha1.ConditionReady, metav1.ConditionTrue, "") &&
			condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "")
		return ok, obj.GetName() + ": " + condString(obj)
	})
}

// accountJournal is the fake's journal for one account.
func accountJournal(t testing.TB, e *testenv.Env, accountID string) []fake.JournalEntry {
	return testenv.ForAccount(e.Journal(t), accountID)
}

// errCrashed is what every call of a crashed client returns.
var errCrashed = errors.New("simulated crash: the manager process is gone")

// crashClient wraps a reconciler's Kubernetes client. It crashes at the first patch of the
// target object that writes the external-id annotation, i.e. right after the Cloudflare create
// and before its ID reaches the API server (reconcile.RecordCreated): that patch and every later
// call fail, as if the process had died there. Crashed is closed at that moment.
type crashClient struct {
	client.Client
	target  string
	mu      sync.Mutex
	dead    bool
	Crashed chan struct{}
}

func newCrashClient(c client.Client, target string) *crashClient {
	return &crashClient{Client: c, target: target, Crashed: make(chan struct{})}
}

func (c *crashClient) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *crashClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.isDead() {
		return errCrashed
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *crashClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.isDead() {
		return errCrashed
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *crashClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.isDead() {
		return errCrashed
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *crashClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.isDead() {
		return errCrashed
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *crashClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.isDead() {
		return errCrashed
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *crashClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return errCrashed
	}
	if obj.GetName() == c.target {
		if data, err := patch.Data(obj); err == nil && strings.Contains(string(data), commonv1alpha1.AnnotationExternalID) {
			c.dead = true
			close(c.Crashed)
			c.mu.Unlock()
			return errCrashed
		}
	}
	c.mu.Unlock()
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *crashClient) Status() client.SubResourceWriter {
	return &crashStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type crashStatus struct {
	client.SubResourceWriter
	c *crashClient
}

func (s *crashStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if s.c.isDead() {
		return errCrashed
	}
	return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (s *crashStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if s.c.isDead() {
		return errCrashed
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// waitCrash waits until the crash client crashed.
func waitCrash(t testing.TB, c *crashClient, timeout time.Duration, why func() string) {
	t.Helper()
	select {
	case <-c.Crashed:
	case <-time.After(timeout):
		t.Fatalf("no crash within %v (the create never reached RecordCreated): %s", timeout, why())
	}
}
