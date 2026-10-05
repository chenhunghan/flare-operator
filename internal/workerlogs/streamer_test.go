package workerlogs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource records calls; Follow returns a stream fed by the test.
type fakeSource struct {
	mu        sync.Mutex
	calls     []string
	queries   []QueryOptions
	events    []Event
	queryErr  error
	followErr error
	stream    *fakeStream
}

func (f *fakeSource) Query(_ context.Context, _ Target, q QueryOptions) (QueryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "query")
	f.queries = append(f.queries, q)
	if f.queryErr != nil {
		return QueryResult{}, f.queryErr
	}
	evs := f.events
	if q.MaxEvents > 0 && len(evs) > q.MaxEvents {
		evs = evs[len(evs)-q.MaxEvents:]
	}
	return QueryResult{Events: evs}, nil
}

func (f *fakeSource) Follow(context.Context, Target) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "follow")
	if f.followErr != nil {
		return nil, f.followErr
	}
	f.stream = &fakeStream{ch: make(chan Event, 16), closed: make(chan struct{})}
	return f.stream, nil
}

func (f *fakeSource) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

type fakeStream struct {
	ch     chan Event
	once   sync.Once
	closed chan struct{}
	err    error
}

func (s *fakeStream) Events() <-chan Event { return s.ch }
func (s *fakeStream) Err() error           { return s.err }
func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func i64(v int64) *int64 { return &v }

var t0 = time.Date(2026, 9, 29, 6, 58, 39, 291e6, time.UTC)

// history: two invocations, the second with a two-line exception (5 lines in all).
func historyEvents() []Event {
	return []Event{
		{Time: t0, Seq: "1", Kind: KindLog, Message: "first"},
		{Time: t0, Seq: "2", Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://h/a", Status: 200, Outcome: "ok", RayID: "r1"}},
		{Time: t0.Add(time.Second), Seq: "3", Kind: KindException, Exception: &Exception{Name: "Error", Message: "boom", Stack: "    at x"}},
		{Time: t0.Add(time.Second), Seq: "4", Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://h/b", Status: 500, Outcome: "exception", RayID: "r2"}},
	}
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestStreamer(src Source, l Limits) Streamer {
	return NewStreamer(src, NewFormatter(), l, func() time.Time { return t0.Add(time.Hour) })
}

func TestLogsHistory(t *testing.T) {
	cases := []struct {
		name      string
		opts      Options
		want      string
		wantMax   int
		wantQuery bool
	}{
		{"all", Options{}, "[log] first\n[fetch] GET https://h/a -> 200 ok ray=r1\n[exception] Error: boom\n    at x\n[fetch] GET https://h/b -> 500 exception ray=r2\n", DefaultMaxEvents, true},
		{"tail 2 lines", Options{TailLines: i64(2)}, "    at x\n[fetch] GET https://h/b -> 500 exception ray=r2\n", 2, true},
		{"tail 3 lines", Options{TailLines: i64(3)}, "[exception] Error: boom\n    at x\n[fetch] GET https://h/b -> 500 exception ray=r2\n", 3, true},
		{"tail more than all", Options{TailLines: i64(1 << 40)}, "[log] first\n[fetch] GET https://h/a -> 200 ok ray=r1\n[exception] Error: boom\n    at x\n[fetch] GET https://h/b -> 500 exception ray=r2\n", DefaultMaxEvents, true},
		{"tail 0", Options{TailLines: i64(0)}, "", 0, false},
		{"timestamps", Options{TailLines: i64(1), Timestamps: true}, "2026-09-29T06:58:40.291000000Z [fetch] GET https://h/b -> 500 exception ray=r2\n", 1, true},
		{"limit bytes", Options{LimitBytes: i64(15)}, "[log] first\n[fe", DefaultMaxEvents, true},
		{"limit bytes on a line end", Options{LimitBytes: i64(12)}, "[log] first\n", DefaultMaxEvents, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{events: historyEvents()}
			rc, err := newTestStreamer(src, Limits{}).Logs(context.Background(), Target{}, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, rc); got != tc.want {
				t.Errorf("got\n%q\nwant\n%q", got, tc.want)
			}
			if !tc.wantQuery {
				if len(src.queries) != 0 {
					t.Errorf("queried with --tail=0")
				}
				return
			}
			if len(src.queries) != 1 {
				t.Fatalf("%d queries", len(src.queries))
			}
			q := src.queries[0]
			if q.MaxEvents != tc.wantMax {
				t.Errorf("MaxEvents = %d, want %d", q.MaxEvents, tc.wantMax)
			}
			now := t0.Add(time.Hour)
			if !q.To.Equal(now) || !q.From.Equal(now.Add(-DefaultWindow)) {
				t.Errorf("window = %v..%v", q.From, q.To)
			}
		})
	}
}

func TestLogsSince(t *testing.T) {
	src := &fakeSource{}
	since := t0.Add(-10 * time.Minute)
	rc, err := newTestStreamer(src, Limits{DefaultWindow: time.Hour}).Logs(context.Background(), Target{}, Options{SinceTime: &since})
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, rc)
	if !src.queries[0].From.Equal(since) {
		t.Errorf("From = %v, want %v", src.queries[0].From, since)
	}
}

func TestLogsErrors(t *testing.T) {
	if _, err := newTestStreamer(&fakeSource{}, Limits{}).Logs(context.Background(), Target{}, Options{Previous: true}); !errors.Is(err, ErrPreviousUnsupported) {
		t.Errorf("previous: %v", err)
	}
	boom := errors.New("403")
	src := &fakeSource{queryErr: boom}
	st := newTestStreamer(src, Limits{MaxFollowers: 1})
	if _, err := st.Logs(context.Background(), Target{}, Options{Follow: true}); !errors.Is(err, boom) {
		t.Fatalf("query error: %v", err)
	}
	// The follow opened for it is closed and its slot is free again.
	select {
	case <-src.stream.closed:
	case <-time.After(time.Second):
		t.Fatal("stream not closed after a failed history query")
	}
	src.queryErr = nil
	rc, err := st.Logs(context.Background(), Target{}, Options{Follow: true})
	if err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	defer rc.Close()

	fsrc := &fakeSource{followErr: ErrInsecureTailURL}
	if _, err := newTestStreamer(fsrc, Limits{}).Logs(context.Background(), Target{}, Options{Follow: true}); !errors.Is(err, ErrInsecureTailURL) {
		t.Errorf("follow error: %v", err)
	}
	if fsrc.callLog() != "follow" {
		t.Errorf("calls = %s, want only follow", fsrc.callLog())
	}
}

func TestLogsTooManyFollowers(t *testing.T) {
	src := &fakeSource{}
	st := newTestStreamer(src, Limits{MaxFollowers: 1})
	rc, err := st.Logs(context.Background(), Target{}, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Logs(context.Background(), Target{}, Options{Follow: true}); !errors.Is(err, ErrTooManyFollowers) {
		t.Fatalf("second follow: %v", err)
	}
	// A non-follow request needs no slot.
	rc2, err := st.Logs(context.Background(), Target{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, rc2)
	_ = rc.Close()
	eventually(t, "the slot to be released", func() bool {
		rc3, err := st.Logs(context.Background(), Target{}, Options{Follow: true})
		if err != nil {
			return false
		}
		_ = rc3.Close()
		return true
	})
}

func TestLogsFollow(t *testing.T) {
	src := &fakeSource{events: historyEvents()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := newTestStreamer(src, Limits{}).Logs(ctx, Target{}, Options{Follow: true, TailLines: i64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if src.callLog() != "follow,query" {
		t.Errorf("calls = %s, want the tail opened before the history query", src.callLog())
	}
	r := bufio.NewReader(rc)
	readLine := func() string {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := readLine() + readLine(); l != "    at x\n[fetch] GET https://h/b -> 500 exception ray=r2\n" {
		t.Errorf("history lines = %q", l)
	}
	// Overlap: the tail repeats the history's last invocation (its exception header was read but
	// cut by --tail; the summary comes without a ray ID or status) and then has new events.
	src.stream.ch <- Event{Time: t0.Add(time.Second), Kind: KindException, Exception: &Exception{Name: "Error", Message: "boom", Stack: "    at x"}}
	src.stream.ch <- Event{Time: t0.Add(time.Second), Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://h/b", Outcome: "exception"}}
	src.stream.ch <- Event{Time: t0.Add(2 * time.Second), Kind: KindLog, Level: "warn", Message: "live"}
	if l := readLine(); l != "[warn] live\n" {
		t.Errorf("live line = %q (duplicates of the history must be dropped)", l)
	}
	// Closing the reader ends the follow.
	_ = rc.Close()
	select {
	case <-src.stream.closed:
	case <-time.After(time.Second):
		t.Fatal("stream not closed when the reader closed")
	}
}

func TestLogsFollowTailZeroAndStreamEnd(t *testing.T) {
	src := &fakeSource{events: historyEvents()}
	rc, err := newTestStreamer(src, Limits{}).Logs(context.Background(), Target{}, Options{Follow: true, TailLines: i64(0), Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	if src.callLog() != "follow" {
		t.Errorf("calls = %s, want no history query for --tail=0", src.callLog())
	}
	src.stream.ch <- Event{Time: t0, Kind: KindLog, Message: "new"}
	src.stream.err = errors.New("the tail was closed by Cloudflare")
	close(src.stream.ch)
	got := readAll(t, rc)
	want := "2026-09-29T06:58:39.291000000Z [log] new\n" +
		"2026-09-29T07:58:39.291000000Z [notice] flare-operator: the live stream ended: the tail was closed by Cloudflare\n"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestLogsContextCancelEndsFollow(t *testing.T) {
	src := &fakeSource{}
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := newTestStreamer(src, Limits{}).Logs(ctx, Target{}, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if len(b) != 0 || (err != nil && !errors.Is(err, context.Canceled)) {
		t.Errorf("got %q, %v", b, err)
	}
	select {
	case <-src.stream.closed:
	case <-time.After(time.Second):
		t.Fatal("stream not closed on cancel")
	}
}

// TestLogsEndToEnd joins the real Source and Streamer against the fake API: the history of
// recording 0054, then live trace-v1 frames; closing the reader closes the socket and deletes
// the tail, and the tail URL appears nowhere in the output.
func TestLogsEndToEnd(t *testing.T) {
	f := newFakeCF(t)
	f.setQuery(replay(loadRecording(t, "0054")))
	src := NewSource(Limits{}, withTuning(fastTuning))
	st := NewStreamer(src, NewFormatter(), Limits{}, func() time.Time { return time.UnixMilli(1790665168007) })
	rc, err := st.Logs(context.Background(), testTarget(newTestClient(t, f.srv.URL)), Options{Follow: true, TailLines: i64(2)})
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	r := bufio.NewReader(rc)
	var lines []string
	for range 2 {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	c.send(t, frameFetch)
	for range 6 {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	out := strings.Join(lines, "")
	want := "[error] FSLMARK7c57e9c4 err path=/e counter=3\n" +
		"[fetch] GET https://flare-spike-logs-1.example-subdomain.workers.dev/e?phase=1 -> 200 ok (wall 0ms, cpu 0ms) ray=a42919d6cd448b3c\n" +
		"[log] info path=/throw {\"n\":1}\n[error] err\nsecond line\n[exception] Error: boom\n    at Object.fetch (worker.mjs:10:13)\n" +
		"[fetch] GET https://w.example/throw?phase=1 -> 500 exception\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	_ = rc.Close()
	waitClosed(t, c)
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 && f.deleted()[0] == c.id })
}
