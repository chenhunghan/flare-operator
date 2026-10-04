package workerlogs

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
)

// SourceOption configures NewSource.
type SourceOption func(*cfSource)

// WithLogger sets the logger (default: discard). Tail URLs are never logged.
func WithLogger(l logr.Logger) SourceOption {
	return func(s *cfSource) { s.log = l }
}

// WithDialer sets the WebSocket dialer of tail connections (default: a dialer with the
// environment's proxy settings and a 15 s handshake timeout). The subprotocol is always
// trace-v1, whatever the dialer says.
func WithDialer(d *websocket.Dialer) SourceOption {
	return func(s *cfSource) {
		if d != nil {
			s.dialer = d
		}
	}
}

// tailTuning holds the follow machinery's timings; tests shrink them.
type tailTuning struct {
	// backoff is the reconnect schedule; after its last attempt fails the tail is given up.
	backoff []time.Duration
	// expiryMargin: a tail is replaced this long before its expires_at.
	expiryMargin time.Duration
	// minLifetime bounds how often a tail is replaced for expiry (a server reporting an
	// expires_at in the past must not make us create tails in a loop).
	minLifetime time.Duration
	// cleanupTimeout bounds the DELETE of a tail.
	cleanupTimeout time.Duration
	// stableAfter is how long a connection must have lived before its loss restarts the back-off
	// schedule; a tail that connects and then drops at once keeps climbing the schedule and is
	// given up after its last step (unlike wrangler, which resets on every open).
	stableAfter time.Duration
	// maxReconnects bounds the reconnects of one tail within reconnectWindow, however long each
	// connection lived; past it the follow ends with an error.
	maxReconnects   int
	reconnectWindow time.Duration
	// dedupWindow is how long, after a planned replacement, frames on the new tail that the old
	// one already delivered are dropped.
	dedupWindow time.Duration
	// subBuffer is each follower's event buffer (subscriberBuffer).
	subBuffer int
	// frameHook, when set, runs before each frame is decoded (tests inject panics with it).
	frameHook func([]byte)
}

func defaultTuning() tailTuning {
	return tailTuning{
		// wrangler's reconnect schedule (SOURCED:
		// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/index.ts#L31-L35).
		backoff: []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second},
		// A tail expires about 6 h after creation (0064). Cloudflare does not close the socket
		// at expiry, delivery just stops (SOURCED:
		// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/index.ts#L260-L264), so the
		// tail is replaced shortly before.
		expiryMargin:   2 * time.Minute,
		minLifetime:    5 * time.Minute,
		cleanupTimeout: 15 * time.Second,
		// Each reconnect costs a create and a delete against the token's budget (design §4.3).
		stableAfter:     30 * time.Second,
		maxReconnects:   10,
		reconnectWindow: 10 * time.Minute,
		dedupWindow:     30 * time.Second,
		subBuffer:       subscriberBuffer,
	}
}

// withTuning adjusts the timings (tests).
func withTuning(f func(*tailTuning)) SourceOption {
	return func(s *cfSource) { f(&s.tune) }
}

const (
	// subscriberBuffer is how many events a follower may lag behind before events are dropped
	// for it (with a notice); one slow reader never blocks the others.
	subscriberBuffer = 1024
	// frameBuffer is how many received frames wait for the follower loop; beyond it the socket
	// reader blocks (the server's own send buffer then applies back-pressure).
	frameBuffer = 4
	// maxFrameBytes bounds one tail frame (records are cut at 256 KB; DOCS:
	// https://developers.cloudflare.com/workers/observability/logs/workers-logs/).
	maxFrameBytes = 4 << 20
)

// NewSource returns the Cloudflare-backed Source (telemetry query + legacy tail).
func NewSource(l Limits, opts ...SourceOption) Source {
	s := &cfSource{
		limits: l.withDefaults(),
		log:    logr.Discard(),
		tune:   defaultTuning(),
		dialer: &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 15 * time.Second,
		},
		tails: map[tailKey]*upstream{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

type cfSource struct {
	limits Limits
	log    logr.Logger
	tune   tailTuning
	dialer *websocket.Dialer

	mu     sync.Mutex
	tails  map[tailKey]*upstream
	unique uint64
}

func (s *cfSource) now() time.Time { return time.Now() }

// Query implements Source.
func (s *cfSource) Query(ctx context.Context, t Target, q QueryOptions) (QueryResult, error) {
	return s.query(ctx, t, q)
}

// Follow implements Source.
func (s *cfSource) Follow(ctx context.Context, t Target) (Stream, error) {
	return s.follow(ctx, t)
}

// tailKey identifies a shared upstream tail: the account and script, and the client (one per
// token in the virtual kubelet), so a follower never receives events through a tail that a
// different token opened. A client whose dynamic type is not comparable is never shared.
type tailKey struct {
	account, script string
	client          any
	unique          uint64
}

func (s *cfSource) keyFor(t Target) tailKey {
	k := tailKey{account: t.AccountID, script: t.Script}
	if t.Client != nil && reflect.TypeOf(t.Client).Comparable() {
		k.client = t.Client
	} else {
		s.unique++
		k.unique = s.unique
	}
	return k
}
