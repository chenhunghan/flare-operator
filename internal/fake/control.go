package fake

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// serveControl implements the test control API (docs/testing-strategy.md §3.3):
//
//	POST   /_fake/reset
//	GET    /_fake/journal                      DELETE /_fake/journal
//	GET    /_fake/response_violations          (Options.ValidateResponses; allowlisted ones marked "allowed")
//	POST   /_fake/clock   {"set":"RFC3339"} | {"advance":"90s"} | {"real":true}
//	POST   /_fake/ids     {"ids":["…"]}
//	POST   /_fake/faults  Fault                DELETE /_fake/faults
//	POST   /_fake/accounts/{account}/tunnels/{id}/connect     {"replicas":1,"connections":4}
//	POST   /_fake/accounts/{account}/tunnels/{id}/disconnect
//	POST   /_fake/tokens  Token                DELETE /_fake/tokens   (tokens.go)
func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/_fake/"), "/"), "/")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	bad := func(msg string) { writeJSON(http.StatusBadRequest, map[string]string{"error": msg}) }

	switch {
	case parts[0] == "reset" && r.Method == http.MethodPost:
		s.Reset()
		writeJSON(http.StatusOK, map[string]bool{"ok": true})

	case parts[0] == "journal" && r.Method == http.MethodGet:
		writeJSON(http.StatusOK, s.Journal())
	case parts[0] == "journal" && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.journal = nil
		s.mu.Unlock()
		writeJSON(http.StatusOK, map[string]bool{"ok": true})

	case parts[0] == "response_violations" && r.Method == http.MethodGet:
		vs := s.ResponseViolations()
		if vs == nil {
			vs = []ResponseViolation{}
		}
		writeJSON(http.StatusOK, vs)

	case parts[0] == "clock" && r.Method == http.MethodPost:
		var req struct {
			Set     string `json:"set"`
			Advance string `json:"advance"`
			Real    bool   `json:"real"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			bad(err.Error())
			return
		}
		switch {
		case req.Real:
			s.Clock.Real()
		case req.Set != "":
			t, err := time.Parse(time.RFC3339Nano, req.Set)
			if err != nil {
				bad(err.Error())
				return
			}
			s.Clock.Set(t)
		case req.Advance != "":
			d, err := time.ParseDuration(req.Advance)
			if err != nil {
				bad(err.Error())
				return
			}
			s.Clock.Advance(d)
		}
		writeJSON(http.StatusOK, map[string]string{"now": s.Clock.Now().Format(time.RFC3339Nano)})

	case parts[0] == "ids" && r.Method == http.MethodPost:
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			bad(err.Error())
			return
		}
		s.EnqueueIDs(req.IDs...)
		writeJSON(http.StatusOK, map[string]int{"queued": len(req.IDs)})

	case parts[0] == "faults" && r.Method == http.MethodPost:
		var f Fault
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			bad(err.Error())
			return
		}
		if err := s.InjectFault(f); err != nil {
			bad(err.Error())
			return
		}
		writeJSON(http.StatusOK, map[string]bool{"ok": true})
	case parts[0] == "faults" && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.faults = nil
		s.mu.Unlock()
		writeJSON(http.StatusOK, map[string]bool{"ok": true})

	case parts[0] == "tokens" && r.Method == http.MethodPost:
		var t Token
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.Value == "" {
			bad("need a JSON Token with a non-empty \"token\"")
			return
		}
		s.AddToken(t)
		writeJSON(http.StatusOK, map[string]bool{"ok": true})
	case parts[0] == "tokens" && r.Method == http.MethodDelete:
		s.ClearTokens()
		writeJSON(http.StatusOK, map[string]bool{"ok": true})

	case len(parts) == 5 && parts[0] == "accounts" && parts[2] == "tunnels" && r.Method == http.MethodPost:
		acct, id, action := parts[1], parts[3], parts[4]
		var found bool
		switch action {
		case "connect":
			req := struct {
				Replicas    int `json:"replicas"`
				Connections int `json:"connections"`
			}{Replicas: 1, Connections: 4}
			_ = json.NewDecoder(r.Body).Decode(&req)
			found = s.ConnectTunnel(acct, id, req.Replicas, req.Connections)
		case "disconnect":
			found = s.DisconnectTunnel(acct, id)
		default:
			bad("unknown tunnel action " + action)
			return
		}
		if !found {
			writeJSON(http.StatusNotFound, map[string]string{"error": "tunnel not found"})
			return
		}
		writeJSON(http.StatusOK, map[string]bool{"ok": true})

	default:
		writeJSON(http.StatusNotFound, map[string]string{"error": "unknown control endpoint"})
	}
}
