// Package live holds the live smoke test (live_test.go, build tag "live") that runs the
// operator's real controllers against the Cloudflare API, and the test-only recording
// RoundTripper it uses. Nothing in cmd/ or internal/ imports this package.
package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Cassette is one recorded exchange, in the JSON format of test/recordings/2026-09-29/*.json
// (the keys internal/fake's conformance test reads), plus request_headers, which carries only
// If-Match and a non-JSON Content-Type when the request had them. Authorization and every
// other request header are never recorded.
type Cassette struct {
	TS              string            `json:"ts"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	RequestBody     any               `json:"request_body"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	Status          int               `json:"status"`
	ElapsedMS       int64             `json:"elapsed_ms"`
	ResponseHeaders map[string]string `json:"response_headers"`
	ResponseBody    any               `json:"response_body"`
}

// recordedResponseHeaders are the response headers the 2026-09-29 recordings keep, plus ETag
// (Resource Tagging may send one).
var recordedResponseHeaders = []string{"Content-Type", "CF-Ray", "Ratelimit", "Ratelimit-Policy", "Etag"}

// Recorder is an http.RoundTripper that forwards to Base and keeps every exchange: in memory
// (Entries, Writes) and, when Dir is set, as numbered cassette files NNNN-<label>.json.
//
// Cassettes are RAW: they contain account IDs, resource IDs, tunnel tokens and anything else the
// API returned. Dir must be outside the repository; run hack/sanitize_recordings.py on it
// before anything is committed.
type Recorder struct {
	Base http.RoundTripper
	// Dir receives the cassettes ("" keeps them in memory only).
	Dir string
	// PathPrefix is stripped from request paths (the API root, "/client/v4").
	PathPrefix string

	mu      sync.Mutex
	n       int
	phase   string
	entries []Cassette
	errs    []error
}

// LabelHeader, when set on a request, replaces the phase in that request's file label. The
// recorder removes it before forwarding and never records it.
const LabelHeader = "X-Flare-Live-Label"

// SetPhase prefixes the labels of the following requests (file names), e.g. "op". Requests are
// labelled <phase>-<method>-<path words>; a request carrying LabelHeader uses that instead.
func (r *Recorder) SetPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

// Len is the number of exchanges recorded so far (a mark for Since).
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Since returns the exchanges recorded after mark (a Len value).
func (r *Recorder) Since(mark int) []Cassette {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mark > len(r.entries) {
		mark = len(r.entries)
	}
	return append([]Cassette(nil), r.entries[mark:]...)
}

// Errors returns cassette write errors (the exchanges themselves still went through).
func (r *Recorder) Errors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errs...)
}

// Writes returns "METHOD path" of every non-GET/HEAD exchange in es.
func Writes(es []Cassette) []string {
	var out []string
	for _, e := range es {
		if e.Method != http.MethodGet && e.Method != http.MethodHead {
			out = append(out, e.Method+" "+e.Path)
		}
	}
	return out
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	label := req.Header.Get(LabelHeader)
	if label != "" {
		req = req.Clone(req.Context())
		req.Header.Del(LabelHeader)
	}
	base := r.Base
	if base == nil {
		base = http.DefaultTransport
	}
	start := time.Now()
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	if rerr != nil {
		return resp, rerr
	}
	elapsed := time.Since(start)

	path := strings.TrimPrefix(req.URL.EscapedPath(), r.PathPrefix)
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	c := Cassette{
		TS:              start.UTC().Format("2006-01-02T15:04:05.000000+00:00"),
		Method:          req.Method,
		Path:            path,
		RequestBody:     bodyValue(reqBody),
		Status:          resp.StatusCode,
		ElapsedMS:       elapsed.Milliseconds(),
		ResponseHeaders: map[string]string{},
		ResponseBody:    bodyValue(respBody),
	}
	if v := req.Header.Get("If-Match"); v != "" {
		c.RequestHeaders = map[string]string{"If-Match": v}
	}
	if ct := req.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		if c.RequestHeaders == nil {
			c.RequestHeaders = map[string]string{}
		}
		c.RequestHeaders["Content-Type"] = ct
	}
	for _, h := range recordedResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.ResponseHeaders[h] = v
		}
	}

	r.mu.Lock()
	r.n++
	n, phase := r.n, r.phase
	r.entries = append(r.entries, c)
	r.mu.Unlock()
	name := Label(phase, req.Method, path)
	if label != "" {
		name = cleanLabel(label)
	}
	if r.Dir != "" {
		if err := writeCassette(filepath.Join(r.Dir, fmt.Sprintf("%04d-%s.json", n, name)), c); err != nil {
			r.mu.Lock()
			r.errs = append(r.errs, err)
			r.mu.Unlock()
		}
	}
	return resp, nil
}

// bodyValue is a body as the recordings store it: decoded JSON, else the raw string, else
// null.
func bodyValue(b []byte) any {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	return string(b)
}

func writeCassette(file string, c Cassette) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(c); err != nil {
		return err
	}
	return os.WriteFile(file, bytes.TrimRight(buf.Bytes(), "\n"), 0o600)
}

var (
	idSegment = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	nonWord   = regexp.MustCompile(`[^a-z0-9]+`)
)

// Label builds a file label: <phase>-<method>-<path words>, without the /accounts/{id} prefix,
// IDs and the query string.
func Label(phase, method, path string) string {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) >= 2 && segs[0] == "accounts" {
		segs = segs[2:]
	}
	words := []string{}
	if phase != "" {
		words = append(words, phase)
	}
	words = append(words, strings.ToLower(method))
	for _, s := range segs {
		if s == "" || idSegment.MatchString(s) {
			continue
		}
		words = append(words, s)
	}
	return cleanLabel(strings.Join(words, "-"))
}

// cleanLabel makes s a file-name label: lower case, [a-z0-9-] only, at most 80 characters.
func cleanLabel(s string) string {
	l := nonWord.ReplaceAllString(strings.ToLower(s), "-")
	l = strings.Trim(l, "-")
	if len(l) > 80 {
		l = strings.TrimRight(l[:80], "-")
	}
	return l
}
