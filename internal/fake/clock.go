package fake

import (
	"sync"
	"time"
)

// Clock is the emulator's notion of time. Tests control it through the /_fake/clock API so that
// rate-limit windows, sunsets and async state machines are deterministic.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
	frozen *time.Time
}

// Now returns the current emulated time (UTC).
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen != nil {
		return *c.frozen
	}
	return time.Now().UTC().Add(c.offset)
}

// Set freezes the clock at t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t = t.UTC()
	c.frozen = &t
}

// Advance moves the clock forward by d (frozen or not).
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen != nil {
		t := c.frozen.Add(d)
		c.frozen = &t
		return
	}
	c.offset += d
}

// Real unfreezes the clock and clears any offset.
func (c *Clock) Real() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = nil
	c.offset = 0
}

// Cloudflare timestamps are RFC 3339 in UTC with microseconds on most products
// (e.g. "2026-09-29T06:57:23.776651Z"), milliseconds on D1 and whole seconds on Workers VPC.
func tsMicro(t time.Time) string  { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }
func tsMilli(t time.Time) string  { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func tsSecond(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
