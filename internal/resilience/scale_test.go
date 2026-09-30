package resilience_test

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudflarev1alpha1 "flare.dev/operator/api/cloudflare/v1alpha1"
	commonv1alpha1 "flare.dev/operator/api/common/v1alpha1"
	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/generic/descriptors"
	"flare.dev/operator/internal/generic/kinds"
	"flare.dev/operator/internal/reconcile"
	"flare.dev/operator/internal/testenv"
)

// createBudget is the most Cloudflare calls the create of one object of a kind may cost, from
// the first reconcile to Ready (docs/resilience.md has the breakdown). Tagged kinds pay for the
// never-tagged read (500, retried once, then the tag index) and the tag write.
var createBudget = map[string]int{
	"KVNamespace":    11,
	"Queue":          11,
	"D1Database":     11,
	"VectorizeIndex": 7,
	"SecretsStore":   7,
	"AIGateway":      7,
	// R2Bucket: the lookup of the client-chosen name (404), the create, then GET item + GET cors,
	// the PUT of the kindsuite fixture's CORS policy, and GET item + GET cors again.
	"R2Bucket": 7,
}

// listPageSize is the smallest default page size of the kinds' lists (KV 20, 0007; the generic
// profile 20, 0153/0155).
const listPageSize = 20

// pollBudget is the most calls one drift poll of an in-sync object may cost: the GET of the item
// and, for tagged kinds, the owner-tag read.
var pollBudget = map[string]int{
	"KVNamespace": 2, "Queue": 2, "D1Database": 2,
	"VectorizeIndex": 1, "SecretsStore": 1, "AIGateway": 1,
	"R2Bucket": 2, // the item and its CORS sub-resource
}

// TestScale creates FLARE_SCALE_OBJECTS (default 24) mixed objects of the generated kinds on one
// account with the default client rate limit (cfclient.DefaultRPS 3.6/s, burst 20; the fake
// enforces Cloudflare's 1200 per 5 minutes). All must converge with no 429, within the per-kind
// create budget, and steady-state polls must stay within the per-poll budget and make no write.
// docs/resilience.md records a 500-object run: FLARE_SCALE_OBJECTS=500 go test -run TestScale
// -timeout 60m ./internal/resilience/.
func TestScale(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for about a minute under the default rate limit")
	}
	t.Parallel()
	n := 24
	if v := os.Getenv("FLARE_SCALE_OBJECTS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("FLARE_SCALE_OBJECTS=%q", v)
		}
	}
	e := testenv.Require(t, env)
	ns := e.Namespace(t)
	var entries []descriptors.Entry
	var names []string
	for _, en := range descriptors.Entries() {
		entries = append(entries, en)
		names = append(names, kinds.Name(en))
	}
	const poll = 15 * time.Second
	e.StartManager(t, testenv.ManagerOptions{Controllers: names, Namespaces: []string{ns}, PollInterval: poll})
	// The zero RateLimitSpec keeps the cfclient defaults (testenv's default is a high test budget).
	acct := e.CreateAccount(t, ns, "acct", testenv.AccountOptions{RateLimit: &cloudflarev1alpha1.RateLimitSpec{}})
	acct.CloudflareAccount = e.WaitAccountCondition(t, ns, "acct", metav1.ConditionTrue, commonv1alpha1.ReasonAvailable)
	startJ := len(accountJournal(t, e, acct.AccountID))

	type item struct {
		en  descriptors.Entry
		obj reconcile.ManagedObject
	}
	var items []item
	start := time.Now()
	for i := 0; i < n; i++ {
		en := entries[i%len(entries)]
		fp := forProvider(t, en, fmt.Sprintf("scale-%s-%d", testenv.RandomHex(3), i))
		o := newGeneric(t, en, ns, fmt.Sprintf("%s-%d", strings.ToLower(en.Kind), i), fp, "Delete")
		if err := e.Client.Create(testenv.Context(t, 10*time.Second), o); err != nil {
			t.Fatal(err)
		}
		items = append(items, item{en, o})
	}
	// Convergence: at 3.6 calls/s the whole run needs about n*9/3.6 s.
	timeout := time.Duration(n)*4*time.Second + 2*time.Minute
	for _, it := range items {
		waitReadySynced(t, e, it.obj, timeout)
	}
	converged := time.Since(start)
	j := accountJournal(t, e, acct.AccountID)[startJ:]
	perKind := map[string]int{}
	objs := map[string]int{}
	for _, it := range items {
		objs[it.en.Kind]++
	}
	idOf := map[string]string{} // item path prefix → kind
	for _, it := range items {
		idOf[strings.NewReplacer("{account_id}", acct.AccountID, "{id}", "").Replace(it.en.CreatePath)] = it.en.Kind
	}
	var tags, throttled int
	for _, en := range j {
		if en.Status == http.StatusTooManyRequests {
			throttled++
		}
		if strings.Contains(en.Path, "/tags") {
			tags++
			continue
		}
		for p, k := range idOf {
			if en.Path == strings.TrimRight(p, "/") || strings.HasPrefix(en.Path, strings.TrimRight(p, "/")+"/") {
				perKind[k]++
			}
		}
	}
	// Each create's adoption lookup lists the kind's collection, one call per page (20 items per
	// page for most kinds, UNVERIFIED for the generic-profile kinds), so it grows with the number
	// of objects of the kind already there.
	budget := 0
	for k, c := range objs {
		budget += c * (createBudget[k] + (c+listPageSize-1)/listPageSize)
	}
	t.Logf("%d objects converged in %v with %d API calls (%d tag calls, %.1f calls/object, budget %d); per kind (without tag calls): %v",
		n, converged.Round(time.Second), len(j), tags, float64(len(j))/float64(n), budget, perKind)
	if throttled > 0 {
		t.Errorf("%d requests answered 429 under the default client rate limit", throttled)
	}
	if len(j) > budget {
		t.Errorf("%d API calls to converge, budget %d", len(j), budget)
	}

	// Steady state: over about two poll rounds, count the calls per poll (item GETs mark polls).
	// The window ends when the journal shows two item GETs per object, not after a fixed time,
	// which a loaded machine may fill with fewer polls (or none).
	itemGet := func(en fake.JournalEntry) bool {
		if en.Method != http.MethodGet || strings.Contains(en.Path, "/tags") {
			return false
		}
		for p := range idOf {
			if strings.HasPrefix(en.Path, strings.TrimRight(p, "/")+"/") {
				return true
			}
		}
		return false
	}
	mark := len(e.Journal(t))
	steady := testenv.ForAccount(e.WaitJournal(t, mark, 4*poll+2*time.Minute, func(j []fake.JournalEntry) (bool, string) {
		got := len(testenv.Filter(testenv.ForAccount(j, acct.AccountID), itemGet))
		return got >= 2*n, fmt.Sprintf("%d item GETs (drift polls), waiting for %d", got, 2*n)
	}), acct.AccountID)
	if w := testenv.Writes(steady); len(w) > 0 {
		t.Errorf("%d writes in steady state:\n%s", len(w), testenv.Summary(w))
	}
	itemGets := map[string]int{}
	tagGets := 0
	for _, en := range steady {
		if en.Method != http.MethodGet {
			continue
		}
		if strings.Contains(en.Path, "/tags") {
			tagGets++
			continue
		}
		for p, k := range idOf {
			if strings.HasPrefix(en.Path, strings.TrimRight(p, "/")+"/") {
				itemGets[k]++
			}
		}
	}
	polls := 0
	var kindsSeen []string
	for k, c := range itemGets {
		polls += c
		kindsSeen = append(kindsSeen, k)
	}
	sort.Strings(kindsSeen)
	if polls == 0 {
		t.Fatalf("no drift poll within %v", 2*poll)
	}
	perPoll := float64(len(steady)) / float64(polls)
	t.Logf("steady state: %d calls for %d polls (%.2f calls/poll, %d tag reads) of %v", len(steady), polls, perPoll, tagGets, kindsSeen)
	maxPerPoll := 0.0
	for k, c := range objs {
		maxPerPoll += float64(c * pollBudget[k])
	}
	maxPerPoll /= float64(n)
	if perPoll > maxPerPoll+0.05 {
		t.Errorf("%.2f calls per poll, budget %.2f", perPoll, maxPerPoll)
	}
	// Steady-state cost per hour at the default 5-minute poll, for the record.
	t.Logf("at the default %v poll: %.0f calls/hour for these %d objects (%.1f%% of the %.0f/hour default client budget)",
		5*time.Minute, perPoll*float64(n)*12, n, 100*perPoll*float64(n)*12/(cfclient.DefaultRPS*3600), cfclient.DefaultRPS*3600)

	// Every object is exactly one Cloudflare resource. The check uses a second token of the
	// account: the manager's polls keep the first one's window (1200 per 5 minutes) busy.
	verify := &testenv.Account{CloudflareAccount: acct.CloudflareAccount, AccountID: acct.AccountID, Token: "verify-" + testenv.RandomHex(8)}
	e.Fake.AddToken(fake.Token{Value: verify.Token, AccountID: acct.AccountID})
	cf := apiClient(t, e, verify)
	for _, it := range items {
		id := it.obj.GetResourceStatus().ID
		have := listField(t, cf, strings.ReplaceAll(it.en.ListPath, "{account_id}", acct.AccountID), it.en.IDField)
		if count(have, id) != 1 {
			t.Errorf("%s %s: %d resources with id %q", it.en.Kind, it.obj.GetName(), count(have, id), id)
		}
	}
}
