package testenv

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconcile tracking. Tests that assert that something did NOT happen (no Cloudflare write, no
// re-verify, no status write) must first know that the controller actually handled the event
// in question; sleeping for a while proves nothing on a loaded machine. Every manager started
// by StartManager tracks its controllers' reconciles from controller-runtime's own log lines
// (the controller's logger carries controller, namespace, name and reconcileID; "Reconciling"
// starts a reconcile, and "Reconcile successful", "Reconcile done, ..." or "Reconciler error"
// ends it), without any hook in the controllers.
//
// Usage: mark := m.Mark(); <the action>; m.WaitReconciled(t, controller, key, mark, 1, timeout).
// A reconcile that started after the mark and finished is one that started after the action's
// write returned. It normally reads a cache that shows the action; one that raced the cache
// would be followed by the reconcile the action's event enqueues, so a test that needs that
// certainty waits for two (n=2) where the object is guaranteed to be reconciled twice.

// reconcileRecord is one reconcile of an object.
type reconcileRecord struct {
	controller, namespace, name string
	start, end                  int64 // sequence numbers; end 0 while running
	failed                      bool  // ended with "Reconciler error" (rate-limited retry)
}

// reconcileTracker records the reconciles of one manager.
type reconcileTracker struct {
	mu   sync.Mutex
	cond *sync.Cond
	seq  int64
	byID map[string]*reconcileRecord
	all  []*reconcileRecord
}

func newReconcileTracker() *reconcileTracker {
	r := &reconcileTracker{byID: map[string]*reconcileRecord{}}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *reconcileTracker) mark() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

func (r *reconcileTracker) started(id, controller, namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	rec := &reconcileRecord{controller: controller, namespace: namespace, name: name, start: r.seq}
	r.byID[id] = rec
	r.all = append(r.all, rec)
}

func (r *reconcileTracker) finished(id string, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byID[id]
	if rec == nil {
		return
	}
	delete(r.byID, id)
	r.seq++
	rec.end, rec.failed = r.seq, failed
	r.cond.Broadcast()
}

func (r *reconcileRecord) matches(controller string, key client.ObjectKey) bool {
	return r.namespace == key.Namespace && r.name == key.Name && (controller == "" || r.controller == controller)
}

// Failures returns how many reconciles of the object key by the named controller ("" = any)
// started after mark and ended with an error (the controller retries those on its
// rate-limited backoff).
func (m *Manager) Failures(controller string, key client.ObjectKey, mark int64) int {
	r := m.tracker
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.all {
		if rec.matches(controller, key) && rec.start > mark && rec.end != 0 && rec.failed {
			n++
		}
	}
	return n
}

// state counts, for the object, the reconciles that started after mark and finished, and those
// running now (any start).
func (r *reconcileTracker) state(controller string, key client.ObjectKey, mark int64) (done, running int) {
	for _, rec := range r.all {
		if !rec.matches(controller, key) {
			continue
		}
		switch {
		case rec.end == 0:
			running++
		case rec.start > mark:
			done++
		}
	}
	return done, running
}

// Mark returns the current position in the manager's reconcile record (see WaitReconciled).
func (m *Manager) Mark() int64 { return m.tracker.mark() }

// WaitReconciled waits (at most timeout) until at least n reconciles of the object key by the
// named controller ("" = any controller of this manager) started after mark and finished, and
// none of it is running. It fails t otherwise. It returns a new mark.
func (m *Manager) WaitReconciled(t testing.TB, controller string, key client.ObjectKey, mark int64, n int, timeout time.Duration) int64 {
	t.Helper()
	r := m.tracker
	deadline := time.Now().Add(timeout)
	// Wake the waiter at the deadline too (sync.Cond has no timed wait).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-stop:
		case <-time.After(timeout):
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
		}
	}()
	r.mu.Lock()
	for {
		done, running := r.state(controller, key, mark)
		if done >= n && running == 0 {
			r.mu.Unlock()
			return m.Mark()
		}
		if !time.Now().Before(deadline) {
			r.mu.Unlock()
			what := controller
			if what == "" {
				what = "any controller"
			}
			t.Fatalf("%s did not finish %d reconcile(s) of %s started after the mark within %v (finished %d, running %d)",
				what, n, key, timeout, done, running)
			return 0
		}
		r.cond.Wait()
	}
}

// trackingSink is a logr.LogSink that feeds a reconcileTracker and forwards everything to the
// sink it wraps (at that sink's own verbosity).
type trackingSink struct {
	under   logr.LogSink
	tracker *reconcileTracker
	values  []any
}

var _ logr.LogSink = &trackingSink{}

func (s *trackingSink) Init(info logr.RuntimeInfo) {
	if s.under != nil {
		s.under.Init(info)
	}
}

// Enabled lets controller-runtime's V(5) reconcile lines through to the tracker.
func (s *trackingSink) Enabled(level int) bool { return level <= 5 || s.underEnabled(level) }

func (s *trackingSink) underEnabled(level int) bool { return s.under != nil && s.under.Enabled(level) }

func (s *trackingSink) Info(level int, msg string, kv ...any) {
	s.observe(msg, kv)
	if s.underEnabled(level) {
		s.under.Info(level, msg, kv...)
	}
}

func (s *trackingSink) Error(err error, msg string, kv ...any) {
	s.observe(msg, kv)
	if s.under != nil {
		s.under.Error(err, msg, kv...)
	}
}

func (s *trackingSink) WithValues(kv ...any) logr.LogSink {
	c := *s
	c.values = append(append([]any(nil), s.values...), kv...)
	if s.under != nil {
		c.under = s.under.WithValues(kv...)
	}
	return &c
}

func (s *trackingSink) WithName(name string) logr.LogSink {
	c := *s
	if s.under != nil {
		c.under = s.under.WithName(name)
	}
	return &c
}

func (s *trackingSink) observe(msg string, kv []any) {
	start := msg == "Reconciling"
	end := msg == "Reconcile successful" || strings.HasPrefix(msg, "Reconcile done") || msg == "Reconciler error"
	if !start && !end {
		return
	}
	get := func(k string) string {
		all := append(append([]any(nil), s.values...), kv...)
		v := ""
		for i := 0; i+1 < len(all); i += 2 {
			if key, ok := all[i].(string); ok && key == k {
				v = fmt.Sprint(all[i+1])
			}
		}
		return v
	}
	id := get("reconcileID")
	if id == "" {
		return
	}
	if start {
		s.tracker.started(id, get("controller"), get("namespace"), get("name"))
		return
	}
	s.tracker.finished(id, msg == "Reconciler error")
}
