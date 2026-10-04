package workerlogs

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

func ptrF(v float64) *float64 { return &v }

func TestFormat(t *testing.T) {
	at := time.Date(2026, 9, 29, 6, 58, 42, 368e6, time.UTC)
	cases := []struct {
		name string
		ev   Event
		want []string
	}{
		{"log default level", Event{Kind: KindLog, Message: "hello"}, []string{"[log] hello"}},
		{"log level", Event{Kind: KindLog, Level: "error", Message: "boom"}, []string{"[error] boom"}},
		{"log multi-line", Event{Kind: KindLog, Level: "info", Message: "a\nb\r\nc\n"}, []string{"[info] a", "b", "c"}},
		{"log empty", Event{Kind: KindLog}, []string{"[log] "}},
		{"log truncated", Event{Kind: KindLog, Message: "x\ny", Truncated: true}, []string{"[log] x [truncated]", "y"}},
		{"exception with stack", Event{Kind: KindException, Exception: &Exception{Name: "Error", Message: "boom", Stack: "    at a (w.mjs:1:1)\n    at b (w.mjs:2:2)"}},
			[]string{"[exception] Error: boom", "    at a (w.mjs:1:1)", "    at b (w.mjs:2:2)"}},
		{"exception no name", Event{Kind: KindException, Exception: &Exception{Message: "boom"}}, []string{"[exception] boom"}},
		{"exception nil", Event{Kind: KindException}, []string{"[exception]"}},
		{"fetch full", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://host/path?q", Status: 200, Outcome: "ok", WallTimeMs: ptrF(1), CPUTimeMs: ptrF(0), RayID: "a42919d6cd448b3c"}},
			[]string{"[fetch] GET https://host/path?q -> 200 ok (wall 1ms, cpu 0ms) ray=a42919d6cd448b3c"}},
		{"fetch from tail", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "POST", URL: "https://h/", Outcome: "exception"}},
			[]string{"[fetch] POST https://h/ -> exception"}},
		{"fetch status only", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://h/", Status: 404, WallTimeMs: ptrF(1.5)}},
			[]string{"[fetch] GET https://h/ -> 404 (wall 1.5ms)"}},
		{"fetch nothing known", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "fetch"}}, []string{"[fetch] -> unknown"}},
		{"scheduled", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "scheduled", Cron: "*/5 * * * *", Outcome: "ok"}}, []string{`[scheduled] "*/5 * * * *" -> ok`}},
		{"alarm", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "alarm", Outcome: "ok"}}, []string{"[alarm] -> ok"}},
		{"queue", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "queue", Queue: "jobs", BatchSize: 3, Outcome: "ok"}}, []string{"[queue] jobs (3 messages) -> ok"}},
		{"queue one", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "queue", Queue: "jobs", BatchSize: 1, Outcome: "ok"}}, []string{"[queue] jobs (1 message) -> ok"}},
		{"email", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "email", MailFrom: "a@x", RcptTo: "b@y", RawSize: 123, Outcome: "ok"}}, []string{"[email] from:a@x to:b@y size:123 -> ok"}},
		{"rpc", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "rpc", Entrypoint: "Api", RPCMethod: "get", Outcome: "ok"}}, []string{"[rpc] Api.get -> ok"}},
		{"rpc no entrypoint", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "rpc", RPCMethod: "get", Outcome: "ok"}}, []string{"[rpc] get -> ok"}},
		{"tail", Event{Kind: KindInvocation, Message: "a,b", Invocation: &Invocation{Type: "tail", Outcome: "ok"}}, []string{"[tail] tailing a,b -> ok"}},
		{"unknown", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "unknown"}}, []string{"[event] -> unknown"}},
		{"invocation nil", Event{Kind: KindInvocation}, []string{"[event] -> unknown"}},
		{"notice", Event{Kind: KindNotice, Message: "Tail is currently in sampling mode"}, []string{"[notice] Tail is currently in sampling mode"}},
		// Terminal injection (design §7.2).
		{"escape C0 and DEL", Event{Kind: KindLog, Message: "a\x1b[31mred\x07\x7f\tb\rc"}, []string{`[log] a\x1b[31mred\x07\x7f` + "\t" + `b\x0dc`}},
		{"escape URL", Event{Kind: KindInvocation, Invocation: &Invocation{Type: "fetch", Method: "GET", URL: "https://h/\x1b]0;pwned\x07", Outcome: "ok"}},
			[]string{`[fetch] GET https://h/\x1b]0;pwned\x07 -> ok`}},
		{"escape bidi, separators and zero-width", Event{Kind: KindLog, Message: "a\u202eb\u2066c\u2069d\u2028e\u2029f\u200bg\u200fh\ufeffi"},
			[]string{`[log] a\u202eb\u2066c\u2069d\u2028e\u2029f\u200bg\u200fh\ufeffi`}},
		{"escape C1 and invalid UTF-8", Event{Kind: KindLog, Message: "a\u009b31m b\xffc 日本"}, []string{`[log] a\u009b31m b\xffc 日本`}},
	}
	f := NewFormatter()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.ev.Time = at
			got := f.Format(tc.ev)
			var texts []string
			for _, l := range got {
				if !l.Time.Equal(at) {
					t.Errorf("line time = %v, want the event's %v", l.Time, at)
				}
				if strings.Contains(l.Text, "\n") {
					t.Errorf("line contains a newline: %q", l.Text)
				}
				texts = append(texts, l.Text)
			}
			if strings.Join(texts, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestEscapeControlCleanIsUnchanged(t *testing.T) {
	for _, s := range []string{"", "plain", "tab\there", "日本語 ✓"} {
		if got := escapeControl(s); got != s {
			t.Errorf("escapeControl(%q) = %q", s, got)
		}
	}
	for c := 0; c < 0x20; c++ {
		if c == '\t' {
			continue
		}
		if got := escapeControl(string(rune(c))); len(got) != 4 || got[:2] != `\x` {
			t.Errorf("escapeControl(%#x) = %q", c, got)
		}
	}
}

// TestFormatRecording0054 renders the events of recording 0054 (log, error, exception with
// stack, fetch summaries) through the Query decoder and the Formatter, with timestamps, and
// compares with testdata/0054.golden.
func TestFormatRecording0054(t *testing.T) {
	rec := loadRecording(t, "0054")
	evs := decodeRecordedEvents(t, rec)
	var b strings.Builder
	w := &lineWriter{w: &b, timestamps: true, remaining: -1}
	f := NewFormatter()
	for _, ev := range evs {
		for _, l := range f.Format(ev) {
			if err := w.write(l); err != nil {
				t.Fatal(err)
			}
		}
	}
	golden(t, "0054.golden", b.String())
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}
