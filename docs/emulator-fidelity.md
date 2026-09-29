# flarefake fidelity: evidence and response validation

flarefake (`internal/fake`) stands in for the Cloudflare API in every test, so a test is only as
good as the emulator is faithful. Live testing is optional (the 2026-09-29 decision). This page
says what the emulator's behaviors rest on, how responses are checked against the pinned spec,
and what is still unproven.

## 1. Evidence tiers

Every emulated behavior carries a comment that cites its evidence. Tiers, strongest first:

| Tier | Comment form | What it proves |
|---|---|---|
| Recording | `// 0029` | The real API did exactly this (a sanitized recording in `test/recordings/`, replayed by `TestConformance`). |
| SOURCED | `// SOURCED: <repo>@<short-sha>:<path>#L<n>` | An official Cloudflare client or its test fixtures show it. |
| DOCS | `// DOCS: <developers.cloudflare.com URL>` | A documented example or statement. |
| UNVERIFIED | `UNVERIFIED` | Spec-only, inferred, or a guess. |

A recording wins over any other source when they conflict: the KV rename 404 keeps the recorded
DELETE code 10013, not wrangler's mock code 10009.

SOURCED comments say how strong their source is:

- **relies**: the client's code depends on the behavior. If the real API behaved otherwise, the
  client would break on every run. Example: wrangler calls `producers_total_count.toString()` on
  every Queues list item, so list items must carry it.
- **statement**: a comment or constant in the client describes the API. Examples: "The versions
  API ignores pagination when `deployable=true`", or `WORKER_NOT_FOUND_ERR_CODE = 10007`.
- **fixture**: a mock response written by Cloudflare engineers (a wrangler msw handler). A
  fixture reflects what the authors expect, and fixtures are often partial.
- **tolerates**: the client merely accepts the shape, for example by decoding into a struct
  with optional fields. This is the weakest kind: it does not show that the API returns the
  shape.

Pinned sources (shallow clones, never vendored into the repo):

| Repo | Commit | Used for |
|---|---|---|
| cloudflare/workers-sdk | `485cfb3` | wrangler and deploy-helpers: Workers, Queues, D1, KV, tokens, errors |
| cloudflare/cloudflared | `ad3c6d1` | tunnel create, cleanup, credentials, virtual networks |
| cloudflare/terraform-provider-cloudflare | `65783c2` | read-after-write, 404 handling, sweepers |
| cloudflare/cloudflare-go | `3da6607` | little: the v6 SDK is generated from the spec, and its tests run against a spec mock, not the API |

Field reports, such as a real error pasted into a GitHub issue, are not a tier. They appear only
as supporting notes next to an `UNVERIFIED` marker, as with the 429 body code 971.

## 2. Strict response validation

`internal/fake/response_validation.go` validates every emulated response body against the
pinned spec's response schema for the matched operation, using kin-openapi
`openapi3filter.ValidateResponse` with `IncludeResponseStatus`. It never changes a response.

| Where | Mode |
|---|---|
| `internal/fake` tests, including `TestConformance` (`main_test.go`) | strict: the run fails on any violation that no allowlist entry covers |
| envtest suites through `testenv.Main` (account, tunnel, generic, examples, and any package that uses it) | strict |
| `internal/reconcile`, `internal/generic/descriptors`, `internal/testenv` (`testenv.StrictMain`) and `internal/cfclient` (its own `TestMain`) | strict |
| `-short` runs | off, because the 26 MB spec is not loaded |
| the `flarefake` binary with `-spec` (`-validate-responses`, on by default) | journal only: `JournalEntry.response_violation`, a log line, and `GET /_fake/response_violations` |

A Server opts out with `Options.NoStrictResponses`. Outside tests, `Options.ValidateResponses`
turns validation on.

**Allowlist.** `responseAllowlist` holds the violations that the emulator reproduces on purpose,
because a real response can't satisfy the spec either. Each entry has exactly one kind of
evidence, and `TestResponseAllowlistReproducedByRecordings` checks it:

- **recording**: the cited recording's real response must show a matching violation.
  `TestRecordedResponsesAgainstSpec` validates all 216 recorded responses; 71 of them violate the
  spec, including responses from surfaces that are not emulated yet.
- **unsatisfiable**: the schema rejects every candidate value (null, {}, [], ""). An example is
  `tunnel_empty_response`, which is `allOf(result anyOf[object,array,string], result enum
  [null])`.
- **field**: a real response quoted in an official repository. The only entry of this kind is the
  429 code 971, which is below the spec's error-code minimum of 1000.

**Results of the first pass.** 17 kinds of emulated-response violation were found:

- 2 were fixed in the emulator:
  - `GET /accounts/{id}` sent `abuse_contact_email: null`.
  - Workers VPC services accepted a host with no `network`, and echoed a host that matched no
    `infra_ServiceHost` variant.
- 15 were allowlisted:
  - 13 are backed by recordings (0019, 0021, 0026, 0036, 0038, 0040, 0044, 0045, 0065, 0095 +
    0163, 0108, 0148, 0160).
  - 1 is unsatisfiable.
  - 1 is a field report.

The broadest class is `unsatisfiable-4xx-allof-success`: many 4XX schemas are
`allOf(<success response>, <common failure>)`, which requires `success` to be both true and
false. Real errors such as 0095 violate them.

When a new violation fails a test, fix the profile. Allowlist it only if a recording (or an
unsatisfiable schema) proves that no conforming response exists. Never widen an entry's regexp
past what its evidence shows.

## 3. Per-surface evidence

Marker counts are occurrences in the non-test source (SOURCED/DOCS count citation markers, not
behaviors). "Recordings" counts the distinct recordings cited. Of the 216 recordings,
`TestConformance` replays 147; the rest hit surfaces that are not emulated yet (Workers
observability, tails, Hyperdrive and others).

| Surface (file) | Routes | Recordings cited | SOURCED | DOCS | UNVERIFIED |
|---|---|---|---|---|---|
| KV namespaces (`kv.go`) | 5 (×2 with the legacy `/workers/namespaces` path) | 11 | 2 | 0 | 5 |
| D1 (`d1.go`) | 7 | 11 | 1 | 0 | 4 |
| Queues (`queues.go`) | 6 | 9 | 7 | 1 | 3 |
| Tunnels + virtual networks (`tunnels.go`) | 12 | 20 | 6 | 0 | 9 |
| Workers VPC services (`vpc.go`) | 5 | 17 | 0 | 0 | 2 |
| Workers scripts (`workers.go`) | 11 | 37 | 7 | 0 | 41 |
| Tokens / account (`tokens.go`) | 3 | 0 | 2 | 2 | 4 |
| Resource Tagging (`tags.go`) | 4 | 0 | 0 | 8 | 8 |
| Cross-cutting (`server.go`, `envelope.go`, `state.go`) | — | 14 | 1 | 2 | 8 |

UNVERIFIED occurrences went from 86 to 84 in this pass. The count fell only a little, for two
reasons:

- Most upgrades resolve the main behavior while a detail stays open. For example, a 404 status
  is now SOURCED but its error code is still UNVERIFIED.
- Behaviors adopted from sources bring their own open details, such as the exact-match queue
  name filter or the tag cursor format.

## 4. Emulator changes made on evidence

| Change | Evidence |
|---|---|
| Queues list items carry `producers`/`consumers` counts (as in GET); `?name=` filters (repeatable) | SOURCED relies: wrangler `queues list`, deploy-helpers `getQueue` |
| Queue settings: delivery delay up to 86400 s (was 43200); out-of-range settings answer code 100128 | SOURCED wrangler `queues/constants.ts`, `queues/utils.ts`; DOCS Queues limits |
| D1 `GET …/d1/database/{name}` finds a database by exact name | SOURCED relies: wrangler `d1/utils.ts` |
| Script subdomain POST: an omitted `previews_enabled` follows `enabled` | SOURCED fixture ("Mimics API behavior") |
| Script versions: `deployable=true` ignores pagination | SOURCED statement: wrangler `versions/list.ts` |
| Tunnel create: a duplicate live name answers 409 | SOURCED relies: cloudflared `cfapi/tunnel.go` |
| Virtual network create reads `is_default_network` (and the deprecated `is_default`) | SOURCED: cloudflared `cfapi/virtual_network.go`; spec |
| Token verify success carries message 10000 "This API Token is valid and active" | DOCS create-token |
| Tags list: cursor pagination, fixed page size 100, `cursor: null` on the last page; more than 20 tag filters answer 1010 | DOCS Resource Tagging filter-resources |
| VPC service host must be ipv4/ipv6 + network or hostname + resolver_network | spec (the rejection code is UNVERIFIED) |
| `GET /accounts/{id}` omits the null `abuse_contact_email` | spec (response validation) |

## 5. How to add evidence

1. Find a source that shows the behavior. Search the pinned clones first:
   - the wrangler msw handlers in `packages/wrangler/src/__tests__/helpers` and the command code
     in `packages/wrangler/src` and `packages/deploy-helpers/src`
   - cloudflared `cfapi/`
   - terraform `internal/services/<resource>/` (resource.go 404 handling, acceptance checks,
     sweepers)

   Then search developers.cloudflare.com.
2. Pin it: `<repo>@<short-sha>:<path>#L<line>`, and say how strong it is (relies, statement,
   fixture or tolerates). For a newer commit, re-clone and update the SHAs in §1.
3. Replace the `UNVERIFIED` marker only for the part the source proves. Leave the rest marked,
   for example "404 SOURCED; code UNVERIFIED".
4. If the source contradicts the emulator and no recording does, change the emulator and add a
   test (`internal/fake/evidence_test.go`). If a recording contradicts the source, the recording
   wins; note the conflict in the comment.
5. Run `make test` and `make conformance`. Response validation catches shapes the spec rejects.
6. A new recording beats all of this. Add it to a `TestConformance` scenario, then change
   `SOURCED`, `DOCS` or `UNVERIFIED` to its number.

## 6. What remains UNVERIFIED, and why

- **Error details without a recording.** These are mostly error codes and messages for invalid
  input, and HTTP statuses where a source shows only the code (token 1000/9109) or only the
  status (tunnel and vnet 404).
  - KV: title required.
  - D1: missing name, invalid read_replication.
  - Queues: settings message and status.
  - Tunnels: config_src, duplicate name code, deleted-config PUT.
  - Tags: 1001/1002/1010 details.
  - Workers: every multipart/metadata 10021 message.
  - Cross-cutting: unknown route 7000, 405/10405, malformed JSON 10002.

  Sources rarely show error bodies; only recordings of failing calls can settle them.
- **Workers details:**
  - version numbering beyond 1
  - newest-first versions (wrangler's fixture lists oldest first and sorts client-side)
  - the deployments 10-item cap
  - etag input
  - handler detection
  - observability defaults and echo of traces/redact_query_string
  - tags/tail_consumers once set
  - `has_modules` for service-worker uploads
  - `usage_model`
  - the whole settings PATCH route (wrangler never uses it; it uses `/script-settings` and
    `/versions`)
  - script subdomain GET/DELETE
  - accounts without a workers.dev subdomain

  These need recordings of multi-version scripts and settings writes.
- **Queues `delivery_paused` read-back.** The sources conflict: wrangler's mock returns it, and
  terraform skips its test because of "API changes causing state issues".
- **List ordering** for D1, Queues, Tunnels and scripts (only KV's order is recorded), and the
  D1 `?name=` search semantics.
- **Rate limiting.** Whether the Workers write policies are separate buckets, and the 429 body
  code 971 (field reports only).
- **Tokens/account.** The account-owned verify message and the `GET /accounts/{id}` values.
- **Tunnel details.** The `credentials_file` shape (redacted in 0040; cloudflared only tolerates
  its keys) and the cleanup-connections result.
- **KV.** Jurisdiction read-back and post-sunset legacy routes.
- **Resource Tagging.** The whole surface has no recording. Docs cover replace-all, the 500 for a
  never-tagged resource, the 204 delete and cursor paging. The etag/If-Match behavior is from the
  spec only.
