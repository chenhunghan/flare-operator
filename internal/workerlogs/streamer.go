package workerlogs

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NewStreamer joins a Source and a Formatter into the kubelet log stream.
func NewStreamer(src Source, f Formatter, l Limits, now func() time.Time) Streamer {
	if now == nil {
		now = time.Now
	}
	l = l.withDefaults()
	return &streamer{src: src, f: f, limits: l, now: now, slots: make(chan struct{}, l.MaxFollowers)}
}

type streamer struct {
	src    Source
	f      Formatter
	limits Limits
	now    func() time.Time
	slots  chan struct{} // one per open follow session
}

// Logs implements Streamer. Errors that a caller can map to an HTTP status (the history query's
// *cfclient.APIError, ErrPreviousUnsupported, ErrTooManyFollowers, ErrInsecureTailURL) are
// returned before any byte is written; once the reader is returned, a later failure only ends
// the stream.
func (s *streamer) Logs(ctx context.Context, t Target, opts Options) (io.ReadCloser, error) {
	if opts.Previous {
		return nil, ErrPreviousUnsupported
	}
	ctx, cancel := context.WithCancel(ctx)
	release := func() {}
	var stream Stream
	if opts.Follow {
		select {
		case s.slots <- struct{}{}:
		default:
			cancel()
			return nil, ErrTooManyFollowers
		}
		var once sync.Once
		release = func() { once.Do(func() { <-s.slots }) }
		// The tail is opened before the history is read, so that events between the query and
		// the tail's start are not lost (design §7.3).
		var err error
		if stream, err = s.src.Follow(ctx, t); err != nil {
			release()
			cancel()
			return nil, err
		}
	}
	cleanup := func() {
		if stream != nil {
			_ = stream.Close()
		}
		release()
		cancel()
	}

	history, read, err := s.history(ctx, t, opts)
	if err != nil {
		cleanup()
		return nil, err
	}

	pr, pw := io.Pipe()
	w := &lineWriter{w: pw, timestamps: opts.Timestamps, remaining: -1}
	if opts.LimitBytes != nil {
		w.remaining = *opts.LimitBytes
	}
	pumped := make(chan struct{})
	go func() {
		// Cancellation alone must release everything, even when the caller never reads or
		// closes the reader: closing the pipe unblocks a pending write.
		select {
		case <-ctx.Done():
			_ = pw.CloseWithError(ctx.Err())
		case <-pumped:
		}
	}()
	go func() {
		defer close(pumped)
		defer cleanup()
		defer func() {
			if r := recover(); r != nil {
				_ = pw.CloseWithError(fmt.Errorf("workerlogs: internal error writing the log stream: %v", r))
			}
		}()
		err := s.pump(ctx, w, history, read, stream)
		if err == errLimitReached || ctx.Err() != nil {
			err = nil
		}
		_ = pw.CloseWithError(err)
	}()
	return &logReader{PipeReader: pr, cancel: cancel}, nil
}

// history reads and formats the events before the stream, honouring TailLines and SinceTime.
// It returns the lines to print and every line read (before the --tail cut).
func (s *streamer) history(ctx context.Context, t Target, opts Options) (printed, read []Line, _ error) {
	if opts.TailLines != nil && *opts.TailLines == 0 {
		return nil, nil, nil // --tail=0: no history (and no query).
	}
	now := s.now()
	from := now.Add(-s.limits.DefaultWindow)
	if opts.SinceTime != nil {
		from = *opts.SinceTime
	}
	maxEvents := s.limits.MaxEvents
	if opts.TailLines != nil && *opts.TailLines < int64(maxEvents) {
		// Every event is at least one line, so the newest N events hold the last N lines.
		maxEvents = int(*opts.TailLines)
	}
	res, err := s.src.Query(ctx, t, QueryOptions{From: from, To: now, MaxEvents: maxEvents})
	if err != nil {
		return nil, nil, err
	}
	for _, ev := range res.Events {
		read = append(read, s.f.Format(ev)...)
	}
	printed = read
	if opts.TailLines != nil && int64(len(printed)) > *opts.TailLines {
		printed = printed[int64(len(printed))-*opts.TailLines:]
	}
	return printed, read, nil
}

// pump writes the history, then the live events until ctx ends or the stream closes.
//
// Overlap: the tail is opened before the history query, so an invocation can arrive both on the
// tail and in the history. A live event is dropped when every one of its lines (by lineKey) is
// among the lines of the newest minute of the history read (printed or cut by --tail);
// anything else is written. The history usually lags 15–30 s behind (spike §1), so this rarely
// triggers, and it never drops an event that the history did not hold.
func (s *streamer) pump(ctx context.Context, w *lineWriter, history, read []Line, stream Stream) error {
	var seen map[string]int
	var newest time.Time
	if stream != nil && len(read) > 0 {
		newest = read[len(read)-1].Time
		seen = map[string]int{}
		for _, l := range read {
			if !l.Time.Before(newest.Add(-overlapWindow)) {
				seen[lineKey(l)]++
			}
		}
	}
	for _, l := range history {
		if err := w.write(l); err != nil {
			return err
		}
	}
	if stream == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-stream.Events():
			if !ok {
				// Say why the live stream ended (the error is free of the tail URL), then end the
				// response normally: kubectl shows the line and exits.
				if err := stream.Err(); err != nil && ctx.Err() == nil {
					for _, l := range s.f.Format(Event{Time: s.now().UTC(), Kind: KindNotice,
						Message: "flare-operator: the live stream ended: " + err.Error()}) {
						if err := w.write(l); err != nil {
							return err
						}
					}
				}
				return nil
			}
			lines := s.f.Format(ev)
			if seen != nil && overlaps(seen, newest, lines) {
				continue
			}
			for _, l := range lines {
				if err := w.write(l); err != nil {
					return err
				}
			}
		}
	}
}

// overlapWindow bounds which history lines are remembered for de-duplication: only the newest
// minute of the history can overlap a tail opened just before the query.
const overlapWindow = time.Minute

// lineKey identifies a line for de-duplication: its time in ms and its text. An invocation
// summary is keyed by the part before " -> ", because the tail does not report everything the
// query does (the ray ID; UNVERIFIED whether it reports the status and times).
func lineKey(l Line) string {
	text := l.Text
	for _, tag := range invocationTags {
		if strings.HasPrefix(text, tag) {
			if i := strings.Index(text, " -> "); i >= 0 {
				text = text[:i]
			}
			break
		}
	}
	return strconv.FormatInt(l.Time.UnixMilli(), 10) + " " + text
}

var invocationTags = []string{"[fetch] ", "[scheduled]", "[alarm] ", "[queue] ", "[email] ", "[rpc] ", "[tail] ", "[event] ->"}

// overlaps reports (and consumes) a live event that the history already printed: every one of
// its lines is in seen, at a time no later than the newest history line.
func overlaps(seen map[string]int, newest time.Time, lines []Line) bool {
	if len(lines) == 0 || len(seen) == 0 {
		return false
	}
	for _, l := range lines {
		if l.Time.After(newest) || seen[lineKey(l)] == 0 {
			return false
		}
	}
	for _, l := range lines {
		k := lineKey(l)
		if seen[k]--; seen[k] == 0 {
			delete(seen, k)
		}
	}
	return true
}

// errLimitReached ends the stream after LimitBytes.
var errLimitReached = io.EOF

// lineWriter writes lines with the optional timestamp prefix and stops after a byte budget,
// cutting the last line like the kubelet's LimitWriter.
type lineWriter struct {
	w          io.Writer
	timestamps bool
	remaining  int64 // -1: unlimited
	buf        strings.Builder
}

func (lw *lineWriter) write(l Line) error {
	if lw.remaining == 0 {
		return errLimitReached
	}
	lw.buf.Reset()
	if lw.timestamps {
		lw.buf.WriteString(l.Time.UTC().Format(TimestampFormat))
		lw.buf.WriteByte(' ')
	}
	lw.buf.WriteString(l.Text)
	lw.buf.WriteByte('\n')
	b := lw.buf.String()
	if lw.remaining > 0 && int64(len(b)) > lw.remaining {
		b = b[:lw.remaining]
	}
	if _, err := io.WriteString(lw.w, b); err != nil {
		return err
	}
	if lw.remaining > 0 {
		lw.remaining -= int64(len(b))
		if lw.remaining == 0 {
			return errLimitReached
		}
	}
	return nil
}

// logReader ends the stream (and the follow) when closed.
type logReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *logReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}
