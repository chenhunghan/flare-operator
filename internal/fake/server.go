// Package fake implements flarefake, a stateful emulator of the Cloudflare v4 REST API used by
// flare-operator's tests. Behaviors are reproduced from sanitized recordings of the real API
// (test/recordings/); every profile cites the recording it was derived from. See
// docs/testing-strategy.md §3.
package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Options configures a Server.
type Options struct {
	// Spec, if set, validates every request against the pinned OpenAPI spec and records
	// violations in the journal. Violations never change the emulated response unless
	// RejectSchemaViolations is set (then: 400, code 10001).
	Spec                   *Spec
	RejectSchemaViolations bool

	// ValidateResponses (needs Spec) also validates every emulated response body against the
	// spec's response schema for the operation, journaling violations as
	// JournalEntry.ResponseViolation. It never changes a response. Tests get this for every
	// Server through EnableStrictResponses; NoStrictResponses opts one Server out of that.
	ValidateResponses bool
	NoStrictResponses bool
	// OnResponseViolation, if set, is called (without the server lock) for every response
	// violation found, allowlisted ones included (ResponseViolation.Allowed).
	OnResponseViolation func(ResponseViolation)

	// RateLimit is the per-token budget per RateWindow. Real API: 1200 per 300 s
	// ("Ratelimit-Policy: \"default\";q=1200;w=300", recording 0001). Zero means default.
	RateLimit  int
	RateWindow time.Duration

	// D1Region is reported as created_in_region / running_in_region when no location hint is
	// given (the real API picks the region nearest the caller; recording 0017 shows "APAC").
	D1Region string

	// WorkersSubdomain is the account's workers.dev subdomain (GET …/workers/subdomain). Zero
	// means "example-subdomain", the sanitized value in recording 0001.
	WorkersSubdomain string

	// Generic lists generated kinds to emulate with the descriptor-driven generic profile
	// (generic.go, UNVERIFIED; e.g. GeneratedGenericKinds()). Kinds with a hand-written profile
	// are skipped. The profile needs the pinned spec: Spec, else LoadDefaultSpec.
	Generic []GenericKind
}

// Server is an in-memory Cloudflare API. It is safe for concurrent use; all state mutations
// happen under a single mutex, which keeps profiles simple.
type Server struct {
	opts  Options
	Clock *Clock

	mu       sync.Mutex
	accounts map[string]*account
	ids      idSource
	journal  []JournalEntry
	faults   []*Fault
	limiter  *limiter
	routes   []route
	seq      int64 // creation sequence; orders lists deterministically even with a frozen clock

	generic         *genericProfile     // generic.go; nil without Options.Generic
	respViolations  []ResponseViolation // response_validation.go
	tokens          map[string]*Token   // tokens.go; nil = open mode
	workerStartupMs int                 // startup_time_ms reported by script uploads (see SetWorkerStartupTime)
}

// nextSeq returns a monotonically increasing creation number. Callers hold s.mu.
func (s *Server) nextSeq() int64 {
	s.seq++
	return s.seq
}

// JournalEntry records one request, so tests can assert on API call patterns
// (e.g. "the second reconcile made zero writes").
type JournalEntry struct {
	Time            time.Time `json:"time"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	Query           string    `json:"query,omitempty"`
	Status          int       `json:"status"`
	SchemaViolation string    `json:"schema_violation,omitempty"`
	// ResponseViolation: the emulated response did not match the spec's response schema (and no
	// allowlist entry covers it); see Options.ValidateResponses.
	ResponseViolation string `json:"response_violation,omitempty"`
	Fault             bool   `json:"fault,omitempty"`
}

// Fault makes matching requests fail. Times<=0 means "until removed".
type Fault struct {
	Method    string `json:"method"`
	PathRegex string `json:"path_regex"`
	Status    int    `json:"status"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Times     int    `json:"times"`
	re        *regexp.Regexp
}

// New returns a Server with default options applied.
func New(opts Options) *Server {
	if opts.RateLimit == 0 {
		opts.RateLimit = 1200
	}
	if opts.RateWindow == 0 {
		opts.RateWindow = 300 * time.Second
	}
	if opts.D1Region == "" {
		opts.D1Region = "WNAM"
	}
	if opts.WorkersSubdomain == "" {
		opts.WorkersSubdomain = "example-subdomain"
	}
	s := &Server{opts: opts, Clock: &Clock{}, accounts: map[string]*account{}, workerStartupMs: workerDefaultStartupMs}
	s.limiter = newLimiter(opts.RateLimit, opts.RateWindow)
	s.registerKV()
	s.registerD1()
	s.registerQueues()
	s.registerTunnels()
	s.registerVPC()
	s.registerTokens()
	s.registerTags()
	s.registerWorkers()
	s.registerGeneric() // last: hand-written profiles take precedence
	return s
}

// Reset drops all state, journal, faults and queued IDs, and restores the real clock.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = map[string]*account{}
	s.journal = nil
	s.respViolations = nil
	s.faults = nil
	s.seq = 0
	s.tokens = nil
	s.workerStartupMs = workerDefaultStartupMs
	s.generic.reset()
	s.ids.reset()
	s.limiter.reset()
	s.Clock.Real()
}

// EnqueueIDs makes the next created resources use exactly these IDs, in order.
func (s *Server) EnqueueIDs(ids ...string) { s.ids.enqueue(ids...) }

// InjectFault adds a fault rule. Status defaults to 500.
func (s *Server) InjectFault(f Fault) error {
	if f.Status == 0 {
		f.Status = http.StatusInternalServerError
	}
	if f.Status < 100 || f.Status > 599 {
		return fmt.Errorf("fault status %d out of range", f.Status)
	}
	re, err := regexp.Compile(f.PathRegex)
	if err != nil {
		return err
	}
	f.re = re
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &f)
	return nil
}

// Journal returns a copy of the request journal.
func (s *Server) Journal() []JournalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]JournalEntry(nil), s.journal...)
}

// ---- routing ---------------------------------------------------------------------------------

type reqCtx struct {
	s       *Server
	r       *http.Request
	params  map[string]string
	query   url.Values
	body    []byte
	now     time.Time
	account *account
}

type handler func(c *reqCtx) response

type route struct {
	method   string
	segments []string
	h        handler
}

func (s *Server) handle(method, pattern string, h handler) {
	s.routes = append(s.routes, route{method: method, segments: strings.Split(strings.Trim(pattern, "/"), "/"), h: h})
}

func (s *Server) match(method, path string) (handler, map[string]string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	pathMatched := false
	for _, rt := range s.routes {
		if len(rt.segments) != len(segs) {
			continue
		}
		params := map[string]string{}
		okm := true
		for i, p := range rt.segments {
			if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
				params[p[1:len(p)-1]] = segs[i]
			} else if p != segs[i] {
				okm = false
				break
			}
		}
		if !okm {
			continue
		}
		pathMatched = true
		if rt.method == method {
			return rt.h, params, true
		}
	}
	return nil, nil, pathMatched
}

// ServeHTTP implements http.Handler. The emulated API lives under /client/v4; the test control
// API under /_fake.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		s.serveControl(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	body, _ := io.ReadAll(r.Body)
	// Content-Type differs by product in recordings: KV and Queues send a charset, D1, Tunnel and
	// Workers VPC do not.
	if strings.Contains(path, "/d1/") || strings.Contains(path, "/cfd_tunnel") ||
		strings.Contains(path, "/teamnet/") || strings.Contains(path, "/connectivity/") {
		w.Header().Set("Content-Type", "application/json")
	}
	now := s.Clock.Now()
	entry := JournalEntry{Time: now, Method: r.Method, Path: path, Query: r.URL.RawQuery}
	var cw *captureWriter
	if s.responseSpec() != nil {
		cw = &captureWriter{ResponseWriter: w}
		w = cw
	}
	defer func() {
		if cw != nil && !entry.Fault && cw.status != 0 { // injected faults are the test's choice, not emulated behavior
			s.validateResponse(r, path, cw.status, cw.Header(), cw.buf.Bytes(), &entry)
		}
		s.mu.Lock()
		s.journal = append(s.journal, entry)
		s.mu.Unlock()
	}()

	// Auth. The real API rejects requests without credentials (not yet recorded) with 400/9106
	// "Missing X-Auth-Key, X-Auth-Email or Authorization headers". SOURCED: wrangler's changelog
	// quotes that raw API error for code 9106, cloudflare/workers-sdk@485cfb3:packages/wrangler/CHANGELOG.md#L3682-L3684,
	// and its tests render 9106 as "Authentication failed (status: 400)",
	// packages/wrangler/src/__tests__/core/handle-errors.test.ts#L285.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" && r.Header.Get("X-Auth-Key") == "" {
		entry.Status = http.StatusBadRequest
		writeResponse(w, fail(http.StatusBadRequest, 9106, "Missing X-Auth-Key, X-Auth-Email or Authorization headers"))
		return
	}

	// Rate limit: enforced per token with a sliding window (documented 1200 req / 300 s). The
	// response headers mimic what the real API actually sends, which does NOT track usage: across
	// 131 recorded responses the header was always r=q-1;t=1, even in bursts. KV, D1 and
	// Workflows send no rate-limit headers at all, and GET /zones uses its own policy name
	// (recordings 0001, 0161, 0003…0027). Clients must therefore self-limit.
	_, reset, allowed := s.limiter.take(token, now)
	if policy, quota, emit := rateHeaderPolicy(r.Method, path); emit {
		window := int(s.opts.RateWindow.Seconds())
		if quota == 0 {
			quota = s.opts.RateLimit
		} else {
			window = 300
		}
		w.Header().Set("Ratelimit", fmt.Sprintf("%q;r=%d;t=1", policy, quota-1))
		w.Header().Set("Ratelimit-Policy", fmt.Sprintf("%q;q=%d;w=%d", policy, quota, window))
	}
	if !allowed {
		// 429 with retry-after: DOCS https://developers.cloudflare.com/fundamentals/api/reference/limits/.
		// The body's code 971 is UNVERIFIED (not recorded, and not in the docs); users report it
		// from the real API through wrangler (cloudflare/workers-sdk issue #10025, a field report).
		w.Header().Set("Retry-After", fmt.Sprint(reset))
		entry.Status = http.StatusTooManyRequests
		writeResponse(w, fail(http.StatusTooManyRequests, 971, "Please wait and consider throttling your request speed"))
		return
	}

	if f := s.takeFault(r.Method, path); f != nil {
		entry.Status, entry.Fault = f.Status, true
		writeResponse(w, fail(f.Status, f.Code, f.Message))
		return
	}

	if s.opts.Spec != nil {
		if err := s.opts.Spec.ValidateRequest(r, body); err != nil {
			entry.SchemaViolation = err.Error()
			if s.opts.RejectSchemaViolations {
				entry.Status = http.StatusBadRequest
				writeResponse(w, fail(http.StatusBadRequest, 10001, "flarefake: request does not match the pinned OpenAPI spec: "+err.Error()))
				return
			}
		}
	}

	h, params, pathKnown := s.match(r.Method, path)
	if h == nil {
		// Unknown routes: real API answers 404/7000 "No route for that URI"; unsupported method on
		// a known route 405/10405 — both UNVERIFIED (not yet recorded).
		resp := fail(http.StatusNotFound, 7000, "No route for that URI")
		if pathKnown {
			resp = fail(http.StatusMethodNotAllowed, 10405, "Method not allowed")
		}
		entry.Status = resp.status
		writeResponse(w, resp)
		return
	}

	if acct, ok := params["account_id"]; ok && acct == "" {
		entry.Status = http.StatusNotFound
		writeResponse(w, fail(http.StatusNotFound, 7000, "No route for that URI")) // UNVERIFIED
		return
	}
	resp := func() response {
		s.mu.Lock()
		defer s.mu.Unlock() // a panicking handler must never wedge the emulator
		c := &reqCtx{s: s, r: r, params: params, query: r.URL.Query(), body: body, now: now}
		if acct, ok := params["account_id"]; ok {
			c.account = s.accountLocked(acct)
		}
		return h(c)
	}()
	entry.Status = resp.status
	writeResponse(w, resp)
}

// captureWriter keeps a copy of the status and body for response validation.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

func (s *Server) takeFault(method, path string) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if (f.Method == "" || strings.EqualFold(f.Method, method)) && f.re.MatchString(path) {
			if f.Times > 0 {
				f.Times--
				if f.Times == 0 {
					s.faults = append(s.faults[:i], s.faults[i+1:]...)
				}
			}
			return f
		}
	}
	return nil
}

// decodeJSON decodes the request body into v; it returns a Cloudflare-style error response on
// malformed JSON (code 10002 UNVERIFIED — products use their own codes where recorded).
func (c *reqCtx) decodeJSON(v any) *response {
	dec := json.NewDecoder(bytes.NewReader(c.body))
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = fmt.Errorf("trailing data after JSON value")
	}
	if err != nil {
		r := fail(http.StatusBadRequest, 10002, "flarefake: malformed JSON body: "+err.Error())
		return &r
	}
	return nil
}

func (c *reqCtx) intQuery(name string, def int) int {
	v := c.query.Get(name)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil {
		return def
	}
	return n
}

// ---- rate limiting ---------------------------------------------------------------------------

type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func (l *limiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = map[string][]time.Time{}
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// take records a request for token at now using a sliding window. It returns the remaining
// budget, seconds until the oldest request leaves the window, and whether the request is allowed.
func (l *limiter) take(token string, now time.Time) (remaining, resetSeconds int, allowed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	h := l.hits[token]
	i := 0
	for i < len(h) && !h[i].After(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= l.limit {
		l.hits[token] = h
		return 0, secondsCeil(h[0].Add(l.window).Sub(now)), false
	}
	h = append(h, now)
	l.hits[token] = h
	return l.limit - len(h), secondsCeil(h[0].Add(l.window).Sub(now)), true
}

// rateHeaderPolicy reports which Ratelimit policy name the real API advertises for a request,
// its advertised quota (0 = the default budget), and whether it sends the headers at all.
//
// Workers script writes advertise their own policies with q=180000;w=300: upload (0036),
// workers.dev subdomain (0037) and delete (0108). The emulator still counts them against the
// default per-token budget; whether they are separate buckets in reality is UNVERIFIED.
func rateHeaderPolicy(method, path string) (string, int, bool) {
	for _, p := range []string{"/storage/kv/", "/d1/", "/workflows"} {
		if strings.Contains(path+"/", p) {
			return "", 0, false
		}
	}
	if method == http.MethodGet && strings.Trim(path, "/") == "zones" {
		return "list_zones", 0, true
	}
	if segs := strings.Split(strings.Trim(path, "/"), "/"); len(segs) >= 5 && segs[0] == "accounts" &&
		segs[2] == "workers" && segs[3] == "scripts" {
		const workersQuota = 180000
		switch {
		case len(segs) == 5 && method == http.MethodPut:
			return "workers_script_upload", workersQuota, true // 0036
		case len(segs) == 5 && method == http.MethodDelete:
			return "workers_script_modify", workersQuota, true // 0108
		case len(segs) == 6 && segs[5] == "subdomain" && method != http.MethodGet:
			return "workers_route_update", workersQuota, true // 0037 (DELETE UNVERIFIED)
		case len(segs) == 6 && segs[5] == "settings" && method == http.MethodPatch:
			return "workers_script_modify", workersQuota, true // UNVERIFIED
		}
	}
	return "default", 0, true
}

func secondsCeil(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}
