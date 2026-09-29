package testenv

import (
	"context"
	"net/http/httptest"
	"testing"

	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
)

// FakeControl and the journal helpers work without envtest.
func TestFakeControlAndJournal(t *testing.T) {
	fs := fake.New(fake.Options{})
	hs := httptest.NewServer(fs)
	defer hs.Close()
	fc := NewFakeControl(hs.URL)
	ctx := context.Background()
	acct := RandomAccountID()

	if err := fc.AddToken(ctx, fake.Token{Value: "t1", AccountID: acct}); err != nil {
		t.Fatal(err)
	}
	cf, err := cfclient.New(cfclient.Options{Token: "t1", BaseURL: hs.URL + "/client/v4", RPS: 1000, Burst: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cf.Do(ctx, cfclient.Request{Method: "GET", Path: "/accounts/" + acct + "/tokens/verify"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cf.Do(ctx, cfclient.Request{Method: "POST", Path: "/accounts/" + acct + "/queues", Body: map[string]string{"queue_name": "q1"}}); err != nil {
		t.Fatal(err)
	}
	j, err := fc.Journal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(j) != 2 || len(Writes(j)) != 1 || Count(j, "POST", "/queues") != 1 || len(ForAccount(j, acct)) != 2 || len(ForAccount(j, RandomAccountID())) != 0 {
		t.Fatalf("journal:\n%s", Summary(j))
	}
	if err := fc.ClearJournal(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := fc.Journal(ctx); len(j) != 0 {
		t.Fatalf("not cleared: %v", j)
	}
	if err := fc.InjectFault(ctx, fake.Fault{Method: "GET", PathRegex: "/queues$", Status: 500, Times: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fc.ClearFaults(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fc.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fc.AddToken(ctx, fake.Token{}); err == nil {
		t.Error("empty token accepted by control API")
	}
}

func TestAssetsDiscovery(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", "/definitely/not/here")
	if d, ok := Assets(); ok || d != "/definitely/not/here" {
		t.Errorf("Assets() = %q, %v", d, ok)
	}
	if MissingAssetsMessage() == "" {
		t.Error("empty message")
	}
}
