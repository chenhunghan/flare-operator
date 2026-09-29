package fake

import (
	"net/http"
	"regexp"
	"time"
)

// FaultShape refines a Fault for resilience tests (internal/resilience). These are test
// controls, not emulated Cloudflare behavior: the bodies and headers they produce are whatever
// the test asks for.
type FaultShape struct {
	// QueryRegex, when set, must also match the raw query string (e.g. `(^|&)page=2(&|$)` fails
	// only the second page of a listing).
	QueryRegex string `json:"query_regex,omitempty"`
	// RetryAfter is sent as the Retry-After header (delta-seconds or an HTTP date).
	RetryAfter string `json:"retry_after,omitempty"`
	// DelayMs holds the answer back this long (bounded by the client going away).
	DelayMs int `json:"delay_ms,omitempty"`
	// Passthrough serves the request normally and only delays the answer by DelayMs: a slow
	// response whose side effects (a create) happen even when the client gives up waiting.
	Passthrough bool `json:"passthrough,omitempty"`
	// Body replaces the v4 envelope with this raw body (a malformed or non-envelope answer);
	// ContentType defaults to application/json.
	Body        string `json:"body,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

func (f *Fault) compileShape() error {
	if f.QueryRegex == "" {
		return nil
	}
	re, err := regexp.Compile(f.QueryRegex)
	if err != nil {
		return err
	}
	f.qre = re
	return nil
}

func (f *Fault) matchesQuery(rawQuery string) bool {
	return f.qre == nil || f.qre.MatchString(rawQuery)
}

// delay waits DelayMs or until the client went away.
func (f *Fault) delay(r *http.Request) {
	if f.DelayMs <= 0 {
		return
	}
	t := time.NewTimer(time.Duration(f.DelayMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.Context().Done():
	}
}

// writeFault answers a request with the fault f.
func (s *Server) writeFault(w http.ResponseWriter, r *http.Request, f *Fault) {
	f.delay(r)
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	if f.Body != "" {
		ct := f.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(f.Status)
		_, _ = w.Write([]byte(f.Body))
		return
	}
	writeResponse(w, fail(f.Status, f.Code, f.Message))
}
