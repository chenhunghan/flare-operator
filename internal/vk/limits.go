package vk

import (
	"errors"
	"sync"
	"time"
)

// ServerLimits bound what callers of the kubelet API can hold: connections, concurrent log
// requests, follow sessions per Worker and per namespace, and how long a stream may stall or
// last. Zero fields take the defaults below.
type ServerLimits struct {
	// MaxConnections caps open connections to the kubelet API (flag --max-connections).
	MaxConnections int
	// MaxConcurrentRequests caps concurrent non-follow log requests
	// (--logs-max-concurrent-requests).
	MaxConcurrentRequests int
	// MaxFollowersPerScript and MaxFollowersPerNamespace cap concurrent `kubectl logs -f` sessions
	// of one Worker (account and script) and of one namespace (--logs-max-followers-per-script,
	// --logs-max-followers-per-namespace). The process-wide cap is workerlogs.Limits.MaxFollowers.
	MaxFollowersPerScript    int
	MaxFollowersPerNamespace int
	// WriteTimeout ends a stream whose reader has not accepted a write for this long
	// (--logs-write-timeout).
	WriteTimeout time.Duration
	// MaxStreamDuration ends any log stream after this long (--logs-max-stream-duration);
	// `kubectl logs -f` then exits and can be run again.
	MaxStreamDuration time.Duration
}

// Defaults of ServerLimits.
const (
	DefaultMaxConnections           = 1000
	DefaultMaxConcurrentRequests    = 32
	DefaultMaxFollowersPerScript    = 10
	DefaultMaxFollowersPerNamespace = 25
	DefaultWriteTimeout             = 60 * time.Second
	DefaultMaxStreamDuration        = 4 * time.Hour
)

// WithDefaults fills zero fields.
func (l ServerLimits) WithDefaults() ServerLimits {
	if l.MaxConnections <= 0 {
		l.MaxConnections = DefaultMaxConnections
	}
	if l.MaxConcurrentRequests <= 0 {
		l.MaxConcurrentRequests = DefaultMaxConcurrentRequests
	}
	if l.MaxFollowersPerScript <= 0 {
		l.MaxFollowersPerScript = DefaultMaxFollowersPerScript
	}
	if l.MaxFollowersPerNamespace <= 0 {
		l.MaxFollowersPerNamespace = DefaultMaxFollowersPerNamespace
	}
	if l.WriteTimeout <= 0 {
		l.WriteTimeout = DefaultWriteTimeout
	}
	if l.MaxStreamDuration <= 0 {
		l.MaxStreamDuration = DefaultMaxStreamDuration
	}
	return l
}

// Errors of the request limits (429).
var (
	errTooManyRequests           = errors.New("too many concurrent log requests on this virtual kubelet; retry later")
	errTooManyFollowersScript    = errors.New("too many concurrent `kubectl logs -f` sessions for this Worker; retry later")
	errTooManyFollowersNamespace = errors.New("too many concurrent `kubectl logs -f` sessions in this namespace; retry later")
	errTooManyAuthFailures       = errors.New("too many failed authentication attempts; retry later")
	errAuthUnavailable           = errors.New("authentication temporarily unavailable; retry later")
	errInvalidTokenCached        = errors.New("token rejected recently")
)

// counter is a set of bounded counters by key.
type counter struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

func newCounter(max int) *counter { return &counter{n: map[string]int{}, max: max} }

// acquire takes one slot of key, reporting false when the key is at its maximum.
func (c *counter) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key] >= c.max {
		return false
	}
	c.n[key]++
	return true
}

func (c *counter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key]--; c.n[key] <= 0 {
		delete(c.n, key)
	}
}

// followLimiter enforces the per-script and per-namespace follow caps.
type followLimiter struct {
	script, namespace *counter
}

func newFollowLimiter(l ServerLimits) *followLimiter {
	return &followLimiter{script: newCounter(l.MaxFollowersPerScript), namespace: newCounter(l.MaxFollowersPerNamespace)}
}

// acquire takes a follow slot for the script key and namespace; release frees both.
func (f *followLimiter) acquire(scriptKey, namespace string) (release func(), err error) {
	if !f.namespace.acquire(namespace) {
		return nil, errTooManyFollowersNamespace
	}
	if !f.script.acquire(scriptKey) {
		f.namespace.release(namespace)
		return nil, errTooManyFollowersScript
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.script.release(scriptKey)
			f.namespace.release(namespace)
		})
	}, nil
}
