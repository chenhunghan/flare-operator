# Testing strategy: emulating the Cloudflare API and handling API versions

Status: **draft, 2026-09-29**

## 1. Constraints

- **No stateful Cloudflare API emulator exists.**
  - LocalStack's Cloudflare extension only covers Workers deploys and needs LocalStack Pro.
  - Stoplight Prism serves responses shaped by the OpenAPI schemas but keeps no state, so a create followed by a get doesn't work.
  - cf-terraforming uses go-vcr recordings.

  We have to build our own emulator.
- **Cloudflare's API has one major version.** The spec says `info.version: 4.0.0`, served under `/client/v4`, and there is no version header in the style of Stripe. Instead, the API evolves through:
  1. **additive changes** (new fields, new endpoints)
  2. **per-product version segments in paths** (123 paths contain `/v2/`, 17 `/v1/`, 14 `/v3/`; examples are `vectorize/v2`, `pipelines/v1` and `alerting/v3`)
  3. **deprecations with end dates**, annotated in the spec:
     - `x-cfDeprecation.eol` on 30 operations
     - `x-forge-sunset.date` on 33
     - `x-stainless-deprecation-message` on 189
  4. **behavior and field changes**, sometimes announced only in the changelog, e.g. DNS record type changes blocked on 2026-06-30 and the tunnel `connections` field removed on 2026-10-05

So for us, **"API version" means a spec snapshot (the `cloudflare/api-schemas` commit) plus a date** (after which sunsets take effect).

**`cf --local` was checked on 2026-09-29 and it is not a control-plane emulator.** Its Miniflare "local explorer" only implements data-plane routes (KV values, R2 objects, workflow instances). The first behavior profiles are recorded in [spike-results-2026-09-29.md §3](spike-results-2026-09-29.md), with sanitized cassettes in `test/recordings/2026-09-29/`.

### 1.1 What Cloudflare's own SDK and Terraform provider do (checked 2026-09-29)

| Project | How it tests against the API | What we take from it |
|---|---|---|
| `cloudflare-go` (v7, generated) | `scripts/test` starts **Prism** (`@stainless-api/prism-cli@5.15.0`) on the OpenAPI spec, and the generated tests call `localhost:4010`. It only checks request and response shapes. **495 of 2,718 tests are skipped** (`t.Skip("TODO: investigate broken test")` ×190, `"HTTP 404 error from prism"` ×30, `"TODO: HTTP 401 from prism"` ×27, auth problems ×50). | Validating requests against the schema is valuable, so `flarefake` does it with kin-openapi. A **stateless mock is not enough**, which is why the emulator keeps state. |
| `terraform-provider-cloudflare` (v5) | No emulator. **Acceptance tests run live** (`TF_ACC=1`) as about 20 product-area matrix jobs (zone-core, dns, workers-core, …), **each with its own test user and account**. IDs come from env vars (`CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_ZONE_ID`, an "alt" zone). | Split L3 by product area, with a separate token or account per job, so each job has its own rate-limit budget and resources. Pass account and zone IDs in through env. |
| Terraform sweepers | `resource.AddTestSweepers` for each resource. `utils.ShouldSweepResource` deletes only names with the test prefix, unless `SWEEP_DANGEROUSLY_DELETE_ALL=true`. `scripts/lint-sweepers` makes sure every resource has one. | `flare-janitor` works the same way (prefix plus tag). CI fails if a generated kind has no janitor sweeper. |
| Drift tests | `testdata/*drift*.tf`, for example `dns_record_fqdn_normalize`, `cname_case`, `modified_on_drift_caa`, `settings_drift`, `tags_drift`. Step 2 re-applies the same config and expects no changes. | **Idempotency tests**: reconcile twice and check through the emulator's request journal that the second pass made **zero writes**. The provider's drift test fixtures are a **list of known API normalization quirks** (FQDN, case, computed fields). Mine them for `flarefake` profiles and diff normalizers. |
| Schema parity | Generated `TestXModelSchemaParity` compares each model struct with its schema. | Generated parity tests: Go type ↔ CRD OpenAPI schema ↔ Cloudflare OpenAPI schema. |
| Migration tests | `migration/v500` state upgraders, `scripts/check-schema-versions` (every schema must be version 500 or higher), and a `migration-tests.yml` job per product area. | CRD conversion and storage-version upgrade tests (§4.4). Tests create objects with release N−1 and reconcile them with release N. |

## 2. Test layers

| Layer | What it tests | Backend | Runs |
|---|---|---|---|
| **L0 unit** | Pure logic: spec-vs-observed diff, Pod → application translation, status mapping, reference resolution | none | every commit |
| **L1 emulator** | Controllers and virtual kubelet end to end: create, update, drift, delete, errors, rate limits | `flarefake`: an emulator generated from the pinned spec. It runs in-process (`httptest`) with envtest, or as a container in kind | every commit |
| **L2 replay** | Tricky flows exactly as the real API behaved | go-vcr cassettes recorded against the real account | every commit |
| **L3 live** | That the whole thing works against the real Cloudflare API | dedicated test account | nightly, and on release branches |
| Worker runtime | The generated front-door router Worker | Miniflare / `vitest-pool-workers` (JS) | every commit |

**One set of tests, two backends.** L1 and L3 scenarios are the same Go tests. Only the API base URL, token and cleanup differ (`FLARE_TEST_TARGET=fake|live`). If a scenario passes against the emulator but fails live, we've found an emulator bug and it gets fixed in the emulator.

## 3. `flarefake`: the emulator

### 3.0 Implementation status (2026-09-29)

`internal/fake` + `cmd/flarefake` implement:

- **Profiles:** KV namespaces (plus the legacy path until its sunset), D1 (control plane), Queues, Tunnels (including config, token, connections, soft delete and the auto-created default virtual network), virtual networks, and Workers VPC services.
- **Engine:** per-token rate limiting (429 responses, with headers matching the real API), fault injection, a request journal, a fake clock (which drives the KV legacy-path and tunnel `connections` sunsets), queued IDs, spec request validation (journaled, or rejected as 400/10001), and the `/_fake` control API.
- **Conformance:** `TestConformance` replays **122 real-API recordings** across 6 scenarios, and all of them match. It checks status, envelope keys (including `null` versus `[]`), errors, the result (timestamps compared by precision; secrets compared by presence and type), `result_info`, rate-limit policy and `Content-Type`.
  - It **fails** if a recording hits an emulated route that no scenario replays, if a hook label doesn't exist, or if a queued ID is left over.
  - **Mutation-checked:** 10 of 10 deliberate emulator regressions were caught.
- **Peer review (2026-09-29):** an independent review found a rate-limiter data race, an overflow in page arithmetic that could wedge the server, a panic when a fault had no status, a wrong queue-list default, filters that were silently ignored, and a user ID left in the recordings. **All are fixed and covered by tests** (`behavior_test.go`, which includes a concurrent test that reproduces the race).
- **Not yet:** Workers scripts, logs, and the resource model generated from `x-fern` annotations. Profiles are hand-declared for now.

### 3.1 Generic engine (driven by the spec)

- **Routing and validation.** Load the pinned `spec/openapi.json` with kin-openapi. Route requests and **validate every request against the schema**, so a malformed body fails in the test instead of in production. Optionally validate our own responses too.
- **Resource model.** Built from the same `x-fern-sdk-group-name` / `x-fern-sdk-method-name` annotations the generator uses:
  - a collection has `create` / `list`
  - an item has `get` / `update` / `edit` / `delete`
  - a singleton has `get` plus `update` / `edit`

  Storage is in memory, keyed by path parameters.
- **Default semantics.**
  - Create stores the body merged with schema `default`s plus server-generated fields (`id`, `created_on`, `modified_on`).
  - PUT replaces the object and PATCH merges into it.
  - List paginates, in both styles: `page` / `per_page` with `result_info`, and cursor-based.
- **Response envelope.** `{success, errors:[{code,message}], messages, result, result_info}`.
- **ID formats per kind.** 32-hex for most resources, UUID for D1, 64-hex for DO / container instances, the name itself for R2 buckets.
- **Authorization.** A fake token carries permission groups, and each operation's `x-api-token-group` decides whether it gets a 403. This **checks the token-permission table in our docs automatically**.
- **Rate limits.** 1,200 requests per 5 minutes per token, returned with `Ratelimit` / `Ratelimit-Policy` headers. Exceeding it returns 429 with `Retry-After` and locks the token for 5 minutes, all on the fake clock.
- **Sunsets.** Once the fake clock passes an operation's `eol` / `sunset` date, it returns `410 Gone`. See §4.3.

### 3.2 Behavior profiles (hand-written, backed by recordings)

Anything the spec can't express lives in `internal/fake/profiles/<kind>.go`:

| Behavior | Examples |
|---|---|
| Write-only fields left out of responses | `x-sensitive` fields, Hyperdrive password, Worker secrets, tunnel secret |
| Immutable fields, returning Cloudflare's own error code | R2 jurisdiction, Vectorize dimensions, DNS record type |
| Uniqueness conflicts | R2 bucket name within an account and jurisdiction, KV namespace title |
| Async state machines | custom hostname `pending → active`, certificate packs, container instance `provisioning → running`, registrar `202` workflows |
| Replace-all lists | tunnel ingress config, ruleset phase entrypoints, `snippet_rules`, tags |
| Dependencies between resources | deleting a tunnel that still has routes, deleting a Worker that is still a queue consumer, deleting a non-empty bucket |
| Eventual consistency | KV reads lag writes by a configurable delay |
| Container scheduler | per-application instance lifecycle, exit codes, restarts, rollouts, injected failures, plus log and exec endpoints matching whatever the spikes discover |

**Rule: every profile must cite the cassette it was derived from** (`// source: test/cassettes/r2-duplicate-bucket.yaml`). This prevents the classic failure where the emulator and the controller share an assumption, so both are wrong the same way and tests still pass. The generic engine reads only the spec, never `generator.yaml`.

### 3.3 Control API for tests

`/_fake/*` endpoints, also wrapped by a Go client in `internal/fake/fakeclient`:

- **Seed state:** accounts, zones, existing resources (for testing adoption).
- **Inject faults:** fail the next N calls to a path with a status or error code, add latency, force 429 or 5xx.
- **Control time:** set or advance the fake clock, which drives state machines, rate-limit windows and sunsets.
- **Inspect:** dump state, and a request journal so tests can assert "exactly one create was made" or "no GET per object was made".
- **Container instances:** crash an instance, set `unhealthy`, emit log lines.

## 4. API versioning

### 4.1 Pinned spec

- `spec/openapi.json` is vendored in the repo.
- `spec/LOCK` records the `api-schemas` commit (today's is `780de88d0324b007c907a1782259b1a0e5e87c7d`) and the fetch date.
- **Everything reads the pinned spec:** the generator, the generated client, the emulator and `hack/classify_api.py`. So a given operator commit is always internally consistent.

### 4.2 Nightly spec-drift job

1. Fetch `api-schemas@HEAD`.
2. Run **oasdiff** between the pinned spec and HEAD to find breaking changes: removed operations or fields, new required fields, narrowed enums, type changes. Run the **classifier** to catch new API groups.
3. **Forward-compat check.** Build the operator from the *pinned* spec and run L1 against an emulator loaded with the *HEAD* spec. This answers "does the released operator still work against tomorrow's API?"
4. **Upgrade check.** Regenerate from HEAD and run L0–L2. Open an automated PR that bumps `spec/LOCK`, with the oasdiff report and the CRD schema diff in the description.
5. **Refresh cassettes.** Cassettes record the spec commit and date they were captured. The next L3 run re-records any cassette whose operations changed in the diff.

### 4.3 Time-travel tests

- **Run the L1 suite with the fake clock set to today + 90 days.** Any call to an operation that sunsets within that window fails the build, which catches changes like the 2026-10-05 tunnel routes change before it lands.
- **The generator refuses to emit a CRD field or endpoint that sunsets within 90 days** unless `generator.yaml` explicitly acknowledges it.

### 4.4 CRD versions are separate from Cloudflare's API versions

- **A Cloudflare path version bump** (e.g. `vectorize/v1 → v2`) usually keeps the CRD version. The controller just switches endpoints, and an L1 test checks that objects created against v1 are still reconciled.
- **A breaking shape change** creates a new CRD version (`v1alpha2`, …) with a conversion webhook. Conversion is tested by envtest round trips plus fuzzing (`k8s.io/apimachinery/pkg/api/apitesting/fuzzer`).
- **Compatibility matrix in CI:**
  - L1 and L2 run for {operator N−1, N, main} × {spec pinned, spec HEAD}.
  - L3 runs only for main.

## 5. The live test account (L3)

- **A separate Cloudflare account**, never production. It has one inexpensive test zone and an account-owned token scoped to the test permission groups.
- **Every resource is tagged** `flare.dev/test-run=<run-id>` and named with the prefix `ft-<run-id>-`.
- **A janitor** (`cmd/flare-janitor`) deletes tagged resources older than 2 hours. It runs before and after every live run and on a cron schedule.
- **Guards:** tier T3 operations (registrar, BYOIP, Magic Transit, cache purge on real zones) never run live. The live suite is kept under about 600 API calls per run, well within the rate limit.
- **Recording mode:** `FLARE_TEST_RECORD=1` writes or refreshes cassettes during live runs. Tokens, account IDs and emails are replaced with placeholders before saving.

## 6. How the spikes feed in

Each virtual-kubelet spike (S1–S6) runs live in recording mode:

- The cassettes it produces become the source for the emulator's container profiles: log query shape, exec transport, routing, and application lifecycle timing.
- Once a spike is recorded, the same flow runs against the emulator on every commit.

## 7. Repository layout

```
spec/openapi.json, spec/LOCK
cmd/flarefake/                 # standalone emulator binary / container image
cmd/flare-janitor/
internal/fake/                 # generic engine (spec-driven)
internal/fake/profiles/        # per-kind behaviors (each cites a cassette)
internal/fake/fakeclient/      # Go client for /_fake control API
test/cassettes/                # go-vcr, sanitized, tagged with spec commit + date
test/e2e/                      # dual-target scenarios (fake | live), kind + virtual kubelet
hack/spec-drift.sh             # oasdiff + classify + regenerate
```

`flarefake` could also be useful outside this project, since no stateful Cloudflare API emulator exists today. We could publish it separately later.
