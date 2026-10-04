package workerlogs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
	"github.com/chenhunghan/flare-operator/internal/fake"
	"github.com/chenhunghan/flare-operator/internal/testenv"
)

// Integration tests: the real Source and Streamer against an in-process flarefake
// (internal/fake workers_logs.go, workers_tail.go), driven through its /_fake control API.

const fakeScript = "flare-spike-logs-1"

type fakeEnv struct {
	fs     *fake.Server
	ctl    *testenv.FakeControl
	target Target
	acct   string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	fs := fake.New(fake.Options{})
	hs := httptest.NewServer(fs)
	t.Cleanup(hs.Close)
	e := &fakeEnv{fs: fs, ctl: testenv.NewFakeControl(hs.URL), acct: testenv.RandomAccountID()}
	c := newTestClient(t, hs.URL)
	e.target = testTarget(c)
	e.target.AccountID = e.acct
	e.target.Script = fakeScript

	// The tail routes need an existing Worker (404/10007 otherwise).
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"; filename="blob"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write([]byte(`{"main_module":"index.js"}`))
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="index.js"; filename="index.js"`)
	h.Set("Content-Type", "application/javascript+module")
	pw, _ = mw.CreatePart(h)
	_, _ = pw.Write([]byte(`export default { async fetch() { return new Response("ok"); } };`))
	_ = mw.Close()
	if _, err := c.Do(context.Background(), cfclient.Request{Method: http.MethodPut,
		Path: "/accounts/" + e.acct + "/workers/scripts/" + fakeScript, RawBody: body.Bytes(), ContentType: mw.FormDataContentType()}); err != nil {
		t.Fatalf("upload the script: %v", err)
	}
	return e
}

func (e *fakeEnv) inject(t *testing.T, inv fake.WorkerInvocation) fake.InjectedLogs {
	t.Helper()
	res, err := e.ctl.InjectWorkerLogs(context.Background(), e.acct, fakeScript, inv)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *fakeEnv) sessions(t *testing.T) []fake.TailSession {
	t.Helper()
	s, err := e.ctl.TailSessions(context.Background(), e.acct, fakeScript)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readLines(t *testing.T, r *bufio.Reader, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for range n {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", out, err)
		}
		out = append(out, strings.TrimSuffix(l, "\n"))
	}
	return out
}

func TestFakeHistory(t *testing.T) {
	e := newFakeEnv(t)
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond).UTC()
	e.inject(t, fake.WorkerInvocation{Timestamp: fake.LogTime{Time: base},
		Request: &fake.InvocationRequest{URL: "https://w.example/a?x=1"}, WallTimeMs: 2, CPUTimeMs: 1,
		Logs: []fake.InvocationLog{
			{Message: []any{"hello", 42, map[string]any{"k": true}}},
			{Level: "warn", Message: "careful\nsecond line"},
		}})
	e.inject(t, fake.WorkerInvocation{Timestamp: fake.LogTime{Time: base.Add(time.Minute)},
		Request:   &fake.InvocationRequest{Method: "post", URL: "https://w.example/throw"},
		Logs:      []fake.InvocationLog{{Level: "error", Message: "about to throw"}},
		Exception: &fake.InvocationException{Message: "boom", Stack: "    at Object.fetch (worker.mjs:10:13)"}})
	e.inject(t, fake.WorkerInvocation{Timestamp: fake.LogTime{Time: base.Add(5 * time.Minute)}, Level: "info", Message: "late"})

	st := NewStreamer(NewSource(Limits{}), NewFormatter(), Limits{}, time.Now)
	logs := func(o Options) []string {
		t.Helper()
		rc, err := st.Logs(context.Background(), e.target, o)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.TrimSuffix(readAll(t, rc), "\n")
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	all := []string{
		`[log] hello 42 {"k":true}`,
		`[warn] careful`, `second line`,
		`[fetch] GET https://w.example/a?x=1 -> 200 ok (wall 2ms, cpu 1ms) ray=`,
		`[error] about to throw`,
		`[exception] Error: boom`, `    at Object.fetch (worker.mjs:10:13)`,
		`[fetch] POST https://w.example/throw -> 500 exception (wall 0ms, cpu 0ms) ray=`,
		`[info] late`,
		`[fetch] GET https://` + fakeScript + `.example-subdomain.workers.dev/ -> 200 ok (wall 0ms, cpu 0ms) ray=`,
	}
	match := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			// Ray IDs are random: compare the prefix up to "ray=".
			if !strings.HasPrefix(got[i], want[i]) || (strings.HasSuffix(want[i], "ray=") && len(got[i]) != len(want[i])+16) {
				return false
			}
		}
		return true
	}
	if got := logs(Options{}); !match(got, all) {
		t.Errorf("all:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(all, "\n"))
	}
	if got := logs(Options{TailLines: i64(3)}); !match(got, all[7:]) {
		t.Errorf("--tail=3:\n%s", strings.Join(got, "\n"))
	}
	since := base.Add(30 * time.Second)
	if got := logs(Options{SinceTime: &since}); !match(got, all[4:]) {
		t.Errorf("--since:\n%s", strings.Join(got, "\n"))
	}
	if got := logs(Options{TailLines: i64(0)}); got != nil {
		t.Errorf("--tail=0: %q", got)
	}
	// --timestamps: the invocation's frozen time in the kubelet format.
	if got := logs(Options{TailLines: i64(1), Timestamps: true}); len(got) != 1 ||
		!strings.HasPrefix(got[0], base.Add(5*time.Minute).Format("2006-01-02T15:04:05.000000000Z")+" [fetch] GET") {
		t.Errorf("--timestamps: %q", got)
	}
}

func TestFakeHistoryPaging(t *testing.T) {
	e := newFakeEnv(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond).UTC()
	const n = 1100 // two events each (log + invocation): 2200 events
	for i := range n {
		if _, err := e.fs.InjectWorkerLogs(e.acct, fakeScript, fake.WorkerInvocation{
			Timestamp: fake.LogTime{Time: base.Add(time.Duration(i) * time.Second)},
			Message:   fmt.Sprintf("event %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ctl.ClearJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := NewSource(Limits{})
	res, err := src.Query(context.Background(), e.target, QueryOptions{From: base.Add(-time.Minute), To: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2*n || res.More {
		t.Fatalf("%d events More=%v, want %d false", len(res.Events), res.More, 2*n)
	}
	for i := 0; i < len(res.Events); i += 2 {
		if l := res.Events[i]; l.Kind != KindLog || l.Message != fmt.Sprintf("event %d", i/2) {
			t.Fatalf("event %d = %+v", i, l)
		}
		if inv := res.Events[i+1]; inv.Kind != KindInvocation {
			t.Fatalf("event %d = %+v", i+1, inv)
		}
	}
	j, err := e.ctl.Journal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q := testenv.Count(j, http.MethodPost, "/telemetry/query"); q != 2 {
		t.Errorf("%d telemetry queries for 2200 events, want 2 (2000 + 200)", q)
	}

	// A MaxEvents cut reports More and keeps the newest events.
	res, err = src.Query(context.Background(), e.target, QueryOptions{From: base.Add(-time.Minute), To: time.Now(), MaxEvents: 2001})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2001 || !res.More || res.Events[2000].Kind != KindInvocation || res.Events[1999].Message != fmt.Sprintf("event %d", n-1) {
		t.Errorf("cut: %d More=%v last=%+v", len(res.Events), res.More, res.Events[len(res.Events)-1])
	}
}

// overlapSource injects an invocation between opening the tail and reading the history, so that
// it reaches both: the Streamer must print it once.
type overlapSource struct {
	Source
	afterFollow func()
}

func (o overlapSource) Follow(ctx context.Context, t Target) (Stream, error) {
	s, err := o.Source.Follow(ctx, t)
	if err == nil {
		o.afterFollow()
	}
	return s, err
}

func TestFakeFollow(t *testing.T) {
	e := newFakeEnv(t)
	e.inject(t, fake.WorkerInvocation{Timestamp: fake.LogTime{Time: time.Now().Add(-time.Minute)}, Message: "old"})
	var overlap fake.InjectedLogs
	log, logs := captureLogs()
	src := overlapSource{Source: NewSource(Limits{}, WithLogger(log), withTuning(fastTuning)), afterFollow: func() {
		overlap = e.inject(t, fake.WorkerInvocation{Level: "error", Message: "both", Request: &fake.InvocationRequest{URL: "https://w.example/both"}})
	}}
	st := NewStreamer(src, NewFormatter(), Limits{}, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := st.Logs(ctx, e.target, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if overlap.TailFrames != 1 {
		t.Fatalf("the overlapping invocation reached %d tails, want 1", overlap.TailFrames)
	}
	r := bufio.NewReader(rc)
	hist := readLines(t, r, 4)
	if hist[0] != "[log] old" || hist[2] != "[error] both" || !strings.HasPrefix(hist[3], "[fetch] GET https://w.example/both -> 200 ok") {
		t.Fatalf("history = %q", hist)
	}
	e.inject(t, fake.WorkerInvocation{Level: "warn", Message: []any{"live", 1}, Request: &fake.InvocationRequest{URL: "https://w.example/live"}})
	live := readLines(t, r, 2)
	// The overlap was dropped: the next lines are the live invocation's.
	if live[0] != "[warn] live 1" || live[1] != "[fetch] GET https://w.example/live -> 200 ok" {
		t.Fatalf("live = %q (the overlapping invocation must not repeat)", live)
	}
	ss := e.sessions(t)
	if len(ss) != 1 || ss[0].Connections != 1 || ss[0].Deleted {
		t.Fatalf("sessions while following = %+v", ss)
	}

	cancel()
	eventually(t, "the tail deleted with no connections", func() bool {
		for _, s := range e.sessions(t) {
			if !s.Deleted || s.Connections != 0 {
				return false
			}
		}
		return true
	})
	// The API lists no tail any more (0075).
	resp, err := e.target.Client.Do(context.Background(), cfclient.Request{Method: http.MethodGet,
		Path: "/accounts/" + e.acct + "/workers/scripts/" + fakeScript + "/tails"})
	if err != nil || strings.TrimSpace(string(resp.Result)) != "[]" {
		t.Errorf("tails after the follow: %v %s", err, resp.Result)
	}
	// The capability URL never reached the logs.
	for _, s := range e.sessions(t) {
		if i := strings.LastIndex(s.URL, "/"); i < 0 || strings.Contains(logs(), s.URL[i+1:]) {
			t.Errorf("the tail URL %q is in the logs", s.URL)
		}
	}
}

func TestFakeFollowReconnect(t *testing.T) {
	e := newFakeEnv(t)
	st := NewStreamer(NewSource(Limits{}, withTuning(fastTuning)), NewFormatter(), Limits{}, time.Now)
	rc, err := st.Logs(context.Background(), e.target, Options{Follow: true, TailLines: i64(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	r := bufio.NewReader(rc)
	eventually(t, "a tail connection", func() bool { ss := e.sessions(t); return len(ss) == 1 && ss[0].Connections == 1 })
	n, err := e.ctl.DisconnectTails(context.Background(), e.acct, fakeScript)
	if err != nil || n != 1 {
		t.Fatalf("disconnect: %d %v", n, err)
	}
	notice := readLines(t, r, 1)[0]
	if !strings.HasPrefix(notice, "[notice] flare-operator: tail reconnected; events between ") {
		t.Fatalf("line = %q, want the gap notice", notice)
	}
	ss := e.sessions(t)
	if len(ss) != 2 || !ss[0].Deleted || ss[0].Connections != 0 || ss[1].Deleted || ss[1].Connections != 1 {
		t.Fatalf("sessions after the reconnect = %+v", ss)
	}
	e.inject(t, fake.WorkerInvocation{Message: "after"})
	if l := readLines(t, r, 1)[0]; l != "[log] after" {
		t.Errorf("line = %q", l)
	}
}

// TestFakeIngestionLag: with a 2 s lag on the emulated clock, an event is not in the history
// until the clock passes its visibility time; the tail delivers it at once.
func TestFakeIngestionLag(t *testing.T) {
	e := newFakeEnv(t)
	if err := e.ctl.SetLogIngestionLag(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	src := NewSource(Limits{}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), e.target)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := e.inject(t, fake.WorkerInvocation{Message: "lagging"})
	if res.TailFrames != 1 {
		t.Fatalf("tail frames = %d", res.TailFrames)
	}
	if ev := recvEvent(t, s); ev.Kind != KindLog || ev.Message != "lagging" {
		t.Fatalf("tail event = %+v", ev)
	}
	query := func() int {
		r, err := src.Query(context.Background(), e.target, QueryOptions{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Events)
	}
	if n := query(); n != 0 {
		t.Fatalf("%d events visible before the lag passed", n)
	}
	e.fs.Clock.Advance(3 * time.Second)
	if n := query(); n != 2 {
		t.Fatalf("%d events after the lag, want 2", n)
	}
}
