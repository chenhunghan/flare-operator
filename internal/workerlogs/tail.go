package workerlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// errTailClosed ends a follow when Cloudflare closes the tail normally (close code 1000), which
// wrangler also treats as the end of the session (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/index.ts#L298-L302).
var errTailClosed = errors.New("the tail was closed by Cloudflare")

// errMissedPong: no pong arrived before the next ping was due (wrangler's keep-alive rule;
// SOURCED: cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/index.ts#L495-L537).
var errMissedPong = errors.New("the tail did not answer a keep-alive ping")

// pingPayload correlates our pings with their pongs.
const pingPayload = "flare-operator tail ping"

// redactedError carries a message from which the tail URL has been removed; it deliberately
// wraps nothing, so no underlying error (which could print the URL) is reachable.
type redactedError struct{ msg string }

func (e *redactedError) Error() string { return e.msg }

// ---- subscriptions ---------------------------------------------------------------------------

// subscription is one follower's Stream.
type subscription struct {
	src *cfSource
	up  *upstream

	// Guarded by src.mu.
	ch      chan Event
	err     error
	closed  bool
	dropped int

	done chan struct{} // closed with ch
}

func (s *subscription) Events() <-chan Event { return s.ch }

func (s *subscription) Err() error {
	s.src.mu.Lock()
	defer s.src.mu.Unlock()
	return s.err
}

// Close ends the subscription; the upstream tail is stopped (socket closed, tail deleted) when
// its last subscriber leaves.
func (s *subscription) Close() error {
	s.src.mu.Lock()
	defer s.src.mu.Unlock()
	s.closeLocked(nil)
	s.up.removeLocked(s)
	return nil
}

// closeLocked closes the channel once, recording err. Caller holds src.mu.
func (s *subscription) closeLocked(err error) {
	if s.closed {
		return
	}
	s.closed = true
	s.err = err
	close(s.ch)
	close(s.done)
}

// offerLocked delivers ev without blocking; when the follower lags, events are dropped and a
// notice says how many before the next delivered event. Caller holds src.mu.
func (s *subscription) offerLocked(ev Event) {
	if s.closed {
		return
	}
	if s.dropped > 0 {
		n := Event{Time: ev.Time, Kind: KindNotice,
			Message: fmt.Sprintf("flare-operator: %d events dropped because the reader is too slow", s.dropped)}
		select {
		case s.ch <- n:
			s.dropped = 0
		default:
			s.dropped++
			return
		}
	}
	select {
	case s.ch <- ev:
	default:
		s.dropped++
	}
}

// ---- follow ----------------------------------------------------------------------------------

func (s *cfSource) follow(ctx context.Context, t Target) (Stream, error) {
	if t.Client == nil {
		return nil, fmt.Errorf("workerlogs: target %s has no Cloudflare client", t.WorkerScript)
	}
	s.mu.Lock()
	key := s.keyFor(t)
	u := s.tails[key]
	if u == nil {
		u = s.newUpstream(key, t)
		s.tails[key] = u
		go u.run()
	}
	sub := &subscription{src: s, up: u, ch: make(chan Event, s.tune.subBuffer), done: make(chan struct{})}
	u.subs[sub] = struct{}{}
	s.mu.Unlock()

	select {
	case <-u.ready:
		if u.initErr != nil {
			_ = sub.Close()
			return nil, u.initErr
		}
	case <-ctx.Done():
		_ = sub.Close()
		return nil, ctx.Err()
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = sub.Close()
		case <-sub.done:
		}
	}()
	return sub, nil
}

// upstream is one legacy tail shared by the followers of (account, script, client).
type upstream struct {
	src    *cfSource
	key    tailKey
	target Target
	ctx    context.Context
	cancel context.CancelFunc
	seq    seqCounter

	ready   chan struct{} // closed once the first connection is up or has failed
	initErr error         // set before ready is closed

	subs map[*subscription]struct{} // guarded by src.mu

	// De-duplication across a planned replacement (run goroutine only): while recording, the
	// events delivered from the old socket are counted in dedup; until dedupUntil, the same
	// events from the new socket are dropped.
	dedup      map[string]int
	recording  bool
	dedupUntil time.Time
}

func (s *cfSource) newUpstream(key tailKey, t Target) *upstream {
	ctx, cancel := context.WithCancel(context.Background())
	return &upstream{
		src: s, key: key, target: t, ctx: ctx, cancel: cancel,
		ready: make(chan struct{}), subs: map[*subscription]struct{}{},
	}
}

// removeLocked drops sub; the last one stops the upstream. Caller holds src.mu.
func (u *upstream) removeLocked(sub *subscription) {
	delete(u.subs, sub)
	if len(u.subs) == 0 {
		u.detachLocked()
		u.cancel()
	}
}

// detachLocked removes u from the hub so that new followers create a new tail.
func (u *upstream) detachLocked() {
	if u.src.tails[u.key] == u {
		delete(u.src.tails, u.key)
	}
}

// end closes every subscription with err and detaches the upstream.
func (u *upstream) end(err error) {
	u.src.mu.Lock()
	defer u.src.mu.Unlock()
	u.detachLocked()
	for sub := range u.subs {
		sub.closeLocked(err)
		delete(u.subs, sub)
	}
	u.cancel()
}

func (u *upstream) broadcast(evs []Event) {
	if len(evs) == 0 {
		return
	}
	u.src.mu.Lock()
	defer u.src.mu.Unlock()
	for sub := range u.subs {
		for _, ev := range evs {
			sub.offerLocked(ev)
		}
	}
}

// serveResult says why serve returned.
type serveResult int

const (
	serveStopped  serveResult = iota // the last follower left
	serveFailed                      // abnormal close, read error or missed pong: reconnect
	serveExpiring                    // expires_at is near: replace the tail
	serveClosed                      // normal close by Cloudflare: end the follow
)

// run owns the tail: it connects, serves frames, keeps the connection alive, replaces the tail
// before it expires, reconnects with wrangler's back-off, and tears the tail down (socket closed,
// tail deleted) on every way out.
func (u *upstream) run() {
	log := u.src.log.WithValues("workerScript", u.target.WorkerScript.String(), "script", u.target.Script)
	var conn *tailConn
	readyClosed := false
	closeReady := func() {
		if !readyClosed {
			readyClosed = true
			close(u.ready)
		}
	}
	defer func() {
		// A bug must not crash the process or leak the tail: end this follow with an error,
		// close the socket and delete the tail.
		if r := recover(); r != nil {
			err := fmt.Errorf("workerlogs: internal error following the tail of %s: %v", u.target.Script, r)
			log.Error(err, "tail follower panicked")
			if conn != nil {
				u.teardownQuietly(conn)
			}
			if !readyClosed {
				u.initErr = err
				closeReady()
			}
			u.end(err)
		}
	}()
	var err error
	conn, err = u.connect()
	u.initErr = err
	closeReady()
	if err != nil {
		u.end(err)
		return
	}
	tune := u.src.tune
	failures := 0          // steps taken on the back-off schedule since the last stable connection
	var recent []time.Time // reconnect times within tune.reconnectWindow
	for {
		res, cause := u.serve(conn)
		switch res {
		case serveStopped:
			u.teardown(conn, false)
			conn = nil
			return
		case serveClosed:
			u.teardown(conn, true)
			conn = nil
			u.end(errTailClosed)
			return
		case serveExpiring:
			// Make before break: open the new tail, then retire the old one. Frames the old
			// socket still holds are delivered and remembered, so that the same events arriving
			// on the new tail during the overlap are dropped.
			next, err := u.connect()
			if err == nil {
				log.V(1).Info("replaced the tail before it expired", "oldTail", conn.id, "newTail", next.id)
				u.dedup = map[string]int{}
				u.recording = true
				u.teardown(conn, true)
				u.recording = false
				u.dedupUntil = u.src.now().Add(tune.dedupWindow)
				conn = next
				continue
			}
			if u.ctx.Err() != nil {
				u.teardown(conn, false)
				conn = nil
				return
			}
			cause = err
		}
		// Failure: reconnect. Only a connection that lived a while restarts the schedule.
		if u.src.now().Sub(conn.openedAt) >= tune.stableAfter {
			failures = 0
		}
		lost := conn.lastAlive
		u.teardown(conn, true)
		conn = nil
		log.Info("tail connection lost; reconnecting", "reason", cause.Error())
		for conn == nil {
			if failures >= len(tune.backoff) {
				u.end(fmt.Errorf("workerlogs: the tail of %s was lost and %d reconnect attempts failed: %w",
					u.target.Script, failures, cause))
				return
			}
			now := u.src.now()
			kept := recent[:0]
			for _, t := range recent {
				if now.Sub(t) < tune.reconnectWindow {
					kept = append(kept, t)
				}
			}
			recent = kept
			if len(recent) >= tune.maxReconnects {
				u.end(fmt.Errorf("workerlogs: the tail of %s keeps failing (%d reconnects within %s): %w",
					u.target.Script, len(recent), tune.reconnectWindow, cause))
				return
			}
			if !sleepCtx(u.ctx, tune.backoff[failures]) {
				return
			}
			failures++
			recent = append(recent, u.src.now())
			c, err := u.connect()
			if err != nil {
				if u.ctx.Err() != nil {
					return
				}
				cause = err
				log.V(1).Info("tail reconnect attempt failed", "attempt", failures, "reason", err.Error())
				continue
			}
			conn = c
		}
		now := u.src.now()
		u.broadcast([]Event{{
			Time: now.UTC(), Seq: u.seq.next(), Kind: KindNotice,
			Message: fmt.Sprintf("flare-operator: tail reconnected; events between %s and %s may be missing",
				lost.UTC().Format(noticeTimeFormat), now.UTC().Format(noticeTimeFormat)),
		}})
	}
}

const noticeTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// sleepCtx waits d or until ctx ends; it reports whether the wait completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// serve processes one connection's frames until it ends, the tail nears expiry, or the last
// follower leaves. Keep-alive is wrangler's: a ping every TailPingInterval, and a connection
// whose pong has not come back by the next ping is lost (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/index.ts#L502-L549).
func (u *upstream) serve(c *tailConn) (serveResult, error) {
	pinger := time.NewTicker(u.src.limits.TailPingInterval)
	defer pinger.Stop()
	pings := pinger.C
	var expiry <-chan time.Time
	if !c.expiresAt.IsZero() {
		d := max(c.expiresAt.Sub(u.src.now())-u.src.tune.expiryMargin, u.src.tune.minLifetime)
		t := time.NewTimer(d)
		defer t.Stop()
		expiry = t.C
	}
	waitingForPong := false
	for {
		select {
		case <-u.ctx.Done():
			return serveStopped, nil
		case m, ok := <-c.frames:
			if !ok {
				return serveFailed, errors.New("the tail connection ended")
			}
			if m.err != nil {
				if websocket.IsCloseError(m.err, websocket.CloseNormalClosure) {
					return serveClosed, nil
				}
				return serveFailed, u.redact(c.url, m.err)
			}
			c.lastAlive = u.src.now()
			u.deliver(m.data)
		case <-c.pongs:
			waitingForPong = false
			c.lastAlive = u.src.now()
		case <-pings:
			if waitingForPong {
				return serveFailed, errMissedPong
			}
			waitingForPong = true
			if err := c.ws.WriteControl(websocket.PingMessage, []byte(pingPayload), time.Now().Add(5*time.Second)); err != nil {
				return serveFailed, u.redact(c.url, err)
			}
		case <-expiry:
			return serveExpiring, nil
		}
	}
}

// deliver decodes one frame and broadcasts its events; a malformed frame is skipped.
func (u *upstream) deliver(data []byte) {
	if h := u.src.tune.frameHook; h != nil {
		h(data)
	}
	evs, err := decodeFrame(data, u.src.now(), u.seq.next)
	if err != nil {
		u.src.log.V(1).Info("skipping a malformed tail frame", "script", u.target.Script, "bytes", len(data), "reason", err.Error())
		return
	}
	switch {
	case u.recording:
		for _, ev := range evs {
			u.dedup[eventKey(ev)]++
		}
	case u.dedup != nil && u.src.now().After(u.dedupUntil):
		u.dedup = nil
	case u.dedup != nil:
		kept := evs[:0]
		for _, ev := range evs {
			if k := eventKey(ev); u.dedup[k] > 0 {
				u.dedup[k]--
				continue
			}
			kept = append(kept, ev)
		}
		evs = kept
	}
	u.broadcast(evs)
}

// eventKey identifies an event by its content (everything but Seq), for de-duplication across
// two tails that both received it.
func eventKey(ev Event) string {
	ev.Seq = ""
	b, _ := json.Marshal(ev)
	return string(b)
}

// ---- one connection --------------------------------------------------------------------------

type frameMsg struct {
	data []byte
	err  error
}

// tailConn is one created tail and its socket. url is the capability URL: it stays here.
type tailConn struct {
	id        string
	url       *url.URL
	expiresAt time.Time
	ws        *websocket.Conn
	frames    chan frameMsg // closed when the reader exits
	pongs     chan struct{}
	stop      chan struct{}
	stopOnce  sync.Once
	openedAt  time.Time
	lastAlive time.Time
}

// tailCreateResult is the result of POST …/tails (0064).
type tailCreateResult struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

func (u *upstream) tailsPath() string {
	return "/accounts/" + url.PathEscape(u.target.AccountID) + "/workers/scripts/" + url.PathEscape(u.target.Script) + "/tails"
}

// connect creates a tail (POST …/tails with body {}, 0064), dials its URL with subprotocol
// trace-v1 and sends {"debug":false} (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/createTail.ts#L152-L195). Any
// failure after the create deletes the tail again.
func (u *upstream) connect() (*tailConn, error) {
	t := u.target
	resp, err := t.Client.Do(u.ctx, cfclient.Request{Method: http.MethodPost, Path: u.tailsPath(), Body: struct{}{}})
	if err != nil {
		return nil, fmt.Errorf("workerlogs: create a tail for %s: %w", t.Script, err)
	}
	var created tailCreateResult
	if err := json.Unmarshal(resp.Result, &created); err != nil || created.ID == "" || created.URL == "" {
		if created.ID != "" {
			u.deleteTail(created.ID)
		}
		return nil, fmt.Errorf("workerlogs: create a tail for %s: unexpected result", t.Script)
	}
	tu, err := url.Parse(created.URL)
	if err != nil || tu.Host == "" {
		u.deleteTail(created.ID)
		return nil, &redactedError{msg: "workerlogs: the tail URL returned by the API is not a valid URL"}
	}
	switch {
	case tu.Scheme == "wss":
	case tu.Scheme == "ws" && t.AllowInsecureTail:
	default:
		u.deleteTail(created.ID)
		return nil, ErrInsecureTailURL
	}
	c := &tailConn{
		id: created.ID, url: tu,
		frames: make(chan frameMsg, frameBuffer), pongs: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	if ts, err := time.Parse(time.RFC3339, created.ExpiresAt); err == nil {
		c.expiresAt = ts
	}

	dialer := *u.src.dialer
	dialer.Subprotocols = []string{TailSubprotocol}
	hdr := http.Header{"User-Agent": []string{cfclient.DefaultUserAgent}}
	ws, hresp, err := dialer.DialContext(u.ctx, tu.String(), hdr)
	if hresp != nil && hresp.Body != nil {
		_ = hresp.Body.Close()
	}
	if err != nil {
		u.deleteTail(created.ID)
		return nil, u.redactDial(tu, err, hresp)
	}
	c.ws = ws
	ws.SetReadLimit(maxFrameBytes)
	ws.SetPongHandler(func(data string) error {
		if data == pingPayload {
			select {
			case c.pongs <- struct{}{}:
			default:
			}
		}
		return nil
	})
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"debug":false}`)); err != nil {
		_ = ws.Close()
		u.deleteTail(created.ID)
		return nil, u.redact(tu, err)
	}
	_ = ws.SetWriteDeadline(time.Time{})
	c.openedAt = u.src.now()
	c.lastAlive = c.openedAt
	go c.read()
	u.src.log.V(1).Info("tail connected", "script", t.Script, "tail", c.id, "host", tu.Host)
	return c, nil
}

// read pumps frames until the socket fails or is closed.
func (c *tailConn) read() {
	defer close(c.frames)
	defer func() {
		// A panic ends this connection like a read error (the follower reconnects); it never
		// crashes the process.
		if r := recover(); r != nil {
			select {
			case c.frames <- frameMsg{err: fmt.Errorf("tail reader panicked: %v", r)}:
			case <-c.stop:
			}
		}
	}()
	for {
		typ, data, err := c.ws.ReadMessage()
		if err != nil {
			select {
			case c.frames <- frameMsg{err: err}:
			case <-c.stop:
			}
			return
		}
		if typ != websocket.TextMessage && typ != websocket.BinaryMessage {
			continue
		}
		select {
		case c.frames <- frameMsg{data: data}:
		case <-c.stop:
			return
		}
	}
}

// teardown closes our socket (a DELETE alone leaves it streaming; spike §1) and deletes the tail
// (0074). With deliver, frames that were already received are still broadcast.
func (u *upstream) teardown(c *tailConn, deliver bool) {
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = c.ws.Close()
	drain := time.NewTimer(2 * time.Second)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case m, ok := <-c.frames:
			if !ok {
				break drainLoop
			}
			if deliver && m.err == nil {
				u.deliver(m.data)
			}
		case <-drain.C:
			break drainLoop
		}
	}
	c.stopOnce.Do(func() { close(c.stop) })
	u.deleteTail(c.id)
}

// teardownQuietly is teardown for the panic path: it never panics itself.
func (u *upstream) teardownQuietly(c *tailConn) {
	defer func() { _ = recover() }()
	if c.ws != nil {
		_ = c.ws.Close()
	}
	c.stopOnce.Do(func() { close(c.stop) })
	u.deleteTail(c.id)
}

// deleteTail deletes a tail with a context that outlives the followers' (the tail must go even
// when the request that opened it was cancelled).
func (u *upstream) deleteTail(id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), u.src.tune.cleanupTimeout)
	defer cancel()
	_, err := u.target.Client.Do(ctx, cfclient.Request{Method: http.MethodDelete, Path: u.tailsPath() + "/" + url.PathEscape(id)})
	if err != nil && !cfclient.IsNotFound(err) {
		u.src.log.Info("could not delete a tail; it expires by itself", "script", u.target.Script, "tail", id, "reason", err.Error())
	}
}

// redact returns err's message with the tail URL (and its path) replaced by scheme://host.
func (u *upstream) redact(tu *url.URL, err error) error {
	return &redactedError{msg: "workerlogs: tail connection to " + tu.Scheme + "://" + tu.Host + ": " + scrubURL(err.Error(), tu)}
}

func (u *upstream) redactDial(tu *url.URL, err error, resp *http.Response) error {
	msg := scrubURL(err.Error(), tu)
	if resp != nil {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, resp.StatusCode)
	}
	return &redactedError{msg: "workerlogs: connect to the tail at " + tu.Scheme + "://" + tu.Host + ": " + msg}
}

// scrubURL removes every form of the capability URL from s: the whole URL, then its path and
// query (escaped and not), leaving "scheme://host" and "<redacted>".
func scrubURL(s string, tu *url.URL) string {
	origin := tu.Scheme + "://" + tu.Host
	s = strings.ReplaceAll(s, tu.String(), origin+"/<redacted>")
	for _, part := range []string{tu.EscapedPath(), tu.Path, tu.RawQuery, tu.RawPath} {
		if len(part) > 1 {
			s = strings.ReplaceAll(s, part, "/<redacted>")
		}
	}
	return s
}
