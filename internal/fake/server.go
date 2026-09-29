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

	// RateLimit is the per-token budget per RateWindow. Real API: 1200 per 300 s
	// ("Ratelimit-Policy: \"default\";q=1200;w=300", recording 0001). Zero means default.
	RateLimit  int
	RateWindow time.Duration

	// D1Region is reported as created_in_region / running_in_region when no location hint is
	// given (the real API picks the region nearest the caller; recording 0017 shows "APAC").
	D1Region string
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

	tokens map[string]*Token // tokens.go; nil = open mode
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
	Fault           bool      `json:"fault,omitempty"`
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
	s := &Server{opts: opts, Clock: &Clock{}, accounts: map[string]*account{}}
	s.limiter = newLimiter(opts.RateLimit, opts.RateWindow)
	s.registerKV()
	s.registerD1()
	s.registerQueues()
	s.registerTunnels()
	s.registerVPC()
	s.registerTokens()
	s.registerTags()
	return s
}

// Reset drops all state, journal, faults and queued IDs, and restores the real clock.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = map[string]*account{}
	s.journal = nil
	s.faults = nil
	s.seq = 0
	s.tokens = nil
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
	defer func() {
		s.mu.Lock()
		s.journal = append(s.journal, entry)
		s.mu.Unlock()
	}()

	// Auth. The real API rejects requests without credentials; exact status/code for the
	// missing-credentials case is UNVERIFIED (not yet recorded) — 400/9106 is Cloudflare's
	// commonly documented response.
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
	if policy, emit := rateHeaderPolicy(r.Method, path); emit {
		w.Header().Set("Ratelimit", fmt.Sprintf("%q;r=%d;t=1", policy, s.opts.RateLimit-1))
		w.Header().Set("Ratelimit-Policy", fmt.Sprintf("%q;q=%d;w=%d", policy, s.opts.RateLimit, int(s.opts.RateWindow.Seconds())))
	}
	if !allowed {
		// 429 body/code UNVERIFIED (not yet recorded); 971 is Cloudflare's documented throttling code.
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
// and whether it sends the headers at all.
func rateHeaderPolicy(method, path string) (string, bool) {
	for _, p := range []string{"/storage/kv/", "/d1/", "/workflows"} {
		if strings.Contains(path+"/", p) {
			return "", false
		}
	}
	if method == http.MethodGet && strings.Trim(path, "/") == "zones" {
		return "list_zones", true
	}
	return "default", true
}

func secondsCeil(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}
