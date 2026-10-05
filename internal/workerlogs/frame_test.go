package workerlogs

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Synthetic trace-v1 frames, one per branch of wrangler's prettyPrintLogs (SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/printing.ts#L42-L124), shaped like
// its TailEventMessage type (createTail.ts#L216-L479). No trace-v1 frame is recorded.
const (
	frameFetch = `{"outcome":"exception","scriptName":"flare-spike-logs-1","eventTimestamp":1790665119291,
	 "scriptVersion":{"id":"2bbbf05d-a4dd-495b-bbb3-7121f176550e"},
	 "event":{"request":{"url":"https://w.example/throw?phase=1","method":"get","headers":{},"cf":{"colo":"SJC"}},"response":{"status":500}},
	 "logs":[{"message":["info path=/throw",{"n":1}],"level":"log","timestamp":1790665119291},
	         {"message":["err\nsecond line"],"level":"error","timestamp":1790665119291}],
	 "exceptions":[{"name":"Error","message":"boom","timestamp":1790665119291,"stack":"    at Object.fetch (worker.mjs:10:13)"}]}`
	frameCron     = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"cron":"*/5 * * * *","scheduledTime":1790665119000},"logs":[],"exceptions":[]}`
	frameAlarm    = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"scheduledTime":"2026-09-29T06:58:39.000Z"},"logs":[],"exceptions":[]}`
	frameEmail    = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"mailFrom":"a@x","rcptTo":"b@y","rawSize":123},"logs":[],"exceptions":[]}`
	frameTail     = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"consumedEvents":[{"scriptName":"a"},{"scriptName":"b"},{"scriptName":"a"},{}]},"logs":[],"exceptions":[]}`
	frameInfo     = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"type":"overload","message":"Tail is currently in sampling mode due to the high volume of messages."},"logs":[],"exceptions":[]}`
	frameQueue    = `{"outcome":"ok","eventTimestamp":1790665119291,"event":{"queue":"jobs","batchSize":3},"logs":[],"exceptions":[]}`
	frameRPC      = `{"outcome":"ok","entrypoint":"Api","eventTimestamp":1790665119291,"event":{"rpcMethod":"get"},"logs":[],"exceptions":[]}`
	frameUnknown  = `{"outcome":"canceled","eventTimestamp":1790665119291,"event":null,"logs":[],"exceptions":[]}`
	frameTrunc    = `{"outcome":"ok","eventTimestamp":1790665119291,"truncated":true,"event":{"request":{"url":"https://w.example/","method":"POST"}},"logs":[],"exceptions":[]}`
	frameExtraFld = `{"outcome":"ok","eventTimestamp":1790665119291,"brandNew":{"x":[1,2]},"event":{"request":{"url":"https://w.example/","method":"GET"},"futureKey":1},"logs":[{"message":"not-an-array","level":"warn","timestamp":0}],"exceptions":[{"name":"TypeError","message":{"code":42},"timestamp":0}]}`
)

func TestDecodeFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  []string
	}{
		{"fetch with logs and exception", frameFetch, []string{
			`[log] info path=/throw {"n":1}`,
			`[error] err`, `second line`,
			`[exception] Error: boom`, `    at Object.fetch (worker.mjs:10:13)`,
			`[fetch] GET https://w.example/throw?phase=1 -> 500 exception`,
		}},
		{"cron", frameCron, []string{`[scheduled] "*/5 * * * *" -> ok`}},
		{"alarm", frameAlarm, []string{`[alarm] -> ok`}},
		{"email", frameEmail, []string{`[email] from:a@x to:b@y size:123 -> ok`}},
		{"tail consumer", frameTail, []string{`[tail] tailing a,b -> ok`}},
		{"tail info", frameInfo, []string{`[notice] Tail is currently in sampling mode due to the high volume of messages.`}},
		{"queue", frameQueue, []string{`[queue] jobs (3 messages) -> ok`}},
		{"rpc", frameRPC, []string{`[rpc] Api.get -> ok`}},
		{"unknown", frameUnknown, []string{`[event] -> canceled`}},
		{"truncated", frameTrunc, []string{`[fetch] POST https://w.example/ -> ok [truncated]`}},
		{"unknown fields tolerated", frameExtraFld, []string{`[warn] not-an-array`, `[exception] TypeError: {"code":42}`, `[fetch] GET https://w.example/ -> ok`}},
	}
	f := NewFormatter()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seq seqCounter
			evs, err := decodeFrame([]byte(tc.frame), time.Now(), seq.next)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for i, ev := range evs {
				if ev.Time.UnixMilli() != 1790665119291 {
					t.Errorf("event %d time = %v", i, ev.Time)
				}
				if i > 0 && ev.Seq <= evs[i-1].Seq {
					t.Errorf("seq not increasing: %q then %q", evs[i-1].Seq, ev.Seq)
				}
				for _, l := range f.Format(ev) {
					got = append(got, l.Text)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestDecodeFrameMalformed(t *testing.T) {
	var seq seqCounter
	for _, bad := range []string{"", "not json", "[1,2]", `{"logs": 5}`, `"str"`} {
		if _, err := decodeFrame([]byte(bad), time.Now(), seq.next); err == nil {
			t.Errorf("decodeFrame(%q) succeeded", bad)
		}
	}
}

func TestSeqCounterOrder(t *testing.T) {
	var c seqCounter
	prev := c.next()
	for range 2000 {
		n := c.next()
		if n <= prev {
			t.Fatalf("%q <= %q", n, prev)
		}
		prev = n
	}
}
