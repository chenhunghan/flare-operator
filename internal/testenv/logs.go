package testenv

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/chenhunghan/flare-operator/internal/fake"
)

// InjectWorkerLogs records one invocation of script in account: its telemetry events become
// queryable after the fake's ingestion lag (or inv.IngestDelay) and its trace-v1 frame goes to
// the script's connected tails at once (POST /_fake/accounts/{account}/workers/{script}/logs).
func (f *FakeControl) InjectWorkerLogs(ctx context.Context, account, script string, inv fake.WorkerInvocation) (fake.InjectedLogs, error) {
	var out fake.InjectedLogs
	err := f.call(ctx, http.MethodPost, "accounts/"+url.PathEscape(account)+"/workers/"+url.PathEscape(script)+"/logs", inv, &out)
	return out, err
}

// SetLogIngestionLag sets how long injected log events take to become queryable.
func (f *FakeControl) SetLogIngestionLag(ctx context.Context, d time.Duration) error {
	return f.call(ctx, http.MethodPost, "log_ingestion_lag", map[string]string{"lag": d.String()}, nil)
}

// TailSessions lists the tails of script (deleted ones too) with their open connections.
func (f *FakeControl) TailSessions(ctx context.Context, account, script string) ([]fake.TailSession, error) {
	var out []fake.TailSession
	err := f.call(ctx, http.MethodGet, "accounts/"+url.PathEscape(account)+"/workers/"+url.PathEscape(script)+"/tails", nil, &out)
	return out, err
}

// DisconnectTails drops every WebSocket connection of script's tails and returns how many.
func (f *FakeControl) DisconnectTails(ctx context.Context, account, script string) (int, error) {
	var out struct {
		Disconnected int `json:"disconnected"`
	}
	err := f.call(ctx, http.MethodPost, "accounts/"+url.PathEscape(account)+"/workers/"+url.PathEscape(script)+"/tails/disconnect", nil, &out)
	return out.Disconnected, err
}
