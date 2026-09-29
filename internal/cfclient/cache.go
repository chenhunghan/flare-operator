package cfclient

import (
	"strings"
	"sync"
	"time"
)

// listCache caches successful GET responses whose result is a JSON array (collection reads) for
// a TTL. Any non-GET request invalidates every entry whose path is the written path, lies under
// it, or is one of its ancestors: a PUT /a/b/c drops cached GETs of /a/b/c, /a/b/c/…, /a/b and /a.
type listCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	path    string
	expires time.Time
	resp    Response
}

func newListCache(ttl time.Duration) *listCache {
	return &listCache{ttl: ttl, now: time.Now, entries: map[string]cacheEntry{}}
}

func (c *listCache) get(key string) (*Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return cloneResponse(&e.resp), true
}

func (c *listCache) put(key, path string, r *Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	// Opportunistic sweep keeps the map bounded by the working set.
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{path: normPath(path), expires: now.Add(c.ttl), resp: *cloneResponse(r)}
}

func (c *listCache) invalidate(path string) {
	p := normPath(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if related(e.path, p) {
			delete(c.entries, k)
		}
	}
}

func normPath(p string) string {
	return "/" + strings.Trim(p, "/")
}

// related reports whether a is b, under b, or an ancestor of b (segment-wise).
func related(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") || a == "/" || b == "/"
}

func cloneResponse(r *Response) *Response {
	out := *r
	if r.Result != nil {
		out.Result = append([]byte(nil), r.Result...)
	}
	if r.ResultInfo != nil {
		ri := *r.ResultInfo
		out.ResultInfo = &ri
	}
	if r.Header != nil {
		out.Header = r.Header.Clone()
	}
	return &out
}
