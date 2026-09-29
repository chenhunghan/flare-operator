package cfclient

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Prometheus metrics of every Client, registered with controller-runtime's registry, so the
// manager's metrics endpoint (--metrics-bind-address) exports them. Labels never carry raw IDs
// or names: route_template is RouteTemplate(path) (a pinned-spec path template or "other"),
// method is the HTTP method and code the HTTP status ("error" for a transport failure).
//
// Counting follows HTTP attempts: a call retried twice adds three requests and two retries.
var (
	// MetricRequests counts HTTP attempts by method, route template and status code.
	MetricRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudflare_api_requests_total",
		Help: "Cloudflare API HTTP requests (attempts, retries included) by method, route template and status code.",
	}, []string{"method", "route_template", "code"})

	// MetricRequestDuration is the latency of one HTTP attempt (until the body is read).
	MetricRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cloudflare_api_request_duration_seconds",
		Help:    "Latency of Cloudflare API HTTP requests (one attempt, body read) by method and route template.",
		Buckets: []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"method", "route_template"})

	// MetricRateLimitWait is the time a call waited for its turn before being sent: reason
	// "limiter" for the client-side token bucket, "retry_after" for a token blocked by a 429.
	MetricRateLimitWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cloudflare_api_rate_limit_wait_seconds",
		Help:    "Time Cloudflare API calls waited before being sent: client-side rate limiter (limiter) or a 429 back-off of the token (retry_after).",
		Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"reason"})

	// MetricThrottled counts 429s: source "api" for answers from the API, "client" for calls
	// refused without being sent because the token is backing off longer than MaxInlineWait.
	MetricThrottled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudflare_api_throttled_total",
		Help: "HTTP 429 rate-limit answers (source=api) and calls refused locally while the token backs off (source=client).",
	}, []string{"source"})

	// MetricRetries counts retries by method, route template and reason (429, 5xx, transport).
	MetricRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudflare_api_retries_total",
		Help: "Cloudflare API request retries by method, route template and reason (429, 5xx, transport).",
	}, []string{"method", "route_template", "reason"})

	// MetricListCacheHits and MetricListCacheMisses count list-cache lookups (Options.ListTTL).
	MetricListCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cloudflare_api_list_cache_hits_total",
		Help: "Collection GETs answered from the client's list cache (CloudflareAccount spec.rateLimit.listCacheTTL).",
	})
	MetricListCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cloudflare_api_list_cache_misses_total",
		Help: "List results fetched from the API because the client's list cache had no fresh entry (item GETs are not counted).",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(MetricRequests, MetricRequestDuration, MetricRateLimitWait, MetricThrottled,
		MetricRetries, MetricListCacheHits, MetricListCacheMisses)
}

// Retry reasons of MetricRetries.
const (
	retry429       = "429"
	retry5xx       = "5xx"
	retryTransport = "transport"
)

func observeAttempt(method, route string, status int, start time.Time) {
	code := "error"
	if status > 0 {
		code = strconv.Itoa(status)
	}
	MetricRequests.WithLabelValues(method, route, code).Inc()
	MetricRequestDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
}
