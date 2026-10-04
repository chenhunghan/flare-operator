package testenv

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenhunghan/flare-operator/internal/fake"
)

// The Workers Logs control helpers work against a fake over HTTP.
func TestFakeControlWorkerLogs(t *testing.T) {
	fs := fake.New(fake.Options{})
	hs := httptest.NewServer(fs)
	defer hs.Close()
	fc := NewFakeControl(hs.URL)
	ctx := context.Background()
	acct := RandomAccountID()

	if err := fc.SetLogIngestionLag(ctx, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	res, err := fc.InjectWorkerLogs(ctx, acct, "w", fake.WorkerInvocation{Level: "error", Message: []any{"boom", 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.EventIDs) != 2 || res.VisibleAt.Before(before.Add(19*time.Second)) || res.TailFrames != 0 {
		t.Fatalf("inject: %+v", res)
	}
	if _, err := fc.InjectWorkerLogs(ctx, acct, "w", fake.WorkerInvocation{Level: "fatal"}); err == nil {
		t.Error("bad level accepted")
	}
	sess, err := fc.TailSessions(ctx, acct, "w")
	if err != nil || len(sess) != 0 {
		t.Fatalf("sessions: %v %v", sess, err)
	}
	if n, err := fc.DisconnectTails(ctx, acct, "w"); err != nil || n != 0 {
		t.Fatalf("disconnect: %d %v", n, err)
	}
}
