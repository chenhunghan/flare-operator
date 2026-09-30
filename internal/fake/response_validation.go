package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

// Response validation checks every emulated response body against the pinned spec's response
// schema for the matched operation (kin-openapi openapi3filter.ValidateResponse). It never
// changes a response: violations are journaled (JournalEntry.ResponseViolation) and collected
// (Server.ResponseViolations), and tests fail on any violation not in responseAllowlist.
// See docs/emulator-fidelity.md §3.

// ResponseViolation is one emulated response that does not match the pinned spec.
type ResponseViolation struct {
	Method string `json:"method"`
	// Operation is the spec's path template (e.g. /accounts/{account_id}/storage/kv/namespaces).
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	// Errors are the individual schema failures ("<json pointer>: <reason>"), sorted.
	Errors []string `json:"errors"`
	// Allowed names the responseAllowlist entry that covers every error, when one does.
	Allowed string `json:"allowed,omitempty"`
}

func (v ResponseViolation) String() string {
	return fmt.Sprintf("%s %s (%s) %d: %s", v.Method, v.Path, v.Operation, v.Status, strings.Join(v.Errors, "; "))
}

// ValidateResponse validates a response (status, headers, body) to request r (whose URL path
// includes /client/v4) against the spec. It returns the operation's path template and the
// individual schema errors; an operation the spec does not define yields ("", nil).
func (s *Spec) ValidateResponse(r *http.Request, status int, header http.Header, body []byte) (string, []string) {
	clone := r.Clone(context.Background())
	clone.Body = http.NoBody
	route, params, err := s.router.FindRoute(clone)
	if err != nil {
		return "", nil
	}
	opts := &openapi3filter.Options{
		AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
		MultiError:            true,
		IncludeResponseStatus: true,
	}
	opts.WithCustomSchemaErrorFunc(func(e *openapi3.SchemaError) string {
		return "/" + strings.Join(e.JSONPointer(), "/") + ": " + e.Reason
	})
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: clone, PathParams: params, Route: route, Options: opts,
		},
		Status:  status,
		Header:  header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		Options: opts,
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		return route.Path, flattenSchemaErrors(err)
	}
	return route.Path, nil
}

// flattenSchemaErrors turns kin-openapi's nested error tree into sorted, de-duplicated leaf
// messages ("<json pointer>: <reason>"). allOf failures are expanded into their branches'
// errors; oneOf/anyOf failures keep their own reason (a branch mismatch is not a defect).
func flattenSchemaErrors(err error) []string {
	seen := map[string]bool{}
	var walk func(error)
	walk = func(e error) {
		switch x := e.(type) {
		case nil:
		case openapi3.MultiError:
			for _, y := range x {
				walk(y)
			}
		case *openapi3filter.ResponseError:
			if x.Err != nil {
				walk(x.Err)
			} else {
				seen["(response): "+x.Reason] = true
			}
		case *openapi3.SchemaError:
			if x.SchemaField == "allOf" && x.Origin != nil {
				if inner := errors.Unwrap(errors.Unwrap(x.Origin)); inner != nil {
					walk(inner)
					return
				}
			}
			seen["/"+strings.Join(x.JSONPointer(), "/")+": "+x.Reason] = true
		default:
			if u := errors.Unwrap(e); u != nil {
				walk(u)
				return
			}
			seen["(response): "+e.Error()] = true
		}
	}
	walk(err)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// allowedResponseViolation is a known way in which the real API (or the pinned spec itself)
// makes a spec-conforming response impossible. Evidence, strongest first:
//   - recording: the cited recording's real response shows the same violation;
//     TestResponseAllowlistReproducedByRecordings re-validates it, so an entry cannot outlive
//     its evidence.
//   - unsatisfiable: the spec's schema for the operation/status accepts no value at all for the
//     failing field (checked by the same test with candidate values).
//
// Field reports (a real response quoted in an issue) are not evidence here: without a
// recording the emulator sends a spec-conformant response instead.
type allowedResponseViolation struct {
	id     string
	method string // "" = any
	// operations are spec path templates ("*" = any operation, then requires must be set).
	operations []string
	status     int  // 0 = any
	status4xx  bool // any 4xx status
	// errRe must match the whole schema error ("<json pointer>: <reason>").
	errRe *regexp.Regexp
	// requires, when set, must match another error of the same response (a signature that
	// pins the entry to one kind of spec defect).
	requires *regexp.Regexp

	recording     string // NNNN of the recording showing the real API does this
	unsatisfiable bool
	// probes (unsatisfiable entries whose failing field is nested) are, per operation, a
	// whole response body with one %s where the field goes: the test substitutes each candidate
	// value there and requires that every error of every candidate matches errRe, which proves
	// that no value of that field validates while the rest of the body does.
	probes map[string]string
	why    string
}

func (a allowedResponseViolation) covers(v *ResponseViolation, e string) bool {
	if a.method != "" && a.method != v.Method || a.status != 0 && a.status != v.Status ||
		a.status4xx && (v.Status < 400 || v.Status > 499) || !a.errRe.MatchString(e) {
		return false
	}
	opOK := false
	for _, op := range a.operations {
		opOK = opOK || op == "*" || op == v.Operation
	}
	if !opOK {
		return false
	}
	if a.requires != nil {
		found := false
		for _, other := range v.Errors {
			found = found || a.requires.MatchString(other)
		}
		return found
	}
	return true
}

const (
	opTunnels       = "/accounts/{account_id}/cfd_tunnel"
	opTunnel        = "/accounts/{account_id}/cfd_tunnel/{tunnel_id}"
	opTunnelConfig  = "/accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations"
	opTunnelConns   = "/accounts/{account_id}/cfd_tunnel/{tunnel_id}/connections"
	opVnets         = "/accounts/{account_id}/teamnet/virtual_networks"
	opVnet          = "/accounts/{account_id}/teamnet/virtual_networks/{virtual_network_id}"
	opScript        = "/accounts/{account_id}/workers/scripts/{script_name}"
	opScriptSetting = "/accounts/{account_id}/workers/scripts/{script_name}/settings"
	opScriptVers    = "/accounts/{account_id}/workers/scripts/{script_name}/versions"
	opD1List        = "/accounts/{account_id}/d1/database"
	opD1            = "/accounts/{account_id}/d1/database/{database_id}"
	opD1Query       = "/accounts/{account_id}/d1/database/{database_id}/query"
	opQueues        = "/accounts/{account_id}/queues"
	opPagesProjects = "/accounts/{account_id}/pages/projects"
	opPagesProject  = "/accounts/{account_id}/pages/projects/{project_name}"
	opPagesDeploys  = "/accounts/{account_id}/pages/projects/{project_name}/deployments"
	opPagesDeploy   = "/accounts/{account_id}/pages/projects/{project_name}/deployments/{deployment_id}"
)

// Probe bodies of the Pages allowlist entries: a conforming project and deployment (the
// emulator's shapes), with %s where the unsatisfiable field goes.
const (
	pagesProbeConfig = `{"env_vars":null,"fail_open":true,"always_use_latest_compatibility_date":false,"compatibility_date":"2026-09-30",` +
		`"compatibility_flags":[],"build_image_major_version":3,"usage_model":"standard"}`
	pagesProbeBuild = `{"build_caching":null,"build_command":null,"destination_dir":null,"root_dir":null,"web_analytics_tag":null,"web_analytics_token":null}`
	pagesProbeStage = `{"name":"deploy","status":"success","started_on":"2026-09-30T00:00:00.000000Z","ended_on":"2026-09-30T00:00:01.000000Z"}`
	// pagesProbeDeployment has %[1]s for source.
	pagesProbeDeployment = `{"id":"8b1f2e4c-3a5d-4e6f-9a7b-0c1d2e3f4a5b","short_id":"8b1f2e4c","project_id":"7b162ea7-7367-4d67-bcde-1160995d5aaa",` +
		`"project_name":"p","environment":"production","url":"https://8b1f2e4c.p.pages.dev","aliases":null,` +
		`"created_on":"2026-09-30T00:00:00.000000Z","modified_on":"2026-09-30T00:00:01.000000Z","is_skipped":false,"skip_reason":null,` +
		`"latest_stage":` + pagesProbeStage + `,"stages":[` + pagesProbeStage + `],` +
		`"deployment_trigger":{"type":"ad_hoc","metadata":{"branch":"main","commit_hash":"","commit_message":"","commit_dirty":false}},` +
		`"build_config":` + pagesProbeBuild + `,"env_vars":null,"uses_functions":null,"source":%[1]s}`
	// pagesProbeProject has %[1]s for latest_deployment and %[2]s for canonical_deployment.
	pagesProbeProject = `{"id":"7b162ea7-7367-4d67-bcde-1160995d5aaa","name":"p","subdomain":"p.pages.dev","domains":["p.pages.dev"],` +
		`"created_on":"2026-09-30T00:00:00.000000Z","production_branch":"main","production_script_name":"pages-worker--1-production",` +
		`"preview_script_name":"pages-worker--1-preview","uses_functions":null,"framework":"","framework_version":"","build_config":` + pagesProbeBuild +
		`,"deployment_configs":{"production":` + pagesProbeConfig + `,"preview":` + pagesProbeConfig + `},` +
		`"latest_deployment":%[1]s,"canonical_deployment":%[2]s}`
	pagesProbeEnvelope = `{"success":true,"errors":[],"messages":[],"result":%s}`
	pagesProbeList     = `{"success":true,"errors":[],"messages":[],"result_info":{"page":1,"per_page":10,"count":1,"total_count":1,"total_pages":1},"result":[%s]}`
)

// pagesProbe fills a probe template: every %s of the Sprintf-ready inner template becomes the
// candidate placeholder "%s" of the probe.
func pagesProbe(outer, inner string, args ...any) string {
	return strings.Replace(outer, "%s", fmt.Sprintf(inner, args...), 1)
}

var reSuccessTrue = regexp.MustCompile(`^/success: value is not one of the allowed values \[true\]$`)

// responseAllowlist: violations the emulator reproduces on purpose. Every recording-backed
// entry was found by TestRecordedResponsesAgainstSpec, which validates every real recorded
// response against the pinned spec.
var responseAllowlist = []allowedResponseViolation{
	{id: "queues-list-null-envelope", method: http.MethodGet, operations: []string{opQueues}, status: 200,
		errRe: regexp.MustCompile(`^/(errors|messages): Value is not nullable$`), recording: "0148",
		why: "Queues list sends errors and messages as null"},
	{id: "versions-list-null-envelope", method: http.MethodGet, operations: []string{opScriptVers}, status: 200,
		errRe: regexp.MustCompile(`^/(errors|messages): Value is not nullable$`), recording: "0038",
		why: "Workers versions list sends errors and messages as null"},
	{id: "d1-list-result-array", method: http.MethodGet, operations: []string{opD1List}, status: 200,
		errRe: regexp.MustCompile(`^/result: value must be an object$`), recording: "0021",
		why: "the spec types D1 list's result as an object; the API returns an array"},
	{id: "d1-error-bare-envelope", method: http.MethodPost, operations: []string{opD1List}, status4xx: true,
		errRe: regexp.MustCompile(`^/(messages|result): property "(messages|result)" is missing$`), recording: "0019",
		why: "D1 validation errors carry only success and errors"},
	{id: "d1-delete-null-result", method: http.MethodDelete, operations: []string{opD1}, status: 200,
		errRe: regexp.MustCompile(`^/result: Value is not nullable$`), recording: "0026",
		why: "D1 delete answers result null; the spec wants an object"},
	{id: "worker-upload-null-log-fields", method: http.MethodPut, operations: []string{opScript}, status: 200,
		errRe: regexp.MustCompile(`^/result/observability/logs/[a-z_]+: Value is not nullable$`), recording: "0036",
		why: "the upload response echoes unset observability.logs fields as null (0036: persist)"},
	{id: "worker-delete-result-object", method: http.MethodDelete, operations: []string{opScript}, status: 200,
		errRe: regexp.MustCompile(`^/result: value is not one of the allowed values \[null\]$`), recording: "0108",
		why: "script delete returns {id: <tag>}; the spec wants null"},
	{id: "worker-settings-placement-empty", operations: []string{opScriptSetting}, status: 200,
		errRe: regexp.MustCompile(`^/result/placement: value doesn't match any schema from "oneOf"$`), recording: "0065",
		why: "settings report placement {} when none is set, which matches no spec placement variant (GET recorded; the emulated PATCH returns the same settings object)"},
	{id: "tunnel-null-timestamps", operations: []string{opTunnels, opTunnel}, status: 200,
		errRe: regexp.MustCompile(`^/result(/\d+)?/(conns_active_at|conns_inactive_at|deleted_at): Value is not nullable$`), recording: "0040",
		why: "tunnel objects carry conns_active_at, conns_inactive_at and deleted_at as null when unset (0040, 0043, 0097); list items are the same object"},
	{id: "tunnel-config-null", method: http.MethodGet, operations: []string{opTunnelConfig}, status: 200,
		errRe: regexp.MustCompile(`^/result/config: Value is not nullable$`), recording: "0044",
		why: "a never-configured tunnel reports config null"},
	{id: "tunnel-config-catchall-hostname", operations: []string{opTunnelConfig}, status: 200,
		errRe: regexp.MustCompile(`^/result/config/ingress/\d+/hostname: property "hostname" is missing$`), recording: "0045",
		why: "the catch-all ingress rule has no hostname (the API requires that, 0050); the spec requires one on every rule"},
	{id: "vnet-null-deleted-at", operations: []string{opVnets, opVnet}, status: 200,
		errRe: regexp.MustCompile(`^/result(/\d+)?/deleted_at: Value is not nullable$`), recording: "0160",
		why: "virtual networks carry deleted_at null when not deleted; create/delete return the same object"},
	{id: "unsatisfiable-4xx-allof-success", operations: []string{"*"}, status4xx: true,
		errRe:    regexp.MustCompile(`^/(success: value is not one of the allowed values \[true\]|result: Value is not nullable|result: doesn't match any schema from "anyOf")$`),
		requires: reSuccessTrue, recording: "0095",
		why: "many 4XX schemas are allOf(<success response>, <failure>), which demands success true and false at once; real errors (0095 tunnel, 0163 vnet) violate it"},
	{id: "d1-query-result-unsatisfiable", method: http.MethodPost, operations: []string{opD1Query}, status: 200,
		errRe: regexp.MustCompile(`^/result: value must be an object$`), unsatisfiable: true,
		why: "the query response is allOf(d1_api-response-common with result an object, result an array of query results): no result value validates (the D1 list's result has the same defect, 0021)"},
	{id: "versions-upload-4xx-unsatisfiable", method: http.MethodPost, operations: []string{opScriptVers}, status4xx: true,
		errRe: regexp.MustCompile(`^/: doesn't match any schema from "anyOf"$`), unsatisfiable: true,
		why: "the version upload's 4XX is anyOf(allOf(<success response>, <failure>), exports-reconciliation error with code 100402 only): no other error validates (the same success-and-failure defect as 0095)"},
	{id: "tunnel-empty-response-unsatisfiable", method: http.MethodDelete, operations: []string{opTunnelConns}, status: 200,
		errRe: regexp.MustCompile(`^/result: doesn't match any schema from "anyOf"$`), unsatisfiable: true,
		why: "tunnel_empty_response is allOf(result anyOf[object,array,string], result enum [null]): no result value validates"},
	{id: "pages-no-deployment-unsatisfiable", operations: []string{opPagesProjects, opPagesProject}, status: 200,
		errRe: regexp.MustCompile(`^/result(/\d+)?/(latest|canonical)_deployment: Value is not nullable$`), unsatisfiable: true,
		probes: map[string]string{
			opPagesProjects: pagesProbe(pagesProbeList, pagesProbeProject, "%s", "%s"),
			opPagesProject:  pagesProbe(pagesProbeEnvelope, pagesProbeProject, "%s", "%s"),
		},
		why: "a project without (production) deployments has none to report, and pages_project requires latest_deployment and canonical_deployment " +
			"as allOf(pages_deployment, {nullable: true}): the nullable is on the other allOf branch, so null never validates, nor does {}, [] or \"\""},
	{id: "pages-direct-upload-source-unsatisfiable", operations: []string{opPagesProjects, opPagesProject, opPagesDeploys, opPagesDeploy}, status: 200,
		errRe: regexp.MustCompile(`^/result(/\d+)?(/(latest|canonical)_deployment)?/source: Value is not nullable$`), unsatisfiable: true,
		probes: map[string]string{
			opPagesProjects: pagesProbe(pagesProbeList, pagesProbeProject, fmt.Sprintf(pagesProbeDeployment, "%s"), fmt.Sprintf(pagesProbeDeployment, "%s")),
			opPagesProject:  pagesProbe(pagesProbeEnvelope, pagesProbeProject, fmt.Sprintf(pagesProbeDeployment, "%s"), fmt.Sprintf(pagesProbeDeployment, "%s")),
			opPagesDeploys:  pagesProbe(pagesProbeList, pagesProbeDeployment, "%s"),
			opPagesDeploy:   pagesProbe(pagesProbeEnvelope, pagesProbeDeployment, "%s"),
		},
		why: "a Direct Upload deployment has no Git source, and pages_deployment requires source as pages_source (type github|gitlab and a full " +
			"repository config): neither null, {}, [] nor \"\" validates, and the emulator does not invent a repository (the live value is UNVERIFIED)"},
}

// classify marks v.Allowed when every error is covered by an allowlist entry.
func classifyResponseViolation(v *ResponseViolation) {
	var ids []string
	for _, e := range v.Errors {
		id := ""
		for _, a := range responseAllowlist {
			if a.covers(v, e) {
				id = a.id
				break
			}
		}
		if id == "" {
			return
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	v.Allowed = strings.Join(dedupe(ids), ",")
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// ---- test-mode collection -------------------------------------------------------------------

// strictSpec, when set, makes every New() validate responses (even with Options.Spec unset)
// and report violations to the process-wide collector. Test mains turn it on with
// EnableStrictResponses; the flarefake binary never does.
var (
	strictMu         sync.Mutex
	strictSpec       *Spec
	strictViolations []ResponseViolation
)

// EnableStrictResponses turns on response validation against spec for every Server created
// afterwards in this process, collecting violations for StrictResponseViolations. It is meant
// for TestMain functions (internal/fake, internal/testenv); see StrictMain.
func EnableStrictResponses(spec *Spec) {
	strictMu.Lock()
	defer strictMu.Unlock()
	strictSpec = spec
}

// StrictResponseViolations returns the violations collected in strict mode that no allowlist
// entry covers, de-duplicated by operation, status and errors.
func StrictResponseViolations() []ResponseViolation {
	strictMu.Lock()
	defer strictMu.Unlock()
	seen := map[string]bool{}
	var out []ResponseViolation
	for _, v := range strictViolations {
		if v.Allowed != "" {
			continue
		}
		k := v.Method + " " + v.Operation + fmt.Sprint(v.Status) + strings.Join(v.Errors, "|")
		if !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out
}

// StrictResponseError summarizes StrictResponseViolations as an error (nil when there are
// none), for a TestMain to print before failing the run.
func StrictResponseError() error {
	vs := StrictResponseViolations()
	if len(vs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "flarefake: %d emulated response(s) violate the pinned spec's response schema "+
		"(fix the profile, or allowlist a real-API defect with its recording in responseAllowlist, "+
		"internal/fake/response_validation.go):\n", len(vs))
	for _, v := range vs {
		b.WriteString("  " + v.String() + "\n")
	}
	return errors.New(b.String())
}

func strictResponseSpec() *Spec {
	strictMu.Lock()
	defer strictMu.Unlock()
	return strictSpec
}

func recordStrictViolation(v ResponseViolation) {
	strictMu.Lock()
	defer strictMu.Unlock()
	if strictSpec != nil {
		strictViolations = append(strictViolations, v)
	}
}

// responseSpec is the spec responses are validated against: Options.Spec when
// Options.ValidateResponses is set, else the strict-mode spec (tests), else none.
func (s *Server) responseSpec() *Spec {
	if s.opts.ValidateResponses && s.opts.Spec != nil {
		return s.opts.Spec
	}
	if s.opts.NoStrictResponses {
		return nil
	}
	return strictResponseSpec()
}

// validateResponse checks one written response and records a violation. Callers do not hold s.mu.
func (s *Server) validateResponse(r *http.Request, path string, status int, header http.Header, body []byte, entry *JournalEntry) {
	spec := s.responseSpec()
	if spec == nil {
		return
	}
	op, errs := spec.ValidateResponse(r, status, header, body)
	if len(errs) == 0 {
		return
	}
	v := ResponseViolation{Method: r.Method, Operation: op, Path: path, Status: status, Errors: errs}
	classifyResponseViolation(&v)
	if v.Allowed == "" {
		entry.ResponseViolation = strings.Join(errs, "; ")
	}
	s.mu.Lock()
	if len(s.respViolations) >= maxKeptViolations { // a long-running flarefake keeps the newest
		s.respViolations = s.respViolations[1:]
	}
	s.respViolations = append(s.respViolations, v)
	s.mu.Unlock()
	if spec == strictResponseSpec() && !s.opts.NoStrictResponses {
		recordStrictViolation(v)
	}
	if s.opts.OnResponseViolation != nil {
		s.opts.OnResponseViolation(v)
	}
}

const maxKeptViolations = 1000

// ResponseViolations returns this server's response-schema violations (allowlisted ones
// included, marked Allowed; the newest maxKeptViolations).
func (s *Server) ResponseViolations() []ResponseViolation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResponseViolation(nil), s.respViolations...)
}
