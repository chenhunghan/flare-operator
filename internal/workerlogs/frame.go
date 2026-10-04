package workerlogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// traceFrame is one trace-v1 message of the legacy tail: a JSON TailEventMessage (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/createTail.ts#L216-L282).
// scriptVersion and truncated are not in wrangler's type (design §7.1; UNVERIFIED) and are read
// when present. No trace-v1 frame is recorded (design §13.5), so unknown fields are ignored and
// known ones are decoded tolerantly.
type traceFrame struct {
	Outcome        string          `json:"outcome"`
	ScriptName     string          `json:"scriptName"`
	Entrypoint     string          `json:"entrypoint"`
	EventTimestamp *float64        `json:"eventTimestamp"`
	Event          json.RawMessage `json:"event"`
	Logs           []struct {
		Message   json.RawMessage `json:"message"`
		Level     string          `json:"level"`
		Timestamp float64         `json:"timestamp"`
	} `json:"logs"`
	Exceptions []struct {
		Name      string          `json:"name"`
		Message   json.RawMessage `json:"message"`
		Timestamp float64         `json:"timestamp"`
		Stack     string          `json:"stack"`
	} `json:"exceptions"`
	ScriptVersion *struct {
		ID string `json:"id"`
	} `json:"scriptVersion"`
	Truncated bool `json:"truncated"`
}

var errNotTraceFrame = errors.New("not a trace-v1 event object")

// decodeFrame turns one tail frame into events in the frame's order: its logs, then its
// exceptions, then the invocation (or a notice for a tail-info event). next numbers Seq; now
// stands in for a frame without any timestamp.
func decodeFrame(data []byte, now time.Time, next func() string) ([]Event, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, errNotTraceFrame
	}
	var f traceFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode trace-v1 frame: %w", err)
	}
	var evTime time.Time
	if f.EventTimestamp != nil {
		evTime = msTime(*f.EventTimestamp)
	}
	version := ""
	if f.ScriptVersion != nil {
		version = f.ScriptVersion.ID
	}
	at := func(ms float64) time.Time {
		if ms > 0 {
			return msTime(ms)
		}
		return evTime
	}

	evs := make([]Event, 0, len(f.Logs)+len(f.Exceptions)+1)
	for _, l := range f.Logs {
		evs = append(evs, Event{
			Time: at(l.Timestamp), Seq: next(), Kind: KindLog, Level: l.Level,
			Message: joinArgs(l.Message), ScriptVersion: version,
		})
	}
	for _, x := range f.Exceptions {
		evs = append(evs, Event{
			Time: at(x.Timestamp), Seq: next(), Kind: KindException, ScriptVersion: version,
			Exception: &Exception{Name: x.Name, Message: jsonText(x.Message), Stack: x.Stack},
		})
	}
	if ev, ok := frameEvent(&f, evTime, version); ok {
		ev.Seq = next()
		evs = append(evs, ev)
	}
	if evTime.IsZero() {
		for i := range evs {
			if evs[i].Time.IsZero() {
				evs[i].Time = now.UTC() // no timestamp at all (UNVERIFIED): arrival time
			}
		}
	}
	return evs, nil
}

// frameEvent classifies the frame's event the way wrangler's prettyPrintLogs does, by the keys
// present, in its order: cron, request, mailFrom, scheduledTime without cron (alarm),
// consumedEvents, message+type (tail info), queue, rpcMethod, else unknown (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/printing.ts#L42-L124 and
// #L144-L188). A tail-info event becomes a KindNotice and no invocation.
func frameEvent(f *traceFrame, t time.Time, version string) (Event, bool) {
	var keys map[string]json.RawMessage
	raw := bytes.TrimSpace(f.Event)
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &keys)
	}
	has := func(k string) bool { _, ok := keys[k]; return ok }
	str := func(k string) string { return jsonText(keys[k]) }
	num := func(k string) float64 {
		var n float64
		_ = json.Unmarshal(keys[k], &n)
		return n
	}
	inv := &Invocation{Outcome: f.Outcome, Entrypoint: f.Entrypoint}
	ev := Event{Time: t, Kind: KindInvocation, Invocation: inv, ScriptVersion: version, Truncated: f.Truncated}
	switch {
	case has("cron"):
		inv.Type = "scheduled"
		inv.Cron = str("cron")
	case has("request"):
		inv.Type = "fetch"
		var r struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		_ = json.Unmarshal(keys["request"], &r)
		inv.Method = strings.ToUpper(r.Method) // printing.ts#L54
		inv.URL = r.URL
		// response.status is not in wrangler's RequestEvent type; read when present (UNVERIFIED).
		var resp struct {
			Status int `json:"status"`
		}
		if has("response") && json.Unmarshal(keys["response"], &resp) == nil {
			inv.Status = resp.Status
		}
	case has("mailFrom"):
		inv.Type = "email"
		inv.MailFrom, inv.RcptTo, inv.RawSize = str("mailFrom"), str("rcptTo"), int64(num("rawSize"))
	case has("scheduledTime"):
		inv.Type = "alarm"
	case has("consumedEvents"):
		inv.Type = "tail"
		var consumed []struct {
			ScriptName string `json:"scriptName"`
		}
		_ = json.Unmarshal(keys["consumedEvents"], &consumed)
		var names []string
		seen := map[string]bool{}
		for _, c := range consumed {
			if c.ScriptName != "" && !seen[c.ScriptName] {
				seen[c.ScriptName] = true
				names = append(names, c.ScriptName)
			}
		}
		// The contract's Invocation has no field for the tailed scripts; Message carries them.
		ev.Message = strings.Join(names, ",")
	case has("message") && has("type"):
		// Tail info, e.g. type "overload" / "overload-stop" when the tail samples.
		return Event{Time: t, Kind: KindNotice, Message: str("message"), ScriptVersion: version}, true
	case has("queue"):
		inv.Type = "queue"
		inv.Queue, inv.BatchSize = str("queue"), int(num("batchSize"))
	case has("rpcMethod"):
		inv.Type = "rpc"
		inv.RPCMethod = str("rpcMethod")
	default:
		inv.Type = "unknown"
	}
	return ev, true
}

func msTime(ms float64) time.Time {
	return time.UnixMilli(int64(ms)).UTC()
}

// seqCounter numbers follow events: zero-padded so that string order is numeric order.
type seqCounter struct{ n uint64 }

func (c *seqCounter) next() string {
	c.n++
	s := strconv.FormatUint(c.n, 10)
	return strings.Repeat("0", 20-len(s)) + s
}
