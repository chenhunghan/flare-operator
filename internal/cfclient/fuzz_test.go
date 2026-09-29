package cfclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Fuzz targets. Seeds live in testdata/fuzz/<Target>/ and run with every `go test`; a real fuzz
// run is `go test -fuzz=FuzzDecodeEnvelope ./internal/cfclient/` (not part of CI).

func decodeRaw(status int, body []byte, retryAfter string) (*Response, bool, *APIError) {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return decode(&http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body))})
}

// FuzzDecodeEnvelope: decode never panics; a non-2xx status is always an *APIError with that
// status; a 2xx is an error only for an envelope with success=false; error text copied from the
// body is sanitized and bounded; a well-formed success envelope keeps its result.
func FuzzDecodeEnvelope(f *testing.F) {
	f.Add(200, []byte(`{"success":true,"errors":[],"messages":[],"result":{"id":"x"}}`), "")
	f.Add(200, []byte(`{"success":true,"result":[1,2],"result_info":{"page":1,"per_page":2,"total_pages":3}}`), "")
	f.Add(200, []byte(`{"success":false,"errors":[{"code":10000,"message":"nope"}]}`), "")
	f.Add(404, []byte(`{"success":false,"errors":[{"code":10007,"message":"not found"}],"result":null}`), "")
	f.Add(429, []byte(`{"success":false,"errors":[{"code":971,"message":"slow"}]}`), "30")
	f.Add(503, []byte(`<html>bad gateway</html>`), "Wed, 21 Oct 2015 07:28:00 GMT")
	f.Add(200, []byte(`{"value":"a KV value that is JSON"}`), "")
	f.Add(200, []byte(`not json at all`), "")
	f.Add(204, []byte(``), "")
	f.Add(500, []byte(``), "")
	f.Add(200, []byte(`{"success":true,"result":null}`), "")
	f.Add(400, []byte("{\"success\":false,\"errors\":[{\"code\":1,\"message\":\"line1\\nline2\\u0000\"}]}"), "")
	f.Fuzz(func(t *testing.T, status int, body []byte, retryAfter string) {
		if status < 100 || status > 599 {
			status = 100 + (status%500+500)%500
		}
		resp, isEnvelope, apiErr := decodeRaw(status, body, retryAfter)
		ok2xx := status >= 200 && status < 300
		if !ok2xx {
			if apiErr == nil || resp != nil {
				t.Fatalf("status %d: resp %v err %v, want an APIError", status, resp, apiErr)
			}
		}
		if apiErr != nil {
			if apiErr.Status != status && !(ok2xx && apiErr.Status == http.StatusBadGateway) {
				t.Fatalf("APIError status %d for HTTP %d", apiErr.Status, status)
			}
			if len(apiErr.Errors) > maxErrorsKept+1 {
				t.Fatalf("%d error details kept", len(apiErr.Errors))
			}
			for _, d := range apiErr.Errors {
				if len(d.Message) > maxErrorMessage+len(truncationSuffix) {
					t.Fatalf("error message of %d bytes", len(d.Message))
				}
				if !utf8.ValidString(d.Message) || strings.ContainsFunc(d.Message, unicode.IsControl) {
					t.Fatalf("unsanitized error message %q", d.Message)
				}
			}
			if apiErr.RetryAfter < 0 {
				t.Fatalf("negative RetryAfter %v", apiErr.RetryAfter)
			}
			if apiErr.RetryAfter != 0 && status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
				t.Fatalf("RetryAfter set for HTTP %d", status)
			}
			_ = apiErr.Error()
			return
		}
		if resp == nil || resp.Status != status {
			t.Fatalf("status %d: no response", status)
		}
		if isEnvelope {
			var env struct {
				Success *bool           `json:"success"`
				Result  json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(body, &env); err != nil || env.Success == nil || !*env.Success {
				t.Fatalf("isEnvelope for a body that is not a success envelope: %q", body)
			}
			if !bytes.Equal(bytes.TrimSpace(resp.Result), bytes.TrimSpace(env.Result)) {
				t.Fatalf("result %q, want %q", resp.Result, env.Result)
			}
		} else if len(bytes.TrimSpace(body)) > 0 && !bytes.Equal(resp.Result, body) {
			t.Fatalf("a non-envelope body must be handed back as is")
		}
	})
}

// FuzzRouteTemplate: every path maps to a spec template or OtherRoute (a bounded label set),
// never to a string carrying a path value.
func FuzzRouteTemplate(f *testing.F) {
	known := map[string]bool{OtherRoute: true}
	for _, r := range specRoutes {
		known[r] = true
	}
	f.Add("/accounts/0123456789abcdef0123456789abcdef/storage/kv/namespaces/ns1/values/key%2F1")
	f.Add("/accounts/a/workers/scripts/hello/settings")
	f.Add("//accounts///")
	f.Add("/zones/z/dns_records/export")
	f.Add("/accounts/x/urlscanner/v2/screenshots/y.png")
	f.Fuzz(func(t *testing.T, p string) {
		got := RouteTemplate(p)
		if !known[got] {
			t.Fatalf("RouteTemplate(%q) = %q, not a spec template", p, got)
		}
	})
}

// pagedClient answers ListAll's GETs from a fuzzed script of pages.
type pagedClient struct {
	pages []Response
	calls int
}

func (c *pagedClient) Do(_ context.Context, req Request) (*Response, error) {
	c.calls++
	if len(c.pages) == 0 {
		return &Response{Status: 200, Result: json.RawMessage(`[]`)}, nil
	}
	r := c.pages[(c.calls-1)%len(c.pages)]
	return &r, nil
}

// FuzzListAll: ListAll terminates within MaxListPages requests for any sequence of pages
// (repeating cursors, bogus totals, missing result_info), and never panics.
func FuzzListAll(f *testing.F) {
	f.Add([]byte(`[{"r":[1,2],"i":{"page":1,"per_page":2,"total_pages":2}},{"r":[3],"i":{"page":2,"per_page":2,"total_pages":2}}]`))
	f.Add([]byte(`[{"r":[1],"i":{"cursor":"a"}},{"r":[2],"i":{"cursor":"a"}}]`))
	f.Add([]byte(`[{"r":[1,2],"i":{"per_page":2}}]`))
	f.Add([]byte(`[{"r":[1,2,3],"i":{"per_page":3,"total_count":1000000}}]`))
	f.Add([]byte(`[{"r":"notarray"}]`))
	f.Add([]byte(`[{"r":[1]}]`))
	f.Fuzz(func(t *testing.T, script []byte) {
		var steps []struct {
			R json.RawMessage `json:"r"`
			I *ResultInfo     `json:"i"`
		}
		if json.Unmarshal(script, &steps) != nil || len(steps) > 50 {
			return
		}
		pc := &pagedClient{}
		for _, s := range steps {
			pc.pages = append(pc.pages, Response{Status: 200, Result: s.R, ResultInfo: s.I})
		}
		items, err := ListAll(context.Background(), pc, Request{Path: "/accounts/a/queues"})
		if pc.calls > MaxListPages {
			t.Fatalf("%d calls", pc.calls)
		}
		if err != nil && items != nil {
			t.Fatalf("items with an error: %v", err)
		}
		_ = fmt.Sprint(items)
	})
}
