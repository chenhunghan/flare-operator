package fake

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Legacy Workers tail (`wrangler tail`): POST/GET/DELETE
// /accounts/{account_id}/workers/scripts/{script_name}/tails[/{id}] and the WebSocket the create
// response points at. Source recordings: test/recordings/2026-09-29/0064, 0073…0075,
// 0139…0141; docs/spike-results-2026-09-29.md §1. The frame format is wrangler's:
// SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/tail/createTail.ts#L216-L282
// (TailEventMessage) and #L287-L380 (RequestEvent).
//
// The WebSocket URL is a capability URL (no other auth, spike §1): ws[s]://<flarefake
// host>/<32 hex>, the shape of the real wss://tail.developers.workers.dev/<32 hex> (the
// sanitizer's pattern, hack/sanitize_recordings.py) on flarefake's own host, or
// Options.TailURLBase.

const (
	// tailLifetime: expires_at is the create time plus 6 h, in whole seconds (0139: created
	// 07:19:56.708, expires_at 13:19:56Z).
	tailLifetime = 6 * time.Hour
	// tailSubprotocol: wrangler connects with Sec-WebSocket-Protocol trace-v1, which "needs to be
	// `trace-v1` to be accepted": SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/tail/createTail.ts#L15,L176-L181.
	tailSubprotocol = "trace-v1"
	tailSendBuffer  = 1024
)

var tailTokenPath = regexp.MustCompile(`^/[0-9a-f]{32}$`)

type workerTail struct {
	ID, Token, Account, Script string
	Created, Expires           time.Time
	Seq                        int64
	URL                        string
	Filters                    []map[string]any
	Deleted                    bool
	conns                      map[*tailConn]struct{}
}

type tailConn struct {
	ws   *websocket.Conn
	send chan []byte
	once sync.Once
	done chan struct{}
}

func (tc *tailConn) close() {
	tc.once.Do(func() {
		close(tc.done)
		_ = tc.ws.Close()
	})
}

// push queues a frame; a connection that cannot keep up loses frames (UNVERIFIED: the real
// tail switches to "sampling mode", SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/__tests__/tail.test.ts#L1812-L1823).
func (tc *tailConn) push(frame []byte) {
	select {
	case tc.send <- frame:
	case <-tc.done:
	default:
	}
}

func (t *workerTail) json() map[string]any {
	return map[string]any{"id": t.ID, "url": t.URL, "expires_at": tsSecond(t.Expires)}
}

func (s *Server) registerTails() {
	base := "/accounts/{account_id}/workers/scripts/{script_name}/tails"
	s.handle(http.MethodPost, base, tailCreate)
	s.handle(http.MethodGet, base, tailList)
	s.handle(http.MethodDelete, base+"/{tail_id}", tailDelete)
	s.handle(http.MethodPost, "/accounts/{account_id}/workers/observability/telemetry/query", telemetryQuery)
}

// tailBaseURL is where the tail WebSocket is served: Options.TailURLBase, else the host the
// client reached flarefake on (wss behind TLS or X-Forwarded-Proto https).
func (c *reqCtx) tailBaseURL() string {
	if b := c.s.opts.TailURLBase; b != "" {
		return strings.TrimRight(b, "/")
	}
	scheme := "ws"
	if c.r.TLS != nil || strings.EqualFold(c.r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	return scheme + "://" + c.r.Host
}

// tailCreate: POST …/tails answers {id, url, expires_at} with messages [] (0064, 0139). The body
// is wrangler's filter message {"filters": [...]} (SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/tail/createTail.ts#L158-L165,
// filters.ts#L135-L137); the spike sent {} (0064). Missing script: 404/10007 (workerNotFound,
// the API's error for a missing Worker on any route).
func tailCreate(c *reqCtx) response {
	name := c.params["script_name"]
	if _, ok := c.account.scripts[name]; !ok {
		return workerNotFound()
	}
	var body struct {
		Filters []map[string]any `json:"filters"`
	}
	if len(strings.TrimSpace(string(c.body))) > 0 {
		if r := c.decodeJSON(&body); r != nil {
			return *r // code 10002 UNVERIFIED
		}
	}
	t := &workerTail{
		ID: c.s.ids.next(hex32), Token: hex32(), Account: c.account.id, Script: name,
		Created: c.now, Expires: c.now.Add(tailLifetime).Truncate(time.Second), Seq: c.s.nextSeq(),
		Filters: body.Filters, conns: map[*tailConn]struct{}{},
	}
	t.URL = c.tailBaseURL() + "/" + t.Token
	if c.account.tails == nil {
		c.account.tails = map[string]*workerTail{}
	}
	c.account.tails[t.ID] = t
	if c.s.tailSockets == nil {
		c.s.tailSockets = map[string]*workerTail{}
	}
	for tok, old := range c.s.tailSockets { // expired URLs no longer upgrade; drop idle ones
		if !c.now.Before(old.Expires) && len(old.conns) == 0 {
			delete(c.s.tailSockets, tok)
		}
	}
	c.s.tailSockets[t.Token] = t
	return ok(t.json())
}

// tailList: GET …/tails lists the script's tails (0073); a deleted tail is gone from it (0075,
// 0141). Expired tails are left out (UNVERIFIED). Missing script: UNVERIFIED (404/10007).
func tailList(c *reqCtx) response {
	name := c.params["script_name"]
	if _, ok := c.account.scripts[name]; !ok {
		return workerNotFound()
	}
	var ts []*workerTail
	for _, t := range c.account.tails {
		if t.Script == name && !t.Deleted && c.now.Before(t.Expires) {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Seq < ts[j].Seq })
	out := make([]any, len(ts))
	for i, t := range ts {
		out[i] = t.json()
	}
	return ok(out)
}

// tailDelete: DELETE …/tails/{id} answers result null (0074, 0140). It does NOT disconnect the
// WebSocket clients of the tail, and its URL still upgrades afterwards (spike §1: still
// upgrading 7 minutes after the DELETE), so the emulator keeps both until the tail expires.
// Whether events keep flowing to those connections is UNVERIFIED (here they do). Unknown tail:
// UNVERIFIED (404/10000).
func tailDelete(c *reqCtx) response {
	t, ok := c.account.tails[c.params["tail_id"]]
	if !ok || t.Deleted || t.Script != c.params["script_name"] {
		return fail(http.StatusNotFound, 10000, "Tail not found") // UNVERIFIED
	}
	t.Deleted = true
	return response{status: http.StatusOK, result: nil}
}

// ---- WebSocket -------------------------------------------------------------------------------

var tailUpgrader = websocket.Upgrader{
	Subprotocols: []string{tailSubprotocol},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// isTailSocket reports whether r is a WebSocket upgrade of a tail URL.
func isTailSocket(r *http.Request) bool {
	return tailTokenPath.MatchString(r.URL.Path) && websocket.IsWebSocketUpgrade(r)
}

// serveTailSocket upgrades a tail URL. Unknown or expired URLs answer 404 and a request without
// the trace-v1 subprotocol 400 (both statuses UNVERIFIED; the subprotocol requirement is
// SOURCED, see tailSubprotocol). Frames are binary (spike §1: "binary JSON frames"; wrangler's
// test fixtures send Buffers, SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/__tests__/tail.test.ts#L1470-L1497).
// The server never closes the socket itself, not even at expiry, when delivery just stops
// (SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/tail/index.ts#L260-L264); it
// answers WebSocket pings (wrangler pings every 10 s and reconnects without a pong, index.ts#L28,L495-L545).
// Client messages (wrangler sends {"debug": false} on open, createTail.ts#L185-L195) are read
// and ignored.
func (s *Server) serveTailSocket(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/")
	now := s.Clock.Now()
	s.mu.Lock()
	t := s.tailSockets[token]
	live := t != nil && now.Before(t.Expires)
	s.mu.Unlock()
	if !live {
		http.Error(w, "tail not found", http.StatusNotFound)
		return
	}
	offered := false
	for _, p := range websocket.Subprotocols(r) {
		offered = offered || p == tailSubprotocol
	}
	if !offered {
		http.Error(w, "Sec-WebSocket-Protocol must be "+tailSubprotocol, http.StatusBadRequest)
		return
	}
	ws, err := tailUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already answered
	}
	tc := &tailConn{ws: ws, send: make(chan []byte, tailSendBuffer), done: make(chan struct{})}
	s.mu.Lock()
	if s.tailSockets[token] != t { // Reset ran during the upgrade
		s.mu.Unlock()
		_ = ws.Close()
		return
	}
	t.conns[tc] = struct{}{}
	s.mu.Unlock()

	go func() { // writer
		for {
			select {
			case <-tc.done:
				return
			case f := <-tc.send:
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
					tc.close()
					return
				}
			}
		}
	}()
	go func() { // reader: drives ping/pong and notices the close
		defer func() {
			tc.close()
			s.mu.Lock()
			delete(t.conns, tc)
			s.mu.Unlock()
		}()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// tailConnsLocked returns the open connections that should receive inv: every tail of the
// script, deleted or not (see tailDelete), that has not expired and whose filters pass.
// Callers hold s.mu.
func (s *Server) tailConnsLocked(account, script string, inv WorkerInvocation, now time.Time) []*tailConn {
	var out []*tailConn
	for _, t := range s.tailSockets {
		if t.Account != account || t.Script != script || !now.Before(t.Expires) || !tailFiltersPass(t.Filters, inv) {
			continue
		}
		for tc := range t.conns {
			out = append(out, tc)
		}
	}
	return out
}

// tailFiltersPass applies wrangler's tail filters (SOURCED cloudflare/workers-sdk@f025bbf:packages/wrangler/src/tail/filters.ts#L67-L137):
// outcome, method and query (a text match in console.log messages) are applied; sampling_rate,
// header and client_ip are accepted and ignored. How the real tail combines them is
// UNVERIFIED (here: all must pass).
func tailFiltersPass(filters []map[string]any, inv WorkerInvocation) bool {
	anyOf := func(v any, want string) bool {
		xs, _ := v.([]any)
		for _, x := range xs {
			if s, ok := x.(string); ok && strings.EqualFold(s, want) {
				return true
			}
		}
		return false
	}
	for _, f := range filters {
		if o, ok := f["outcome"]; ok && !anyOf(o, inv.Outcome) {
			return false
		}
		if m, ok := f["method"]; ok && !anyOf(m, inv.Request.Method) {
			return false
		}
		if q, ok := f["query"].(string); ok && q != "" {
			hit := false
			for _, l := range inv.Logs {
				hit = hit || strings.Contains(joinLogArgs(l.Message), q)
			}
			if !hit {
				return false
			}
		}
	}
	return true
}

// traceFrame is the trace-v1 message of one invocation (wrangler's TailEventMessage): outcome,
// scriptName, exceptions [{name, message, timestamp, stack?}], logs [{message: [args],
// level, timestamp}], eventTimestamp, event {request {url, method, headers, cf}}.
// event.response {status}: DOCS https://developers.cloudflare.com/workers/runtime-apis/handlers/tail/
// (FetchEventInfo has request and response).
func traceFrame(script string, inv WorkerInvocation) map[string]any {
	logs := make([]any, len(inv.Logs))
	for i, l := range inv.Logs {
		args, isArr := l.Message.([]any)
		if !isArr {
			args = []any{l.Message}
		}
		logs[i] = map[string]any{"message": args, "level": l.Level, "timestamp": l.Timestamp.UnixMilli()}
	}
	exceptions := []any{}
	if e := inv.Exception; e != nil {
		x := map[string]any{"name": e.Name, "message": e.Message, "timestamp": inv.Timestamp.UnixMilli()}
		if e.Stack != "" {
			x["stack"] = e.Stack
		}
		exceptions = append(exceptions, x)
	}
	headers := map[string]string{}
	for k, v := range inv.Request.Headers {
		headers[strings.ToLower(k)] = v
	}
	req := map[string]any{"url": inv.Request.URL, "method": inv.Request.Method, "headers": headers}
	cf := inv.Request.CF
	if cf == nil {
		cf = map[string]any{}
	}
	req["cf"] = cf
	return map[string]any{
		"outcome":        inv.Outcome,
		"scriptName":     script,
		"exceptions":     exceptions,
		"logs":           logs,
		"eventTimestamp": inv.Timestamp.UnixMilli(),
		"event":          map[string]any{"request": req, "response": map[string]any{"status": inv.Request.Status}},
	}
}

// ---- control helpers -------------------------------------------------------------------------

// TailSession describes one tail for tests (GET /_fake/accounts/{account}/workers/{script}/tails).
type TailSession struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	ExpiresAt   time.Time `json:"expires_at"`
	Deleted     bool      `json:"deleted"`
	Connections int       `json:"connections"`
}

// TailSessions lists every tail ever created for script in account (deleted ones too, while
// they can still hold connections), oldest first.
func (s *Server) TailSessions(account, script string) []TailSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ts []*workerTail
	for _, t := range s.tailSockets {
		if t.Account == account && t.Script == script {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Seq < ts[j].Seq })
	out := []TailSession{}
	for _, t := range ts {
		out = append(out, TailSession{ID: t.ID, URL: t.URL, ExpiresAt: t.Expires, Deleted: t.Deleted, Connections: len(t.conns)})
	}
	return out
}

// DisconnectTails drops every WebSocket connection of script's tails (an abrupt network
// close, for reconnect tests) and returns how many it dropped. The tails stay usable.
func (s *Server) DisconnectTails(account, script string) int {
	s.mu.Lock()
	var conns []*tailConn
	for _, t := range s.tailSockets {
		if t.Account == account && t.Script == script {
			for tc := range t.conns {
				conns = append(conns, tc)
			}
		}
	}
	s.mu.Unlock()
	for _, tc := range conns {
		tc.close()
	}
	return len(conns)
}

// closeAllTailsLocked drops every tail connection (Reset). Callers hold s.mu.
func (s *Server) closeAllTailsLocked() {
	for _, t := range s.tailSockets {
		for tc := range t.conns {
			go tc.close()
		}
	}
	s.tailSockets = nil
}
