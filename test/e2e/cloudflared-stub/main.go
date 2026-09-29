// Command cloudflared-stub stands in for cloudflared in e2e tests against flarefake, which has
// no tunnel edge to connect to. The Tunnel controller runs its connector image as
// `<image> tunnel --no-autoupdate --metrics 0.0.0.0:2000 run` with TUNNEL_TOKEN from the token
// Secret, and counts a replica as ready when GET :2000/ready answers 200.
//
// The stub ignores its arguments, decodes TUNNEL_TOKEN the way cloudflared does
// (base64 JSON {"a": account, "t": tunnel ID, "s": secret}) and serves /ready (200 only when
// the token decodes and names a tunnel) and /metrics on :2000. It never talks to Cloudflare,
// so the tunnel stays "inactive" in flarefake unless a test calls /_fake/.../connect.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type token struct {
	AccountTag   string `json:"a"`
	TunnelID     string `json:"t"`
	TunnelSecret string `json:"s"`
}

func decode(v string) (token, error) {
	var t token
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return t, fmt.Errorf("TUNNEL_TOKEN is not base64: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("TUNNEL_TOKEN is not a tunnel token: %w", err)
	}
	if t.AccountTag == "" || t.TunnelID == "" || t.TunnelSecret == "" {
		return t, errors.New("TUNNEL_TOKEN lacks a, t or s")
	}
	return t, nil
}

func main() {
	addr := os.Getenv("STUB_ADDR")
	if addr == "" {
		addr = ":2000"
	}
	tok, tokErr := decode(os.Getenv("TUNNEL_TOKEN"))
	if tokErr != nil {
		log.Printf("not ready: %v", tokErr)
	} else {
		log.Printf("stub connector for tunnel %s (account %s), args %q", tok.TunnelID, tok.AccountTag, os.Args[1:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if tokErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 503, "readyConnections": 0, "error": tokErr.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "readyConnections": 4, "connectorId": tok.TunnelID})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "# cloudflared-stub")
		fmt.Fprintln(w, "cloudflared_tunnel_ha_connections 4")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
