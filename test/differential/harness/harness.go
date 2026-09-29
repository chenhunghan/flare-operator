// Package harness runs flarefake in-process for the differential tests (test/differential and
// test/differential/go), which drive real Cloudflare clients (wrangler, cloudflare-go,
// cloudflared) against it. See docs/differential-testing.md.
//
// The harness adds three things around fake.Server:
//   - a capture of every request the client sent (method, path, query, Content-Type,
//     User-Agent, body prefix) and what it got back (status, envelope error codes), so a test
//     can show a client's exact request sequence;
//   - shims: test-local answers for routes flarefake gets wrong, registered only for a known
//     discrepancy so that the rest of a client's flow can still be exercised. Each shim names
//     the discrepancy it stands in for; flarefake itself is never changed here;
//   - Discrepancy (discrepancy.go), which turns a known mismatch into a skipped subtest that
//     carries its evidence.
//
// Nothing in cmd/ or internal/ imports this package.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"flare.dev/operator/internal/fake"
)

// AccountID is the account every differential scenario uses (the example ID from the spec).
const AccountID = "023e105f4ecef8ad9ca31a8372d0c353"

// Token is the fake API token handed to clients. flarefake runs in open token mode, so any
// non-empty bearer token is accepted; this one is not a Cloudflare credential.
const Token = "flare-differential-fake-token-0000000000"

// Request is one captured client request.
type Request struct {
	Method      string `json:"method"`
	Path        string `json:"path"` // without the /client/v4 prefix
	Query       string `json:"query,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	Body        string `json:"body,omitempty"` // first MaxBody bytes
	Status      int    `json:"status"`
	// ErrorCodes are the codes of the response envelope's errors, if any.
	ErrorCodes []int `json:"error_codes,omitempty"`
	// Shim is the discrepancy ID whose shim answered instead of flarefake ("" = flarefake).
	Shim string `json:"shim,omitempty"`
}

// NoRoute reports whether flarefake answered that it has no such route (404/7000) or method
// (405/10405), i.e. the client called an endpoint flarefake does not emulate.
func (r Request) NoRoute() bool {
	for _, c := range r.ErrorCodes {
		if c == 7000 || c == 10405 {
			return true
		}
	}
	return false
}

// MaxBody bounds the captured request body.
const MaxBody = 2048

// Shim answers matching requests instead of flarefake. It exists only so that a client flow
// can continue past a known discrepancy.
type Shim struct {
	Discrepancy string         // discrepancy ID, e.g. "W-SERVICES-404"
	Method      string         // "" = any
	Path        *regexp.Regexp // matched against the path without /client/v4
	// Serve answers the request; body is the request body (already read, and r.Body is reset to
	// it). Serve may call next.ServeHTTP to let flarefake answer after all.
	Serve func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler)
}

// Fake is a running in-process flarefake.
type Fake struct {
	Server *fake.Server
	HTTP   *httptest.Server
	// Root is the server root (http://127.0.0.1:port); API is Root + "/client/v4".
	Root, API string

	mu       sync.Mutex
	requests []Request
	shims    []Shim
}

// Options configures Start.
type Options struct {
	// NoSpec disables request validation and the generic profile. By default requests are
	// validated against the pinned spec and violations are journaled (the flarefake default).
	NoSpec bool
	// RejectSchemaViolations answers spec-invalid requests with 400 (flarefake strict mode).
	RejectSchemaViolations bool
}

// Start starts flarefake on a random loopback port and stops it when t ends.
func Start(t testing.TB, o Options) *Fake {
	t.Helper()
	opts := fake.Options{RejectSchemaViolations: o.RejectSchemaViolations}
	if !o.NoSpec {
		spec, err := fake.LoadDefaultSpec()
		if err != nil {
			t.Fatalf("load pinned spec: %v", err)
		}
		opts.Spec = spec
		opts.Generic = fake.GeneratedGenericKinds()
	}
	f := &Fake{Server: fake.New(opts)}
	f.HTTP = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.HTTP.Close)
	f.Root = f.HTTP.URL
	f.API = f.HTTP.URL + "/client/v4"
	return f
}

// AddShim registers a shim; a later shim wins over an earlier one for the same request.
func (f *Fake) AddShim(s Shim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shims = append([]Shim{s}, f.shims...)
}

type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer // response body prefix, for the error codes
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if room := 64<<10 - w.buf.Len(); room > 0 {
		w.buf.Write(b[:min(room, len(b))])
	}
	return w.ResponseWriter.Write(b)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		f.Server.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	p := strings.TrimPrefix(r.URL.Path, "/client/v4")
	req := Request{
		Method: r.Method, Path: p, Query: r.URL.RawQuery,
		ContentType: r.Header.Get("Content-Type"), UserAgent: r.Header.Get("User-Agent"),
		Body: string(body),
	}
	if len(body) > MaxBody {
		req.Body = string(body[:MaxBody]) + "…"
	}
	cw := &captureWriter{ResponseWriter: w}

	f.mu.Lock()
	var shim *Shim
	for i := range f.shims {
		if s := f.shims[i]; (s.Method == "" || s.Method == r.Method) && s.Path.MatchString(p) {
			shim = &s
			break
		}
	}
	f.mu.Unlock()

	if shim != nil {
		req.Shim = shim.Discrepancy
		shim.Serve(cw, r, body, f.Server)
	} else {
		f.Server.ServeHTTP(cw, r)
	}
	req.Status = cw.status
	if cw.status >= 400 {
		var env struct {
			Errors []APIError `json:"errors"`
		}
		if json.Unmarshal(cw.buf.Bytes(), &env) == nil {
			for _, e := range env.Errors {
				req.ErrorCodes = append(req.ErrorCodes, e.Code)
			}
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
}

// Requests returns the captured requests in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Mark returns the current position in the request log, for RequestsSince.
func (f *Fake) Mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// RequestsSince returns the requests captured after mark.
func (f *Fake) RequestsSince(mark int) []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests[mark:]...)
}

// Journal returns flarefake's own request journal (it carries the spec-validation results).
func (f *Fake) Journal() []fake.JournalEntry { return f.Server.Journal() }

// SchemaViolations returns the journaled requests that broke the pinned spec, as
// "METHOD path: first line of the violation", deduplicated and sorted.
func (f *Fake) SchemaViolations() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range f.Journal() {
		if e.SchemaViolation == "" {
			continue
		}
		k := fmt.Sprintf("%s %s: %s", e.Method, e.Path, firstLine(e.SchemaViolation))
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Unanswered returns the captured requests that hit a route flarefake does not emulate
// (Request.NoRoute) and no shim answered, as "METHOD path" (deduplicated, in order).
func (f *Fake) Unanswered() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range f.Requests() {
		if r.Shim == "" && r.NoRoute() {
			if k := r.Method + " " + r.Path; !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// Log writes reqs to t, one line each.
func Log(t testing.TB, reqs []Request) {
	t.Helper()
	for _, r := range reqs {
		line := r.Method + " " + r.Path
		if r.Query != "" {
			line += "?" + r.Query
		}
		line += fmt.Sprintf(" -> %d", r.Status)
		if len(r.ErrorCodes) > 0 {
			line += fmt.Sprintf(" %v", r.ErrorCodes)
		}
		if r.Shim != "" {
			line += " [shim " + r.Shim + "]"
		}
		t.Logf("  %s", line)
	}
}

// CaptureDirEnv names the directory that WriteCapture writes to; unset means no capture.
const CaptureDirEnv = "FLARE_DIFF_CAPTURE_DIR"

// WriteCapture writes every request f captured to $FLARE_DIFF_CAPTURE_DIR/<name>.json, when
// the variable is set.
func WriteCapture(t testing.TB, f *Fake, name string) {
	t.Helper()
	dir := os.Getenv(CaptureDirEnv)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, name+".json")
	if err := WriteRequests(file, f.Requests()); err != nil {
		t.Fatal(err)
	}
	t.Logf("captured requests written to %s", file)
}

// WriteRequests writes reqs as indented JSON to file.
func WriteRequests(file string, reqs []Request) error {
	b, err := json.MarshalIndent(reqs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0o644)
}

// Envelope is a decoded Cloudflare v4 response.
type Envelope struct {
	Success bool            `json:"success"`
	Errors  []APIError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

// Call sends a request straight to flarefake (not captured, no shims) and decodes the
// envelope. Shims use it to look at flarefake's state.
func (f *Fake) Call(method, path string, body []byte) (int, Envelope) {
	req := httptest.NewRequest(method, "/client/v4"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.Server.ServeHTTP(rec, req)
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env
}

// Control calls flarefake's test control API (/_fake<path>, e.g.
// "/accounts/{a}/tunnels/{id}/connect") and returns the status and body.
func (f *Fake) Control(method, path, body string) (int, string) {
	req := httptest.NewRequest(method, "/_fake"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.Server.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Forward lets flarefake answer r as if it had been sent to path (with query, "" = keep r's).
func Forward(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler, path, query string) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/client/v4" + path
	r2.URL.RawPath = ""
	if query != "" {
		r2.URL.RawQuery = query
	}
	r2.RequestURI = ""
	r2.Body = io.NopCloser(bytes.NewReader(body))
	next.ServeHTTP(w, r2)
}

// APIError is one entry of an envelope's errors.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// WriteEnvelope writes a Cloudflare v4 envelope (for shims).
func WriteEnvelope(w http.ResponseWriter, status int, result any, errs ...APIError) {
	if errs == nil {
		errs = []APIError{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": status < 300, "errors": errs, "messages": []any{}, "result": result,
	})
}
