package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"flare.dev/operator/internal/fake"
)

// FakeControl drives a flarefake through its HTTP control API (/_fake/…). It works the same
// for the in-process fake (Env.Control) and for one running elsewhere (e2e: NewFakeControl
// with that fake's root URL, without the /client/v4 suffix).
type FakeControl struct {
	URL  string
	HTTP *http.Client
}

// NewFakeControl returns a FakeControl for the fake at rootURL (e.g. http://127.0.0.1:8787).
func NewFakeControl(rootURL string) *FakeControl {
	return &FakeControl{URL: strings.TrimRight(rootURL, "/"), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (f *FakeControl) call(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.URL+"/_fake/"+p, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("flarefake %s /_fake/%s: %d %s", method, p, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Journal returns every request the fake has seen since the last clear.
func (f *FakeControl) Journal(ctx context.Context) ([]fake.JournalEntry, error) {
	var j []fake.JournalEntry
	return j, f.call(ctx, http.MethodGet, "journal", nil, &j)
}

// ClearJournal empties the journal.
func (f *FakeControl) ClearJournal(ctx context.Context) error {
	return f.call(ctx, http.MethodDelete, "journal", nil, nil)
}

// Reset drops all fake state (resources, journal, faults, tokens → open mode).
func (f *FakeControl) Reset(ctx context.Context) error {
	return f.call(ctx, http.MethodPost, "reset", nil, nil)
}

// AddToken registers a token (strict token mode).
func (f *FakeControl) AddToken(ctx context.Context, t fake.Token) error {
	return f.call(ctx, http.MethodPost, "tokens", t, nil)
}

// InjectFault adds a fault rule.
func (f *FakeControl) InjectFault(ctx context.Context, flt fake.Fault) error {
	return f.call(ctx, http.MethodPost, "faults", flt, nil)
}

// ClearFaults removes all fault rules.
func (f *FakeControl) ClearFaults(ctx context.Context) error {
	return f.call(ctx, http.MethodDelete, "faults", nil, nil)
}

// Journal returns the fake's journal, failing t on error.
func (e *Env) Journal(t testing.TB) []fake.JournalEntry {
	t.Helper()
	j, err := e.Control.Journal(Context(t, 10*time.Second))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	return j
}

// ClearJournal empties the fake's journal, failing t on error.
func (e *Env) ClearJournal(t testing.TB) {
	t.Helper()
	if err := e.Control.ClearJournal(Context(t, 10*time.Second)); err != nil {
		t.Fatalf("clear journal: %v", err)
	}
}

// IsWrite reports whether a journaled request is a write (anything but GET/HEAD/OPTIONS).
func IsWrite(e fake.JournalEntry) bool {
	switch e.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// Writes returns the write requests of j.
func Writes(j []fake.JournalEntry) []fake.JournalEntry {
	return Filter(j, func(e fake.JournalEntry) bool { return IsWrite(e) })
}

// Filter returns the entries of j for which keep is true.
func Filter(j []fake.JournalEntry, keep func(fake.JournalEntry) bool) []fake.JournalEntry {
	var out []fake.JournalEntry
	for _, e := range j {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// ForAccount keeps entries whose path is under /accounts/<id>/ (or is /accounts/<id>).
func ForAccount(j []fake.JournalEntry, accountID string) []fake.JournalEntry {
	p := "/accounts/" + accountID
	return Filter(j, func(e fake.JournalEntry) bool { return e.Path == p || strings.HasPrefix(e.Path, p+"/") })
}

// Count returns how many entries have method (""=any) and a path containing pathSubstr.
func Count(j []fake.JournalEntry, method, pathSubstr string) int {
	return len(Filter(j, func(e fake.JournalEntry) bool {
		return (method == "" || e.Method == method) && strings.Contains(e.Path, pathSubstr)
	}))
}

// CountPath returns how many entries have method (""=any) and exactly path (no query).
func CountPath(j []fake.JournalEntry, method, path string) int {
	return len(Filter(j, func(e fake.JournalEntry) bool {
		return (method == "" || e.Method == method) && e.Path == path
	}))
}

// WaitJournal polls the fake's journal entries after since (a journal length) until ready
// reports true for them, and returns them; it fails t with ready's last message after timeout.
// It is the positive signal tests wait for before asserting that something did not happen
// (e.g. "no writes while the object was re-observed n times").
func (e *Env) WaitJournal(t testing.TB, since int, timeout time.Duration, ready func(j []fake.JournalEntry) (bool, string)) []fake.JournalEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := e.Journal(t)
		if since < len(j) {
			j = j[since:]
		} else {
			j = nil
		}
		ok, msg := ready(j)
		if ok {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal condition not met within %v: %s", timeout, msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Summary renders entries one per line ("PUT /accounts/…/tags 200"), for failure messages.
func Summary(j []fake.JournalEntry) string {
	var b strings.Builder
	for _, e := range j {
		fmt.Fprintf(&b, "%s %s %d\n", e.Method, e.Path, e.Status)
	}
	return b.String()
}
