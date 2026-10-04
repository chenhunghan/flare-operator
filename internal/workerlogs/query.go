package workerlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// queryRequest is the body of POST /accounts/{a}/workers/observability/telemetry/query, in the
// shape of recording 0054 (paging fields: 0093).
type queryRequest struct {
	QueryID         string          `json:"queryId"`
	View            string          `json:"view"`
	Limit           int             `json:"limit"`
	Dry             bool            `json:"dry"`
	Timeframe       queryTimeframe  `json:"timeframe"`
	Offset          string          `json:"offset,omitempty"`
	OffsetDirection string          `json:"offsetDirection,omitempty"`
	Parameters      queryParameters `json:"parameters"`
}

type queryTimeframe struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

type queryParameters struct {
	Datasets          []string      `json:"datasets"`
	FilterCombination string        `json:"filterCombination"`
	Filters           []queryFilter `json:"filters"`
	Calculations      []any         `json:"calculations"`
	GroupBys          []any         `json:"groupBys"`
	Havings           []any         `json:"havings"`
}

type queryFilter struct {
	Key       string `json:"key"`
	Operation string `json:"operation"`
	Type      string `json:"type"`
	Value     string `json:"value"`
}

// queryResult is the part of the result we read (0054: result.events.events[]).
type queryResult struct {
	Events struct {
		Events []telemetryEvent `json:"events"`
	} `json:"events"`
}

// telemetryEvent is one event of view "events" (recording 0054). Fields not recorded for
// non-fetch triggers are decoded tolerantly (UNVERIFIED shapes; design §13.5).
type telemetryEvent struct {
	Timestamp float64         `json:"timestamp"`
	Source    json.RawMessage `json:"source"`
	Workers   struct {
		Truncated     bool   `json:"truncated"`
		EventType     string `json:"eventType"`
		Outcome       string `json:"outcome"`
		Entrypoint    string `json:"entrypoint"`
		ScriptVersion struct {
			ID string `json:"id"`
		} `json:"scriptVersion"`
		WallTimeMs *float64        `json:"wallTimeMs"`
		CPUTimeMs  *float64        `json:"cpuTimeMs"`
		Event      json.RawMessage `json:"event"`
	} `json:"$workers"`
	Metadata struct {
		ID        string          `json:"id"`
		RequestID string          `json:"requestId"`
		RayID     string          `json:"rayId"`
		Type      string          `json:"type"`
		Level     string          `json:"level"`
		Message   json.RawMessage `json:"message"`
	} `json:"$metadata"`
}

// telemetrySource is the "source" object of a cf-worker event: {level, message} for a console
// call, plus exception{name, stack} for an uncaught exception (0054).
type telemetrySource struct {
	Level     string          `json:"level"`
	Message   json.RawMessage `json:"message"`
	Exception *struct {
		Name    string          `json:"name"`
		Message json.RawMessage `json:"message"`
		Stack   string          `json:"stack"`
	} `json:"exception"`
}

// telemetryInvocation is $workers.event: request{method,url} and response{status} are recorded
// (0054); the other triggers' fields are UNVERIFIED and named as in the tail's event objects.
type telemetryInvocation struct {
	Request *struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"request"`
	Response *struct {
		Status int `json:"status"`
	} `json:"response"`
	Cron      string  `json:"cron"`
	Queue     string  `json:"queue"`
	BatchSize int     `json:"batchSize"`
	MailFrom  string  `json:"mailFrom"`
	RcptTo    string  `json:"rcptTo"`
	RawSize   float64 `json:"rawSize"`
	RPCMethod string  `json:"rpcMethod"`
}

// query implements Source.Query.
func (s *cfSource) query(ctx context.Context, t Target, q QueryOptions) (QueryResult, error) {
	if t.Client == nil {
		return QueryResult{}, fmt.Errorf("workerlogs: target %s has no Cloudflare client", t.WorkerScript)
	}
	maxEvents := q.MaxEvents
	if maxEvents <= 0 {
		maxEvents = s.limits.MaxEvents
	}
	to := q.To
	if to.IsZero() {
		to = s.now()
	}
	from := q.From
	if floor := to.Add(-maxQueryWindow); from.Before(floor) {
		from = floor
	}
	if !from.Before(to) {
		return QueryResult{}, nil
	}

	req := queryRequest{
		QueryID:   QueryID,
		View:      "events",
		Dry:       true,
		Timeframe: queryTimeframe{From: from.UnixMilli(), To: to.UnixMilli()},
		Parameters: queryParameters{
			Datasets:          []string{},
			FilterCombination: "and",
			Filters: []queryFilter{{
				Key: "$metadata.service", Operation: "eq", Type: "string", Value: t.Script,
			}},
			Calculations: []any{},
			GroupBys:     []any{},
			Havings:      []any{},
		},
	}
	path := "/accounts/" + url.PathEscape(t.AccountID) + "/workers/observability/telemetry/query"

	var (
		events []Event
		seen   = map[string]bool{}
		more   bool
	)
	for len(events) < maxEvents {
		req.Limit = min(s.limits.PageSize, maxEvents-len(events))
		resp, err := t.Client.Do(ctx, cfclient.Request{Method: http.MethodPost, Path: path, Body: req})
		if err != nil {
			return QueryResult{}, fmt.Errorf("workerlogs: query the logs of %s: %w", t.Script, err)
		}
		var res queryResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			return QueryResult{}, fmt.Errorf("workerlogs: decode the telemetry query result: %w", err)
		}
		page := res.Events.Events
		added := 0
		for _, te := range page {
			id := te.Metadata.ID
			if id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			if len(events) == maxEvents {
				break
			}
			events = append(events, te.toEvent())
			added++
		}
		// Results are newest first (0054). A short page is the last one; a full page may have
		// older events behind it, reached by offset=<last $metadata.id>, offsetDirection next
		// (0093).
		last := ""
		if len(page) > 0 {
			last = page[len(page)-1].Metadata.ID
		}
		if len(page) < req.Limit || added == 0 || last == "" {
			break
		}
		if len(events) >= maxEvents {
			// Full: older events may remain (the exact answer would cost one more request).
			more = true
			break
		}
		req.Offset = last
		req.OffsetDirection = "next"
	}
	sortEvents(events)
	return QueryResult{Events: events, More: more}, nil
}

// sortEvents orders events oldest first; events with equal Time (one invocation: Cloudflare
// freezes the timestamp) by Seq ($metadata.id, whose suffix counts up within the invocation;
// 0054), which puts console lines before exceptions before the invocation summary.
func sortEvents(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].Time.Equal(evs[j].Time) {
			return evs[i].Time.Before(evs[j].Time)
		}
		if len(evs[i].Seq) != len(evs[j].Seq) {
			return len(evs[i].Seq) < len(evs[j].Seq)
		}
		return evs[i].Seq < evs[j].Seq
	})
}

func (te *telemetryEvent) toEvent() Event {
	ev := Event{
		Time:          time.UnixMilli(int64(te.Timestamp)).UTC(),
		Seq:           te.Metadata.ID,
		ScriptVersion: te.Workers.ScriptVersion.ID,
		RequestID:     te.Metadata.RequestID,
		Truncated:     te.Workers.Truncated,
	}
	var src telemetrySource
	if len(te.Source) > 0 && te.Source[0] == '{' {
		_ = json.Unmarshal(te.Source, &src) // tolerant: a foreign shape leaves the zero value
	}
	switch {
	case te.Metadata.Type == "cf-worker-event":
		ev.Kind = KindInvocation
		ev.Invocation = te.invocation()
	case src.Exception != nil:
		ev.Kind = KindException
		msg := jsonText(src.Message)
		if msg == "" {
			msg = jsonText(src.Exception.Message)
		}
		ev.Exception = &Exception{Name: src.Exception.Name, Message: msg, Stack: src.Exception.Stack}
	default:
		// cf-worker (a console call) and anything unknown: show its message.
		ev.Kind = KindLog
		ev.Level = src.Level
		if ev.Level == "" {
			ev.Level = te.Metadata.Level
		}
		ev.Message = joinArgs(src.Message)
		if len(src.Message) == 0 {
			ev.Message = joinArgs(te.Metadata.Message)
		}
		if len(src.Message) == 0 && len(te.Metadata.Message) == 0 && len(te.Source) > 0 && te.Source[0] != '{' {
			ev.Message = joinArgs(te.Source) // a bare source value (UNVERIFIED shape)
		}
	}
	return ev
}

func (te *telemetryEvent) invocation() *Invocation {
	inv := &Invocation{
		Type:       telemetryEventType(te.Workers.EventType),
		Outcome:    te.Workers.Outcome,
		Entrypoint: te.Workers.Entrypoint,
		WallTimeMs: te.Workers.WallTimeMs,
		CPUTimeMs:  te.Workers.CPUTimeMs,
		RayID:      te.Metadata.RayID,
	}
	var e telemetryInvocation
	if len(te.Workers.Event) > 0 && te.Workers.Event[0] == '{' {
		_ = json.Unmarshal(te.Workers.Event, &e)
	}
	if e.Request != nil {
		inv.Method = strings.ToUpper(e.Request.Method)
		inv.URL = e.Request.URL
	}
	if e.Response != nil {
		inv.Status = e.Response.Status
	}
	inv.Cron, inv.Queue, inv.BatchSize = e.Cron, e.Queue, e.BatchSize
	inv.MailFrom, inv.RcptTo, inv.RawSize = e.MailFrom, e.RcptTo, int64(e.RawSize)
	inv.RPCMethod = e.RPCMethod
	return inv
}

// telemetryEventType maps $workers.eventType to Invocation.Type. "fetch" is recorded (0054);
// the others are UNVERIFIED spellings.
func telemetryEventType(t string) string {
	switch strings.ToLower(t) {
	case "fetch":
		return "fetch"
	case "scheduled", "cron":
		return "scheduled"
	case "queue":
		return "queue"
	case "alarm":
		return "alarm"
	case "email":
		return "email"
	case "rpc", "jsrpc":
		return "rpc"
	case "tail", "trace":
		return "tail"
	default:
		return "unknown"
	}
}

// jsonText returns a JSON string's value, or the compact JSON of any other value ("" for
// absent or null).
func jsonText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// joinArgs renders console arguments: an array is joined with single spaces (strings as is,
// other values as compact JSON); any other value is jsonText.
func joinArgs(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return jsonText(raw)
	}
	var args []json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return jsonText(raw)
	}
	parts := make([]string, len(args))
	for i, a := range args {
		if bytes.Equal(bytes.TrimSpace(a), []byte("null")) {
			parts[i] = "null"
		} else {
			parts[i] = jsonText(a)
		}
	}
	return strings.Join(parts, " ")
}
