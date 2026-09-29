package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"flare.dev/operator/internal/cfclient"
	"flare.dev/operator/internal/fake"
	"flare.dev/operator/internal/reconcile"
)

const tagAcct = "0123456789abcdef0123456789abcdef"

// recordingClient records requests and can run a hook before each one (to simulate a
// concurrent writer).
type recordingClient struct {
	cfclient.Client
	mu     sync.Mutex
	reqs   []cfclient.Request
	before func(req cfclient.Request)
}

func (c *recordingClient) Do(ctx context.Context, req cfclient.Request) (*cfclient.Response, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	hook := c.before
	c.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	return c.Client.Do(ctx, req)
}

func (c *recordingClient) writes() []cfclient.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []cfclient.Request
	for _, r := range c.reqs {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

type tagFixture struct {
	fs     *fake.Server
	raw    cfclient.Client
	cf     *recordingClient
	target reconcile.TagTarget
}

func newTagFixture(t *testing.T) *tagFixture {
	t.Helper()
	fs := fake.New(fake.Options{})
	hs := httptest.NewServer(fs)
	t.Cleanup(hs.Close)
	raw, err := cfclient.New(cfclient.Options{Token: "tagger-" + t.Name(), BaseURL: hs.URL + "/client/v4", RPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := raw.Do(context.Background(), cfclient.Request{Method: "POST", Path: "/accounts/" + tagAcct + "/storage/kv/namespaces", Body: map[string]string{"title": "owned"}})
	if err != nil {
		t.Fatal(err)
	}
	var ns struct{ ID string }
	if err := json.Unmarshal(resp.Result, &ns); err != nil {
		t.Fatal(err)
	}
	return &tagFixture{fs: fs, raw: raw, cf: &recordingClient{Client: raw}, target: reconcile.TagTarget{Type: "kv_namespace", ID: ns.ID}}
}

// setTags writes tags directly (another tool / user).
func (f *tagFixture) setTags(t *testing.T, tags map[string]string) {
	t.Helper()
	if _, err := f.raw.Do(context.Background(), cfclient.Request{Method: "PUT", Path: "/accounts/" + tagAcct + "/tags",
		Body: map[string]any{"resource_type": f.target.Type, "resource_id": f.target.ID, "tags": tags}}); err != nil {
		t.Fatal(err)
	}
}

func (f *tagFixture) tags(t *testing.T) map[string]string {
	t.Helper()
	tags, _, err := reconcile.ResourceTagger{}.Get(context.Background(), f.raw, tagAcct, f.target)
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

func TestTaggerSendsIfMatch(t *testing.T) {
	f := newTagFixture(t)
	ctx := context.Background()
	f.setTags(t, map[string]string{"team": "x"})
	if err := (reconcile.ResourceTagger{}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a"); err != nil {
		t.Fatal(err)
	}
	w := f.cf.writes()
	if len(w) != 1 || w[0].Method != http.MethodPut || w[0].Header.Get("If-Match") == "" {
		t.Fatalf("writes %+v", w)
	}
	// Removing the last tag: DELETE with If-Match.
	f.setTags(t, map[string]string{reconcile.OwnerTagKey: "c/ns/a"})
	f.cf.reqs = nil
	if err := (reconcile.ResourceTagger{}).RemoveOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a"); err != nil {
		t.Fatal(err)
	}
	w = f.cf.writes()
	if len(w) != 1 || w[0].Method != http.MethodDelete || w[0].Header.Get("If-Match") == "" {
		t.Fatalf("writes %+v", w)
	}
}

func TestTaggerRetriesOn412(t *testing.T) {
	f := newTagFixture(t)
	ctx := context.Background()
	f.setTags(t, map[string]string{"team": "x"})
	// A concurrent writer changes the tags between our GET and our first PUT.
	var once sync.Once
	f.cf.before = func(req cfclient.Request) {
		if req.Method == http.MethodPut {
			once.Do(func() { f.setTags(t, map[string]string{"team": "x", "env": "prod"}) })
		}
	}
	if err := (reconcile.ResourceTagger{}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a"); err != nil {
		t.Fatal(err)
	}
	got := f.tags(t)
	if got["team"] != "x" || got["env"] != "prod" || got[reconcile.OwnerTagKey] != "c/ns/a" {
		t.Fatalf("concurrent tag lost: %v", got)
	}
	if n := len(f.cf.writes()); n != 2 {
		t.Errorf("PUTs = %d, want 2 (412 then success)", n)
	}

	// A writer that always races: bounded retries, then the 412 surfaces.
	f.setTags(t, map[string]string{"team": "x"})
	f.cf.reqs = nil
	i := 0
	f.cf.before = func(req cfclient.Request) {
		if req.Method == http.MethodPut {
			i++
			f.setTags(t, map[string]string{"team": "x", "n": string(rune('a' + i))})
		}
	}
	err := (reconcile.ResourceTagger{MaxConflictRetries: 2}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/b")
	if ae, ok := cfclient.AsAPIError(err); !ok || ae.Status != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %v", err)
	}
	if n := len(f.cf.writes()); n != 3 {
		t.Errorf("PUTs = %d, want 3 (1 + 2 retries)", n)
	}
	if got := f.tags(t); got[reconcile.OwnerTagKey] != "" {
		t.Errorf("owner written despite conflicts: %v", got)
	}
}

func TestTaggerAmbiguous500(t *testing.T) {
	ctx := context.Background()
	getFault := func(f *tagFixture, times int) {
		t.Helper()
		if err := f.fs.InjectFault(fake.Fault{Method: "GET", PathRegex: `/tags$`, Status: 500, Code: 1000, Message: "Internal Server Error", Times: times}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("transient-500-keeps-foreign-tags", func(t *testing.T) {
		f := newTagFixture(t)
		f.setTags(t, map[string]string{"team": "x"})
		getFault(f, 2) // the GET and its one cfclient retry both fail
		if err := (reconcile.ResourceTagger{}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a"); err != nil {
			t.Fatal(err)
		}
		if got := f.tags(t); got["team"] != "x" || got[reconcile.OwnerTagKey] != "c/ns/a" {
			t.Fatalf("foreign tag dropped: %v", got)
		}
		w := f.cf.writes()
		if len(w) != 1 || w[0].Header.Get("If-Match") == "" {
			t.Errorf("write not guarded: %+v", w)
		}
	})

	t.Run("transient-500-then-index-fails-writes-nothing", func(t *testing.T) {
		f := newTagFixture(t)
		f.setTags(t, map[string]string{"team": "x"})
		getFault(f, 2)
		if err := f.fs.InjectFault(fake.Fault{Method: "GET", PathRegex: `/tags/resources$`, Status: 503, Code: 1000, Message: "down", Times: 10}); err != nil {
			t.Fatal(err)
		}
		err := (reconcile.ResourceTagger{}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a")
		if err == nil {
			t.Fatal("no error")
		}
		var ae *cfclient.APIError
		if !errors.As(err, &ae) || ae.Status != 500 {
			t.Errorf("want the GET's 500 wrapped, got %v", err)
		}
		if w := f.cf.writes(); len(w) != 0 {
			t.Errorf("wrote from an ambiguous read: %+v", w)
		}
		if got := f.tags(t); len(got) != 1 || got["team"] != "x" {
			t.Errorf("tags changed: %v", got)
		}
	})

	t.Run("never-tagged-confirmed-by-index", func(t *testing.T) {
		f := newTagFixture(t)
		if err := (reconcile.ResourceTagger{}).EnsureOwner(ctx, f.cf, tagAcct, f.target, "c/ns/a"); err != nil {
			t.Fatal(err)
		}
		var sawIndex bool
		for _, r := range f.cf.reqs {
			if r.Path == "/accounts/"+tagAcct+"/tags/resources" && r.Query.Get("id") == f.target.ID && r.Query.Get("type") == f.target.Type {
				sawIndex = true
			}
		}
		w := f.cf.writes()
		if !sawIndex || len(w) != 1 || w[0].Header.Get("If-Match") != "" {
			t.Errorf("index=%v writes=%+v", sawIndex, w)
		}
		if got := f.tags(t); got[reconcile.OwnerTagKey] != "c/ns/a" || len(got) != 1 {
			t.Errorf("tags %v", got)
		}
	})
}
