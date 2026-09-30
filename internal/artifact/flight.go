package artifact

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// flightTimeout bounds one shared load. A shared load runs detached from its callers'
// contexts: one caller's timeout or cancellation must not fail the others that wait for the
// same content (possibly for other namespaces). It stops when the last waiting caller gives
// up, or after flightTimeout.
const flightTimeout = 5 * time.Minute

// flight is one shared load in progress.
type flight struct {
	done    chan struct{}
	tree    *Tree
	err     error
	waiters int
	cancel  context.CancelFunc
}

// flights deduplicates concurrent loads of the same content.
type flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

// do runs fn once for concurrent callers of key and returns its result to each of them. fn
// gets a context that carries ctx's values but not its cancellation; a caller whose ctx ends
// returns ctx.Err() at once, and the load is cancelled when no caller waits for it any more.
func (g *flights) do(ctx context.Context, key string, fn func(context.Context) (*Tree, error)) (*Tree, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flight{}
	}
	f, ok := g.m[key]
	if !ok {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		g.m[key] = f
		go func() {
			var t *Tree
			var err error
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("artifact load panicked: %v", r)
				}
				cancel()
				g.mu.Lock()
				f.tree, f.err = t, err
				if g.m[key] == f {
					delete(g.m, key)
				}
				g.mu.Unlock()
				close(f.done)
			}()
			t, err = fn(fctx)
		}()
	}
	f.waiters++
	g.mu.Unlock()
	select {
	case <-f.done:
		return f.tree, f.err
	case <-ctx.Done():
		g.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			// Nobody waits: stop the load, and let the next caller start a fresh one.
			f.cancel()
			if g.m[key] == f {
				delete(g.m, key)
			}
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}
