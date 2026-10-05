package workerlogs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// flap closes every tail connection with an abnormal code as soon as it opens.
func flap(t *testing.T, f *fakeCF) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case c := <-f.connCh:
				c.closeWith(websocket.CloseInternalServerErr)
			case <-done:
				return
			}
		}
	}()
}

func waitEnd(t *testing.T, s Stream) error {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return s.Err()
			}
		case <-deadline:
			t.Fatal("stream did not end")
		}
	}
}

// A tail that accepts and then drops at once climbs the back-off schedule instead of
// restarting it on every open, and is given up after the last step.
func TestRegressFlappingTailGivesUp(t *testing.T) {
	f := newFakeCF(t)
	flap(t, f)
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		tt.backoff = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	}))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	err = waitEnd(t, s)
	if err == nil || !strings.Contains(err.Error(), "3 reconnect attempts failed") {
		t.Fatalf("Err = %v", err)
	}
	if n := f.createCount(); n != 4 {
		t.Errorf("%d tails created, want 4 (1 + 3 reconnects)", n)
	}
	eventually(t, "every tail deleted", func() bool { return len(f.deleted()) == 4 })
}

// Even when each connection counts as stable, reconnects are capped per window.
func TestRegressReconnectWindowCap(t *testing.T) {
	f := newFakeCF(t)
	flap(t, f)
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		tt.backoff = []time.Duration{5 * time.Millisecond}
		tt.stableAfter = 0
		tt.maxReconnects = 3
		tt.reconnectWindow = time.Minute
	}))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	err = waitEnd(t, s)
	if err == nil || !strings.Contains(err.Error(), "keeps failing (3 reconnects within 1m0s)") {
		t.Fatalf("Err = %v", err)
	}
	if n := f.createCount(); n != 4 {
		t.Errorf("%d tails created, want 4", n)
	}
	eventually(t, "every tail deleted", func() bool { return len(f.deleted()) == 4 })
}

// Giving up ends `kubectl logs -f` with the [notice] line.
func TestRegressGiveUpNotice(t *testing.T) {
	f := newFakeCF(t)
	flap(t, f)
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		tt.backoff = []time.Duration{5 * time.Millisecond}
	}))
	st := NewStreamer(src, NewFormatter(), Limits{}, time.Now)
	rc, err := st.Logs(context.Background(), testTarget(newTestClient(t, f.srv.URL)), Options{Follow: true, TailLines: i64(0)})
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Split(strings.TrimSuffix(readAll(t, rc), "\n"), "\n")
	last := out[len(out)-1]
	if !strings.HasPrefix(last, "[notice] flare-operator: the live stream ended: workerlogs: the tail of flare-spike-logs-1 was lost and 1 reconnect attempts failed") {
		t.Errorf("output = %q", out)
	}
}

// Cancelling the request alone (the reader is never read or closed) releases the
// subscription and the follower slot.
func TestRegressCancelWithoutClose(t *testing.T) {
	src := &fakeSource{}
	st := newTestStreamer(src, Limits{MaxFollowers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := st.Logs(ctx, Target{}, Options{Follow: true, TailLines: i64(0)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		src.stream.ch <- Event{Time: t0, Kind: KindLog, Message: "unread"}
	}
	time.Sleep(20 * time.Millisecond) // the pump is now blocked writing to the pipe
	cancel()
	select {
	case <-src.stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription not closed after the context was cancelled")
	}
	eventually(t, "the follower slot to be released", func() bool {
		rc, err := st.Logs(context.Background(), Target{}, Options{Follow: true, TailLines: i64(0)})
		if err != nil {
			return false
		}
		_ = rc.Close()
		return true
	})
}

// A panic in the follower ends the stream with an error, closes the socket and deletes the tail.
func TestRegressPanicEndsFollow(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		fastTuning(tt)
		tt.frameHook = func(b []byte) {
			if strings.Contains(string(b), "PANIC") {
				panic("boom")
			}
		}
	}))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	c := f.nextConn(t)
	c.send(t, `{"PANIC":true}`)
	err = waitEnd(t, s)
	if err == nil || !strings.Contains(err.Error(), "internal error following the tail") {
		t.Fatalf("Err = %v", err)
	}
	waitClosed(t, c)
	eventually(t, "tail DELETE", func() bool { return len(f.deleted()) == 1 && f.deleted()[0] == c.id })
}

// Frames the old socket delivers during a planned replacement are not repeated when the new
// tail receives the same events.
func TestRegressExpiryReplacementDedup(t *testing.T) {
	f := newFakeCF(t)
	f.with(func() { f.expiresIn = time.Hour })
	src := NewSource(Limits{}, withTuning(func(tt *tailTuning) {
		fastTuning(tt)
		tt.expiryMargin = time.Hour + time.Minute
		tt.minLifetime = 200 * time.Millisecond
	}))
	s, err := src.Follow(context.Background(), testTarget(newTestClient(t, f.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c1 := f.nextConn(t)
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	f.with(func() { f.createGate, f.createStarted = gate, started })
	select {
	case <-started: // the replacement's create is held: the old socket is not being read
	case <-time.After(5 * time.Second):
		t.Fatal("no replacement")
	}
	c1.send(t, frameRPC)
	time.Sleep(50 * time.Millisecond)
	f.with(func() { f.createGate = nil })
	close(gate)
	c2 := f.nextConn(t)
	c2.send(t, frameRPC) // the same event, seen by both tails
	c2.send(t, frameQueue)
	if ev := recvEvent(t, s); ev.Invocation == nil || ev.Invocation.Type != "rpc" {
		t.Fatalf("first = %+v", ev)
	}
	if ev := recvEvent(t, s); ev.Invocation == nil || ev.Invocation.Type != "queue" {
		t.Fatalf("second = %+v, want the queue event (the duplicate rpc dropped)", ev)
	}
}

// A slow follower loses events with a notice instead of growing its buffer.
func TestRegressSlowFollowerDropNotice(t *testing.T) {
	sub := &subscription{ch: make(chan Event, 2), done: make(chan struct{})}
	for i := range 5 {
		sub.offerLocked(Event{Kind: KindLog, Message: string(rune('a' + i))})
	}
	if len(sub.ch) != 2 || sub.dropped != 3 {
		t.Fatalf("buffered %d dropped %d", len(sub.ch), sub.dropped)
	}
	<-sub.ch
	<-sub.ch
	sub.offerLocked(Event{Kind: KindLog, Message: "f"})
	if n := <-sub.ch; n.Kind != KindNotice || n.Message != "flare-operator: 3 events dropped because the reader is too slow" {
		t.Errorf("notice = %+v", n)
	}
	if ev := <-sub.ch; ev.Message != "f" {
		t.Errorf("event = %+v", ev)
	}
	if subscriberBuffer > 1024 || frameBuffer > 4 {
		t.Errorf("buffers %d/%d exceed the bounds", subscriberBuffer, frameBuffer)
	}
}
