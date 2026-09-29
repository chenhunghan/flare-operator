package cfclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/time/rate"
)

// Defaults applied by New when the corresponding Options field is zero.
const (
	DefaultBaseURL = "https://api.cloudflare.com/client/v4"
	// DefaultRPS stays under Cloudflare's global limit of 1200 requests per 300 s per token
	// (4 rps) with a 10% margin for other users of the same token (cf CLI, dashboards).
	DefaultRPS        = 3.6
	DefaultBurst      = 20
	DefaultMaxRetries = 4
	DefaultUserAgent  = "flare-operator"

	// MaxInlineWait bounds how long Do sleeps for a single Retry-After (or a client-side
	// block after a 429). Longer waits are surfaced to the caller as an *APIError with
	// RetryAfter set, so a reconciler can requeue instead of blocking a worker.
	MaxInlineWait = 30 * time.Second

	// max429Retries bounds consecutive 429 retries of one call (independent of MaxRetries).
	max429Retries = 8
)

// Back-off parameters; variables so tests can shrink them.
var (
	backoffBase = 250 * time.Millisecond
	backoffCap  = 10 * time.Second
)

// tokenState is shared by every Client built with the same token: one rate limiter and one
// "blocked until" instant set by 429 responses, so a throttled token pauses all its users.
type tokenState struct {
	lim *rate.Limiter

	mu           sync.Mutex
	blockedUntil time.Time
}

func (t *tokenState) block(until time.Time) {
	t.mu.Lock()
	if until.After(t.blockedUntil) {
		t.blockedUntil = until
	}
	t.mu.Unlock()
}

func (t *tokenState) blocked(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.blockedUntil.After(now) {
		return t.blockedUntil.Sub(now)
	}
	return 0
}

var (
	tokensMu sync.Mutex
	tokens   = map[string]*tokenState{} // key: sha256(token)
)

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// stateFor returns the shared state for token. Explicit (non-zero) rps/burst values update the
// shared limiter, so the most recent explicit configuration wins; zero values leave an existing
// limiter unchanged.
func stateFor(token string, rps float64, burst int) *tokenState {
	tokensMu.Lock()
	defer tokensMu.Unlock()
	k := tokenKey(token)
	st, ok := tokens[k]
	if !ok {
		r, b := rps, burst
		if r <= 0 {
			r = DefaultRPS
		}
		if b <= 0 {
			b = DefaultBurst
		}
		st = &tokenState{lim: rate.NewLimiter(rate.Limit(r), b)}
		tokens[k] = st
		return st
	}
	if rps > 0 && float64(st.lim.Limit()) != rps {
		st.lim.SetLimit(rate.Limit(rps))
	}
	if burst > 0 && st.lim.Burst() != burst {
		st.lim.SetBurst(burst)
	}
	return st
}

type client struct {
	base       *url.URL
	token      string
	http       *http.Client
	ua         string
	maxRetries int
	state      *tokenState
	cache      *listCache // nil when disabled
}

// New builds a Client. Clients built with the same Token share one rate limiter and one 429
// back-off state (see Options).
func New(opts Options) (Client, error) {
	if opts.Token == "" {
		return nil, errors.New("cfclient: empty token")
	}
	if strings.ContainsAny(opts.Token, "\r\n") {
		return nil, errors.New("cfclient: token contains a newline")
	}
	raw := opts.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("cfclient: bad base URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("cfclient: base URL %q must be http or https", raw)
	}
	c := &client{
		base:       base,
		token:      opts.Token,
		http:       opts.HTTPClient,
		ua:         opts.UserAgent,
		maxRetries: opts.MaxRetries,
		state:      stateFor(opts.Token, opts.RPS, opts.Burst),
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.ua == "" {
		c.ua = DefaultUserAgent
	}
	if c.maxRetries == 0 {
		c.maxRetries = DefaultMaxRetries
	}
	if c.maxRetries < 0 {
		c.maxRetries = 0
	}
	if opts.ListTTL > 0 {
		c.cache = newListCache(opts.ListTTL)
	}
	return c, nil
}

// ---- per-call options carried in the context -----------------------------------------------

type ctxKey int

const (
	ctxNoCache ctxKey = iota
	ctxMaxRetries
)

// WithoutCache makes Do bypass (but still refresh) the list cache for calls made with ctx.
func WithoutCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxNoCache, true)
}

// WithMaxRetries overrides Options.MaxRetries for 5xx/transport retries of calls made with ctx
// (0 disables them). 429 handling is unaffected.
func WithMaxRetries(ctx context.Context, n int) context.Context {
	if n < 0 {
		n = 0
	}
	return context.WithValue(ctx, ctxMaxRetries, n)
}

// ---- Do ----------------------------------------------------------------------------------------

type envelope struct {
	Success    *bool           `json:"success"`
	Errors     []ErrorDetail   `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *ResultInfo     `json:"result_info"`
}

func (c *client) Do(ctx context.Context, req Request) (*Response, error) {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	if req.Body != nil && req.RawBody != nil {
		return nil, errors.New("cfclient: both Body and RawBody set")
	}
	var body []byte
	contentType := req.ContentType
	switch {
	case req.Body != nil:
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("cfclient: encode body: %w", err)
		}
		body = b
		if contentType == "" {
			contentType = "application/json"
		}
	case req.RawBody != nil:
		body = req.RawBody
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	u := c.url(req.Path, req.Query)
	cacheKey := req.Path + "?" + req.Query.Encode()
	// Conditional or otherwise header-dependent GETs are not served from (or stored in) the cache.
	useCache := c.cache != nil && method == http.MethodGet && ctx.Value(ctxNoCache) == nil && len(req.Header) == 0
	if useCache {
		if r, ok := c.cache.get(cacheKey); ok {
			return r, nil
		}
	}
	if c.cache != nil && method != http.MethodGet && method != http.MethodHead {
		// Before and after the write: a concurrent GET may re-cache the old state while the
		// write is in flight.
		c.cache.invalidate(req.Path)
		defer c.cache.invalidate(req.Path)
	}

	maxRetries := c.maxRetries
	if v, ok := ctx.Value(ctxMaxRetries).(int); ok {
		maxRetries = v
	}
	idempotent := method == http.MethodGet || method == http.MethodHead || method == http.MethodPut ||
		method == http.MethodDelete || method == http.MethodOptions

	for attempt, throttled := 0, 0; ; {
		if err := c.waitTurn(ctx); err != nil {
			return nil, err
		}
		resp, err := c.send(ctx, method, u, body, contentType, req.Header)
		if err != nil {
			if ctx.Err() != nil || !idempotent || attempt >= maxRetries {
				return nil, err
			}
			attempt++
			if err := sleep(ctx, jitterBackoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		out, apiErr := decode(resp)
		if apiErr == nil {
			if c.cache != nil && method == http.MethodGet && len(req.Header) == 0 && isJSONArray(out.Result) {
				c.cache.put(cacheKey, req.Path, out)
			}
			return out, nil
		}
		switch {
		case apiErr.Status == http.StatusTooManyRequests:
			// The request was rejected, so retrying is safe for every method. Block the whole
			// token (all clients sharing it) for Retry-After.
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = jitterBackoff(throttled + 1)
			}
			c.state.block(time.Now().Add(wait))
			throttled++
			if wait > MaxInlineWait || throttled > max429Retries {
				return nil, apiErr
			}
			// waitTurn at the top of the loop honours the block.
		case apiErr.Status >= 500 && idempotent && attempt < maxRetries:
			attempt++
			wait := jitterBackoff(attempt)
			if apiErr.RetryAfter > 0 && apiErr.RetryAfter <= MaxInlineWait {
				wait = apiErr.RetryAfter
			}
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
		default:
			return nil, apiErr
		}
	}
}

// waitTurn blocks while the token is backed off after a 429, then takes a limiter token.
func (c *client) waitTurn(ctx context.Context) error {
	if d := c.state.blocked(time.Now()); d > 0 {
		if d > MaxInlineWait {
			return &APIError{Status: http.StatusTooManyRequests, RetryAfter: d,
				Errors: []ErrorDetail{{Code: 971, Message: "flare-operator: token is backing off after HTTP 429"}}}
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
	return c.state.lim.Wait(ctx)
}

// url joins the base URL, p and q. p may carry percent-escaped segments (e.g. a KV key
// "a%2Fb" built with url.PathEscape); they are sent as-is, not double-encoded, so an escaped
// '/' stays inside its segment. A p that is not a valid escaping (a bare '%') is taken
// literally and encoded.
func (c *client) url(p string, q url.Values) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := *c.base
	if dec, err := url.PathUnescape(p); err == nil {
		u.Path = c.base.Path + dec
		// URL.String uses RawPath only when it is a valid encoding of Path; otherwise (e.g. p
		// has a space) it re-encodes Path, which is also correct.
		u.RawPath = c.base.EscapedPath() + p
	} else {
		u.Path = c.base.Path + p
		u.RawPath = ""
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ownedHeaders are set by the client and never taken from Request.Header.
var ownedHeaders = map[string]bool{"Authorization": true, "User-Agent": true, "Content-Type": true}

func (c *client) send(ctx context.Context, method, u string, body []byte, contentType string, extra http.Header) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("cfclient: %w", err)
	}
	hreq.Header.Set("Accept", "application/json")
	for k, vs := range extra {
		ck := http.CanonicalHeaderKey(k)
		if ownedHeaders[ck] {
			continue
		}
		hreq.Header.Del(ck)
		for _, v := range vs {
			hreq.Header.Add(ck, v)
		}
	}
	hreq.Header.Set("Authorization", "Bearer "+c.token)
	hreq.Header.Set("User-Agent", c.ua)
	if contentType != "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("cfclient: %s %s: %w", method, hreq.URL.Path, err)
	}
	return resp, nil
}

// decode reads the v4 envelope. The Ratelimit/Ratelimit-Policy headers are deliberately ignored:
// the real API does not track usage in them (recordings 0001, 0161; see internal/fake).
func decode(resp *http.Response) (*Response, *APIError) {
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	status := resp.StatusCode
	ok2xx := status >= 200 && status < 300
	fail := func(details []ErrorDetail) *APIError {
		e := &APIError{Status: status, Errors: details}
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return e
	}
	if readErr != nil {
		if ok2xx {
			status = http.StatusBadGateway
		}
		return nil, fail([]ErrorDetail{{Message: "flare-operator: reading response body: " + readErr.Error()}})
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if ok2xx { // e.g. 204 No Content (DELETE /accounts/{id}/tags)
			return &Response{Status: status, Header: resp.Header.Clone()}, nil
		}
		return nil, fail(nil)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if ok2xx {
			// Not an envelope (e.g. a raw download). Hand the body to the caller as Result.
			return &Response{Status: status, Result: json.RawMessage(raw), Header: resp.Header.Clone()}, nil
		}
		return nil, fail([]ErrorDetail{{Message: "non-JSON response: " + Sanitize(string(raw), maxBodySnippet)}})
	}
	if ok2xx && env.Success == nil {
		// A JSON document that is not a v4 envelope (no "success" field), e.g. a KV value whose
		// content is a JSON object: it is the payload itself.
		return &Response{Status: status, Result: json.RawMessage(raw), Header: resp.Header.Clone()}, nil
	}
	if !ok2xx || (env.Success != nil && !*env.Success) {
		return nil, fail(sanitizeDetails(env.Errors))
	}
	return &Response{Status: status, Result: env.Result, ResultInfo: env.ResultInfo, Header: resp.Header.Clone()}, nil
}

// Limits for server-supplied text copied into errors (which end up in status conditions): a
// base URL may point at a server that is not Cloudflare, so its bodies are untrusted.
const (
	maxBodySnippet   = 120
	maxErrorMessage  = 300
	maxErrorsKept    = 10
	truncationSuffix = "…"
)

func sanitizeDetails(in []ErrorDetail) []ErrorDetail {
	if len(in) == 0 {
		return in
	}
	out := make([]ErrorDetail, 0, min(len(in), maxErrorsKept))
	for i, d := range in {
		if i == maxErrorsKept {
			out = append(out, ErrorDetail{Message: fmt.Sprintf("flare-operator: %d more errors omitted", len(in)-maxErrorsKept)})
			break
		}
		out = append(out, ErrorDetail{Code: d.Code, Message: Sanitize(d.Message, maxErrorMessage)})
	}
	return out
}

// Sanitize makes server-supplied text safe to show in a status message: control characters
// (newlines included) and invalid UTF-8 become spaces, runs of whitespace collapse, and the
// result is cut to at most n bytes (on a rune boundary) with "…" appended when cut.
func Sanitize(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return truncate(strings.TrimRight(b.String(), " "), n)
}

// truncate cuts s to at most n bytes on a rune boundary, appending "…" when it cuts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + truncationSuffix
}

// parseRetryAfter accepts delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0
		}
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// jitterBackoff returns a "full jitter" exponential back-off for the n-th retry (n ≥ 1),
// in [base*2^(n-1)/2, base*2^(n-1)], capped at backoffCap.
func jitterBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := backoffBase << min(n-1, 16)
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isJSONArray(r json.RawMessage) bool {
	b := bytes.TrimSpace(r)
	return len(b) > 0 && b[0] == '['
}
