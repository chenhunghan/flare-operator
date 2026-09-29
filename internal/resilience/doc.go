// Package resilience holds the operator's resilience tests (docs/resilience.md), run against
// envtest and the in-process flarefake:
//
//   - crash consistency: the manager dies between a Cloudflare create and the write of the new
//     ID to the object; after a restart exactly one Cloudflare resource exists and the object
//     has adopted it (every managed kind);
//   - fault tolerance: 429 storms with Retry-After, 5xx bursts, slow responses and timeouts,
//     malformed and non-envelope bodies, a failing page of a paginated list; bounded API calls
//     and eventual convergence;
//   - scale: many mixed objects under the default rate limit, with the API-call budget and the
//     steady-state polling cost;
//   - metrics: the manager's Prometheus endpoint exports the Cloudflare client and reconcile
//     metrics with bounded labels.
//
// The package has no non-test code.
package resilience
