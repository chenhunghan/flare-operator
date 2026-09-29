package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The conformance suite replays sanitized recordings of the real Cloudflare API
// (test/recordings/) against flarefake and requires the same status, envelope (keys, null vs
// empty), errors, result, result_info, Ratelimit policy and Content-Type. Timestamps are
// compared by format/precision, secrets by presence and JSON type. It is what keeps the emulator
// honest: a profile change that diverges from real behavior fails here. See
// docs/testing-strategy.md §3.2.

const recordingsDir = "../../test/recordings/2026-09-29"

type recording struct {
	file            string
	TS              string            `json:"ts"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	RequestBody     json.RawMessage   `json:"request_body"`
	Status          int               `json:"status"`
	ResponseHeaders map[string]string `json:"response_headers"`
	ResponseBody    json.RawMessage   `json:"response_body"`
}

type scenario struct {
	name string
	// include selects recordings by label (file name without the NNNN- prefix).
	include func(label string) bool
	// before runs just before replaying the recording with the given label.
	before map[string]func(s *Server)
	opts   Options
}

const tunnelID = "5456b9ca-8b73-4b44-93df-67ad7acc8e2c"

func oneOf(l string, labels ...string) bool {
	for _, x := range labels {
		if l == x {
			return true
		}
	}
	return false
}

var scenarios = []scenario{
	{name: "kv", include: func(l string) bool {
		return strings.HasPrefix(l, "kv-") && !strings.HasPrefix(l, "kv-order") || l == "final-kv-namespaces"
	}},
	{name: "kv-order", include: func(l string) bool { return strings.HasPrefix(l, "kv-order") }},
	{name: "d1", opts: Options{D1Region: "APAC"}, include: func(l string) bool {
		return strings.HasPrefix(l, "d1-") || l == "final-d1"
	}},
	{name: "queues", include: func(l string) bool { return strings.HasPrefix(l, "q-") || l == "final-queues" }},
	{
		name: "tunnel+vpc",
		include: func(l string) bool {
			return strings.HasPrefix(l, "vpc-") && !strings.Contains(l, "worker") || strings.HasPrefix(l, "vnet-") ||
				oneOf(l, "final-virtual-networks", "final-tunnels-active", "final-vpc-services", "verify-tunnels", "verify-vpc")
		},
		before: map[string]func(s *Server){
			// The tunnel create also auto-creates the default virtual network (0160); queue its ID.
			"vpc-tunnel-create":            func(s *Server) { s.EnqueueIDs(tunnelID, "32c0a24c-c920-4a50-8475-643853700c5b") },
			"vpc-tunnel-connections-poll1": func(s *Server) { s.ConnectTunnel("ACCOUNT_ID", tunnelID, 1, 4) },
			"vpc-tunnel-get-after-kill":    func(s *Server) { s.DisconnectTunnel("ACCOUNT_ID", tunnelID) },
		},
	},
	{
		// In-cluster cloudflared spike (docs/spike-results-2026-09-29.md §2.1), plus the final
		// account-wide verification listings taken after it.
		name: "k8s",
		include: func(l string) bool {
			return strings.HasPrefix(l, "k8s-") && !strings.Contains(l, "worker") && !strings.Contains(l, "hyperdrive") ||
				oneOf(l, "final2-tunnels-active", "final2-vpc-services", "final2-kv", "final2-d1", "final2-queues", "final2-vnets")
		},
		before: map[string]func(s *Server){
			"k8s-tunnel-create":              func(s *Server) { s.EnqueueIDs(k8sTunnelID, "32c0a24c-c920-4a50-8475-643853700c5b") },
			"k8s-tunnel-connections":         func(s *Server) { s.ConnectTunnel("ACCOUNT_ID", k8sTunnelID, 1, 4) },
			"k8s-cleanup-tunnel-connections": func(s *Server) { s.DisconnectTunnel("ACCOUNT_ID", k8sTunnelID) },
		},
	},
}

const k8sTunnelID = "2c2ebad5-acd8-472f-8d20-bd5b78b34083"

func loadRecordings(t *testing.T) []recording {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(recordingsDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no recordings in %s: %v", recordingsDir, err)
	}
	sort.Strings(files)
	var out []recording
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var rec recording
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		rec.file = filepath.Base(f)
		out = append(out, rec)
	}
	return out
}

func label(file string) string { return strings.TrimSuffix(file[5:], ".json") }

func TestConformance(t *testing.T) {
	recs := loadRecordings(t)
	labels := map[string]bool{}
	for _, r := range recs {
		labels[label(r.file)] = true
	}
	used := map[string]bool{}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			for l := range sc.before {
				if !labels[l] {
					t.Fatalf("hook for unknown recording label %q", l)
				}
			}
			srv := New(sc.opts)
			hs := httptest.NewServer(srv)
			defer hs.Close()
			n := 0
			for _, rec := range recs {
				l := label(rec.file)
				if !sc.include(l) {
					continue
				}
				used[rec.file] = true
				n++
				if fn := sc.before[l]; fn != nil {
					fn(srv)
				}
				replay(t, srv, hs.URL, rec)
			}
			if n == 0 {
				t.Fatalf("scenario %s matched no recordings", sc.name)
			}
			if p := srv.ids.pending(); p != 0 {
				t.Errorf("%d queued IDs were never consumed — a create in the recordings has no emulated counterpart", p)
			}
			t.Logf("%s: replayed %d recordings", sc.name, n)
		})
	}
	// Any recording that hits an emulated route must be part of a scenario.
	probe := New(Options{})
	var unused []string
	for _, rec := range recs {
		if used[rec.file] {
			continue
		}
		path := rec.Path
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		if h, _, _ := probe.match(rec.Method, path); h != nil {
			t.Errorf("%s hits emulated route %s %s but no scenario replays it", rec.file, rec.Method, path)
		}
		unused = append(unused, label(rec.file))
	}
	t.Logf("recordings for surfaces not emulated yet (%d): %s", len(unused), strings.Join(unused, ", "))
}

func replay(t *testing.T, srv *Server, base string, rec recording) {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal(rec.ResponseBody, &want); err != nil {
		t.Fatalf("%s: bad recorded body: %v", rec.file, err)
	}
	// Reproduce server-assigned IDs so values (and ID-ordered lists) match exactly. Tunnel IDs
	// are queued by the scenario hook together with the default virtual network's ID.
	if rec.Method == http.MethodPost && rec.Status == 200 && !strings.Contains(rec.Path, "/cfd_tunnel") {
		if m, ok := want["result"].(map[string]any); ok {
			for _, k := range []string{"id", "uuid", "queue_id", "service_id"} {
				if id, ok := m[k].(string); ok {
					srv.EnqueueIDs(id)
					break
				}
			}
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, rec.TS); err == nil {
		srv.Clock.Set(ts)
	}

	var body []byte
	if len(rec.RequestBody) > 0 && string(rec.RequestBody) != "null" {
		var v any
		_ = json.Unmarshal(rec.RequestBody, &v)
		body, _ = json.Marshal(v) // compact, keys sorted — matches how the spike sent them
	}
	req, _ := http.NewRequest(rec.Method, base+"/client/v4"+rec.Path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", rec.file, err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("%s: decode: %v", rec.file, err)
	}

	where := fmt.Sprintf("%s (%s %s)", rec.file, rec.Method, rec.Path)
	if resp.StatusCode != rec.Status {
		t.Errorf("%s: status %d, recorded %d; errors=%v", where, resp.StatusCode, rec.Status, got["errors"])
		return
	}
	if g, w := sortedKeys(got), sortedKeys(want); g != w {
		t.Errorf("%s: envelope keys %s, recorded %s", where, g, w)
	}
	for _, k := range []string{"success", "errors", "messages", "result_info"} {
		if !reflect.DeepEqual(got[k], want[k]) {
			t.Errorf("%s: %s = %v, recorded %v", where, k, got[k], want[k])
		}
	}
	if g, w := normalize(rec.Path, got["result"]), normalize(rec.Path, want["result"]); !reflect.DeepEqual(g, w) {
		gj, _ := json.Marshal(g)
		wj, _ := json.Marshal(w)
		t.Errorf("%s: result\n  got  %s\n  want %s", where, gj, wj)
	}
	// The real header's r= value is almost always q-1 and does not track the 5-minute budget
	// (r=1198 was seen once, right after another call in the same second), so compare only the
	// presence and policy name.
	policy := func(h string) string { return strings.SplitN(h, ";", 2)[0] }
	if g, w := policy(resp.Header.Get("Ratelimit")), policy(rec.ResponseHeaders["Ratelimit"]); g != w {
		t.Errorf("%s: Ratelimit policy %q, recorded %q", where, g, w)
	}
	if g, w := resp.Header.Get("Content-Type"), rec.ResponseHeaders["Content-Type"]; w != "" && g != w {
		t.Errorf("%s: Content-Type %q, recorded %q", where, g, w)
	}
}

func sortedKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

var tsRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.(\d+))?Z$`)

// volatile keys: values legitimately differ between the real API and the emulator (secrets,
// sizes, connector metadata); their presence and JSON type are compared, not their value.
var volatile = map[string]bool{
	"token": true, "file_size": true, "bookmark": true, "arch": true, "colo_name": true,
}

// presenceOnly keys: recordings redact the whole value to the string "REDACTED", so even the
// JSON type of the real value is unknown.
var presenceOnly = map[string]bool{"credentials_file": true}

func normalize(path string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			switch {
			case presenceOnly[k]:
				out[k] = "<present>"
			case volatile[k] || strings.HasSuffix(path, "/connections") && (k == "version" || k == "features"):
				// connector metadata reported by cloudflared itself (0042: 2025.11.1, 0175: 2026.9.3)
				out[k] = fmt.Sprintf("<%s>", jsonType(val))
			case k == "connections" || k == "conns":
				out[k] = summarizeConns(val)
			default:
				out[k] = normalize(path, val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			n := normalize(path, el)
			if m, ok := n.(map[string]any); ok && strings.HasSuffix(path, "/connections") {
				m["id"] = "<client-id>" // connector IDs are generated by cloudflared, not the API
			}
			out[i] = n
		}
		return out
	case string:
		if strings.HasSuffix(path, "/token") {
			return "<token>" // GET …/token returns the bare token string (redacted in recordings)
		}
		if m := tsRe.FindStringSubmatch(x); m != nil {
			return fmt.Sprintf("<ts:%d>", len(m[2])) // keep sub-second precision, drop the value
		}
		return x
	}
	return v
}

func jsonType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	}
	return "null"
}

// summarizeConns reduces a connections array to its length and element key set.
func summarizeConns(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	var keys []string
	if len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
		}
	}
	return map[string]any{"len": len(arr), "keys": strings.Join(keys, ",")}
}
