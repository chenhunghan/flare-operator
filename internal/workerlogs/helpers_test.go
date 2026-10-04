package workerlogs

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/types"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

const recordingsDir = "../../test/recordings/2026-09-29"

// recording is the part of a test/recordings file the tests replay.
type recording struct {
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Status       int             `json:"status"`
	RequestBody  json.RawMessage `json:"request_body"`
	ResponseBody json.RawMessage `json:"response_body"`
}

func loadRecording(t *testing.T, prefix string) recording {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(recordingsDir, prefix+"-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("recording %s: %v (%d matches)", prefix, err, len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var r recording
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

var tokenSeq atomic.Int64

// newTestClient builds a cfclient with its own token (so tests don't share a rate limiter) and
// a generous rate.
func newTestClient(t *testing.T, baseURL string) cfclient.Client {
	t.Helper()
	c, err := cfclient.New(cfclient.Options{
		Token:      fmt.Sprintf("test-token-%s-%d", t.Name(), tokenSeq.Add(1)),
		BaseURL:    baseURL + "/client/v4",
		RPS:        1000,
		Burst:      1000,
		MaxRetries: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testTarget(c cfclient.Client) Target {
	return Target{
		Client:            c,
		AccountID:         "ACCOUNT_ID",
		Script:            "flare-spike-logs-1",
		WorkerScript:      types.NamespacedName{Namespace: "team", Name: "api"},
		AllowInsecureTail: true,
	}
}

func writeEnvelope(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": status < 300, "errors": []any{}, "messages": []any{}, "result": result,
	})
}

// ---- a fake Cloudflare API with the legacy tail ----------------------------------------------

// fakeCF serves telemetry/query (through query), the tails routes (0064, 0074) and a trace-v1
// WebSocket per tail, shaped like wrangler expects (createTail.ts).
type fakeCF struct {
	t   *testing.T
	srv *httptest.Server

	query func(w http.ResponseWriter, body []byte)

	// tail behavior
	createStatus atomic.Int32 // non-zero: POST …/tails answers this status
	// createGate, when set (under mu), holds every tail create until it can receive; the
	// handler first signals createStarted.
	createGate    chan struct{}
	createStarted chan struct{}
	tailScheme    string // "ws" (default) or e.g. "http"
	expiresIn     time.Duration
	noPong        atomic.Bool
	rejectUpgrade atomic.Bool

	mu        sync.Mutex
	creates   int
	deletes   []string
	queries   [][]byte
	conns     map[string]*fakeTailConn
	connCh    chan *fakeTailConn
	secretFor map[string]string
}

type fakeTailConn struct {
	id       string
	ws       *websocket.Conn
	wmu      sync.Mutex
	debugMsg string
	closed   chan struct{} // the client closed the socket (server read failed)
}

func (c *fakeTailConn) send(t *testing.T, frame string) {
	t.Helper()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// Real tails send binary frames (spike §1).
	if err := c.ws.WriteMessage(websocket.BinaryMessage, []byte(frame)); err != nil {
		t.Errorf("send frame: %v", err)
	}
}

func (c *fakeTailConn) closeWith(code int) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
	if code != websocket.CloseNormalClosure {
		_ = c.ws.Close()
	}
}

func newFakeCF(t *testing.T) *fakeCF {
	f := &fakeCF{t: t, tailScheme: "ws", expiresIn: 6 * time.Hour,
		conns: map[string]*fakeTailConn{}, connCh: make(chan *fakeTailConn, 16), secretFor: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

const tailsPrefix = "/client/v4/accounts/ACCOUNT_ID/workers/scripts/flare-spike-logs-1/tails"

func (f *fakeCF) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/client/v4/accounts/ACCOUNT_ID/workers/observability/telemetry/query":
		f.mu.Lock()
		f.queries = append(f.queries, body)
		q := f.query
		f.mu.Unlock()
		// Every query asks for this script only (recording 0054's filter).
		var req queryRequest
		if err := json.Unmarshal(body, &req); err != nil || len(req.Parameters.Filters) != 1 ||
			req.Parameters.Filters[0] != (queryFilter{Key: "$metadata.service", Operation: "eq", Type: "string", Value: "flare-spike-logs-1"}) {
			f.t.Errorf("query body %s", body)
		}
		if q == nil {
			writeEnvelope(w, 200, map[string]any{"events": map[string]any{"events": []any{}}})
			return
		}
		q(w, body)
	case r.Method == http.MethodPost && r.URL.Path == tailsPrefix:
		if s := f.createStatus.Load(); s != 0 {
			writeEnvelope(w, int(s), nil)
			return
		}
		f.mu.Lock()
		gate, started := f.createGate, f.createStarted
		f.mu.Unlock()
		if gate != nil {
			started <- struct{}{}
			<-gate
		}
		if strings.TrimSpace(string(body)) != "{}" {
			f.t.Errorf("tail create body = %s, want {} (0064)", body)
		}
		f.mu.Lock()
		f.creates++
		id := fmt.Sprintf("tail%d", f.creates)
		secret := fmt.Sprintf("SECRET-capability-%d", f.creates)
		f.secretFor[id] = secret
		scheme, expiresIn := f.tailScheme, f.expiresIn
		f.mu.Unlock()
		host := strings.TrimPrefix(f.srv.URL, "http://")
		writeEnvelope(w, 200, map[string]any{
			"id":         id,
			"url":        scheme + "://" + host + "/tail/" + id + "/" + secret,
			"expires_at": time.Now().Add(expiresIn).UTC().Format(time.RFC3339),
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, tailsPrefix+"/"):
		f.mu.Lock()
		f.deletes = append(f.deletes, strings.TrimPrefix(r.URL.Path, tailsPrefix+"/"))
		f.mu.Unlock()
		writeEnvelope(w, 200, nil)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tail/"):
		f.upgrade(w, r)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		writeEnvelope(w, 404, nil)
	}
}

func (f *fakeCF) upgrade(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tail/"), "/")
	if f.rejectUpgrade.Load() {
		http.Error(w, "nope", http.StatusForbidden)
		return
	}
	if got := websocket.Subprotocols(r); len(got) != 1 || got[0] != TailSubprotocol {
		f.t.Errorf("subprotocols = %v, want [trace-v1]", got)
	}
	up := websocket.Upgrader{Subprotocols: []string{TailSubprotocol}}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("upgrade: %v", err)
		return
	}
	c := &fakeTailConn{id: parts[0], ws: ws, closed: make(chan struct{})}
	if f.noPong.Load() {
		ws.SetPingHandler(func(string) error { return nil })
	}
	_, msg, err := ws.ReadMessage()
	if err != nil {
		return // the client went away (a test may have ended); it never shows as a conn
	}
	c.debugMsg = string(msg)
	f.mu.Lock()
	f.conns[c.id] = c
	f.mu.Unlock()
	f.connCh <- c
	go func() {
		defer close(c.closed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (f *fakeCF) setQuery(q func(http.ResponseWriter, []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.query = q
}

// with changes the fake's settings under its lock.
func (f *fakeCF) with(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakeCF) queryBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.queries...)
}

func (f *fakeCF) nextConn(t *testing.T) *fakeTailConn {
	t.Helper()
	select {
	case c := <-f.connCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no tail connection")
		return nil
	}
}

func (f *fakeCF) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeCF) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitClosed(t *testing.T, c *fakeTailConn) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("tail socket %s was not closed by the client", c.id)
	}
}

func recvEvent(t *testing.T, s Stream) Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatalf("stream closed: %v", s.Err())
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}
