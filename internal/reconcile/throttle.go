package reconcile

import (
	"fmt"
	"net/http"
	"time"

	"github.com/chenhunghan/flare-operator/internal/cfclient"
)

// ReasonRateLimited is the Synced=False reason of an object whose reconcile hit Cloudflare's
// rate limit (HTTP 429, or the client refusing calls while the token backs off).
const ReasonRateLimited = "RateLimited"

// MinThrottleRequeue is the shortest requeue after a 429 (Retry-After 0 or missing).
const MinThrottleRequeue = time.Second

// Throttled reports whether err is a Cloudflare rate-limit answer and how long to wait before
// the next attempt: its Retry-After, at least MinThrottleRequeue. cfclient already waits out
// short Retry-After values inline and blocks the whole token, so an error reaching a reconciler
// means a long back-off: requeueing after it (instead of the controller's rate-limited retry,
// which starts at a few milliseconds) keeps a throttled operator from spinning.
func Throttled(err error) (time.Duration, bool) {
	ae, ok := cfclient.AsAPIError(err)
	if !ok || ae.Status != http.StatusTooManyRequests {
		return 0, false
	}
	return max(ae.RetryAfter, MinThrottleRequeue), true
}

// throttleMessage explains a 429 in a condition message.
func throttleMessage(err error, wait time.Duration) string {
	return fmt.Sprintf("Cloudflare rate limit reached for this account's token (HTTP 429); every client of the token pauses, next attempt in %s: %v",
		wait.Round(time.Second), err)
}
