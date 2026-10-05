package fake

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Workers Logs: POST /accounts/{account_id}/workers/observability/telemetry/query over an
// in-memory per-account event store, which tests fill through the control API (POST
// /_fake/accounts/{account}/workers/{script}/logs, InjectWorkerLogs). Source recordings:
// test/recordings/2026-09-29/0054 (events view), 0077…0088 and 0124…0130 (ingestion lag, needle),
// 0092…0094 (limit, offset paging), 0096 (limit above 2000), 0098 (30-day window), 0122/0123
// (script-version filter); docs/spike-results-2026-09-29.md §1.
//
// Only view "events" is emulated. Not emulated: the other views (calculations, invocations,
// traces…), telemetry/keys (0047) and telemetry/values (0053), and the newer
// telemetry/live-tail (0107, 0115…0138). The legacy tail is in workers_tail.go.

const (
	telemetryDataset = "cloudflare-workers" // 0054: every event's "dataset"
	// telemetryMaxLimit: limit above 2000 answers 400 ZodError too_big (0096; the spec's
	// maximum too).
	telemetryMaxLimit = 2000
	// telemetryDefaultLimit applies when limit is omitted. UNVERIFIED (every recording sends one).
	telemetryDefaultLimit = 50

	// DefaultLogRetention is how far back a query reaches: a 30-day timeframe comes back with
	// from clamped to the server's now minus 7 days (0098: from 1790060757553 = 07:05:57.553
	// minus 7 d, on the Free plan). Paid-plan retention is UNVERIFIED.
	DefaultLogRetention = 7 * 24 * time.Hour

	telemetryRunIDChars = "0123456789abcdefghijklmnopqrstuvwxyz" // 0054: run.id "lc2igo3yr7c658o5jwltg3e7"
)

// logEvent is one stored telemetry event. body is the event exactly as the query returns it.
type logEvent struct {
	id        string // $metadata.id
	ts        int64  // timestamp (ms)
	visibleAt time.Time
	body      map[string]any
}

// ---- ingestion ---------------------------------------------------------------------------------

// LogTime is a log timestamp. In JSON it is epoch milliseconds (as the API reports them) or an
// RFC 3339 string; it marshals as milliseconds.
type LogTime struct{ time.Time }

func (t LogTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatInt(t.UnixMilli(), 10)), nil
}

func (t *LogTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "null" || s == `""`:
		t.Time = time.Time{}
	case strings.HasPrefix(s, `"`):
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := time.Parse(time.RFC3339Nano, str)
		if err != nil {
			return err
		}
		t.Time = v.UTC()
	default:
		ms, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("timestamp: want epoch milliseconds or RFC 3339, got %s", s)
		}
		t.Time = time.UnixMilli(int64(ms)).UTC()
	}
	return nil
}

// WorkerInvocation describes one Worker invocation (a fetch) and what it logged, for
// InjectWorkerLogs and POST /_fake/accounts/{account}/workers/{script}/logs. Every field is
// optional. It becomes telemetry events (queryable after the ingestion lag) and one trace-v1
// frame for the script's connected tails (immediately).
type WorkerInvocation struct {
	// Timestamp of the invocation; every event of it carries the same timestamp (the API freezes
	// time for the length of a request: 0054 shows the request's events sharing one timestamp).
	// Zero means the emulator's now.
	Timestamp LogTime `json:"timestamp"`
	// ScriptVersion is the version ID ($workers.scriptVersion.id). Empty means the script's
	// deployed version.
	ScriptVersion string `json:"scriptVersion,omitempty"`
	// Outcome: ok (default), exception (default when Exception is set), canceled, exceededCpu,
	// exceededMemory, unknown.
	Outcome string             `json:"outcome,omitempty"`
	Request *InvocationRequest `json:"request,omitempty"`
	// Level and Message are a shorthand for a single log line (appended to Logs).
	Level      string               `json:"level,omitempty"`
	Message    any                  `json:"message,omitempty"`
	Logs       []InvocationLog      `json:"logs,omitempty"`
	Exception  *InvocationException `json:"exception,omitempty"`
	WallTimeMs int                  `json:"wallTimeMs,omitempty"`
	CPUTimeMs  int                  `json:"cpuTimeMs,omitempty"`
	// IngestDelay overrides the server's ingestion lag for these events (a Go duration such as
	// "20s"; "0s" = queryable at once). Empty means the server default (SetLogIngestionLag).
	IngestDelay string `json:"ingestDelay,omitempty"`
}

// InvocationRequest is the request that triggered the invocation.
type InvocationRequest struct {
	Method string `json:"method,omitempty"` // default GET
	// URL defaults to https://<script>.<workers subdomain>.workers.dev/.
	URL     string            `json:"url,omitempty"`
	Status  int               `json:"status,omitempty"` // default 200, or 500 with an exception (0054 /throw)
	Headers map[string]string `json:"headers,omitempty"`
	CF      map[string]any    `json:"cf,omitempty"` // request.cf; omitted when nil
}

// InvocationLog is one console.* call.
type InvocationLog struct {
	// Level: log (default), info, warn, error, debug.
	Level string `json:"level,omitempty"`
	// Message is a string or the array of arguments console.log received.
	Message   any     `json:"message"`
	Timestamp LogTime `json:"timestamp"`
}

// InvocationException is an uncaught exception.
type InvocationException struct {
	Name    string `json:"name,omitempty"` // default Error
	Message any    `json:"message"`
	Stack   string `json:"stack,omitempty"`
}

// InjectedLogs reports what InjectWorkerLogs stored.
type InjectedLogs struct {
	RequestID string    `json:"requestId"`
	EventIDs  []string  `json:"eventIds"`
	VisibleAt time.Time `json:"visibleAt"`
	// TailFrames is how many tail WebSocket connections the trace-v1 frame was queued for.
	TailFrames int `json:"tailFrames"`
}

// SetLogIngestionLag sets how long injected events take to become queryable (emulated clock).
// The real lag was 15–30 s (spike results §1; 0077…0081: 14.7–18.4 s, 0082…0088: 25.3–29.5 s,
// 0124…0130: 22.7–26.4 s); the default is Options.LogIngestionLag (0 = at once).
func (s *Server) SetLogIngestionLag(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLag = d
}

// InjectWorkerLogs stores the telemetry events of one invocation of script in account and
// pushes its trace-v1 frame to the script's connected tails.
func (s *Server) InjectWorkerLogs(account, script string, inv WorkerInvocation) (InjectedLogs, error) {
	if script == "" {
		return InjectedLogs{}, fmt.Errorf("script name is empty")
	}
	lag := time.Duration(-1)
	if inv.IngestDelay != "" {
		d, err := time.ParseDuration(inv.IngestDelay)
		if err != nil || d < 0 {
			return InjectedLogs{}, fmt.Errorf("ingestDelay: want a non-negative duration such as \"20s\", got %q", inv.IngestDelay)
		}
		lag = d
	}
	for _, l := range inv.Logs {
		if !validLogLevel(l.Level) {
			return InjectedLogs{}, fmt.Errorf("log level %q: want log, info, warn, error or debug", l.Level)
		}
	}
	if !validLogLevel(inv.Level) {
		return InjectedLogs{}, fmt.Errorf("level %q: want log, info, warn, error or debug", inv.Level)
	}

	now := s.Clock.Now()
	s.mu.Lock()
	if lag < 0 {
		lag = s.logLag
	}
	a := s.accountLocked(account)
	version := inv.ScriptVersion
	if w, ok := a.scripts[script]; ok && version == "" {
		version = w.deployedVersion().ID
	}
	inv = inv.withDefaults(script, s.opts.WorkersSubdomain, now)
	events, reqID := buildInvocationEvents(a, script, version, inv)
	visible := now.Add(lag)
	ids := make([]string, len(events))
	for i, ev := range events {
		a.logs = append(a.logs, &logEvent{id: ev.id, ts: ev.ts, visibleAt: visible, body: ev.body})
		ids[i] = ev.id
	}
	frame, _ := json.Marshal(traceFrame(script, inv))
	conns := s.tailConnsLocked(account, script, inv, now)
	s.mu.Unlock()

	for _, tc := range conns {
		tc.push(frame)
	}
	return InjectedLogs{RequestID: reqID, EventIDs: ids, VisibleAt: visible, TailFrames: len(conns)}, nil
}

// InjectTelemetryEvents stores raw telemetry events (as the query returns them, with
// $metadata.id and timestamp) that become queryable at visibleAt. It does not reach tails. It
// exists so that tests can replay recorded datasets verbatim.
func (s *Server) InjectTelemetryEvents(account string, visibleAt time.Time, events ...map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accountLocked(account)
	for _, ev := range events {
		md, _ := ev["$metadata"].(map[string]any)
		id, _ := md["id"].(string)
		ts, ok := ev["timestamp"].(float64)
		if id == "" || !ok {
			return fmt.Errorf("event needs $metadata.id and a numeric timestamp: %v", ev)
		}
		a.logs = append(a.logs, &logEvent{id: id, ts: int64(ts), visibleAt: visibleAt, body: ev})
	}
	return nil
}

func validLogLevel(l string) bool {
	switch l {
	case "", "log", "info", "warn", "error", "debug":
		return true
	}
	return false
}

func (inv WorkerInvocation) withDefaults(script, subdomain string, now time.Time) WorkerInvocation {
	if inv.Timestamp.IsZero() {
		inv.Timestamp = LogTime{now}
	}
	req := InvocationRequest{}
	if inv.Request != nil {
		req = *inv.Request
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	req.Method = strings.ToUpper(req.Method)
	if req.URL == "" {
		req.URL = "https://" + script + "." + subdomain + ".workers.dev/"
	}
	if req.Status == 0 {
		req.Status = http.StatusOK
		if inv.Exception != nil {
			req.Status = http.StatusInternalServerError // 0054: the /throw request reports response.status 500
		}
	}
	inv.Request = &req
	if inv.Outcome == "" {
		inv.Outcome = "ok"
		if inv.Exception != nil {
			inv.Outcome = "exception" // 0054: outcome "exception" for the throwing request
		}
	}
	if inv.Message != nil {
		inv.Logs = append(append([]InvocationLog(nil), inv.Logs...), InvocationLog{Level: inv.Level, Message: inv.Message})
	}
	for i := range inv.Logs {
		if inv.Logs[i].Level == "" {
			inv.Logs[i].Level = "log"
		}
		if inv.Logs[i].Timestamp.IsZero() {
			inv.Logs[i].Timestamp = inv.Timestamp
		}
	}
	if inv.Exception != nil && inv.Exception.Name == "" {
		e := *inv.Exception
		e.Name = "Error"
		inv.Exception = &e
	}
	return inv
}

type builtEvent struct {
	id   string
	ts   int64
	body map[string]any
}

// telemetryEventID is the event ID: the ULID time prefix of the timestamp (10 Crockford base32
// characters) and a 16-digit decimal sequence within the request. 0054: "01M3NZARJ0" encodes
// 1790665122368 and the request's events are …0001, …0002 (console.log, console.error) and
// …0004 (the request event); with an uncaught exception the exception is …0003 and the
// request event …0005. The skipped number is not visible in the events view.
func telemetryEventID(ms int64, seq int) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [10]byte
	v := uint64(ms)
	for i := 9; i >= 0; i-- {
		b[i] = crockford[v%32]
		v /= 32
	}
	return fmt.Sprintf("%s%016d", b[:], seq)
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n / 2)) }

// buildInvocationEvents turns an invocation into the telemetry events of 0054: one "cf-worker"
// event per console call and per uncaught exception, then the "cf-worker-event" request event.
// Callers hold s.mu.
func buildInvocationEvents(a *account, script, version string, inv WorkerInvocation) ([]builtEvent, string) {
	ts := inv.Timestamp.UnixMilli()
	req := inv.Request
	u, _ := url.Parse(req.URL)
	path, search := "/", map[string]any(nil)
	if u != nil {
		if u.Path != "" {
			path = u.Path
		}
		// 0054: ?phase=1 is reported as search {"phase": 1}; 0092: ?phase=lag as {"phase": "lag"}.
		// Repeated parameters: UNVERIFIED (the first value is kept).
		for k, vs := range u.Query() {
			if search == nil {
				search = map[string]any{}
			}
			if f, err := strconv.ParseFloat(vs[0], 64); err == nil {
				search[k] = f
			} else {
				search[k] = vs[0]
			}
		}
	}
	trigger := req.Method + " " + path // 0054: "GET /e"
	requestID, rayID, traceID, spanID := randHex(32), randHex(16), randHex(32), randHex(16)

	// Sequence numbers are unique per millisecond in the account, so that two invocations in the
	// same millisecond do not share IDs (UNVERIFIED: how the real service numbers them).
	if a.logSeq == nil {
		a.logSeq = map[int64]int{}
	}
	base := a.logSeq[ts]
	n := len(inv.Logs)
	if inv.Exception != nil {
		n++
	}
	a.logSeq[ts] = base + n + 2

	common := func() map[string]any {
		w := map[string]any{"truncated": false, "scriptName": script, "eventType": "fetch", "executionModel": "stateless"}
		if version != "" {
			w["scriptVersion"] = map[string]any{"id": version}
		}
		return w
	}
	logRequest := map[string]any{"method": req.Method, "url": req.URL, "path": path}
	if search != nil {
		logRequest["search"] = search
	}
	var out []builtEvent
	add := func(seq int, source, workers, meta map[string]any) {
		id := telemetryEventID(ts, base+seq)
		meta["id"] = id
		out = append(out, builtEvent{id: id, ts: ts, body: map[string]any{
			"source": source, "dataset": telemetryDataset, "timestamp": float64(ts), "$workers": workers, "$metadata": meta,
		}})
	}
	logMeta := func(level, msg string) map[string]any {
		m := map[string]any{"requestId": requestID, "rayId": rayID, "traceId": traceID, "spanId": spanID, "trigger": trigger,
			"service": script, "message": msg, "account": a.id, "type": "cf-worker", "origin": "fetch"}
		if level != "" {
			m["level"] = level
		}
		if level == "error" {
			m["error"] = msg // 0054: error-level events repeat the message as $metadata.error
		}
		return m
	}
	for i, l := range inv.Logs {
		msg := joinLogArgs(l.Message)
		source := map[string]any{"message": msg}
		level := l.Level
		// 0054: console.log has no level at all (source or $metadata); console.error has
		// "error". info, warn and debug carrying their own name: UNVERIFIED.
		if level == "log" {
			level = ""
		}
		if level != "" {
			source["level"] = level
		}
		w := common()
		w["event"] = map[string]any{"request": logRequest}
		add(i+1, source, w, logMeta(level, msg))
	}
	seq := len(inv.Logs)
	if e := inv.Exception; e != nil {
		seq++
		msg := joinLogArgs(e.Message)
		exc := map[string]any{"name": e.Name, "timestamp": float64(ts)}
		if e.Stack != "" {
			exc["stack"] = e.Stack
		}
		// 0054: {"message": "<error message>", "exception": {name, stack, timestamp}}; level
		// "error" in $metadata only.
		w := common()
		w["event"] = map[string]any{"request": logRequest}
		add(seq, map[string]any{"message": msg, "exception": exc}, w, logMeta("error", msg))
	}

	// The request event (0054 "cf-worker-event"): level info, or error when the invocation did
	// not succeed; message "<METHOD> <url>".
	level := "info"
	if inv.Outcome != "ok" {
		level = "error"
	}
	msg := req.Method + " " + req.URL
	headers := map[string]any{}
	for k, v := range req.Headers {
		headers[strings.ToLower(k)] = v
	}
	if _, ok := headers["host"]; !ok && u != nil && u.Host != "" {
		headers["host"] = u.Host
	}
	evReq := map[string]any{"headers": headers, "method": req.Method, "url": req.URL}
	if req.CF != nil {
		evReq["cf"] = req.CF
	}
	ev := map[string]any{"request": evReq, "path": path, "response": map[string]any{"status": float64(req.Status)}}
	if search != nil {
		ev["search"] = search
	}
	w := common()
	w["event"] = ev
	w["outcome"] = inv.Outcome
	w["wallTimeMs"] = float64(inv.WallTimeMs)
	w["cpuTimeMs"] = float64(inv.CPUTimeMs)
	meta := map[string]any{"requestId": requestID, "rayId": rayID, "traceId": traceID, "trigger": trigger, "service": script,
		"level": level, "message": msg, "account": a.id, "type": "cf-worker-event", "origin": "fetch"}
	if level == "error" {
		meta["error"] = msg
	}
	add(seq+2, map[string]any{"level": level, "message": msg}, w, meta)
	return out, requestID
}

// joinLogArgs renders console arguments as the event's message string: strings as they are,
// other values as JSON, separated by spaces. UNVERIFIED beyond a single string argument (0054).
func joinLogArgs(m any) string {
	switch v := m.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = joinLogArgs(x)
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(v, " ")
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- query ---------------------------------------------------------------------------------------

type telemetryFilter struct {
	Kind              string            `json:"kind"`
	FilterCombination string            `json:"filterCombination"`
	Filters           []telemetryFilter `json:"filters"`
	Key               string            `json:"key"`
	Operation         string            `json:"operation"`
	Type              string            `json:"type"`
	Value             any               `json:"value"`
}

type telemetryNeedle struct {
	Value     any  `json:"value"`
	IsRegex   bool `json:"isRegex"`
	MatchCase bool `json:"matchCase"`
}

type telemetryQueryRequest struct {
	QueryID         *string `json:"queryId"`
	View            string  `json:"view"`
	Limit           *int    `json:"limit"`
	Dry             bool    `json:"dry"`
	Offset          string  `json:"offset"`
	OffsetDirection string  `json:"offsetDirection"`
	Timeframe       *struct {
		From *int64 `json:"from"`
		To   *int64 `json:"to"`
	} `json:"timeframe"`
	Parameters struct {
		FilterCombination string            `json:"filterCombination"`
		Filters           []telemetryFilter `json:"filters"`
		Needle            *telemetryNeedle  `json:"needle"`
	} `json:"parameters"`
}

// zodError is the observability API's validation error: no v4 envelope, the input echoed as
// "_i" and zod's issues as "_c" (0096: limit 5000 → too_big).
func zodError(input any, issues ...map[string]any) response {
	body, _ := json.Marshal(map[string]any{"success": false, "_e": nil, "_i": input,
		"_c": map[string]any{"issues": issues, "name": "ZodError"}})
	return response{status: http.StatusBadRequest, raw: append(body, '\n'), rawType: "application/json"}
}

func telemetryQuery(c *reqCtx) response {
	var input any
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.UseNumber() // echo numbers exactly in _i
	if err := dec.Decode(&input); err != nil {
		// UNVERIFIED: a body that is not JSON (the shape follows 0096).
		return zodError(nil, map[string]any{"code": "invalid_type", "expected": "object", "path": []any{}, "message": "Invalid input"})
	}
	var q telemetryQueryRequest
	if err := json.Unmarshal(c.body, &q); err != nil {
		return zodError(input, map[string]any{"code": "invalid_type", "path": []any{}, "message": "Invalid input"}) // UNVERIFIED
	}
	// Required fields (spec: queryId, timeframe{from,to}); issue shapes UNVERIFIED, after 0096.
	var issues []map[string]any
	if q.QueryID == nil {
		issues = append(issues, map[string]any{"expected": "string", "code": "invalid_type", "path": []any{"queryId"}, "message": "Invalid input"})
	}
	if q.Timeframe == nil || q.Timeframe.From == nil || q.Timeframe.To == nil {
		issues = append(issues, map[string]any{"expected": "object", "code": "invalid_type", "path": []any{"timeframe"}, "message": "Invalid input"})
	}
	if q.Limit != nil && *q.Limit > telemetryMaxLimit { // 0096
		issues = append(issues, map[string]any{"origin": "number", "code": "too_big", "maximum": telemetryMaxLimit,
			"inclusive": true, "path": []any{"limit"}, "message": "Invalid input"})
	}
	if len(issues) > 0 {
		return zodError(input, issues...)
	}
	if q.View == "" {
		q.View = "events" // UNVERIFIED default
	}
	if q.View != "events" {
		return fail(http.StatusBadRequest, 10001, "flarefake: only view \"events\" of the telemetry query is emulated, not "+strconv.Quote(q.View))
	}
	limit := telemetryDefaultLimit
	if q.Limit != nil {
		limit = *q.Limit
	}
	if limit < 0 {
		limit = 0
	}

	from, to := *q.Timeframe.From, *q.Timeframe.To
	if min := c.now.Add(-c.s.opts.LogRetention).UnixMilli(); from < min {
		from = min // 0098
	}
	anyFilter := strings.EqualFold(q.Parameters.FilterCombination, "or")
	needle, nerr := compileNeedle(q.Parameters.Needle)
	if nerr != nil {
		return zodError(input, map[string]any{"code": "custom", "path": []any{"parameters", "needle", "value"}, "message": nerr.Error()}) // UNVERIFIED
	}

	var matched []*logEvent
	var scannedBytes int
	for _, ev := range c.account.logs {
		if ev.visibleAt.After(c.now) {
			continue // ingestion lag (0077…0081)
		}
		scannedBytes += 512
		if ev.ts < from || ev.ts > to {
			continue
		}
		if !filtersMatch(q.Parameters.Filters, anyFilter, ev.body) || needle != nil && !needle(ev.body) {
			continue
		}
		matched = append(matched, ev)
	}
	// Newest first (0054, 0092). IDs start with the timestamp's ULID prefix, so ID order is
	// time order, and within one request the sequence suffix orders the events (0054: …0004,
	// …0002, …0001).
	sort.Slice(matched, func(i, j int) bool { return matched[i].id > matched[j].id })

	page := pageEvents(matched, q.Offset, q.OffsetDirection, limit)
	events := make([]any, len(page))
	for i, ev := range page {
		events[i] = ev.body
	}

	granularity := telemetryGranularity(from, to)
	stats := map[string]any{"elapsed": 0.001, "rows_read": len(c.account.logs), "bytes_read": scannedBytes, "abr_level": 1}
	params := map[string]any{}
	if m, ok := input.(map[string]any); ok {
		if p, ok := m["parameters"].(map[string]any); ok {
			params = p
		}
	}
	queryID := *q.QueryID
	created := tsMilli(c.now)
	run := map[string]any{
		"id": c.s.ids.next(telemetryRunID),
		// 0054: the ad-hoc query is echoed with these fields only; the spec's adhoc, createdBy and
		// updatedBy are absent and name is "" (allowlisted: telemetry-query-run-query).
		"query": map[string]any{"id": queryID, "description": "", "name": "", "generated": false, "parameters": params,
			"workspaceId": c.account.id, "environmentId": "cloudflare", "userId": workerAuthorID, "created": created, "updated": created},
		"accountId":   c.account.id,
		"timeframe":   map[string]any{"from": from, "to": to},
		"userId":      workerAuthorID,
		"status":      "COMPLETED",
		"granularity": granularity,
		"dry":         q.Dry,
		"statistics":  stats,
	}
	resp := ok(map[string]any{
		"run":        run,
		"statistics": stats,
		"events": map[string]any{
			"events": events,
			"fields": telemetryFields(page),
			"count":  len(events),
			"series": telemetrySeries(matched, from, to, granularity),
		},
	})
	resp.messages = []any{map[string]any{"message": "Successful request"}} // 0054
	return resp
}

func telemetryRunID() string {
	b := randBytes(24)
	for i := range b {
		b[i] = telemetryRunIDChars[int(b[i])%len(telemetryRunIDChars)]
	}
	return string(b)
}

// pageEvents applies offset paging to newest-first events. offsetDirection "next" continues
// after the offset event (older ones; 0093 returns the four events after 0092's last);
// "prev" goes back to the events before it (newer ones; 0094 from 0093's first returns 0092's
// page). With more than limit newer events, "prev" returns the limit events closest to the
// offset: UNVERIFIED (0094 had exactly limit newer events).
func pageEvents(evs []*logEvent, offset, dir string, limit int) []*logEvent {
	if offset == "" {
		if len(evs) > limit {
			return evs[:limit]
		}
		return evs
	}
	// The first index past the offset ID in newest-first order (works whether or not the
	// offset event still matches).
	i := sort.Search(len(evs), func(i int) bool { return evs[i].id <= offset })
	if strings.EqualFold(dir, "prev") {
		newer := evs[:i]
		if len(newer) > limit {
			newer = newer[len(newer)-limit:]
		}
		return newer
	}
	if i < len(evs) && evs[i].id == offset {
		i++
	}
	older := evs[i:]
	if len(older) > limit {
		older = older[:limit]
	}
	return older
}

// telemetryGranularity is the series bucket width: the timeframe split into about 60 buckets,
// rounded up to whole seconds. Recorded: 15 min → 15000 (0054), 428 s → 8000 (0092), 1 h →
// 60000 (0122), 63 s → 2000 (0077), 7 d → 10080000 (0098). The 1 s floor is UNVERIFIED.
func telemetryGranularity(from, to int64) int64 {
	g := int64(math.Ceil(float64(to-from)/60/1000)) * 1000
	if g < 1000 {
		g = 1000
	}
	return g
}

// telemetrySeries counts the matched events (not just the returned page) per bucket. Buckets
// are aligned to multiples of the granularity in epoch time and run from the second boundary
// after from to the bucket holding to: 0054 (from 06:44:28.007, 15 s) starts at 06:44:45, 0092
// (from 06:58:20, 8 s) at 06:58:32, and every recording ends with the bucket of to. Bucket
// times are "YYYY-MM-DD hh:mm:ss" UTC; an empty bucket has data []. errors counts events at
// level error (0054: 19 events, 8 errors). A from exactly on a boundary: UNVERIFIED.
func telemetrySeries(evs []*logEvent, from, to, g int64) []any {
	first := (from+g-1)/g*g + g
	last := to / g * g
	type agg struct{ n, errs int }
	counts := map[int64]*agg{}
	for _, ev := range evs {
		b := ev.ts / g * g
		if b < first || b > last {
			continue
		}
		a := counts[b]
		if a == nil {
			a = &agg{}
			counts[b] = a
		}
		a.n++
		if md, _ := ev.body["$metadata"].(map[string]any); md["level"] == "error" {
			a.errs++
		}
	}
	out := []any{}
	for b := first; b <= last; b += g {
		data := []any{}
		if a := counts[b]; a != nil {
			data = append(data, map[string]any{
				"aggregates": map[string]any{"_count": a.n, "_countErrors": a.errs, "_interval": 1},
				"count":      a.n, "errors": a.errs, "interval": 1, "sampleInterval": 1,
			})
		}
		out = append(out, map[string]any{"time": time.UnixMilli(b).UTC().Format("2006-01-02 15:04:05"), "data": data})
	}
	return out
}

// telemetryFields lists the keys of the returned events with their types: source fields
// without a prefix ("level", "message", "exception.name"), $workers fields with theirs, not
// $metadata, timestamp or dataset; strings first, then booleans, then numbers (0054, 0092,
// 0123). Within a type the real order is the backend's own and not reproduced (UNVERIFIED);
// here it is first appearance.
func telemetryFields(evs []*logEvent) []any {
	type field struct{ key, typ string }
	var order []field
	seen := map[field]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, x[k])
			}
		case string, float64, json.Number, bool, int:
			typ := "string"
			switch x.(type) {
			case bool:
				typ = "boolean"
			case float64, json.Number, int:
				typ = "number"
			}
			key := prefix
			// Header names are listed in lower case even when the event spells them otherwise
			// (0054: the event's header "CF-Ray", the field "$workers.event.request.headers.cf-ray").
			if h := "$workers.event.request.headers."; strings.HasPrefix(key, h) {
				key = h + strings.ToLower(strings.TrimPrefix(key, h))
			}
			f := field{key, typ}
			if !seen[f] {
				seen[f] = true
				order = append(order, f)
			}
		}
	}
	for _, ev := range evs {
		walk("", ev.body["source"])
		walk("$workers", ev.body["$workers"])
	}
	rank := map[string]int{"string": 0, "boolean": 1, "number": 2}
	sort.SliceStable(order, func(i, j int) bool { return rank[order[i].typ] < rank[order[j].typ] })
	out := make([]any, len(order))
	for i, f := range order {
		out[i] = map[string]any{"type": f.typ, "key": f.key}
	}
	return out
}

// lookupField resolves a query key in an event: "$metadata.service", "$workers.scriptVersion.id"
// address the event itself; a key without "$" addresses the event's source ("level",
// "message"), as the fields list names them (0054).
func lookupField(ev map[string]any, key string) (any, bool) {
	var cur any = ev
	if !strings.HasPrefix(key, "$") {
		cur = ev["source"]
	}
	for _, seg := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// filtersMatch evaluates the filter tree. Recorded: "eq" on $metadata.service (0054),
// $metadata.type (0092) and $workers.scriptVersion.id (0122/0123) combined with "and". The
// other operations follow the spec's enum; their semantics (case sensitivity, type coercion)
// are UNVERIFIED.
func filtersMatch(fs []telemetryFilter, or bool, ev map[string]any) bool {
	if len(fs) == 0 {
		return true
	}
	for _, f := range fs {
		var m bool
		if f.Kind == "group" {
			m = filtersMatch(f.Filters, strings.EqualFold(f.FilterCombination, "or"), ev)
		} else {
			m = f.match(ev)
		}
		if or && m {
			return true
		}
		if !or && !m {
			return false
		}
	}
	return !or
}

func (f telemetryFilter) match(ev map[string]any) bool {
	v, found := lookupField(ev, f.Key)
	op := strings.ToLower(f.Operation)
	switch op {
	case "exists":
		return found && v != nil
	case "is_null":
		return !found || v == nil
	}
	if !found {
		return op == "neq" || op == "not_includes" || op == "not_in"
	}
	sv := fmt.Sprint(v)
	if f, ok := v.(float64); ok {
		sv = strconv.FormatFloat(f, 'f', -1, 64)
	}
	target := fmt.Sprint(f.Value)
	if fv, ok := f.Value.(float64); ok {
		target = strconv.FormatFloat(fv, 'f', -1, 64)
	}
	num := func() (float64, float64, bool) {
		a, err1 := strconv.ParseFloat(sv, 64)
		b, err2 := strconv.ParseFloat(target, 64)
		return a, b, err1 == nil && err2 == nil
	}
	list := func() []string {
		switch x := f.Value.(type) {
		case []any:
			out := make([]string, len(x))
			for i, e := range x {
				out[i] = fmt.Sprint(e)
				if fv, ok := e.(float64); ok {
					out[i] = strconv.FormatFloat(fv, 'f', -1, 64)
				}
			}
			return out
		case string:
			return strings.Split(x, ",")
		}
		return []string{target}
	}
	switch op {
	case "eq", "=", "==":
		return sv == target
	case "neq", "!=":
		return sv != target
	case "includes":
		return strings.Contains(sv, target)
	case "not_includes":
		return !strings.Contains(sv, target)
	case "starts_with":
		return strings.HasPrefix(sv, target)
	case "ends_with":
		return strings.HasSuffix(sv, target)
	case "regex":
		re, err := regexp.Compile(target)
		return err == nil && re.MatchString(sv)
	case "in", "not_in":
		hit := false
		for _, x := range list() {
			hit = hit || strings.TrimSpace(x) == sv
		}
		return hit == (op == "in")
	case "gt", "gte", "lt", "lte":
		a, b, ok := num()
		if !ok {
			return false
		}
		switch op {
		case "gt":
			return a > b
		case "gte":
			return a >= b
		case "lt":
			return a < b
		}
		return a <= b
	}
	return false
}

// compileNeedle is the free-text search (0077: {"value": "laga6e44d", "isRegex": false,
// "matchCase": false} finds the request's three events). Which fields it searches is
// UNVERIFIED: here the message, error and exception name of $metadata and source.
func compileNeedle(n *telemetryNeedle) (func(map[string]any) bool, error) {
	if n == nil {
		return nil, nil
	}
	val := fmt.Sprint(n.Value)
	if n.Value == nil || val == "" {
		return nil, nil
	}
	var match func(string) bool
	switch {
	case n.IsRegex:
		expr := val
		if !n.MatchCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		match = re.MatchString
	case n.MatchCase:
		match = func(s string) bool { return strings.Contains(s, val) }
	default:
		low := strings.ToLower(val)
		match = func(s string) bool { return strings.Contains(strings.ToLower(s), low) }
	}
	return func(ev map[string]any) bool {
		for _, k := range []string{"$metadata.message", "$metadata.error", "message", "exception.name"} {
			if v, ok := lookupField(ev, k); ok {
				if s, ok := v.(string); ok && match(s) {
					return true
				}
			}
		}
		return false
	}, nil
}
