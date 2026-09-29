package resilience_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	_ "flare.dev/operator/internal/generic/kinds" // registers the kvnamespace controller
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// kvEnv is one fault test: a namespace-scoped manager running the KVNamespace controller and a
// Ready account.
type kvEnv struct {
	t    *testing.T
	e    *testenv.Env
	ns   string
	acct *testenv.Account
	cf   cfclient.Client
	mgr  *testenv.Manager
}

type kvOpts struct {
	mo        testenv.ManagerOptions
	rateLimit *cloudflarev1alpha1.RateLimitSpec
}

func startKV(t *testing.T, o kvOpts) *kvEnv {
	t.Helper()
	e := testenv.Require(t, env)
	k := &kvEnv{t: t, e: e, ns: e.Namespace(t)}
	o.mo.Controllers = append(o.mo.Controllers, "kvnamespace")
	o.mo.Namespaces = []string{k.ns}
	if o.mo.PollInterval == 0 {
		o.mo.PollInterval = time.Hour
	}
	k.mgr = e.StartManager(t, o.mo)
	k.acct = e.CreateAccount(t, k.ns, "acct", testenv.AccountOptions{RateLimit: o.rateLimit})
	k.acct.CloudflareAccount = e.WaitAccountCondition(t, k.ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	k.cf = apiClient(t, e, k.acct)
	return k
}

func (k *kvEnv) kvPath() string { return "/accounts/" + k.acct.AccountID + "/storage/kv/namespaces" }

func (k *kvEnv) fault(f fake.Fault) {
	k.t.Helper()
	if err := k.e.Fake.InjectFault(f); err != nil {
		k.t.Fatal(err)
	}
}

func (k *kvEnv) journal() []fake.JournalEntry { return accountJournal(k.t, k.e, k.acct.AccountID) }

// createKV creates n KVNamespaces obj-0…obj-<n-1> with unique titles; it returns them and their titles.
func (k *kvEnv) createKV(n int, prefix string) ([]reconcile.ManagedObject, []string) {
	k.t.Helper()
	en := entry(k.t, "KVNamespace")
	var objs []reconcile.ManagedObject
	var titles []string
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("%s-%s-%d", prefix, testenv.RandomHex(3), i)
		o := newGeneric(k.t, en, k.ns, fmt.Sprintf("obj-%d", i), map[string]any{"title": title}, "Delete")
		if err := k.e.Client.Create(testenv.Context(k.t, 10*time.Second), o); err != nil {
			k.t.Fatal(err)
		}
		objs, titles = append(objs, o), append(titles, title)
	}
	return objs, titles
}

func (k *kvEnv) waitAll(objs []reconcile.ManagedObject, timeout time.Duration) {
	k.t.Helper()
	for _, o := range objs {
		waitReadySynced(k.t, k.e, o, timeout)
	}
}

// assertOnePerTitle: every title names exactly one namespace in the fake (no duplicate, none lost).
func (k *kvEnv) assertOnePerTitle(titles []string) {
	k.t.Helper()
	have := listField(k.t, k.cf, k.kvPath(), "title")
	for _, ti := range titles {
		if n := count(have, ti); n != 1 {
			k.t.Errorf("%d namespaces titled %q, want 1", n, ti)
		}
	}
}

func (k *kvEnv) creates(j []fake.JournalEntry) []fake.JournalEntry {
	p := k.kvPath()
	return testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Method == http.MethodPost && e.Path == p })
}

// TestRateLimitStorm: a burst of 429s with a short Retry-After. cfclient waits it out inline and
// pauses every client of the token meanwhile: after each 429, no request of the token reaches
// the API before Retry-After has passed. Everything converges, with a bounded number of calls.
func TestRateLimitStorm(t *testing.T) {
	t.Parallel()
	k := startKV(t, kvOpts{})
	const storm = 5
	k.fault(fake.Fault{PathRegex: "^" + k.kvPath(), Status: http.StatusTooManyRequests, Code: 971,
		Message: "Please wait and consider throttling your request speed", Times: storm, FaultShape: fake.FaultShape{RetryAfter: "1"}})
	objs, titles := k.createKV(4, "storm")
	k.waitAll(objs, 90*time.Second)
	k.assertOnePerTitle(titles)

	j := k.journal()
	sort.SliceStable(j, func(a, b int) bool { return j[a].Time.Before(j[b].Time) })
	var throttled int
	for i, e := range j {
		if e.Status != http.StatusTooManyRequests {
			continue
		}
		throttled++
		// A request already on the wire when the 429 arrived may land just after it.
		for _, n := range j[i+1:] {
			d := n.Time.Sub(e.Time)
			if d > 50*time.Millisecond && d < 900*time.Millisecond {
				t.Errorf("%s %s arrived %v after a 429 with Retry-After 1s: the token was not paused", n.Method, n.Path, d)
			}
		}
	}
	if throttled != storm {
		t.Errorf("%d 429s answered, want %d", throttled, storm)
	}
	if n := len(k.creates(j)); n != len(objs) {
		t.Errorf("%d creates, want %d", n, len(objs))
	}
	// Budget: the storm adds its 429s (each retried once) to the normal cost of 4 creates.
	t.Logf("API calls: %d for %d objects and %d 429s", len(j), len(objs), storm)
	if budget := storm + len(objs)*kvCreateBudget; len(j) > budget {
		t.Errorf("%d API calls, budget %d:\n%s", len(j), budget, testenv.Summary(j))
	}
}

// kvCreateBudget is the most Cloudflare calls one KVNamespace create costs with ownership tags
// (docs/resilience.md): list for adoption, POST, tag read of a never-tagged resource (500, retried
// once, then the tag index), tag PUT, GET, and the sync's tag read and GET, plus slack for the
// re-reconcile the annotation write triggers.
const kvCreateBudget = 14

// TestLongRetryAfter: a 429 whose Retry-After exceeds cfclient.MaxInlineWait is surfaced: the
// object reports Synced=False, reason RateLimited, the token makes no further calls until
// Retry-After has passed (no hot loop), and the object converges afterwards.
func TestLongRetryAfter(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a Retry-After longer than cfclient.MaxInlineWait (30s)")
	}
	t.Parallel()
	k := startKV(t, kvOpts{})
	wait := cfclient.MaxInlineWait + 2*time.Second
	k.fault(fake.Fault{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: http.StatusTooManyRequests, Code: 971,
		Message: "Please wait and consider throttling your request speed", Times: 1,
		FaultShape: fake.FaultShape{RetryAfter: fmt.Sprint(int(wait.Seconds()))}})
	start := time.Now()
	objs, titles := k.createKV(1, "longwait")
	o := objs[0]
	testenv.Eventually(t, 20*time.Second, func() (bool, string) {
		_ = k.e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(o), o)
		return condIs(o, commonv1alpha1.ConditionSynced, metav1.ConditionFalse, reconcile.ReasonRateLimited), condString(o)
	})
	if c := reconcile.GetCondition(o, commonv1alpha1.ConditionSynced); !strings.Contains(c.Message, "rate limit") {
		t.Errorf("Synced message %q does not explain the rate limit", c.Message)
	}
	k.waitAll(objs, wait+30*time.Second)
	// While the token backed off, nothing of this token reached the API (no hot loop).
	j := k.journal()
	var at time.Time
	for _, e := range j {
		if e.Status == http.StatusTooManyRequests {
			at = e.Time
		}
	}
	for _, e := range j {
		if d := e.Time.Sub(at); d > 50*time.Millisecond && d < wait-time.Second {
			t.Errorf("%s %s sent %v into a %v back-off", e.Method, e.Path, d, wait)
		}
	}
	k.assertOnePerTitle(titles)
	if el := time.Since(start); el < wait-2*time.Second {
		t.Errorf("converged after %v, before Retry-After %v", el, wait)
	}
}

// Test5xxBurst: 503s on reads and on the create. Reads are retried by cfclient with back-off; a
// failed create is retried by the next reconcile (which looks the name up first). Everything
// converges with no duplicate and a bounded number of calls.
func Test5xxBurst(t *testing.T) {
	t.Parallel()
	k := startKV(t, kvOpts{})
	const burst = 8
	k.fault(fake.Fault{PathRegex: "^" + k.kvPath(), Status: http.StatusServiceUnavailable, Code: 10000, Message: "service unavailable", Times: burst})
	objs, titles := k.createKV(3, "burst")
	k.waitAll(objs, 90*time.Second)
	k.assertOnePerTitle(titles)
	j := k.journal()
	t.Logf("API calls: %d for %d objects and %d 503s", len(j), len(objs), burst)
	if budget := burst + len(objs)*kvCreateBudget; len(j) > budget {
		t.Errorf("%d API calls, budget %d:\n%s", len(j), budget, testenv.Summary(j))
	}
}

// TestSlowResponses covers client timeouts and reconcile deadlines.
func TestSlowResponses(t *testing.T) {
	t.Parallel()
	// A read slower than the HTTP client timeout is a transport error and is retried.
	t.Run("read timeout retried", func(t *testing.T) {
		t.Parallel()
		k := startKV(t, kvOpts{mo: testenv.ManagerOptions{AccountsOptions: []reconcile.AccountsOption{
			reconcile.WithHTTPClient(&http.Client{Timeout: 500 * time.Millisecond})}}})
		k.fault(fake.Fault{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Times: 2, FaultShape: fake.FaultShape{DelayMs: 2000}})
		objs, titles := k.createKV(1, "slowread")
		k.waitAll(objs, 60*time.Second)
		k.assertOnePerTitle(titles)
	})
	// The create succeeds in Cloudflare but its answer arrives after the client gave up (a lost
	// response): the next reconcile finds the namespace by title and adopts it. One create.
	t.Run("lost create response adopted", func(t *testing.T) {
		t.Parallel()
		k := startKV(t, kvOpts{mo: testenv.ManagerOptions{AccountsOptions: []reconcile.AccountsOption{
			reconcile.WithHTTPClient(&http.Client{Timeout: 500 * time.Millisecond})}}})
		k.fault(fake.Fault{Method: http.MethodPost, PathRegex: "^" + k.kvPath() + "$", Times: 1,
			FaultShape: fake.FaultShape{DelayMs: 2000, Passthrough: true}})
		objs, titles := k.createKV(1, "lostresp")
		k.waitAll(objs, 60*time.Second)
		k.assertOnePerTitle(titles)
		if n := len(k.creates(k.journal())); n != 1 {
			t.Errorf("%d creates, want 1 (the lost one is adopted):\n%s", n, testenv.Summary(k.journal()))
		}
		ids := listField(t, k.cf, k.kvPath(), "id")
		if got := objs[0].GetAnnotations()[commonv1alpha1.AnnotationExternalID]; count(ids, got) != 1 {
			t.Errorf("external-id %q is not the namespace the lost create made (%v)", got, ids)
		}
	})
	// A hung call is cut by the reconcile deadline (--reconcile-timeout), not by the (long)
	// HTTP timeout: the object converges well before the stalled answer would have arrived.
	t.Run("reconcile deadline", func(t *testing.T) {
		t.Parallel()
		const stall = 20 * time.Second
		k := startKV(t, kvOpts{mo: testenv.ManagerOptions{ReconcileTimeout: time.Second}})
		k.fault(fake.Fault{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Times: 1,
			FaultShape: fake.FaultShape{DelayMs: int(stall / time.Millisecond)}})
		start := time.Now()
		objs, titles := k.createKV(1, "deadline")
		k.waitAll(objs, 60*time.Second)
		k.assertOnePerTitle(titles)
		if el := time.Since(start); el >= stall {
			t.Errorf("converged after %v: the reconcile deadline did not cut the %v stall", el, stall)
		}
	})
}

// TestMalformedBodies: non-envelope and truncated answers are errors, never "empty": a list that
// cannot be decoded must not lead to a create (a duplicate), and every object converges.
func TestMalformedBodies(t *testing.T) {
	t.Parallel()
	k := startKV(t, kvOpts{})
	// A title that already exists: a list mistaken for empty would try to create it again.
	existing := "malformed-existing-" + testenv.RandomHex(3)
	resp, err := k.cf.Do(testenv.Context(t, 10*time.Second), cfclient.Request{Method: http.MethodPost, Path: k.kvPath(), Body: map[string]string{"title": existing}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp
	for _, f := range []fake.Fault{
		{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: 200, Times: 2,
			FaultShape: fake.FaultShape{Body: "<html><body>upstream error</body></html>", ContentType: "text/html"}},
		{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: 200, Times: 1, FaultShape: fake.FaultShape{Body: `{"success":true,"result":{"not":"a list"}}`}},
		{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: 502, Times: 2, FaultShape: fake.FaultShape{Body: "Bad Gateway", ContentType: "text/plain"}},
		{Method: http.MethodPost, PathRegex: "^" + k.kvPath() + "$", Status: 200, Times: 1, FaultShape: fake.FaultShape{Body: `{"success":tr`}},
		{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "/[0-9a-f]{32}$", Status: 200, Times: 1, FaultShape: fake.FaultShape{Body: `{"result":`}},
	} {
		k.fault(f)
	}
	en := entry(t, "KVNamespace")
	adopt := newGeneric(t, en, k.ns, "adopt", map[string]any{"title": existing}, "Orphan")
	if err := k.e.Client.Create(testenv.Context(t, 10*time.Second), adopt); err != nil {
		t.Fatal(err)
	}
	objs, titles := k.createKV(2, "malformed")
	k.waitAll(append(objs, adopt), 90*time.Second)
	k.assertOnePerTitle(append(titles, existing))
	j := k.journal()
	if n := len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Fault })); n != 7 {
		t.Errorf("%d faulted requests, want all 7 consumed:\n%s", n, testenv.Summary(j))
	}
	t.Logf("API calls: %d for 3 objects and 7 malformed answers", len(j))
}

// TestPartialListFailure: the adoption lookup's second page fails. ListAll fails as a whole (a
// partial listing is never taken as "not found"), so the object does not create a duplicate of
// the namespace on page 2; it adopts it once the page reads again.
func TestPartialListFailure(t *testing.T) {
	t.Parallel()
	k := startKV(t, kvOpts{})
	ctx := testenv.Context(t, 60*time.Second)
	for i := 0; i < 25; i++ {
		if _, err := k.cf.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: k.kvPath(),
			Body: map[string]string{"title": fmt.Sprintf("paged-%s-%02d", testenv.RandomHex(2), i)}}); err != nil {
			t.Fatal(err)
		}
	}
	page2, err := k.cf.Do(ctx, cfclient.Request{Path: k.kvPath(), Query: map[string][]string{"page": {"2"}}})
	if err != nil {
		t.Fatal(err)
	}
	var items []struct{ ID, Title string }
	if err := json.Unmarshal(page2.Result, &items); err != nil || len(items) == 0 {
		t.Fatalf("page 2: %v %s", err, page2.Result)
	}
	target := items[0]
	const failures = 12
	k.fault(fake.Fault{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: 500, Code: 10000, Message: "internal error",
		Times: failures, FaultShape: fake.FaultShape{QueryRegex: `(^|&)page=2(&|$)`}})
	start := len(k.journal())
	en := entry(t, "KVNamespace")
	o := newGeneric(t, en, k.ns, "paged", map[string]any{"title": target.Title}, "Orphan")
	if err := k.e.Client.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	sawError := false
	testenv.Eventually(t, 90*time.Second, func() (bool, string) {
		_ = k.e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(o), o)
		if c := reconcile.GetCondition(o, commonv1alpha1.ConditionSynced); c != nil && c.Status == metav1.ConditionFalse &&
			strings.Contains(c.Message, "list for adoption") {
			sawError = true
		}
		return condIs(o, commonv1alpha1.ConditionReady, metav1.ConditionTrue, "") && condIs(o, commonv1alpha1.ConditionSynced, metav1.ConditionTrue, ""), condString(o)
	})
	j := k.journal()[start:]
	if n := len(k.creates(j)); n != 0 {
		t.Errorf("%d creates, want 0 (the namespace exists on page 2):\n%s", n, testenv.Summary(j))
	}
	if got := o.GetAnnotations()[commonv1alpha1.AnnotationExternalID]; got != target.ID {
		t.Errorf("adopted %q, want %q", got, target.ID)
	}
	k.assertOnePerTitle([]string{target.Title})
	if !sawError {
		t.Log("the list failure was not observed in a condition (it may have cleared between polls)")
	}
	faulted := len(testenv.Filter(j, func(e fake.JournalEntry) bool { return e.Fault }))
	if faulted != failures {
		t.Errorf("%d faulted requests, want all %d consumed", faulted, failures)
	}
}
