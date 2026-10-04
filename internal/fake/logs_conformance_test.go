package fake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The logs spike's telemetry dataset (docs/spike-results-2026-09-29.md §1), rebuilt for the
// conformance replay of the telemetry query recordings (0054, 0077…0098, 0122…0130):
//
//   - every event some recording returned, verbatim;
//   - stand-ins for the events no recording returned but whose existence the recorded series
//     prove: per 8 s bucket of 0092 (version 1, type cf-worker) and per minute of 0123
//     (version 2), as many events (and errors) as the bucket counts beyond the returned ones.
//     They are older than every event a recorded page returned, so they only show in series.
//
// Visibility follows the recorded ingestion lag: an event becomes queryable 15 s after its
// timestamp, except the three lag probes, whose recorded polls bracket their lag: the
// "laga6e44d" request (07:04:15.932) was missing at 07:04:30.649 (0080) and present at
// 07:04:34.296 (0081); "lagb81262" (07:04:41.520) missing at 07:05:06.825 (0087), present at
// 07:05:10.990 (0088); "lag664d2f" (07:10:59.285) missing at 07:11:21.993 (0129), present at
// 07:11:25.686 (0130).

const spikeAccount = "ACCOUNT_ID"

var spikeLagProbes = map[string]time.Duration{ // event ID prefix (the request's ULID time) → lag
	"01M3NZMY9W": 17 * time.Second,
	"01M3NZNQ9G": 27 * time.Second,
	"01M3P0186N": 25 * time.Second,
}

func readRecordingFile(name string) recording {
	raw, err := os.ReadFile(filepath.Join(recordingsDir, name))
	if err != nil {
		panic(err)
	}
	var rec recording
	if err := json.Unmarshal(raw, &rec); err != nil {
		panic(err)
	}
	return rec
}

type spikeQueryResult struct {
	Result struct {
		Events struct {
			Events []map[string]any `json:"events"`
			Series []struct {
				Time string `json:"time"`
				Data []struct {
					Count  int `json:"count"`
					Errors int `json:"errors"`
				} `json:"data"`
			} `json:"series"`
		} `json:"events"`
	} `json:"result"`
}

func spikeQuery(name string) spikeQueryResult {
	var r spikeQueryResult
	if err := json.Unmarshal(readRecordingFile(name).ResponseBody, &r); err != nil {
		panic(err)
	}
	return r
}

// seedSpikeLogs loads the dataset into s.
func seedSpikeLogs(s *Server) {
	files, _ := filepath.Glob(filepath.Join(recordingsDir, "*logs-telemetry-*.json"))
	byID := map[string]map[string]any{}
	for _, f := range files {
		rec := readRecordingFile(filepath.Base(f))
		if rec.Status != 200 || !strings.HasSuffix(rec.Path, "/telemetry/query") {
			continue
		}
		for _, ev := range spikeQuery(filepath.Base(f)).Result.Events.Events {
			byID[ev["$metadata"].(map[string]any)["id"].(string)] = ev
		}
	}
	known := make([]map[string]any, 0, len(byID))
	for _, ev := range byID {
		known = append(known, ev)
	}
	sort.Slice(known, func(i, j int) bool { return evID(known[i]) < evID(known[j]) })

	const v1, v2 = "2bbbf05d-a4dd-495b-bbb3-7121f176550e", "1b8ff0e5-5b98-4a00-9f78-f3894e14c37b"
	fill := append(standIns(known, "0092-logs-telemetry-query-tail4.json", 8000, v1),
		standIns(known, "0123-logs-telemetry-query-byversion.json", 60000, v2)...)

	for _, ev := range append(known, fill...) {
		ts := int64(ev["timestamp"].(float64))
		lag := 15 * time.Second
		if l, ok := spikeLagProbes[evID(ev)[:10]]; ok {
			lag = l
		}
		if err := s.InjectTelemetryEvents(spikeAccount, time.UnixMilli(ts).Add(lag), ev); err != nil {
			panic(err)
		}
	}
}

func evID(ev map[string]any) string { return ev["$metadata"].(map[string]any)["id"].(string) }

func evVersion(ev map[string]any) string {
	w, _ := ev["$workers"].(map[string]any)
	sv, _ := w["scriptVersion"].(map[string]any)
	id, _ := sv["id"].(string)
	return id
}

// standIns returns, per non-empty bucket of the recorded series (type cf-worker, one script
// version), the cf-worker events that the known events do not account for.
func standIns(known []map[string]any, file string, g int64, version string) []map[string]any {
	var template map[string]any
	type agg struct{ n, errs int }
	have := map[int64]*agg{}
	for _, ev := range known {
		md := ev["$metadata"].(map[string]any)
		if md["type"] != "cf-worker" || evVersion(ev) != version {
			continue
		}
		if template == nil {
			template = ev
		}
		b := int64(ev["timestamp"].(float64)) / g * g
		if have[b] == nil {
			have[b] = &agg{}
		}
		have[b].n++
		if md["level"] == "error" {
			have[b].errs++
		}
	}
	var out []map[string]any
	for _, sb := range spikeQuery(file).Result.Events.Series {
		if len(sb.Data) == 0 {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", sb.Time)
		if err != nil {
			panic(err)
		}
		b := t.UnixMilli()
		h := have[b]
		if h == nil {
			h = &agg{}
		}
		missing, missingErrs := sb.Data[0].Count-h.n, sb.Data[0].Errors-h.errs
		if missing < 0 || missingErrs < 0 || missingErrs > missing {
			panic(fmt.Sprintf("%s bucket %s: known events exceed the recorded counts", file, sb.Time))
		}
		for i := 0; i < missing; i++ {
			ts := b + int64(i) // within the bucket, older than any known event of it
			var ev map[string]any
			raw, _ := json.Marshal(template)
			_ = json.Unmarshal(raw, &ev)
			md := ev["$metadata"].(map[string]any)
			src := map[string]any{"message": fmt.Sprintf("stand-in %d of %s", i, sb.Time)}
			delete(md, "level")
			delete(md, "error")
			if i < missingErrs {
				src["level"] = "error"
				md["level"] = "error"
				md["error"] = src["message"]
			}
			md["message"] = src["message"]
			md["id"] = telemetryEventID(ts, 9000+i)
			ev["source"] = src
			ev["timestamp"] = float64(ts)
			out = append(out, ev)
		}
	}
	return out
}
