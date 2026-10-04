package fake

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const logsAcct = "0123456789abcdef0123456789abcdef"

// logsFixture is a server with an uploaded script "w" and a frozen clock.
func logsFixture(t *testing.T, opts Options) (*Server, *client, time.Time) {
	t.Helper()
	s := New(opts)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	s.Clock.Set(now)
	c := newClient(t, s)
	if st, env, _ := c.upload("w", map[string]any{"main_module": "index.js"}); st != 200 {
		t.Fatalf("upload: %d %v", st, env.Errors)
	}
	return s, c, now
}

func inject(t *testing.T, s *Server, script string, inv WorkerInvocation) InjectedLogs {
	t.Helper()
	res, err := s.InjectWorkerLogs(logsAcct, script, inv)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func query(t *testing.T, c *client, body map[string]any) (int, map[string]any) {
	t.Helper()
	st, env, _ := c.do("POST", acct+"/workers/observability/telemetry/query", body)
	m, _ := env.Result.(map[string]any)
	return st, m
}

func serviceQuery(script string, from, to time.Time, limit int, extra ...map[string]any) map[string]any {
	filters := []any{map[string]any{"key": "$metadata.service", "operation": "eq", "type": "string", "value": script}}
	for _, f := range extra {
		filters = append(filters, f)
	}
	return map[string]any{
		"queryId": "q", "view": "events", "limit": limit, "dry": true,
		"timeframe": map[string]any{"from": from.UnixMilli(), "to": to.UnixMilli()},
		"parameters": map[string]any{"datasets": []any{}, "filterCombination": "and", "filters": filters,
			"calculations": []any{}, "groupBys": []any{}, "havings": []any{}},
	}
}

func events(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	evs := res["events"].(map[string]any)["events"].([]any)
	out := make([]map[string]any, len(evs))
	for i, e := range evs {
		out[i] = e.(map[string]any)
	}
	return out
}

func meta(ev map[string]any) map[string]any { return ev["$metadata"].(map[string]any) }

var typeLog = map[string]any{"key": "$metadata.type", "operation": "eq", "type": "string", "value": "cf-worker"}

func TestTelemetryQueryEventShapes(t *testing.T) {
	s, c, now := logsFixture(t, Options{})
	res := inject(t, s, "w", WorkerInvocation{
		Timestamp: LogTime{now.Add(-time.Minute)},
		Request:   &InvocationRequest{Method: "get", URL: "https://w.example-subdomain.workers.dev/throw?phase=1"},
		Logs: []InvocationLog{
			{Message: []any{"hello", map[string]any{"a": 1.0}}},
			{Level: "error", Message: "bad"},
		},
		Exception: &InvocationException{Message: "boom", Stack: "    at fetch (index.js:1:1)"},
	})
	if len(res.EventIDs) != 4 || !strings.HasSuffix(res.EventIDs[3], "0000000000000005") {
		t.Fatalf("event IDs (want …1, …2, …3 exception, …5 request event): %v", res.EventIDs)
	}
	st, out := query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 10))
	if st != 200 {
		t.Fatalf("query: %d", st)
	}
	evs := events(t, out)
	if len(evs) != 4 {
		t.Fatalf("want 4 events, got %d", len(evs))
	}
	// Newest first: by ID descending within the request.
	wantTypes := []string{"cf-worker-event", "cf-worker", "cf-worker", "cf-worker"}
	for i, ev := range evs {
		if meta(ev)["type"] != wantTypes[i] {
			t.Errorf("event %d type %v", i, meta(ev)["type"])
		}
		if ev["dataset"] != "cloudflare-workers" || ev["timestamp"] != float64(now.Add(-time.Minute).UnixMilli()) {
			t.Errorf("event %d dataset/timestamp: %v %v", i, ev["dataset"], ev["timestamp"])
		}
	}
	reqEv, exc, errLog, plain := evs[0], evs[1], evs[2], evs[3]
	w := reqEv["$workers"].(map[string]any)
	if w["outcome"] != "exception" || meta(reqEv)["level"] != "error" || meta(reqEv)["trigger"] != "GET /throw" ||
		w["event"].(map[string]any)["response"].(map[string]any)["status"] != 500.0 ||
		w["scriptVersion"].(map[string]any)["id"] == "" {
		t.Errorf("request event: %v", reqEv)
	}
	if src := exc["source"].(map[string]any); src["message"] != "boom" || src["exception"].(map[string]any)["name"] != "Error" || meta(exc)["level"] != "error" {
		t.Errorf("exception event: %v", exc)
	}
	if src := errLog["source"].(map[string]any); src["level"] != "error" || meta(errLog)["error"] != "bad" {
		t.Errorf("console.error event: %v", errLog)
	}
	src := plain["source"].(map[string]any)
	if _, has := src["level"]; has || src["message"] != `hello {"a":1}` {
		t.Errorf("console.log event (no level, joined args): %v", plain)
	}
	if _, has := meta(plain)["level"]; has {
		t.Errorf("console.log has no $metadata.level: %v", meta(plain))
	}
	search := plain["$workers"].(map[string]any)["event"].(map[string]any)["request"].(map[string]any)["search"].(map[string]any)
	if search["phase"] != 1.0 {
		t.Errorf("search: %v", search)
	}
	ev := out["events"].(map[string]any)
	if ev["count"] != 4.0 {
		t.Errorf("count %v", ev["count"])
	}
	fields := ev["fields"].([]any)
	seenLevel := false
	lastRank := 0
	for _, f := range fields {
		m := f.(map[string]any)
		seenLevel = seenLevel || m["key"] == "level"
		r := map[string]int{"string": 0, "boolean": 1, "number": 2}[m["type"].(string)]
		if r < lastRank {
			t.Errorf("fields not grouped string, boolean, number: %v", fields)
			break
		}
		lastRank = r
	}
	if !seenLevel {
		t.Errorf("fields lack level: %v", fields)
	}
}

func TestTelemetryQueryFiltersLimitPaging(t *testing.T) {
	s, c, now := logsFixture(t, Options{})
	v1 := s.accounts[logsAcct].scripts["w"].deployedVersion().ID
	base := now.Add(-10 * time.Minute)
	for i := 0; i < 5; i++ {
		inject(t, s, "w", WorkerInvocation{Timestamp: LogTime{base.Add(time.Duration(i) * time.Second)}, Message: "line"})
	}
	inject(t, s, "other", WorkerInvocation{Timestamp: LogTime{base}, Message: "elsewhere"})
	inject(t, s, "w", WorkerInvocation{Timestamp: LogTime{base.Add(time.Minute)}, ScriptVersion: "v2", Message: "new"})

	_, out := query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 100, typeLog))
	all := events(t, out)
	if len(all) != 6 {
		t.Fatalf("want 6 log lines of w, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if meta(all[i-1])["id"].(string) <= meta(all[i])["id"].(string) || all[i-1]["timestamp"].(float64) < all[i]["timestamp"].(float64) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	// Script-version filter (kubectl logs --previous).
	_, out = query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 100, typeLog,
		map[string]any{"key": "$workers.scriptVersion.id", "operation": "eq", "type": "string", "value": v1}))
	if n := len(events(t, out)); n != 5 {
		t.Errorf("version filter: %d events", n)
	}
	// Timeframe.
	_, out = query(t, c, serviceQuery("w", base.Add(1500*time.Millisecond), base.Add(3*time.Second), 100, typeLog))
	if n := len(events(t, out)); n != 2 {
		t.Errorf("timeframe: %d events", n)
	}
	// limit + offset paging, both directions.
	_, out = query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 2, typeLog))
	p1 := events(t, out)
	q := serviceQuery("w", now.Add(-time.Hour), now, 2, typeLog)
	q["offset"], q["offsetDirection"] = meta(p1[1])["id"], "next"
	_, out = query(t, c, q)
	p2 := events(t, out)
	if len(p2) != 2 || meta(p2[0])["id"] != meta(all[2])["id"] || meta(p2[1])["id"] != meta(all[3])["id"] {
		t.Fatalf("next page: %v", p2)
	}
	q["offset"], q["offsetDirection"] = meta(p2[0])["id"], "prev"
	_, out = query(t, c, q)
	back := events(t, out)
	if len(back) != 2 || meta(back[0])["id"] != meta(p1[0])["id"] || meta(back[1])["id"] != meta(p1[1])["id"] {
		t.Fatalf("prev page: %v", back)
	}
	// limit above 2000: zod's too_big (0096), no v4 envelope.
	req, _ := http.NewRequest("POST", c.base+acct+"/workers/observability/telemetry/query",
		strings.NewReader(mustJSON(t, serviceQuery("w", now.Add(-time.Hour), now, 2001))))
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var zod map[string]any
	_ = json.Unmarshal(raw, &zod)
	issue := zod["_c"].(map[string]any)["issues"].([]any)[0].(map[string]any)
	if resp.StatusCode != 400 || zod["success"] != false || issue["code"] != "too_big" || issue["maximum"] != 2000.0 {
		t.Fatalf("limit 2001: %d %s", resp.StatusCode, raw)
	}
	if _, has := zod["errors"]; has {
		t.Errorf("zod error has an errors array: %s", raw)
	}
	// 2000 itself is fine.
	if st, _ := query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 2000)); st != 200 {
		t.Errorf("limit 2000: %d", st)
	}
	// Needle: case-insensitive text search.
	nq := serviceQuery("w", now.Add(-time.Hour), now, 100)
	nq["parameters"].(map[string]any)["needle"] = map[string]any{"value": "NEW", "isRegex": false, "matchCase": false}
	_, out = query(t, c, nq)
	if n := len(events(t, out)); n != 1 {
		t.Errorf("needle: %d events", n)
	}
}

func TestTelemetryQueryIngestionLagAndRetention(t *testing.T) {
	s, c, now := logsFixture(t, Options{LogIngestionLag: 20 * time.Second})
	inject(t, s, "w", WorkerInvocation{Message: "late"})
	q := serviceQuery("w", now.Add(-time.Minute), now.Add(time.Minute), 10, typeLog)
	if _, out := query(t, c, q); len(events(t, out)) != 0 {
		t.Fatal("event visible before the ingestion lag")
	}
	s.Clock.Advance(19 * time.Second)
	if _, out := query(t, c, q); len(events(t, out)) != 0 {
		t.Fatal("event visible at 19 s")
	}
	s.Clock.Advance(time.Second)
	if _, out := query(t, c, q); len(events(t, out)) != 1 {
		t.Fatal("event not visible at 20 s")
	}
	// Per-invocation override, and the control API's lag setter.
	inject(t, s, "w", WorkerInvocation{Message: "now", IngestDelay: "0s"})
	if _, out := query(t, c, q); len(events(t, out)) != 2 {
		t.Fatal("ingestDelay 0s not visible at once")
	}

	// A 30-day window is clamped to 7 days back from now (0098).
	now = s.Clock.Now()
	_, out := query(t, c, serviceQuery("w", now.Add(-30*24*time.Hour), now, 5))
	run := out["run"].(map[string]any)
	if from := run["timeframe"].(map[string]any)["from"].(float64); int64(from) != now.Add(-7*24*time.Hour).UnixMilli() {
		t.Errorf("from %v, want now-7d", from)
	}
	if run["granularity"] != 10080000.0 || run["status"] != "COMPLETED" || run["dry"] != true {
		t.Errorf("run: %v", run)
	}
	if len(out["events"].(map[string]any)["series"].([]any)) == 0 {
		t.Error("no series")
	}
}

func TestLogsControlAPI(t *testing.T) {
	_, c, now := logsFixture(t, Options{})
	post := func(path string, body string) (int, map[string]any) {
		resp, err := http.Post(c.base+"/_fake/"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if st, m := post("log_ingestion_lag", `{"lag":"30s"}`); st != 200 || m["lag"] != "30s" {
		t.Fatalf("lag: %d %v", st, m)
	}
	st, m := post("accounts/"+logsAcct+"/workers/w/logs",
		`{"timestamp":"2026-10-04T11:59:00Z","level":"warn","message":["a",1],"request":{"method":"POST","url":"https://w.example.com/x","status":201},"ingestDelay":"0s"}`)
	if st != 200 || len(m["eventIds"].([]any)) != 2 {
		t.Fatalf("inject: %d %v", st, m)
	}
	_, out := query(t, c, serviceQuery("w", now.Add(-time.Hour), now, 10))
	evs := events(t, out)
	if len(evs) != 2 || evs[1]["source"].(map[string]any)["level"] != "warn" || evs[1]["source"].(map[string]any)["message"] != "a 1" {
		t.Fatalf("injected events: %v", evs)
	}
	if st, _ := post("accounts/"+logsAcct+"/workers/w/logs", `{"level":"fatal","message":"x"}`); st != 400 {
		t.Errorf("bad level: %d", st)
	}
	if st, _ := post("accounts/"+logsAcct+"/workers/w/logs", `{"message":"x","ingestDelay":"soon"}`); st != 400 {
		t.Errorf("bad ingestDelay: %d", st)
	}
}

// ---- tails -----------------------------------------------------------------------------------

func createTail(t *testing.T, c *client, script string) map[string]any {
	t.Helper()
	st, env, _ := c.do("POST", acct+"/workers/scripts/"+script+"/tails", map[string]any{})
	if st != 200 {
		t.Fatalf("tail create: %d %v", st, env.Errors)
	}
	return resultMap(t, env)
}

func dialTail(t *testing.T, u string) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{"trace-v1"}, HandshakeTimeout: 5 * time.Second}
	ws, resp, err := d.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (%v)", u, err, resp)
	}
	if ws.Subprotocol() != "trace-v1" {
		t.Fatalf("subprotocol %q", ws.Subprotocol())
	}
	t.Cleanup(func() { ws.Close() })
	// wrangler's first message.
	_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"debug":false}`))
	return ws
}

func readFrame(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, b, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if typ != websocket.BinaryMessage {
		t.Errorf("frame type %d, want binary", typ)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("frame %q: %v", b, err)
	}
	return m
}

// waitConns waits until script's tails hold n connections (the server registers them after
// the handshake).
func waitConns(t *testing.T, s *Server, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		total := 0
		for _, ts := range s.TailSessions(logsAcct, "w") {
			total += ts.Connections
		}
		if total == n {
			return
		}
	}
	t.Fatalf("tail connections did not reach %d: %+v", n, s.TailSessions(logsAcct, "w"))
}

func TestTailLifecycle(t *testing.T) {
	_, c, now := logsFixture(t, Options{})
	tail := createTail(t, c, "w")
	id, u := tail["id"].(string), tail["url"].(string)
	if len(id) != 32 || !tailURLRe.MatchString(u) || !strings.HasPrefix(u, "ws://127.0.0.1:") {
		t.Fatalf("tail: %v", tail)
	}
	if tail["expires_at"] != now.Add(6*time.Hour).Format("2006-01-02T15:04:05Z") {
		t.Errorf("expires_at %v", tail["expires_at"])
	}
	_, env, _ := c.do("GET", acct+"/workers/scripts/w/tails", nil)
	if l := env.Result.([]any); len(l) != 1 || l[0].(map[string]any)["id"] != id {
		t.Fatalf("list: %v", env.Result)
	}
	if st, env, _ := c.do("DELETE", acct+"/workers/scripts/w/tails/"+id, nil); st != 200 || env.Result != nil {
		t.Fatalf("delete: %d %v", st, env.Result)
	}
	_, env, _ = c.do("GET", acct+"/workers/scripts/w/tails", nil)
	if l := env.Result.([]any); len(l) != 0 {
		t.Fatalf("list after delete: %v", l)
	}
	if st, _, _ := c.do("DELETE", acct+"/workers/scripts/w/tails/"+id, nil); st != 404 {
		t.Errorf("second delete: %d", st)
	}
	if st, env, _ := c.do("POST", acct+"/workers/scripts/nope/tails", map[string]any{}); st != 404 || env.Errors[0].Code != 10007 {
		t.Errorf("tail of a missing script: %d %v", st, env.Errors)
	}
}

func TestTailWebSocketFrames(t *testing.T) {
	s, c, now := logsFixture(t, Options{})
	u := createTail(t, c, "w")["url"].(string)

	// Without the trace-v1 subprotocol the upgrade is refused.
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil || resp == nil || resp.StatusCode != 400 {
		t.Fatalf("dial without subprotocol: %v %v", err, resp)
	}
	// An unknown token is 404.
	if _, resp, err := (&websocket.Dialer{Subprotocols: []string{"trace-v1"}}).Dial(u[:len(u)-32]+strings.Repeat("0", 32), nil); err == nil || resp.StatusCode != 404 {
		t.Fatalf("dial unknown tail: %v %v", err, resp)
	}

	ws := dialTail(t, u)
	waitConns(t, s, 1)
	// Pings are answered (wrangler's keep-alive).
	pong := make(chan string, 1)
	ws.SetPongHandler(func(d string) error { pong <- d; return nil })
	if err := ws.WriteControl(websocket.PingMessage, []byte("wrangler tail ping"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	res := inject(t, s, "w", WorkerInvocation{
		Timestamp: LogTime{now},
		Request:   &InvocationRequest{URL: "https://w.example-subdomain.workers.dev/p", Headers: map[string]string{"User-Agent": "curl"}},
		Logs:      []InvocationLog{{Message: []any{"x", 2.0}}, {Level: "error", Message: "y"}},
		Exception: &InvocationException{Message: "boom"},
	})
	if res.TailFrames != 1 {
		t.Fatalf("frame queued for %d connections", res.TailFrames)
	}
	f := readFrame(t, ws)
	// The pong can arrive after the frame (the server's reader answers the ping while its writer
	// sends the frame), and gorilla only runs the pong handler inside a read: keep reading. The
	// test doesn't read ws after this point.
	go func() {
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case d := <-pong:
		if d != "wrangler tail ping" {
			t.Errorf("pong %q", d)
		}
	case <-time.After(5 * time.Second):
		t.Error("no pong")
	}
	if f["outcome"] != "exception" || f["scriptName"] != "w" || f["eventTimestamp"] != float64(now.UnixMilli()) {
		t.Errorf("frame: %v", f)
	}
	logs := f["logs"].([]any)
	l0 := logs[0].(map[string]any)
	if len(logs) != 2 || l0["level"] != "log" || l0["message"].([]any)[1] != 2.0 || logs[1].(map[string]any)["message"].([]any)[0] != "y" {
		t.Errorf("logs: %v", logs)
	}
	if ex := f["exceptions"].([]any); len(ex) != 1 || ex[0].(map[string]any)["name"] != "Error" || ex[0].(map[string]any)["message"] != "boom" {
		t.Errorf("exceptions: %v", ex)
	}
	req := f["event"].(map[string]any)["request"].(map[string]any)
	if req["method"] != "GET" || req["url"] != "https://w.example-subdomain.workers.dev/p" || req["headers"].(map[string]any)["user-agent"] != "curl" {
		t.Errorf("event.request: %v", req)
	}
	// Another script's logs do not reach this tail.
	if res := inject(t, s, "other", WorkerInvocation{Message: "no"}); res.TailFrames != 0 {
		t.Errorf("other script reached %d tails", res.TailFrames)
	}
}

func TestTailDeleteKeepsConnections(t *testing.T) {
	s, c, _ := logsFixture(t, Options{})
	tail := createTail(t, c, "w")
	ws := dialTail(t, tail["url"].(string))
	waitConns(t, s, 1)
	if st, _, _ := c.do("DELETE", acct+"/workers/scripts/w/tails/"+tail["id"].(string), nil); st != 200 {
		t.Fatal(st)
	}
	// The open connection is not closed and keeps receiving; the URL still upgrades (spike §1).
	inject(t, s, "w", WorkerInvocation{Message: "after delete"})
	if f := readFrame(t, ws); f["logs"].([]any)[0].(map[string]any)["message"].([]any)[0] != "after delete" {
		t.Errorf("frame after delete: %v", f)
	}
	s.Clock.Advance(7 * time.Minute)
	ws2 := dialTail(t, tail["url"].(string))
	waitConns(t, s, 2)
	sess := s.TailSessions(logsAcct, "w")
	if len(sess) != 1 || !sess[0].Deleted || sess[0].Connections != 2 {
		t.Fatalf("sessions: %+v", sess)
	}
	// Closing on the client side is what releases them.
	ws2.Close()
	waitConns(t, s, 1)
	// DisconnectTails drops the rest (reconnect tests).
	if n := s.DisconnectTails(logsAcct, "w"); n != 1 {
		t.Errorf("disconnected %d", n)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Error("connection still open after DisconnectTails")
	}
	waitConns(t, s, 0)
	// After expiry, delivery stops and the URL no longer upgrades.
	s.Clock.Advance(6 * time.Hour)
	d := websocket.Dialer{Subprotocols: []string{"trace-v1"}}
	if _, resp, err := d.Dial(tail["url"].(string), nil); err == nil || resp.StatusCode != 404 {
		t.Errorf("dial after expiry: %v", err)
	}
}

func TestTailURLBaseOption(t *testing.T) {
	_, c, _ := logsFixture(t, Options{TailURLBase: "wss://tail.example.test/"})
	u := createTail(t, c, "w")["url"].(string)
	if !regexp.MustCompile(`^wss://tail\.example\.test/[0-9a-f]{32}$`).MatchString(u) {
		t.Errorf("url %s", u)
	}
}
