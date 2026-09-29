package cfclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricValue sums the samples of the named metric (counter value or histogram count) whose
// labels include want.
func metricValue(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !hasLabels(m, want) {
				continue
			}
			switch {
			case m.Counter != nil:
				sum += m.GetCounter().GetValue()
			case m.Histogram != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

func hasLabels(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, l := range m.GetLabel() {
			if l.GetName() == k && l.GetValue() == v {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestMetricsCountAttemptsRetriesAndThrottling drives one GET through a 503, a 429 and a 200,
// and checks every metric the client exports, with route templates as labels.
func TestMetricsCountAttemptsRetriesAndThrottling(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			writeEnv(w, 503, `{"success":false,"errors":[{"code":10000,"message":"x"}]}`)
		case 2:
			w.Header().Set("Retry-After", "0")
			writeEnv(w, 429, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`)
		default:
			writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":[{"id":"a"}]}`)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.ListTTL = time.Minute })
	const acct = "feedfacefeedfacefeedfacefeedface"
	path := "/accounts/" + acct + "/storage/kv/namespaces"
	route := "/accounts/{account_id}/storage/kv/namespaces"
	lbl := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	before := map[string]float64{
		"ok":     metricValue(t, "cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "200")),
		"503":    metricValue(t, "cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "503")),
		"429":    metricValue(t, "cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "429")),
		"dur":    metricValue(t, "cloudflare_api_request_duration_seconds", lbl("method", "GET", "route_template", route)),
		"r5xx":   metricValue(t, "cloudflare_api_retries_total", lbl("route_template", route, "reason", "5xx")),
		"r429":   metricValue(t, "cloudflare_api_retries_total", lbl("route_template", route, "reason", "429")),
		"thr":    metricValue(t, "cloudflare_api_throttled_total", lbl("source", "api")),
		"wait":   metricValue(t, "cloudflare_api_rate_limit_wait_seconds", lbl("reason", "limiter")),
		"hits":   metricValue(t, "cloudflare_api_list_cache_hits_total", nil),
		"misses": metricValue(t, "cloudflare_api_list_cache_misses_total", nil),
	}
	for i := 0; i < 2; i++ { // the second call is a list-cache hit
		if _, err := c.Do(context.Background(), Request{Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	for key, c := range map[string]struct {
		name string
		lbl  map[string]string
		want float64
	}{
		"ok":     {"cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "200"), 1},
		"503":    {"cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "503"), 1},
		"429":    {"cloudflare_api_requests_total", lbl("method", "GET", "route_template", route, "code", "429"), 1},
		"dur":    {"cloudflare_api_request_duration_seconds", lbl("method", "GET", "route_template", route), 3},
		"r5xx":   {"cloudflare_api_retries_total", lbl("route_template", route, "reason", "5xx"), 1},
		"r429":   {"cloudflare_api_retries_total", lbl("route_template", route, "reason", "429"), 1},
		"thr":    {"cloudflare_api_throttled_total", lbl("source", "api"), 1},
		"wait":   {"cloudflare_api_rate_limit_wait_seconds", lbl("reason", "limiter"), 3},
		"hits":   {"cloudflare_api_list_cache_hits_total", nil, 1},
		"misses": {"cloudflare_api_list_cache_misses_total", nil, 1},
	} {
		if got := metricValue(t, c.name, c.lbl) - before[key]; got != c.want {
			t.Errorf("%s %v: +%v, want +%v", c.name, c.lbl, got, c.want)
		}
	}
	// No label anywhere carries the account ID.
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), "cloudflare_api_") {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), acct) {
					t.Errorf("%s label %s=%q carries a raw ID", f.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
}

// TestMetricsClientSideThrottle: a token blocked for longer than MaxInlineWait refuses calls
// locally (source=client) without sending them.
func TestMetricsClientSideThrottle(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		w.Header().Set("Retry-After", "3600")
		writeEnv(w, 429, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)
	before := metricValue(t, "cloudflare_api_throttled_total", map[string]string{"source": "client"})
	for i := 0; i < 3; i++ {
		_, err := c.Do(context.Background(), Request{Path: "/accounts/x/queues"})
		ae, ok := AsAPIError(err)
		if !ok || ae.Status != http.StatusTooManyRequests || ae.RetryAfter < time.Hour-time.Minute {
			t.Fatalf("call %d: %v, want a 429 with RetryAfter about 1h", i, err)
		}
	}
	if s := sent.Load(); s != 1 {
		t.Errorf("%d requests sent, want 1 (later calls are refused locally)", s)
	}
	if got := metricValue(t, "cloudflare_api_throttled_total", map[string]string{"source": "client"}) - before; got != 2 {
		t.Errorf("client-side throttles +%v, want +2", got)
	}
}

// TestMetricsListCacheMissesCountOnlyLists: an item GET (an object result, never cached) is not
// a list-cache miss; a list fetched from the API is one, and its repeat a hit.
func TestMetricsListCacheMissesCountOnlyLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/namespaces") {
			writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":[{"id":"a"}]}`)
			return
		}
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":{"id":"a"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.ListTTL = time.Minute })
	const acct = "feedfacefeedfacefeedfacefeedface"
	item, list := "/accounts/"+acct+"/storage/kv/namespaces/a", "/accounts/"+acct+"/storage/kv/namespaces"
	hits0 := metricValue(t, "cloudflare_api_list_cache_hits_total", nil)
	misses0 := metricValue(t, "cloudflare_api_list_cache_misses_total", nil)
	for _, p := range []string{item, item, list, list} {
		if _, err := c.Do(context.Background(), Request{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	if got := metricValue(t, "cloudflare_api_list_cache_misses_total", nil) - misses0; got != 1 {
		t.Errorf("misses +%v, want +1 (the first list only)", got)
	}
	if got := metricValue(t, "cloudflare_api_list_cache_hits_total", nil) - hits0; got != 1 {
		t.Errorf("hits +%v, want +1", got)
	}
}
