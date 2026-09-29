package harness

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func do(t *testing.T, f *Fake, method, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, f.API+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+Token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestCaptureAndShims(t *testing.T) {
	f := Start(t, Options{NoSpec: true})
	base := "/accounts/" + AccountID
	do(t, f, http.MethodPost, base+"/storage/kv/namespaces", `{"title":"a"}`)
	do(t, f, http.MethodGet, base+"/no/such/route", "")
	mark := f.Mark()
	f.AddShim(Shim{Discrepancy: "X-1", Method: http.MethodGet, Path: regexp.MustCompile(`/no/such/route$`),
		Serve: func(w http.ResponseWriter, _ *http.Request, _ []byte, _ http.Handler) {
			WriteEnvelope(w, http.StatusOK, map[string]string{"ok": "yes"})
		}})
	do(t, f, http.MethodGet, base+"/no/such/route", "")
	// A forwarding shim lets flarefake answer at another path.
	f.AddShim(Shim{Discrepancy: "X-2", Method: http.MethodGet, Path: regexp.MustCompile(`/alias$`),
		Serve: func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) {
			Forward(w, r, body, next, base+"/storage/kv/namespaces", "")
		}})
	if resp := do(t, f, http.MethodGet, base+"/alias", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("forwarded GET: %d", resp.StatusCode)
	}

	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("captured %d requests, want 4: %+v", len(reqs), reqs)
	}
	if r := reqs[0]; r.Method != http.MethodPost || r.Path != base+"/storage/kv/namespaces" || r.Body != `{"title":"a"}` || r.Status != 200 || r.ContentType != "application/json" {
		t.Errorf("first request %+v", r)
	}
	if r := reqs[1]; !r.NoRoute() || r.Status != http.StatusNotFound {
		t.Errorf("unknown route: %+v, want 404/7000", r)
	}
	if r := reqs[2]; r.Shim != "X-1" || r.Status != http.StatusOK || r.NoRoute() {
		t.Errorf("shimmed request %+v", r)
	}
	if got := f.RequestsSince(mark); len(got) != 2 || got[0].Shim != "X-1" || got[1].Shim != "X-2" {
		t.Errorf("RequestsSince %+v", got)
	}
	if u := f.Unanswered(); len(u) != 1 || u[0] != "GET "+base+"/no/such/route" {
		t.Errorf("Unanswered %v", u)
	}
	if status, env := f.Call(http.MethodGet, base+"/storage/kv/namespaces", nil); status != 200 || !env.Success {
		t.Errorf("Call: %d %+v", status, env)
	}
	if len(f.Requests()) != 4 {
		t.Errorf("Call was captured")
	}
	if st, _ := f.Control(http.MethodPost, "/reset", ""); st != http.StatusOK {
		t.Errorf("control reset: %d", st)
	}
}

func TestDiscrepancyCheck(t *testing.T) {
	d := Discrepancy{ID: "T-1", Client: "c@1", Summary: "s", Evidence: "e"}
	d.Check(t, func() error { return errors.New("still broken") }) // must skip, not fail
	if !strings.Contains(d.String(), "T-1 (c@1): s [evidence: e]") {
		t.Errorf("String() = %q", d.String())
	}
}

func TestUnexplainedSchemaViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	f := Start(t, Options{})
	base := "/accounts/" + AccountID
	do(t, f, http.MethodPost, base+"/storage/kv/namespaces", `{"title":"a"}`)
	status, env := f.Call(http.MethodGet, base+"/storage/kv/namespaces", nil)
	if status != 200 {
		t.Fatal(status)
	}
	id := strings.Split(strings.Split(string(env.Result), `"id":"`)[1], `"`)[0]
	do(t, f, http.MethodDelete, base+"/storage/kv/namespaces/"+id, "") // known spec defect (0013)
	do(t, f, http.MethodPost, base+"/queues", `{"queue_name":1}`)      // a real violation
	all, left := f.SchemaViolations(), f.UnexplainedSchemaViolations()
	if len(all) != 2 || len(left) != 1 || !strings.HasPrefix(left[0], "POST "+base+"/queues:") {
		t.Errorf("SchemaViolations %q, unexplained %q", all, left)
	}
	if got := f.UnexplainedSchemaViolations(left[0]); len(got) != 0 {
		t.Errorf("an explicitly known violation is still reported: %q", got)
	}
}
