package generic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/controller"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/generic/kinds"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

var env *testenv.Env

// TestMain starts envtest and an in-process flarefake. Outside -short the fake validates every
// request against the pinned spec, and the tests fail on violations other than the known spec
// defects (internal/generic/descriptors/emulator_test.go).
func TestMain(m *testing.M) {
	flag.Parse()
	opts := testenv.Options{}
	if !testing.Short() {
		spec, err := fake.LoadDefaultSpec()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load spec:", err)
			os.Exit(1)
		}
		opts.Fake.Spec = spec
	}
	testenv.Main(m, &env, opts)
}

// poll is the drift-detection interval of the reconcilers under test.
const poll = 250 * time.Millisecond

// recorder captures every request the operator sends (method, path, body).
type recorder struct {
	mu   sync.Mutex
	reqs []request
}

type request struct {
	Method, Path string
	Body         []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, request{Method: req.Method, Path: strings.TrimPrefix(req.URL.Path, "/client/v4"), Body: body})
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (r *recorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *recorder) since(n int) []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.reqs[n:]...)
}

func writesOf(rs []request) []request {
	var out []request
	for _, r := range rs {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func summary(rs []request) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%s %s %s\n", r.Method, r.Path, r.Body)
	}
	return b.String()
}

type harness struct {
	t    *testing.T
	e    *testenv.Env
	ns   string
	acct *testenv.Account
	cf   cfclient.Client // the test's own API client (not recorded)
	rec  *recorder
	// journal0 is the journal length when the harness started.
	journal0 int
}

// Which controllers a harness's manager runs besides the account controller.
type controllers int

const (
	// recorded: a generic reconciler per generated kind, poll interval `poll`, requests recorded.
	recorded controllers = iota
	// registered: the controllers as registered for cmd/manager (default settings).
	registered
	// accountOnly: no generic controllers (tests call Reconcile themselves).
	accountOnly
)

// newHarness starts a manager (see controllers) plus a Ready account in a fresh namespace.
func newHarness(t *testing.T, which controllers) *harness {
	t.Helper()
	e := testenv.Require(t, env)
	h := &harness{t: t, e: e, rec: &recorder{}}
	o := testenv.ManagerOptions{}
	switch which {
	case registered:
		o.Controllers = kinds.Names()
	case recorded:
		o.Setup = []func(ctrl.Manager, controller.Deps) error{func(m ctrl.Manager, d controller.Deps) error {
			d.Accounts = reconcile.NewAccounts(m.GetClient(), reconcile.WithHTTPClient(&http.Client{Transport: h.rec}))
			for _, en := range descriptors.Entries() {
				r := kinds.NewReconciler(en, m.GetClient(), d)
				r.PollInterval = poll
				if err := r.SetupWithManager(m, kinds.Name(en)); err != nil {
					return err
				}
			}
			return nil
		}}
	}
	e.StartManager(t, o)
	h.ns = e.Namespace(t)
	h.acct = e.CreateReadyAccount(t, h.ns, "acct")
	cf, err := cfclient.New(cfclient.Options{Token: h.acct.Token, BaseURL: e.BaseURL, RPS: 1000, Burst: 1000, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	h.cf = cf
	h.journal0 = len(e.Journal(t))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("operator requests:\n%s", summary(h.rec.since(0)))
		}
	})
	return h
}

func (h *harness) ctx() context.Context { return testenv.Context(h.t, 30*time.Second) }

func (h *harness) path(p, id string) string {
	return strings.NewReplacer("{account_id}", h.acct.AccountID, "{id}", id).Replace(p)
}

// api calls the fake directly (as someone outside the operator would).
func (h *harness) api(method, p string, body any) (map[string]any, error) {
	h.t.Helper()
	resp, err := h.cf.Do(h.ctx(), cfclient.Request{Method: method, Path: p, Body: body})
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(resp.Result) > 0 && string(resp.Result) != "null" {
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (h *harness) mustAPI(method, p string, body any) map[string]any {
	h.t.Helper()
	out, err := h.api(method, p, body)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, p, err)
	}
	return out
}

// createExternal creates a resource of the kind through the API from forProvider JSON and
// returns its ID.
func (h *harness) createExternal(en descriptors.Entry, forProvider string) string {
	h.t.Helper()
	var fp map[string]any
	if err := json.Unmarshal([]byte(forProvider), &fp); err != nil {
		h.t.Fatal(err)
	}
	body := map[string]any{}
	for _, f := range en.CreateFields {
		if v, ok := fp[f]; ok {
			body[f] = v
		}
	}
	res := h.mustAPI(http.MethodPost, h.path(en.CreatePath, ""), body)
	id, _ := res[en.IDField].(string)
	if id == "" {
		h.t.Fatalf("created %s has no %s: %v", en.Kind, en.IDField, res)
	}
	return id
}

// newObj builds an object of the kind from a spec JSON (accountRef is filled in).
func (h *harness) newObj(en descriptors.Entry, name, spec string) reconcile.ManagedObject {
	h.t.Helper()
	obj := en.New().(reconcile.ManagedObject)
	if err := json.Unmarshal([]byte(`{"spec":`+spec+`}`), obj); err != nil {
		h.t.Fatalf("spec %s: %v", spec, err)
	}
	obj.SetNamespace(h.ns)
	obj.SetName(name)
	obj.GetResourceSpec().AccountRef.Name = "acct"
	return obj
}

func (h *harness) create(obj reconcile.ManagedObject) {
	h.t.Helper()
	if err := h.e.Client.Create(h.ctx(), obj); err != nil {
		h.t.Fatalf("create %s: %v", obj.GetName(), err)
	}
}

func (h *harness) get(obj reconcile.ManagedObject) error {
	return h.e.Client.Get(h.ctx(), client.ObjectKeyFromObject(obj), obj)
}

// setForProvider replaces spec.forProvider (retrying on conflicts with the controller).
func (h *harness) setForProvider(obj reconcile.ManagedObject, forProvider string) {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := h.get(obj); err != nil {
			return err
		}
		v := reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("ForProvider")
		v.Set(reflect.Zero(v.Type()))
		if err := json.Unmarshal([]byte(forProvider), v.Addr().Interface()); err != nil {
			return err
		}
		return h.e.Client.Update(h.ctx(), obj)
	})
	if err != nil {
		h.t.Fatalf("update forProvider: %v", err)
	}
}

func (h *harness) delete(obj reconcile.ManagedObject) {
	h.t.Helper()
	if err := h.e.Client.Delete(h.ctx(), obj); err != nil {
		h.t.Fatalf("delete: %v", err)
	}
}

func (h *harness) waitGone(obj reconcile.ManagedObject) {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		err := h.get(obj)
		return apierrors.IsNotFound(err), fmt.Sprintf("still there (err %v, finalizers %v)", err, obj.GetFinalizers())
	})
}

func toMap(t testing.TB, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func atProvider(t testing.TB, obj any) map[string]any {
	st, _ := toMap(t, obj)["status"].(map[string]any)
	ap, _ := st["atProvider"].(map[string]any)
	return ap
}

// waitFor polls obj until cond holds for the current generation's state.
func (h *harness) waitFor(obj reconcile.ManagedObject, what string, cond func() (bool, string)) {
	h.t.Helper()
	testenv.Eventually(h.t, 30*time.Second, func() (bool, string) {
		if err := h.get(obj); err != nil {
			return false, err.Error()
		}
		ok, msg := cond()
		return ok, what + ": " + msg + "\n" + conditions(obj)
	})
}

func conditions(obj reconcile.ManagedObject) string {
	var b strings.Builder
	for _, c := range obj.GetResourceStatus().Conditions {
		fmt.Fprintf(&b, "%s=%s %s %q (gen %d/%d); ", c.Type, c.Status, c.Reason, c.Message, c.ObservedGeneration, obj.GetGeneration())
	}
	return b.String()
}

func condIs(obj reconcile.ManagedObject, typ string, st metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(obj.GetResourceStatus().Conditions, typ)
	return c != nil && c.Status == st && (reason == "" || c.Reason == reason) && c.ObservedGeneration == obj.GetGeneration()
}

// waitSynced waits for Ready=True and Synced=True for the current generation, with atProvider
// covering the desired non-write-only fields.
func (h *harness) waitSynced(obj reconcile.ManagedObject, en descriptors.Entry) {
	h.t.Helper()
	h.waitFor(obj, "Ready and Synced", func() (bool, string) {
		if !condIs(obj, commonv1alpha1.ConditionReady, metav1.ConditionTrue, "") ||
			!condIs(obj, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, "") ||
			obj.GetResourceStatus().ObservedGeneration != obj.GetGeneration() {
			return false, "conditions"
		}
		desired, err := generic.ForProvider(obj)
		if err != nil {
			return false, err.Error()
		}
		for _, f := range en.WriteOnly {
			delete(desired, f)
		}
		ap := atProvider(h.t, obj)
		if !generic.Covers(desired, ap) {
			return false, fmt.Sprintf("atProvider %v does not match forProvider %v", ap, desired)
		}
		return true, ""
	})
}

// journal returns the fake's journal entries for this harness's account since start.
func (h *harness) journal() []fake.JournalEntry {
	return testenv.ForAccount(h.e.Journal(h.t)[h.journal0:], h.acct.AccountID)
}

// assertNoWrites waits d and fails if the operator wrote anything to Cloudflare meanwhile
// (checked in the flarefake journal), while it did observe (GETs of the item).
func (h *harness) assertNoWrites(what string, d time.Duration, itemPath string) {
	h.t.Helper()
	n := len(h.e.Journal(h.t))
	time.Sleep(d)
	j := testenv.ForAccount(h.e.Journal(h.t)[n:], h.acct.AccountID)
	if w := testenv.Writes(j); len(w) != 0 {
		h.t.Errorf("%s: %d Cloudflare writes, want 0:\n%s", what, len(w), testenv.Summary(w))
	}
	if itemPath != "" && testenv.Count(j, http.MethodGet, itemPath) == 0 {
		h.t.Errorf("%s: no GET %s in %v: the object was not re-observed", what, itemPath, d)
	}
}

// ownerTag returns the owner tag of a resource ("" when untagged).
func (h *harness) ownerTag(typ, id string) string {
	h.t.Helper()
	tags, _, err := reconcile.ResourceTagger{}.Get(h.ctx(), h.cf, h.acct.AccountID, reconcile.TagTarget{Type: typ, ID: id})
	if err != nil {
		h.t.Fatalf("get tags: %v", err)
	}
	return tags[reconcile.OwnerTagKey]
}

// checkSpecViolations fails on requests of this harness's account that violate the pinned
// spec, except the known spec defects.
func (h *harness) checkSpecViolations() {
	h.t.Helper()
	known := []struct{ method, contains string }{
		{http.MethodPost, "primary_location_hint"},                                                      // spec enum is lower-case; the API wants upper-case (0019)
		{"", `parameter "database_id" in path has an error: input matches more than one oneOf schemas`}, // 0020
		{http.MethodDelete, "request body has an error: value is required but missing"},                 // KV DELETE (0013)
	}
	for _, j := range h.journal() {
		if j.SchemaViolation == "" {
			continue
		}
		ok := false
		for _, k := range known {
			ok = ok || ((k.method == "" || k.method == j.Method) && strings.Contains(j.SchemaViolation, k.contains))
		}
		if !ok {
			h.t.Errorf("request violates the pinned spec: %s %s: %s", j.Method, j.Path, j.SchemaViolation)
		}
	}
}

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

func randName(prefix string) string { return prefix + "-" + testenv.RandomHex(4) }
