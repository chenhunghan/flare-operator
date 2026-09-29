# Operator resilience, API budget and metrics

What the operator guarantees when things go wrong between it, the Kubernetes API server and
Cloudflare, what it costs in Cloudflare API calls, and how to watch it. Everything here is
tested against envtest and the in-process flarefake (`internal/resilience`, `go test
./internal/resilience/`); nothing was checked against the live API (production readiness
through the emulator, 2026-09-29 decision). Emulator behavior these tests lean on is marked
UNVERIFIED where the recordings do not show it.

## 1. Crash consistency: create, then record

A create is two steps: the Cloudflare call, then the write of the new ID to the object
(`reconcile.RecordCreated`, the external-id and ownership-proof annotations, written without an
optimistic lock). A manager that dies, is evicted or loses the API server between the two leaves
a Cloudflare resource that the object does not know. The rule is: **never a second resource,
never a lost one** — the next reconcile must find that resource and adopt it.

| Kind | How the lost create is found | Names unique in the API? |
|---|---|---|
| KVNamespace, Queue, D1Database, VectorizeIndex, SecretsStore | adoption by `NameField` from the list (generic reconciler) | KV 10014 (0004), Queues and D1 (7502) yes; Vectorize: the name is the ID; SecretsStore UNVERIFIED |
| AIGateway | **new:** the create body carries the ID (`IDField` in `CreateFields`, no `NameField`), so the reconciler GETs that ID before creating (`findByClientID`). Before, the re-sent POST got 409/10010 forever. | the ID is client-chosen |
| Tunnel, tagging on | adoption by name after the owner tag is written (untagged → claimed) | UNVERIFIED (not recorded; flarefake allows duplicates) |
| Tunnel, tagging off | **new:** create-pending record (below). Before: NameConflict for its own tunnel. | UNVERIFIED |
| VPCService | **new:** create-pending record. Before: NameConflict for its own service (the documented known gap). | yes, 5101 (0059) |
| WorkerScript | **new:** create-pending record carrying the upload's hashes. Before: NameConflict (no owner tag yet) or, with the tag written, a second upload of the same content. | the name is the ID (PUT) |

**Create-pending record** (`internal/reconcile/pending.go`). Kinds that never adopt a same-named
resource on their own write `cloudflare.flare.dev/create-pending: <uid>/<key>` right before the
Cloudflare create, after a lookup found nothing (a merge patch with a UID precondition, like
RecordCreated). After a crash, a resource with that name is the object's own lost create: it is
adopted with RecordCreated, which also clears the record (RecordOwnership and PersistExternalID
clear it too). A create the API refuses (a permanent 4xx) clears the record, so a later
same-named resource of someone else is not adopted. WorkerScript's key also carries the hashes
of the content, settings and secrets it uploaded, so the adopted script is not uploaded again.
With tagging, a readable owner tag naming another object still wins (NameConflict).

Residual risks, by design:

- Someone creating a same-named resource between the object's "not found" lookup and its own
  create call (one API round trip) would be adopted as the object's own (create-pending kinds),
  or, for kinds that adopt by name anyway, adopted like any same-named resource.
- The generic reconciler adopts by name without a create-pending record, and without an owner tag
  (tagging off, or an untaggable kind: VectorizeIndex, SecretsStore, AIGateway) the adoption only
  pins the ID; it records no ownership proof. The pin is what lets deletion proceed
  (`reconcile.MayDeleteExternal`).
- If a list lags a create (eventual consistency, UNVERIFIED for every product), a lost create can
  be missed and re-sent: for unique names the API refuses the duplicate and the next reconcile
  adopts; for Tunnel names (uniqueness UNVERIFIED) a duplicate is possible.
- A POST whose answer is lost (client timeout, connection reset) is never retried by the client
  (not idempotent); the next reconcile's lookup adopts what it made (tested: "lost create
  response adopted").

Tested by `TestCrashBetweenCreateAndRecord` for every kind (tagged and untagged where it
matters, WorkerScript also with a failed tag write): a wrapped client "crashes" the reconciler at
the first patch that writes the external-id annotation — after the Cloudflare create — and fails
every later call; the manager is stopped and a fresh one started. The test asserts exactly one
Cloudflare resource, exactly one create call in the flarefake journal, the object's external-id
and ownership record, no leftover create-pending record, and for WorkerScript no second upload.
With the fixes disabled the AIGateway, VPCService, Tunnel/untagged and all three WorkerScript
cases fail.

## 2. Faults

| Fault | Behavior | Test |
|---|---|---|
| 429 with Retry-After ≤ 30 s (`cfclient.MaxInlineWait`) | waited out inline; the whole token (every client built with it) pauses until Retry-After has passed; at most 8 consecutive 429 retries per call | `TestRateLimitStorm`: no request of the token reaches the API within Retry-After of a 429 |
| 429 with a longer Retry-After | the call fails at once; later calls of the token are refused locally until then (`cloudflare_api_throttled_total{source="client"}`); the object shows `Synced=False`, reason **RateLimited**, with the wait in the message, and is requeued after Retry-After instead of the controller's millisecond retry backoff (no hot loop); deletions likewise | `TestLongRetryAfter` |
| 5xx, transport errors | idempotent methods (GET, PUT, DELETE) retried up to `spec.rateLimit.maxRetries` (default 4) with full-jitter back-off (250 ms … 10 s); POST/PATCH are not retried by the client, the next reconcile looks the name up first | `Test5xxBurst` |
| slow answers | per-request HTTP timeout (`--cloudflare-request-timeout`, default 60 s) and a context deadline per reconcile (`--reconcile-timeout`, default 5 m) | `TestSlowResponses` (read timeout, lost create response, reconcile deadline cutting a 20 s stall) |
| malformed or non-envelope bodies | errors, never "empty": an HTML 200 or a non-array result on a list fails the lookup, so nothing is created from it; a truncated create answer is an error and the next reconcile adopts or creates | `TestMalformedBodies`, `FuzzDecodeEnvelope`, `FuzzListAll` |
| one page of a paginated list fails | `cfclient.ListAll` fails as a whole; a partial list is never taken as "not found" | `TestPartialListFailure`: a namespace on page 2 is adopted, never duplicated |

flarefake's `Fault` gained test-only shapes for this (`internal/fake/fault.go`): `retry_after`,
`delay_ms`, `passthrough` (serve the request, answer late), a raw `body`, and `query_regex`.

## 3. API-call budget and polling cost

The client limits itself per token to `cfclient.DefaultRPS` 3.6 calls/s (burst 20): 90 % of
Cloudflare's 1200 calls per 5 minutes, leaving room for other users of the token. That is
**12 960 calls/hour**. `spec.rateLimit` on the CloudflareAccount changes it per account.

Measured costs (flarefake journal, `TestScale`, ownership tags on):

| | calls |
|---|---|
| create of a tagged kind (KVNamespace, Queue, D1Database) | ≤ 11 (lookup list, POST, tag read of a never-tagged resource: 500, retry, tag index, tag PUT, GET, the sync's GET and tag read, and the re-reconcile the annotation write triggers) |
| create of an untaggable generic kind (VectorizeIndex, SecretsStore, AIGateway) | ≤ 7 |
| one drift poll of an in-sync object | 2 for tagged kinds (GET item, owner-tag read), 1 for untaggable ones; no write |

Steady state per hour for N in-sync objects polled every P: `N × callsPerPoll × 3600 / P`.

| objects (tagged) | P = 5 m (default) | P = 10 m | P = 15 m |
|---|---|---|---|
| 100 | 2 400/h (19 %) | 1 200/h | 800/h |
| 300 | 7 200/h (56 %) | 3 600/h | 2 400/h |
| 500 | 12 000/h (93 %) | 6 000/h (46 %) | 4 000/h (31 %) |

Percentages are of the default 12 960/h client budget. Above about 300 tagged objects per token
at the 5-minute default, drift polling crowds out creates and updates; raise
`--poll-interval` (chart `reconcile.pollInterval`), split the objects across tokens, or raise
`spec.rateLimit` if the token is not shared. Polls carry up to 10 % random jitter, so objects
created together do not poll in lockstep.

Scale runs (`TestScale`, one account, default client rate limit, mixed generated kinds):

- 24 objects (the CI size): converged in 41 s with 165 calls (6.9/object, no 429); steady state
  1.50 calls per poll.
- 500 objects (`FLARE_SCALE_OBJECTS=500 go test -run TestScale -timeout 60m
  ./internal/resilience/`): SCALE500

## 4. Flags and chart values

| flag | chart value | default |
|---|---|---|
| `--poll-interval` | `reconcile.pollInterval` | 0: controller defaults (5 m generated kinds, 10 m Tunnel, VPCService, WorkerScript); minimum 10 s |
| `--max-concurrent-reconciles` | `reconcile.maxConcurrentReconciles` | 1 per controller (all workers of a token share its rate limit) |
| `--reconcile-timeout` | `reconcile.timeout` | 5 m |
| `--cloudflare-request-timeout` | `reconcile.cloudflareRequestTimeout` | 60 s |

## 5. Metrics

On the manager's metrics endpoint (`--metrics-bind-address`, chart `metrics.port`), with
controller-runtime's own (`controller_runtime_reconcile_total`, `_errors_total`,
`_reconcile_time_seconds`, workqueue metrics). Labels never carry IDs or names: `route_template`
is the pinned spec's path template (`cfclient.RouteTemplate`, generated from
`spec/openapi.json.gz` into `internal/cfclient/zz_generated_routes.go`), or `other`.

| metric | labels | meaning |
|---|---|---|
| `cloudflare_api_requests_total` | method, route_template, code | HTTP attempts (retries included); code `error` for a transport failure |
| `cloudflare_api_request_duration_seconds` | method, route_template | latency of one attempt |
| `cloudflare_api_rate_limit_wait_seconds` | reason (`limiter`, `retry_after`) | time a call waited before being sent |
| `cloudflare_api_throttled_total` | source (`api`, `client`) | 429 answers; calls refused locally while the token backs off |
| `cloudflare_api_retries_total` | method, route_template, reason (`429`, `5xx`, `transport`) | retries |
| `cloudflare_api_list_cache_hits_total`, `_misses_total` | | list cache (`spec.rateLimit.listCacheTTL`) |
| `flare_managed_sync_failures_total` | kind, reason | reconciles that ended with Synced=False (RateLimited, ReconcileError, AccountNotReady, NameConflict, Immutable, …), including those reported through conditions and a timed requeue rather than an error |

`TestMetricsEndpoint` scrapes a manager and checks each of them and that no label carries a
32-hex ID.

## 6. Fuzzing

Seeds run with every `go test`; longer runs are manual (`go test -run '^$' -fuzz=<Target>
<package>`, with `KUBEBUILDER_ASSETS=/nonexistent` so envtest does not start in every fuzz
worker): `FuzzDecodeEnvelope`, `FuzzRouteTemplate`, `FuzzListAll` (internal/cfclient),
`FuzzCovers`, `FuzzWriteOnlyHashAndWithout` (internal/generic), `FuzzBuildMultipart`
(internal/controller/workerscript; it found that a module name with invalid UTF-8 would reach
the metadata as U+FFFD and no longer name its part, now rejected; the input is kept as a seed).
