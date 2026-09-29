package fake

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestFaultShapes covers the resilience-test controls of Fault (fault.go).
func TestFaultShapes(t *testing.T) {
	s := New(Options{})
	c := newClient(t, s)
	kv := acct + "/storage/kv/namespaces"
	p := strings.TrimPrefix(kv, "/client/v4")

	// Retry-After header.
	if err := s.InjectFault(Fault{PathRegex: "^" + p + "$", Status: 429, Code: 971, Times: 1, FaultShape: FaultShape{RetryAfter: "7"}}); err != nil {
		t.Fatal(err)
	}
	if st, _, h := c.do("GET", kv, nil); st != 429 || h.Get("Retry-After") != "7" {
		t.Errorf("429 fault: status %d Retry-After %q", st, h.Get("Retry-After"))
	}

	// QueryRegex: only page 2 fails.
	if err := s.InjectFault(Fault{PathRegex: "^" + p + "$", Status: 500, Times: 1, FaultShape: FaultShape{QueryRegex: `(^|&)page=2(&|$)`}}); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := c.do("GET", kv+"?page=1", nil); st != 200 {
		t.Errorf("page 1: %d", st)
	}
	if st, _, _ := c.do("GET", kv+"?page=2", nil); st != 500 {
		t.Errorf("page 2: %d, want the fault", st)
	}

	// Raw body.
	if err := s.InjectFault(Fault{PathRegex: "^" + p + "$", Status: 200, Times: 1, FaultShape: FaultShape{Body: "<html>", ContentType: "text/html"}}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", c.base+kv, nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "<html>" || resp.Header.Get("Content-Type") != "text/html" {
		t.Errorf("raw body fault: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}

	// Passthrough: served (the namespace is created) but answered late.
	if err := s.InjectFault(Fault{Method: "POST", PathRegex: "^" + p + "$", Times: 1, FaultShape: FaultShape{Passthrough: true, DelayMs: 300}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	st, env, _ := c.do("POST", kv, map[string]string{"title": "slow"})
	if st != 200 || !env.Success || time.Since(start) < 300*time.Millisecond {
		t.Errorf("passthrough: %d %v after %v", st, env.Success, time.Since(start))
	}
	j := s.Journal()
	if last := j[len(j)-1]; !last.Fault || last.Status != 200 || last.Method != "POST" {
		t.Errorf("journal of a passthrough fault: %+v", last)
	}
	if err := s.InjectFault(Fault{PathRegex: "x", FaultShape: FaultShape{QueryRegex: "("}}); err == nil {
		t.Error("a bad query regex was accepted")
	}
}
