# Differential testing: real Cloudflare clients against flarefake

flarefake is only as trustworthy as the evidence behind it (CLAUDE.md: recording > SOURCED > DOCS > UNVERIFIED). The differential tests add one more check. They run Cloudflare's own clients against an in-process flarefake and assert two things:

- each client works end to end;
- whatever it reads back is populated and decodes.

They make **no Cloudflare API calls**.

Each mismatch is recorded as a **known discrepancy**. It becomes a skipped subtest, `discrepancy/<ID>`, whose skip message carries the evidence: the client's file, version and line. flarefake is not changed here. Whoever fixes flarefake sees that subtest fail with "no longer reproduces", then deletes the entry and its shim.

## Layout

| Path | What |
|---|---|
| `test/differential/harness/` | In-process flarefake on a random port (`harness.Start`). It captures every request (method, path, query, Content-Type, User-Agent, body prefix, status, envelope error codes). It also holds shims, `Discrepancy`, and `KnownSpecDefects`. There is no build tag, and its unit tests run in `make test`. |
| `test/differential/wrangler_test.go` | wrangler scenario (build tag `differential`) |
| `test/differential/wrangler_shims_test.go` | Test-local answers for routes flarefake lacks. Each shim is registered only for its discrepancy, so the deploy flow can go on. |
| `test/differential/cloudflared_test.go` | cloudflared management-API scenario |
| `test/differential/go/` | A **separate Go module** (its own `go.mod`, with `replace flare.dev/operator => ../../..`) that pins cloudflare-go. The operator's `go.mod` stays free of the SDK. |
| `test/differential/npm/` | `package.json` and `package-lock.json` that pin wrangler and its dependency tree |
| `test/differential/versions.go` | Pinned versions. `TestPinnedVersionsMatch` (run by `make test`) checks them against the Makefile and the npm lockfile. |
| `hack/differential-tools.sh` | Installer used by `make differential-tools` |

## Pinned clients

| Client | Version | Source for citations | How it is pointed at flarefake |
|---|---|---|---|
| wrangler | 4.143.0 (npm, locked tree) | cloudflare/workers-sdk tag `wrangler@4.143.0` = `3bdcd0d` | `CLOUDFLARE_API_BASE_URL=<fake>/client/v4`. The override is defined in `packages/workers-utils/src/environment-variables/misc-variables.ts#L153-L166` (`getCloudflareApiBaseUrl`); `CF_API_BASE_URL` is its deprecated alias. |
| cloudflare-go | v7.11.0 (`3da6607`), the current major | module cache | `option.WithBaseURL(<fake>/client/v4)`, `option.WithMaxRetries(0)` |
| cloudflared | 2026.9.3 (release binary, sha256 = GitHub asset digest) | cloudflare/cloudflared tag `2026.9.3` | `TUNNEL_API_URL` (hidden `--api-url` flag, `cmd/cloudflared/tunnel/cmd.go#L737-L743`), plus a generated origin cert (`TUNNEL_ORIGIN_CERT`) holding the fake token |

Every client gets a fake token (`harness.Token`) and account (`harness.AccountID`). flarefake runs in open token mode.

The wrangler environment has these safety settings:

- `WRANGLER_SEND_METRICS=false`, `WRANGLER_SEND_ERROR_REPORTS=false` and `DO_NOT_TRACK=1`;
- `WRANGLER_HIDE_BANNER=true`, which also skips the npm update check;
- `CI=1`, a temporary `HOME`, and `CLOUDFLARE_CF_FETCH_ENABLED=false`.

Every client also runs with `HTTPS_PROXY`/`HTTP_PROXY` set to a closed loopback port and `NO_PROXY=127.0.0.1,localhost`. Any stray non-loopback request (telemetry, update check, a wrong base URL) therefore fails instead of reaching the internet. cloudflared runs only management commands and never `tunnel run`, so it never contacts the edge.

## How to run

```sh
make differential-tools   # once: npm ci wrangler, download and verify cloudflared into ~/.cache/flare-operator/differential
make differential         # wrangler + cloudflared, then cloudflare-go (its module is vetted first)
make differential FLARE_DIFF_CAPTURE_DIR=/tmp/diff   # also write every client's captured requests as JSON
```

A client that is not installed is skipped with a hint. You can override where a binary is found with `FLARE_DIFF_WRANGLER`, `FLARE_DIFF_CLOUDFLARED` or `FLARE_DIFF_CACHE`. cloudflared falls back to the one on `PATH`, and logs a version warning if it differs. `make ci` does not run the clients, but it does vet and gofmt `test/differential` and runs the harness unit tests.

## What is covered (2026-09-30)

**wrangler 4.143.0:**
- KV:
  - `kv namespace create/list/rename/delete`
  - `kv key put/get` (discrepancy)
- Queues:
  - `queues create/info/delete`
  - `queues list` (discrepancy)
  - create against strict mode (discrepancy)
- D1:
  - `d1 create/list --json/delete`
  - `d1 execute --remote` (discrepancy)
- Workers:
  - `deploy` of an ES-module Worker with KV, Queue and D1 bindings to the resources created above. The stored bindings and the workers.dev state are checked in flarefake.
  - `versions list`, `deployments list`, `deployments status`
  - workers.dev off and back on (`triggers deploy`)
  - `delete`
  - `versions view` and a redeploy (both discrepancies)

  The test also asserts that every request wrangler made either matches the pinned spec or is a known discrepancy or spec defect, and that it hit only emulated routes apart from the discrepancies.

**cloudflare-go v7.11.0:**
- User token verify.
- KV namespaces: create/get/rename/list/delete, then a 404 after delete.
- Queues: create/get/edit/list/delete, then a 404.
- D1: create/get/list/delete, then a 404.
- Tunnels:
  - create/get/token/list/delete
  - connections, with a connector attached through `/_fake`
  - configurations put/get
- Virtual networks: create/list/delete.
- Workers VPC services: create/get/list/delete.
- Resource Tagging: set/get/list.
- Workers:
  - script upload with KV and plain_text bindings
  - list, versions, deployments
  - per-script and account subdomain
  - delete

Every response goes through `sdkdiff.Inspect`. It reads cloudflare-go's apijson metadata (`IsMissing`/`IsNull`/`IsInvalid`, `internal/apijson/field.go#L22-L24`) because cloudflare-go decodes leniently and never returns an error for a wrong field. The rules are:
- An `invalid` field fails the test.
- A field tagged `api:"required"` that is missing fails the test.
- Optional fields that are missing, and unknown extra fields, are logged.

The extras seen so far (`credentials_file`, `token`, `ha_status`, D1 `file_size`/`num_tables`/`*_in_region`) are all present in the recordings too.

**cloudflared 2026.9.3:**
- `tunnel create`: checks the credentials file.
- `tunnel list`.
- `tunnel info`, with a connector attached: 1 client and 2 connections decode.
- `tunnel token`: decodes to `{a, t, s}`.
- `tunnel cleanup`.
- `tunnel vnet add/list/delete`.
- `tunnel route ip show` (discrepancy).
- `tunnel delete`.

## Known discrepancies

Each discrepancy is a `harness.Discrepancy` in the test that found it. **Shimmed** means a test-local shim answers that route so the rest of the flow still exercises flarefake.

| ID | Mismatch | Evidence |
|---|---|---|
| WR-SERVICE-GET (shimmed) | `GET …/workers/services/{name}` is not emulated (404/7000) and is not in the pinned spec. `wrangler deploy` needs 404 **10007**/10090 for a new Worker, or `default_environment.script.tag`, and aborts otherwise. | `wrangler-dist/cli.js#L174943`; workers-sdk `packages/deploy-helpers/src/deploy/helpers/worker-not-found-error.ts#L4,L9` |
| WR-SECRETS-LIST (shimmed) | `GET …/scripts/{name}/secrets` is not emulated. deploy lists secrets whenever there are bindings, and tolerates only 404/10007. | `…/deploy/helpers/check-remote-secrets-override.ts#L10,L35-L42` |
| WR-WORKER-GET (shimmed) | `GET …/workers/workers/{name}` is not emulated. wrangler reads `subdomain.enabled/previews_enabled` from it after every upload. | `packages/deploy-helpers/src/triggers/subdomain.ts#L159-L173` |
| WR-SERVICE-DELETE (shimmed) | `wrangler delete` calls `DELETE …/workers/services/{name}?force=`, which is not emulated. | `packages/wrangler/src/delete.ts#L154-L159` |
| WR-REDEPLOY-VERSIONS | Redeploying an existing Worker uses `POST …/versions` (405 in flarefake), then `POST …/deployments` and `PATCH …/script-settings`. The upload carries `{"type":"inherit"}` bindings. | `…/deploy/deploy.ts#L423-L432,L517-L525`; `…/helpers/versions-api.ts#L145,L180` |
| WR-VERSION-GET | `GET …/versions/{id}` is not emulated (`wrangler versions view`). | `…/helpers/versions-api.ts#L27-L31` |
| WR-QUEUE-LIST-COUNTS | Queue list items lack `producers_total_count`/`consumers_total_count` (UNVERIFIED shape, since 0148 is an empty list). `wrangler queues list` crashes on `.toString()`. | `packages/wrangler/src/queues/cli/commands/list.ts#L45-L46,L56-L57` |
| WR-QUEUE-CREATE-CONTENT-TYPE | The queue create body is sent with no Content-Type, so undici sends `text/plain;charset=UTF-8`. Spec validation flags it, and strict mode (`-reject-schema-violations`) rejects real wrangler. | `packages/wrangler/src/queues/client.ts#L55-L64` |
| WR-KV-VALUES | KV values routes are not emulated. wrangler also sends values as `text/plain`, which the spec does not allow (only octet-stream or multipart). | `packages/wrangler/src/kv/helpers.ts#L247-L252` |
| WR-D1-EXECUTE | `POST …/d1/database/{id}/query` answers 501/99999, because SQL is not emulated. | `packages/wrangler/src/d1/execute.ts#L630-L640` |
| CFD-TEAMNET-ROUTES | `…/teamnet/routes` (tunnel IP routes) is not emulated. | cloudflared `cfapi/base_client.go#L53`, `cfapi/ip_route.go` |
| SDK-VPC-LIST-PAGINATION | The VPC services list ignores `?page=` and repeats the full list on every page. cloudflare-go's V4PagePaginationArray asks for page N+1 until a page comes back empty, so `ListAutoPaging` never ends. | cloudflare-go `packages/pagination/pagination.go#L215-L227`, `connectivity/directoryservice.go#L82-L101` |
| FAKE-HANDLER-DETECTION | flarefake's line-start handler regex (UNVERIFIED) reports `handlers: []` for `export default { async fetch() {…} }`, the shape esbuild and wrangler emit. Recording 0036 shows `[fetch]`. | cloudflare-go `workers/script.go` (`Handlers`); recording 0036 |
| SDK-WORKERS-UPLOAD-FORM | cloudflare-go's `Workers.Scripts.Update` flattens the multipart form (`metadata.main_module`, `files.0`) instead of sending a JSON `metadata` part, and flarefake answers 400/10021. flarefake follows the spec, the recorded upload (0036) and what wrangler sends (a JSON `metadata` part), so this may be an SDK defect. What the live API does is UNVERIFIED. | cloudflare-go `workers/script.go#L4154-L4167` |
| SDK-SETTINGS-PLACEMENT-PANIC | **Client defect, not flarefake.** cloudflare-go panics decoding `"placement": {}` in `GET …/settings`, and the live API sends exactly that (0065, 0091). Operator code must not use this SDK call. | cloudflare-go `workers/scriptscriptandversionsetting.go#L8368-L8375`, `internal/apijson/port.go#L84` |

`harness.KnownSpecDefects` lists places where clients break the pinned spec but a recording proves the live API accepts the request. These are spec defects, not flarefake discrepancies:

- the KV DELETE body (0013);
- the D1 `database_id` oneOf (0020);
- a catch-all ingress rule without `hostname` (0045, 0046). cloudflare-go also marks this field required, which is `knownSDKDecodeGaps`.

What wrangler sends that flarefake already accepts, useful as SOURCED evidence for PR-1:

- The upload is `PUT …/scripts/{name}?excludeScript=true&bindings_inherit=strict`.
  - The module part is `application/javascript+module`.
  - Metadata includes `code_update_strategy: {mode: "deferred", max_delay: 300}`.
  - D1 bindings use `id`, not `database_id`.
- workers.dev is toggled with `POST …/subdomain {"enabled":true|false}`.
- `GET …/queues?name=` is used for lookups.
- `GET …/versions?deployable=true`.

A `FLARE_DIFF_CAPTURE_DIR` run gives the full request list.

## Adding a scenario or a client

1. Pin the client:
   - an npm package gets a lockfile under `test/differential/<client>/`;
   - a binary gets a version and sha256 in `hack/differential-tools.sh` and a constant in `versions.go`;
   - a Go SDK is pinned in `test/differential/go/go.mod`.
2. Find the client's base-URL override **in its source at the pinned version** and cite it next to the env var or option.
3. Start flarefake with `harness.Start(t, harness.Options{})` **after** locating the client, so a missing client skips without loading the spec. Point the client at `f.API` and give it `harness.Token`. Use `run()` (clients_test.go), which applies the proxy safety net and a minimal environment.
4. After each step, assert on the client's output **and** on flarefake's state (`f.Call`, which bypasses capture and shims). End with the `spec-and-routes` checks: `f.UnexplainedSchemaViolations(known...)` and `f.Unanswered()`.
5. For each mismatch:
   - declare a `harness.Discrepancy` with the client file, version and line as evidence, and wrap the failing step in `d.Check(t, func() error {...})`;
   - add a shim (`f.AddShim`) only if later steps need the route, and name the discrepancy in it.

   Never change `internal/fake` in a differential-test change.
6. Add the discrepancy to the table above.
