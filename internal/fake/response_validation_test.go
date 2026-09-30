package fake

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// recordedViolations validates every recorded real-API response against the pinned spec and
// returns the violations keyed by recording number (NNNN).
func recordedViolations(t *testing.T, spec *Spec) map[string]ResponseViolation {
	t.Helper()
	out := map[string]ResponseViolation{}
	for _, rec := range loadRecordings(t) {
		req, _ := http.NewRequest(rec.Method, "http://x/client/v4"+rec.Path, nil)
		h := http.Header{}
		for k, v := range rec.ResponseHeaders {
			h.Set(k, v)
		}
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/json")
		}
		body := []byte(rec.ResponseBody)
		op, errs := spec.ValidateResponse(req, rec.Status, h, body)
		if len(errs) == 0 {
			continue
		}
		path := rec.Path
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		out[rec.file[:4]] = ResponseViolation{Method: rec.Method, Operation: op, Path: path, Status: rec.Status, Errors: errs}
	}
	return out
}

// TestRecordedResponsesAgainstSpec logs how the real API's recorded responses violate the
// pinned spec: the evidence base for responseAllowlist.
func TestRecordedResponsesAgainstSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	vs := recordedViolations(t, spec)
	keys := make([]string, 0, len(vs))
	for k := range vs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("recording %s: %s", k, vs[k])
	}
	t.Logf("%d of the recorded real responses violate the pinned spec", len(vs))
}

// TestResponseAllowlistReproducedByRecordings checks every allowlist entry's evidence: a
// recording-backed entry must match a violation of its cited recording's real response; an
// unsatisfiable entry's schema must reject every candidate result value.
func TestResponseAllowlistReproducedByRecordings(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	vs := recordedViolations(t, spec)
	ids := map[string]bool{}
	for _, a := range responseAllowlist {
		if ids[a.id] {
			t.Errorf("duplicate allowlist id %s", a.id)
		}
		ids[a.id] = true
		kinds := 0
		for _, k := range []bool{a.recording != "", a.unsatisfiable} {
			if k {
				kinds++
			}
		}
		if kinds != 1 || a.why == "" || a.errRe == nil || len(a.operations) == 0 {
			t.Errorf("%s: needs exactly one kind of evidence, a reason, errRe and operations", a.id)
			continue
		}
		for _, op := range a.operations {
			if op == "*" && a.requires == nil {
				t.Errorf("%s: an any-operation entry needs a requires signature", a.id)
			}
		}
		switch {
		case a.recording != "":
			v, ok := vs[a.recording]
			if !ok {
				t.Errorf("%s: recording %s shows no spec violation", a.id, a.recording)
				continue
			}
			matched := false
			for _, e := range v.Errors {
				matched = matched || a.covers(&v, e)
			}
			if !matched {
				t.Errorf("%s: recording %s does not show this violation (it shows %s)", a.id, a.recording, v)
			}
		case a.unsatisfiable:
			for _, op := range a.operations {
				method := a.method
				if method == "" {
					method = http.MethodGet
				}
				path := strings.NewReplacer("{account_id}", "a", "{tunnel_id}", "t", "{script_name}", "s",
					"{database_id}", "8b1f2e4c-3a5d-4e6f-9a7b-0c1d2e3f4a5b", "{project_name}", "p",
					"{deployment_id}", "8b1f2e4c-3a5d-4e6f-9a7b-0c1d2e3f4a5b").Replace(op)
				req, _ := http.NewRequest(method, "http://x/client/v4"+path, nil)
				if a.probes != nil {
					// A nested field: every candidate in its place fails, and only there.
					probe, ok := a.probes[op]
					if !ok {
						t.Errorf("%s: no probe for %s", a.id, op)
						continue
					}
					for _, cand := range []string{`null`, `{}`, `[]`, `""`} {
						body := []byte(strings.ReplaceAll(probe, "%s", cand))
						got, errs := spec.ValidateResponse(req, a.status, http.Header{"Content-Type": {"application/json"}}, body)
						if got != op {
							t.Errorf("%s: %s %s resolves to operation %q", a.id, method, path, got)
						}
						if len(errs) == 0 {
							t.Errorf("%s: %s validates on %s %s, so the field is satisfiable", a.id, cand, method, op)
						}
						for _, e := range errs {
							if cand == `null` && !a.errRe.MatchString(e) {
								t.Errorf("%s: the probe of %s fails elsewhere too: %s", a.id, op, e)
							}
						}
					}
					continue
				}
				// A success status gets success envelopes; a 4XX entry gets failure envelopes
				// (with an ordinary error code) at status 400.
				status, envelope := a.status, `{"success":true,"errors":[],"messages":[],"result":%s}`
				if a.status4xx {
					status, envelope = http.StatusBadRequest, `{"success":false,"errors":[{"code":10021,"message":"m"}],"messages":[],"result":%s}`
				}
				for _, cand := range []string{`null`, `{}`, `[]`, `""`} {
					body := []byte(strings.Replace(envelope, "%s", cand, 1))
					got, errs := spec.ValidateResponse(req, status, http.Header{"Content-Type": {"application/json"}}, body)
					if got != op {
						t.Errorf("%s: %s %s resolves to operation %q", a.id, method, path, got)
					}
					if len(errs) == 0 {
						t.Errorf("%s: result %s validates on %s %s, so the schema is satisfiable", a.id, cand, method, op)
					}
				}
			}
		}
	}
	// The any-operation 4XX entry cites 0095; the virtual-network delete 0163 shows it too.
	if v, ok := vs["0163"]; !ok || v.Allowed == "" && !func() bool {
		classifyResponseViolation(&v)
		return v.Allowed != ""
	}() {
		t.Errorf("recording 0163's violation is not covered by the allowlist: %v", v)
	}
}

// TestResponseValidationJournals: a response that violates the spec is journaled but still
// served unchanged, and the strict collector sees it.
func TestResponseValidationJournals(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the pinned spec")
	}
	spec, err := LoadDefaultSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Spec: spec, ValidateResponses: true, NoStrictResponses: true})
	// An error envelope whose code is a string violates every spec error schema.
	req, _ := http.NewRequest(http.MethodGet, "http://x/client/v4/accounts/a/storage/kv/namespaces", nil)
	body, _ := json.Marshal(map[string]any{"success": false, "errors": []any{map[string]any{"code": "x", "message": "m"}}, "messages": []any{}, "result": nil})
	op, errs := spec.ValidateResponse(req, http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}, body)
	if op != "/accounts/{account_id}/storage/kv/namespaces" || len(errs) == 0 {
		t.Fatalf("ValidateResponse = %q, %v; want a violation", op, errs)
	}
	var entry JournalEntry
	s.validateResponse(req, "/accounts/a/storage/kv/namespaces", http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}, body, &entry)
	if entry.ResponseViolation == "" || len(s.ResponseViolations()) != 1 {
		t.Fatalf("violation not journaled: %+v, %v", entry, s.ResponseViolations())
	}
	// A normal round trip on a validating server records nothing.
	c := newClient(t, s)
	if st, _, _ := c.do(http.MethodPost, "/client/v4/accounts/a/storage/kv/namespaces", map[string]any{"title": "flare-spike-x"}); st != http.StatusOK {
		t.Fatalf("create: %d", st)
	}
	for _, j := range s.Journal() {
		if j.ResponseViolation != "" {
			t.Errorf("unexpected response violation: %s", j.ResponseViolation)
		}
	}
}
