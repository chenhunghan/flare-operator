package workerlogs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/gorilla/websocket"
)

// fastTuning keeps reconnects and expiry in test time.
func fastTuning(tt *tailTuning) {
	tt.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	tt.cleanupTimeout = 2 * time.Second
}

// captureLogs returns a logger that records every line at every verbosity.
func captureLogs() (logr.Logger, func() string) {
	var mu sync.Mutex
	var b strings.Builder
	l := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		b.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
	return l, func() string { mu.Lock(); defer mu.Unlock(); return b.String() }
}

func TestFollowStream(t *testing.T) {
	f := newFakeCF(t)
	log, logs := captureLogs()
	src := NewSource(Limits{}, WithLogger(log), withTuning(fastTuning))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := src.Follow(ctx, testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	if c.debugMsg != `{"debug":false}` {
		t.Errorf("first message = %q", c.debugMsg)
	}
	c.send(t, frameFetch)
	var got []EventKind
	var last string
	for range 4 {
		ev := recvEvent(t, s)
		got = append(got, ev.Kind)
		if ev.Seq <= last {
			t.Errorf("Seq %q after %q", ev.Seq, last)
		}
		last = ev.Seq
	}
	want := []EventKind{KindLog, KindLog, KindException, KindInvocation}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("kinds = %v", got)
	}
	// A malformed frame is skipped and the stream goes on.
	c.send(t, `{not json`)
	c.send(t, frameCron)
	if ev := recvEvent(t, s); ev.Invocation == nil || ev.Invocation.Cron != "*/5 * * * *" || ev.Seq <= last {
		t.Errorf("event = %+v", ev)
	}

	// Cancelling the follower's context closes the socket and deletes the tail.
	cancel()
	waitClosed(t, c)
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 && f.deleted()[0] == c.id })
	eventually(t, "stream closed", func() bool {
		select {
		case _, ok := <-s.Events():
			return !ok
		default:
			return false
		}
	})
	if s.Err() != nil {
		t.Errorf("Err after cancel = %v, want nil", s.Err())
	}
	if out := logs(); strings.Contains(out, "SECRET") {
		t.Errorf("logs contain the tail URL:\n%s", out)
	}
}

func TestFollowMalformedFrameSkipped(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := f.nextConn(t)
	for _, bad := range []string{"garbage", `[1]`, `{"logs":"x"}`, ""} {
		c.send(t, bad)
	}
	c.send(t, frameQueue)
	ev := recvEvent(t, s)
	if ev.Kind != KindInvocation || ev.Invocation.Queue != "jobs" {
		t.Fatalf("event = %+v", ev)
	}
	if f.createCount() != 1 {
		t.Errorf("malformed frames caused a reconnect (%d tails)", f.createCount())
	}
}

func TestFollowServerNormalClose(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	c.closeWith(websocket.CloseNormalClosure)
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream not closed after a normal close")
	}
	if !errors.Is(s.Err(), errTailClosed) {
		t.Errorf("Err = %v, want errTailClosed", s.Err())
	}
	waitClosed(t, c)
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 })
	if f.createCount() != 1 {
		t.Errorf("%d tails created, want 1 (no reconnect after a normal close)", f.createCount())
	}
	_ = s.Close() // idempotent after the end
	_ = s.Close()
}

func TestFollowReconnectsAfterAbnormalClose(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c1 := f.nextConn(t)
	c1.closeWith(websocket.CloseInternalServerErr)
	c2 := f.nextConn(t)
	ev := recvEvent(t, s)
	if ev.Kind != KindNotice || !strings.Contains(ev.Message, "tail reconnected; events between") {
		t.Fatalf("event = %+v, want the gap notice", ev)
	}
	eventually(t, "old tail deleted", func() bool { d := f.deleted(); return len(d) == 1 && d[0] == c1.id })
	c2.send(t, frameAlarm)
	if ev := recvEvent(t, s); ev.Invocation == nil || ev.Invocation.Type != "alarm" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestFollowMissedPongReconnects(t *testing.T) {
	f := newFakeCF(t)
	f.noPong.Store(true)
	src := NewSource(Limits{TailPingInterval: 30 * time.Millisecond}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c1 := f.nextConn(t)
	f.noPong.Store(false)
	c2 := f.nextConn(t)
	waitClosed(t, c1)
	if ev := recvEvent(t, s); ev.Kind != KindNotice {
		t.Fatalf("event = %+v, want the gap notice", ev)
	}
	// With pongs answered, the new connection stays.
	time.Sleep(150 * time.Millisecond)
	if f.createCount() != 2 {
		t.Errorf("%d tails created, want 2", f.createCount())
	}
	_ = c2
}

func TestFollowGivesUpAfterBackoff(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(fastTuning))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	f.createStatus.Store(500)
	c.closeWith(websocket.CloseGoingAway)
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream not ended")
	}
	if s.Err() == nil || !strings.Contains(s.Err().Error(), "2 reconnect attempts failed") {
		t.Errorf("Err = %v", s.Err())
	}
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 })
}

func TestFollowExpiryReplacesTail(t *testing.T) {
	f := newFakeCF(t)
	f.with(func() { f.expiresIn = time.Hour })
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		fastTuning(tt)
		tt.expiryMargin = time.Hour + time.Minute // already "near" expiry
		tt.minLifetime = 50 * time.Millisecond
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := src.Follow(ctx, testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	c1 := f.nextConn(t)
	c2 := f.nextConn(t)
	waitClosed(t, c1)
	eventually(t, "old tail deleted", func() bool { d := f.deleted(); return len(d) >= 1 && d[0] == c1.id })
	c2.send(t, frameRPC)
	ev := recvEvent(t, s)
	if ev.Kind != KindInvocation {
		t.Fatalf("event = %+v (a planned replacement has no gap notice)", ev)
	}
}

func TestFollowSharedTail(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(fastTuning))
	tgt := testTarget(newTestClient(t, f.srv.URL))
	s1, err := src.Follow(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	s2, err := src.Follow(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	if f.createCount() != 1 {
		t.Fatalf("%d tails for two followers, want 1", f.createCount())
	}
	c.send(t, frameEmail)
	for _, s := range []Stream{s1, s2} {
		if ev := recvEvent(t, s); ev.Invocation == nil || ev.Invocation.Type != "email" {
			t.Fatalf("event = %+v", ev)
		}
	}
	_ = s1.Close()
	time.Sleep(50 * time.Millisecond)
	if len(f.deleted()) != 0 {
		t.Fatal("tail deleted while a follower remains")
	}
	c.send(t, frameEmail)
	recvEvent(t, s2)
	_ = s2.Close()
	waitClosed(t, c)
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 })

	// A new follower after the last left gets a new tail.
	s3, err := src.Follow(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	f.nextConn(t)
	if f.createCount() != 2 {
		t.Errorf("%d tails, want 2", f.createCount())
	}
}

func TestFollowCreateFails(t *testing.T) {
	f := newFakeCF(t)
	f.createStatus.Store(403)
	src := NewSource(Limits{})
	_, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

// TestFollowURLRedaction: a failing dial and an insecure URL never put the capability URL in
// errors or logs, and the created tail is deleted.
func TestFollowURLRedaction(t *testing.T) {
	t.Run("dial rejected", func(t *testing.T) {
		f := newFakeCF(t)
		f.rejectUpgrade.Store(true)
		log, logs := captureLogs()
		src := NewSource(Limits{}, WithLogger(log))
		_, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/tail/") {
			t.Errorf("error leaks the tail URL: %v", err)
		}
		if !strings.Contains(err.Error(), "ws://127.0.0.1") || !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("error = %v, want scheme://host and the status", err)
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("error wraps %v", errors.Unwrap(err))
		}
		eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 })
		if strings.Contains(logs(), "SECRET") {
			t.Errorf("logs leak the tail URL:\n%s", logs())
		}
	})
	t.Run("insecure URL", func(t *testing.T) {
		f := newFakeCF(t)
		src := NewSource(Limits{})
		tgt := testTarget(newTestClient(t, f.srv.URL))
		tgt.AllowInsecureTail = false
		_, err := src.Follow(context.Background(), tgt)
		if !errors.Is(err, ErrInsecureTailURL) {
			t.Fatalf("err = %v", err)
		}
		eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 })
	})
	t.Run("not a websocket scheme", func(t *testing.T) {
		f := newFakeCF(t)
		f.with(func() { f.tailScheme = "http" })
		src := NewSource(Limits{})
		_, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
		if !errors.Is(err, ErrInsecureTailURL) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scrub", func(t *testing.T) {
		u, err := url.Parse("wss://tail.example/abc%2Fdef/SECRET?k=SECRET2")
		if err != nil {
			t.Fatal(err)
		}
		msg := scrubURL(`dial "wss://tail.example/abc%2Fdef/SECRET?k=SECRET2": oops /abc/def/SECRET k=SECRET2`, u)
		if strings.Contains(msg, "SECRET") {
			t.Errorf("scrubbed = %q", msg)
		}
	})
}
