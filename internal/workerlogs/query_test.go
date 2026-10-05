package workerlogs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// replay answers every query with the recording's status and body.
func replay(rec recording) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(rec.ResponseBody)
	}
}

// decodeRecordedEvents runs Query against a server replaying rec and returns its events.
func decodeRecordedEvents(t *testing.T, rec recording) []Event {
	t.Helper()
	f := newFakeCF(t)
	f.setQuery(replay(rec))
	src := NewSource(Limits{})
	var tf queryTimeframe
	var req struct {
		Timeframe queryTimeframe `json:"timeframe"`
	}
	if err := json.Unmarshal(rec.RequestBody, &req); err != nil {
		t.Fatal(err)
	}
	tf = req.Timeframe
	res, err := src.Query(context.Background(), testTarget(newTestClient(t, f.srv.URL)),
		QueryOptions{From: time.UnixMilli(tf.From), To: time.UnixMilli(tf.To), MaxEvents: 50})
	if err != nil {
		t.Fatal(err)
	}
	return res.Events
}

func TestQueryRecording0054(t *testing.T) {
	rec := loadRecording(t, "0054")
	f := newFakeCF(t)
	f.setQuery(replay(rec))
	src := NewSource(Limits{})
	from, to := time.UnixMilli(1790664268007), time.UnixMilli(1790665168007)
	res, err := src.Query(context.Background(), testTarget(newTestClient(t, f.srv.URL)), QueryOptions{From: from, To: to, MaxEvents: 50})
	if err != nil {
		t.Fatal(err)
	}

	// The request has the recorded shape; only queryId differs (an ad-hoc ID either way).
	if len(f.queryBodies()) != 1 {
		t.Fatalf("%d requests, want 1", len(f.queryBodies()))
	}
	var got, want map[string]any
	_ = json.Unmarshal(f.queryBodies()[0], &got)
	_ = json.Unmarshal(rec.RequestBody, &want)
	if got["queryId"] != QueryID {
		t.Errorf("queryId = %v", got["queryId"])
	}
	got["queryId"], want["queryId"] = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body\n got %v\nwant %v", got, want)
	}

	if len(res.Events) != 19 || res.More {
		t.Fatalf("%d events, More=%v; want 19, false", len(res.Events), res.More)
	}
	for i := 1; i < len(res.Events); i++ {
		a, b := res.Events[i-1], res.Events[i]
		if b.Time.Before(a.Time) || (b.Time.Equal(a.Time) && b.Seq < a.Seq) {
			t.Errorf("events %d and %d out of order: %v/%s, %v/%s", i-1, i, a.Time, a.Seq, b.Time, b.Seq)
		}
	}
	// The /throw invocation: info log, error log, exception, then the summary.
	var throw []Event
	for _, ev := range res.Events {
		if ev.RequestID == "7d53eb75c06f6ee230d5dd5b00b3d409" {
			throw = append(throw, ev)
		}
	}
	kinds := []EventKind{}
	for _, ev := range throw {
		kinds = append(kinds, ev.Kind)
	}
	if !reflect.DeepEqual(kinds, []EventKind{KindLog, KindLog, KindException, KindInvocation}) {
		t.Fatalf("kinds = %v", kinds)
	}
	if x := throw[2].Exception; x.Name != "Error" || x.Message != "FSLMARK7c57e9c4 boom counter=1" || x.Stack != "    at Object.fetch (worker.mjs:10:13)" {
		t.Errorf("exception = %+v", x)
	}
	inv := throw[3].Invocation
	if inv.Type != "fetch" || inv.Method != "GET" || inv.Status != 500 || inv.Outcome != "exception" ||
		inv.RayID != "a42919c389e7cf26" || inv.WallTimeMs == nil || *inv.WallTimeMs != 1 {
		t.Errorf("invocation = %+v", inv)
	}
	if throw[0].ScriptVersion != "2bbbf05d-a4dd-495b-bbb3-7121f176550e" || throw[0].Time.UnixMilli() != 1790665119291 {
		t.Errorf("event = %+v", throw[0])
	}
}

// TestQueryPagingRecorded replays 0092 (first page, limit 4) and 0093 (offset=<last id>,
// offsetDirection next).
func TestQueryPagingRecorded(t *testing.T) {
	p1, p2 := loadRecording(t, "0092"), loadRecording(t, "0093")
	var want2 struct {
		Offset          string `json:"offset"`
		OffsetDirection string `json:"offsetDirection"`
	}
	_ = json.Unmarshal(p2.RequestBody, &want2)
	f := newFakeCF(t)
	f.setQuery(func(w http.ResponseWriter, body []byte) {
		var q queryRequest
		_ = json.Unmarshal(body, &q)
		if q.Limit != 4 {
			t.Errorf("limit = %d, want 4", q.Limit)
		}
		switch {
		case q.Offset == "" && q.OffsetDirection == "":
			replay(p1)(w, body)
		case q.Offset == want2.Offset && q.OffsetDirection == want2.OffsetDirection:
			replay(p2)(w, body)
		default:
			t.Errorf("unexpected page request offset=%q dir=%q", q.Offset, q.OffsetDirection)
			writeEnvelope(w, 400, nil)
		}
	})
	src := NewSource(Limits{PageSize: 4, MaxEvents: 8})
	res, err := src.Query(context.Background(), testTarget(newTestClient(t, f.srv.URL)),
		QueryOptions{From: time.UnixMilli(1790665100000), To: time.UnixMilli(1790665528177)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queryBodies()) != 2 {
		t.Fatalf("%d requests, want 2", len(f.queryBodies()))
	}
	if len(res.Events) != 8 || !res.More {
		t.Fatalf("%d events More=%v, want 8 true", len(res.Events), res.More)
	}
	first, last := res.Events[0], res.Events[7]
	if first.Message != "FSLMARK7c57e9c4 info path=/x1 counter=1 t=2026-09-29T07:03:44.952Z" ||
		last.Message != "FSLMARK7c57e9c4 err path=/lagb81262 counter=2" || last.Level != "error" {
		t.Errorf("first %q, last %q", first.Message, last.Message)
	}
}

// syntheticTelemetry serves n events newest first with the recorded paging rules: limit at
// most 2000 (else the 0096 error), offset=<id> with offsetDirection next continues after id.
func syntheticTelemetry(t *testing.T, n int, base time.Time) func(http.ResponseWriter, []byte) {
	tooBig := loadRecording(t, "0096")
	ids := make([]string, n) // newest first
	for i := range ids {
		ids[i] = fmt.Sprintf("01SYNTH%019d", n-i)
	}
	return func(w http.ResponseWriter, body []byte) {
		var q queryRequest
		if err := json.Unmarshal(body, &q); err != nil {
			t.Errorf("bad body: %v", err)
		}
		if q.Limit > MaxPageSize {
			replay(tooBig)(w, body)
			return
		}
		start := 0
		if q.Offset != "" {
			if q.OffsetDirection != "next" {
				t.Errorf("offsetDirection = %q", q.OffsetDirection)
			}
			for i, id := range ids {
				if id == q.Offset {
					start = i + 1
				}
			}
		}
		end := min(start+q.Limit, n)
		evs := []map[string]any{}
		for i := start; i < end; i++ {
			evs = append(evs, map[string]any{
				"source":    map[string]any{"level": "info", "message": fmt.Sprintf("event %d", n-i)},
				"timestamp": base.Add(time.Duration(n-i) * time.Millisecond).UnixMilli(),
				"$workers":  map[string]any{"truncated": false, "eventType": "fetch"},
				"$metadata": map[string]any{"id": ids[i], "type": "cf-worker", "service": "flare-spike-logs-1"},
			})
		}
		writeEnvelope(w, 200, map[string]any{"events": map[string]any{"events": evs}})
	}
}

func TestQueryPagingSynthetic(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		n, max     int
		wantLimits []int
		wantN      int
		wantMore   bool
	}{
		{"all of 4500", 4500, 10000, []int{2000, 2000, 2000}, 4500, false},
		{"newest 3000 of 4500", 4500, 3000, []int{2000, 1000}, 3000, true},
		{"exactly one page", 2000, 10000, []int{2000, 2000}, 2000, false},
		{"empty", 0, 10000, []int{2000}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCF(t)
			f.setQuery(syntheticTelemetry(t, tc.n, base))
			src := NewSource(Limits{MaxEvents: tc.max})
			res, err := src.Query(context.Background(), testTarget(newTestClient(t, f.srv.URL)),
				QueryOptions{From: base, To: base.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			var limits []int
			for _, b := range f.queryBodies() {
				var q queryRequest
				_ = json.Unmarshal(b, &q)
				limits = append(limits, q.Limit)
			}
			if !reflect.DeepEqual(limits, tc.wantLimits) {
				t.Errorf("limits = %v, want %v", limits, tc.wantLimits)
			}
			if len(res.Events) != tc.wantN || res.More != tc.wantMore {
				t.Fatalf("%d events More=%v, want %d %v", len(res.Events), res.More, tc.wantN, tc.wantMore)
			}
			if tc.wantN > 0 {
				// Oldest first, ending with the newest event.
				if res.Events[tc.wantN-1].Message != fmt.Sprintf("event %d", tc.n) ||
					res.Events[0].Message != fmt.Sprintf("event %d", tc.n-tc.wantN+1) {
					t.Errorf("first %q last %q", res.Events[0].Message, res.Events[tc.wantN-1].Message)
				}
			}
		})
	}
}

func TestQueryErrors(t *testing.T) {
	f := newFakeCF(t)
	f.setQuery(replay(loadRecording(t, "0096")))
	// A PageSize above the maximum is clamped, so only a misbehaving server yields too_big.
	src := NewSource(Limits{PageSize: 5000})
	_, err := src.Query(context.Background(), testTarget(newTestClient(t, f.srv.URL)), QueryOptions{From: time.Now().Add(-time.Hour), To: time.Now()})
	ae, ok := cfclient.AsAPIError(err)
	if !ok || ae.Status != 400 {
		t.Fatalf("err = %v, want an APIError 400", err)
	}
	var q queryRequest
	_ = json.Unmarshal(f.queryBodies()[0], &q)
	if q.Limit != MaxPageSize {
		t.Errorf("limit = %d, want %d", q.Limit, MaxPageSize)
	}

	f2 := newFakeCF(t)
	f2.setQuery(func(w http.ResponseWriter, _ []byte) { writeEnvelope(w, 403, nil) })
	_, err = src.Query(context.Background(), testTarget(newTestClient(t, f2.srv.URL)), QueryOptions{From: time.Now().Add(-time.Hour), To: time.Now()})
	if ae, ok := cfclient.AsAPIError(err); !ok || ae.Status != 403 {
		t.Fatalf("err = %v, want an APIError 403", err)
	}
}

func TestQueryWindow(t *testing.T) {
	f := newFakeCF(t)
	src := NewSource(Limits{})
	tgt := testTarget(newTestClient(t, f.srv.URL))
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// A window over 30 days is clamped to 30 days (0098: accepted).
	if _, err := src.Query(context.Background(), tgt, QueryOptions{From: to.Add(-90 * 24 * time.Hour), To: to}); err != nil {
		t.Fatal(err)
	}
	var q queryRequest
	_ = json.Unmarshal(f.queryBodies()[0], &q)
	if q.Timeframe.To != to.UnixMilli() || q.Timeframe.From != to.Add(-30*24*time.Hour).UnixMilli() {
		t.Errorf("timeframe = %+v", q.Timeframe)
	}
	// An empty window sends nothing.
	res, err := src.Query(context.Background(), tgt, QueryOptions{From: to, To: to})
	if err != nil || len(res.Events) != 0 || len(f.queryBodies()) != 1 {
		t.Errorf("empty window: %v %v %d requests", res, err, len(f.queryBodies()))
	}
}

func TestTelemetryDecodingTolerance(t *testing.T) {
	raw := `[
	 {"timestamp": 5, "source": {"message": ["a", 1, {"k": true}, null]}, "$metadata": {"id": "3", "type": "cf-worker"}},
	 {"timestamp": 5, "source": "bare", "$metadata": {"id": "2", "type": "something-new"}},
	 {"timestamp": 5, "source": {}, "$metadata": {"id": "1", "type": "cf-worker", "level": "warn", "message": "from metadata"}},
	 {"timestamp": 5, "$workers": {"eventType": "scheduled", "outcome": "ok", "event": {"cron": "* * * * *"}}, "$metadata": {"id": "4", "type": "cf-worker-event"}}
	]`
	var tes []telemetryEvent
	if err := json.Unmarshal([]byte(raw), &tes); err != nil {
		t.Fatal(err)
	}
	var got []string
	f := NewFormatter()
	for _, te := range tes {
		for _, l := range f.Format(te.toEvent()) {
			got = append(got, l.Text)
		}
	}
	want := []string{`[log] a 1 {"k":true} null`, `[log] bare`, `[warn] from metadata`, `[scheduled] "* * * * *" -> ok`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
