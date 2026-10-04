package resilience_test

import (
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// TestMetricsEndpoint scrapes a manager's metrics endpoint after reconciles that met a 503, a
// 429 and a missing account, and checks that the Cloudflare client and reconcile metrics are
// exported with route templates (never raw IDs) as labels.
func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	k := startKV(t, kvOpts{mo: testenv.ManagerOptions{MetricsBindAddress: addr}})
	k.fault(fake.Fault{Method: http.MethodGet, PathRegex: "^" + k.kvPath() + "$", Status: 503, Code: 10000, Message: "unavailable", Times: 1})
	k.fault(fake.Fault{Method: http.MethodPost, PathRegex: "^" + k.kvPath() + "$", Status: 429, Code: 971, Message: "slow down", Times: 1,
		FaultShape: fake.FaultShape{RetryAfter: "0"}})
	objs, _ := k.createKV(1, "metrics")
	en := entry(t, "KVNamespace")
	orphan := newGeneric(t, en, k.ns, "no-account", map[string]any{"title": "metrics-no-account"}, "")
	orphan.GetResourceSpec().AccountRef.Name = "missing"
	if err := k.e.Client.Create(testenv.Context(t, 10*time.Second), orphan); err != nil {
		t.Fatal(err)
	}
	k.waitAll(objs, 60*time.Second)
	testenv.Eventually(t, 30*time.Second, func() (bool, string) {
		_ = k.e.Client.Get(testenv.Context(t, 5*time.Second), client.ObjectKeyFromObject(orphan), orphan)
		c := orphan.GetResourceStatus().Conditions
		return len(c) > 0 && strings.Contains(condString(orphan), commonv1alpha1.ReasonAccountNotReady), condString(orphan)
	})

	var body string
	testenv.Eventually(t, 20*time.Second, func() (bool, string) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == 200 && strings.Contains(body, "cloudflare_api_requests_total"), resp.Status
	})
	route := `route_template="/accounts/{account_id}/storage/kv/namespaces"`
	for _, want := range []string{
		`cloudflare_api_requests_total{code="200",method="GET",` + route + `}`,
		`cloudflare_api_requests_total{code="503",method="GET",` + route + `}`,
		`cloudflare_api_requests_total{code="429",method="POST",` + route + `}`,
		`cloudflare_api_request_duration_seconds_bucket{method="POST",` + route,
		`cloudflare_api_rate_limit_wait_seconds_bucket{reason="limiter"`,
		`cloudflare_api_throttled_total{source="api"}`,
		`cloudflare_api_retries_total{method="GET",reason="5xx",` + route + `}`,
		`cloudflare_api_retries_total{method="POST",reason="429",` + route + `}`,
		`cloudflare_api_list_cache_hits_total`,
		`cloudflare_api_list_cache_misses_total`,
		`controller_runtime_reconcile_total{controller="kvnamespace",result="success"}`,
		`controller_runtime_reconcile_errors_total{controller="kvnamespace"}`,
		`flare_managed_sync_failures_total{kind="KVNamespace",reason="AccountNotReady"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	hexID := regexp.MustCompile(`[0-9a-f]{32}|` + regexp.QuoteMeta(k.acct.AccountID))
	for _, line := range strings.Split(body, "\n") {
		if (strings.HasPrefix(line, "cloudflare_api_") || strings.HasPrefix(line, "flare_")) && hexID.MatchString(line) {
			t.Errorf("metric label carries a raw ID: %s", line)
		}
	}
}
