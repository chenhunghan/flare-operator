package cfclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestWithBearerToken: a call made with WithBearerToken authenticates with that token (the
// Workers assets upload JWT), a Request.Header cannot do the same, other calls keep the
// client's token, a malformed token is ignored, and a GET with an override is neither served
// from nor stored in the list cache.
func TestWithBearerToken(t *testing.T) {
	var (
		mu   sync.Mutex
		auth []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		writeEnv(w, 200, `{"success":true,"errors":[],"messages":[],"result":[]}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, func(o *Options) { o.ListTTL = time.Minute })
	own := "Bearer tok-" + t.Name()
	bg := context.Background()
	calls := []struct {
		ctx  context.Context
		req  Request
		want string
	}{
		{WithBearerToken(bg, "jwt-1"), Request{Method: http.MethodPost, Path: "/a"}, "Bearer jwt-1"},
		{bg, Request{Method: http.MethodPost, Path: "/a"}, own},
		{WithBearerToken(bg, "bad\ntoken"), Request{Method: http.MethodPost, Path: "/a"}, own},
		{WithBearerToken(bg, ""), Request{Method: http.MethodPost, Path: "/a"}, own},
		{bg, Request{Method: http.MethodGet, Path: "/list"}, own}, // cached from here on
		{WithBearerToken(bg, "jwt-2"), Request{Method: http.MethodGet, Path: "/list"}, "Bearer jwt-2"},
	}
	for i, call := range calls {
		if _, err := c.Do(call.ctx, call.req); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		mu.Lock()
		got := auth[len(auth)-1]
		n := len(auth)
		mu.Unlock()
		if n != i+1 {
			t.Fatalf("call %d was not sent (served from the cache?)", i)
		}
		if got != call.want {
			t.Errorf("call %d: Authorization %q, want %q", i, got, call.want)
		}
	}
	// The plain GET is still cached (the override did not replace the cached entry).
	if _, err := c.Do(bg, Request{Method: http.MethodGet, Path: "/list"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != len(calls) {
		t.Errorf("the cached GET was sent again (%d requests)", len(auth))
	}
}
