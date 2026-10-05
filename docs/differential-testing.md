# Differential testing: real Cloudflare clients against flarefake

flarefake is only as trustworthy as the evidence behind it (CLAUDE.md: recording > SOURCED > DOCS > UNVERIFIED). The differential tests add one more check. They run Cloudflare's own clients against an in-process flarefake and assert two things:

- each client works end to end;
- whatever it reads back is populated and decodes.

They make **no Cloudflare API calls**.

Each mismatch is recorded as a **known discrepancy**. It becomes a skipped subtest, `discrepancy/<ID>`, whose skip message carries the evidence: the client's file, version and line. flarefake is not changed here. Whoever fixes flarefake sees that subtest fail with "no longer reproduces", then deletes the entry and its shim.

## Layout

| Path | What |
|---|---|
| `test/differential/harness/` | In-process flarefake on a random port (`harness.Start`). It captures every request (method, path, query, Content-Type, User-Agent, body prefix, status, envelope error codes). It also holds `Discrepancy`, `KnownSpecDefects`, and shims (test-local answers for a route flarefake lacks, registered only together with the discrepancy they name; none is in use). There is no build tag, and its unit tests run in `make test`. |
| `test/differential/wrangler_test.go` | wrangler scenario (build tag `differential`) |
| `test/differential/wrangler_assets_test.go` | wrangler static-assets scenario (a Worker with assets, an assets-only site) |
| `test/differential/cloudflared_test.go` | cloudflared management-API scenario |
| `test/differential/go/` | A **separate Go module** (its own `go.mod`, with `replace github.com/chenhunghan/flare-operator => ../../..`) that pins cloudflare-go. The operator's `go.mod` stays free of the SDK. |
| `test/differential/npm/` | `package.json` and `package-lock.json` that pin wrangler and its dependency tree |
| `test/differential/versions.go` | Pinned versions. `TestPinnedVersionsMatch` (run by `make test`) checks them against the Makefile and the npm lockfile. |
| `hack/differential-tools.sh` | Installer used by `make differential-tools` |

## Pinned clients

| Client | Version | Source for citations | How it is pointed at flarefake |
|---|---|---|---|
| wrangler | 4.147.0 (npm, locked tree) | cloudflare/workers-sdk tag `wrangler@4.147.0` = `64c1337` | `CLOUDFLARE_API_BASE_URL=<fake>/client/v4`. The override is defined in `packages/workers-utils/src/environment-variables/misc-variables.ts#L153-L166` (`getCloudflareApiBaseUrl`); `CF_API_BASE_URL` is its deprecated alias. |
| cloudflare-go | v7.12.0 (`05ca1e4`), the current major | module cache | `option.WithBaseURL(<fake>/client/v4)`, `option.WithMaxRetries(0)` |
| cloudflared | 2026.9.3 (release binary, sha256 = GitHub asset digest) | cloudflare/cloudflared tag `2026.9.3` | `TUNNEL_API_URL` (hidden `--api-url` flag, `cmd/cloudflared/tunnel/cmd.go#L737-L743`), plus a generated origin cert (`TUNNEL_ORIGIN_CERT`) holding the fake token |

Every client gets a fake token (`harness.Token`) and account (`harness.AccountID`). flarefake runs in open token mode.

The wrangler environment has these safety settings:

- `WRANGLER_SEND_METRICS=false`, `WRANGLER_SEND_ERROR_REPORTS=false` and `DO_NOT_TRACK=1`;
- `WRANGLER_HIDE_BANNER=true`, which also skips the npm update check;
- `CI=1`, a temporary `HOME`, and `CLOUDFLARE_CF_FETCH_ENABLED=false`.

wrangler and cloudflared run with `HTTPS_PROXY`/`HTTP_PROXY` set to a closed loopback port and `NO_PROXY=127.0.0.1,localhost`, so a stray non-loopback request from wrangler (telemetry, update check, a wrong base URL) fails instead of reaching the internet. The net does not cover everything:

- cloudflared's API client builds its own `http.Transport` without a proxy function (`cfapi/base_client.go#L65-L68`), so it ignores the proxy variables. It is pointed at flarefake by `TUNNEL_API_URL` only, and it runs only management commands, never `tunnel run`, so it never contacts the edge.
- cloudflare-go runs in-process with `option.WithBaseURL` and no proxy set.

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
  - `kv key put` (plain and with `--metadata`), `kv key get`, `kv key list`, `kv key delete`
- Queues:
  - `queues create/info/list/delete`
  - `queues create` against strict mode (`-reject-schema-violations`)
- D1:
  - `d1 create/list --json/delete`
  - `d1 execute --remote --command` with constant `SELECT`s (flarefake executes nothing else; see below)
- Workers:
  - `deploy` of an ES-module Worker with KV, Queue and D1 bindings to the resources created above. The stored bindings and the workers.dev state are checked in flarefake.
  - `versions list`, `versions view` (JSON and text), `deployments list`, `deployments status`
  - workers.dev off and back on (`triggers deploy`)
  - a redeploy of the existing Worker, which goes through the versions API: `POST …/versions`, `POST …/deployments` at 100%, `PATCH …/script-settings`
  - `delete` (`DELETE …/workers/services/{name}?force=true`)
- R2 (the generic profile's R2Bucket extensions, 2026-09-30):
  - `r2 bucket create`, one bucket in the default jurisdiction (`--storage-class`) and one in the EU (`-J eu`: the `cf-r2-jurisdiction` header)
  - `r2 bucket list`, with and without `-J eu`: each jurisdiction lists only its own bucket
  - `r2 bucket update storage-class` (a bodiless `PATCH` with `cf-r2-storage-class`)
  - `r2 bucket cors set/list/delete` in the EU jurisdiction
  - `r2 bucket delete` of both buckets

  The first run found a discrepancy: `r2 bucket create` sends its JSON body with no Content-Type (text/plain). flarefake now validates that body as JSON, as it already did for `queues create` (`internal/fake/spec.go` `plainTextBodies`, SOURCED). `r2 bucket info` is not run: it also queries the GraphQL analytics API, which flarefake does not emulate.
- Workers static assets (`wrangler_assets_test.go`, `TestWranglerAssets`):
  - `deploy` of a Worker with an assets directory (an `ASSETS` binding, `html_handling`, `not_found_handling`, `run_worker_first` rules, `_headers`, `_redirects` and an `.assetsignore`), with flarefake set to two files per bucket so the upload takes a 202 and a 201. flarefake's stored manifest, files and config are checked against the directory.
  - a redeploy with unchanged assets opens one session and uploads nothing; one changed file is one bucket with one part.
  - `deploy` and an unchanged redeploy of an assets-only site (no `main`), then `delete` of both.
  - No discrepancy was found: wrangler's hashes pass flarefake's hash check, and every request matches the pinned spec (the assets upload's undeclared `assets_jwt` security scheme is skipped, `spec.go` `withoutUndeclaredSecurity`).

  The test also asserts that every request wrangler made either matches the pinned spec or is a known client-side spec violation, and that it hit only emulated routes. No route is shimmed any more.
- Pages (`TestWranglerPages`, `pages_test.go`):
  - `pages project create --production-branch`, `pages project list`
  - `pages deploy <dir>` of a directory with nested files, a dot directory, `node_modules` (ignored) and `_headers`/`_redirects`: upload token, `check-missing`, bucketed `upload`, `upsert-hashes`, the deployment create and the status poll until deploy/success. The manifest's hashes, computed by wrangler (blake3-wasm), must equal the operator's `pagesdeployment.HashFile`, and the stored assets and routing files are checked in flarefake.
  - a second deploy of the same directory uploads nothing
  - `pages deployment list`; a preview deploy (`--branch feature-x`) with its branch alias; `pages deployment delete` of the aliased preview (without `--force` a non-interactive run declines the confirmation and sends nothing; with it, `DELETE ?force=true`)
  - `pages project delete --yes`

  Known client-side spec violation: the deployment create sends `manifest` as a plain form field; the spec's multipart encoding wants `application/json`.

**cloudflare-go v7.11.0:**
- User token verify.
- KV namespaces: create/get/rename/list/delete, then a 404 after delete.
- Queues: create/get/edit/list/delete, then a 404.
- D1: create/get/list/delete, then a 404.
- R2 buckets in the EU jurisdiction: create (with a storage class), get, a 404 without the jurisdiction header, edit (the storage class header), list, CORS update/get/delete, delete, then a 404.
- Tunnels:
  - create/get/token/list/delete
  - connections, with a connector attached through `/_fake`
  - configurations put/get
- Virtual networks: create/list/delete.
- Workers VPC services: create/get/list (one page, and `ListAutoPaging` to the end)/delete.
- Resource Tagging: set/get/list.
- Workers:
  - script upload with KV and plain_text bindings, and a one-line module whose handler must still be detected
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
- `tunnel route ip add/show/get/delete` (delete by network, which cloudflared resolves with a `network_subset` + `network_superset` lookup).
- `tunnel delete`.

## Known discrepancies

Each discrepancy is a `harness.Discrepancy` in the test that found it. Two remain, both on the client side:

| ID | Mismatch | Evidence |
|---|---|---|
| SDK-WORKERS-UPLOAD-FORM | cloudflare-go's `Workers.Scripts.Update` flattens the multipart form (`metadata.main_module`, `files.0`) instead of sending a JSON `metadata` part, and flarefake answers 400/10021. flarefake follows the spec, the recorded upload (0036) and what wrangler sends (a JSON `metadata` part), so this may be an SDK defect. What the live API does is UNVERIFIED. | cloudflare-go `workers/script.go#L4154-L4167` |
| SDK-SETTINGS-PLACEMENT-PANIC | **Client defect, not flarefake.** cloudflare-go panics decoding `"placement": {}` in `GET …/settings`, and the live API sends exactly that (0065, 0091). Operator code must not use this SDK call. | cloudflare-go `workers/scriptscriptandversionsetting.go#L8368-L8375`, `internal/apijson/port.go#L84` |

**Fixed in flarefake on 2026-09-30** (entries and shims deleted; the scenario steps above now cover them). The emulator code cites its evidence at each behavior (`internal/fake`):

| ID | Fix |
|---|---|
| WR-SERVICE-GET, WR-SERVICE-DELETE | `GET`/`DELETE …/workers/services/{name}` (absent from the pinned spec): 404/10007 for a missing Worker, else `default_environment.script` with `tag`, `tags`, `last_deployed_from`; the DELETE deletes the script (`workers_versions.go`) |
| WR-SECRETS-LIST | `GET …/scripts/{name}/secrets` lists secret bindings by name and type, 404/10007 for a missing Worker |
| WR-WORKER-GET | `GET …/workers/workers/{id}` (by name or tag) answers the spec's `workers_Worker`, including `subdomain` |
| WR-REDEPLOY-VERSIONS | `POST …/versions` (not deployed; `inherit` bindings and `keep_bindings` resolved against the deployed version, 400/10057 for an unresolvable inherit under `bindings_inherit=strict`), `POST …/deployments` (percentages over known versions, adding up to 100), `GET`/`PATCH …/script-settings` (no new version) |
| WR-VERSION-GET | `GET …/versions/{id}` with `resources.bindings/script/script_runtime` |
| WR-QUEUE-LIST-COUNTS | already fixed by the emulator evidence pass (list items carry the producer/consumer counts); the discrepancy only reported "no longer reproduces" |
| WR-QUEUE-CREATE-CONTENT-TYPE | request validation reads a `text/plain` body as the declared media type for the two operations where wrangler is shown to send one (queue create, KV value put, `spec.go` `plainTextBodies`); the body is still validated, and `text/plain` stays a violation everywhere else |
| WR-KV-VALUES | KV values `PUT`/`GET`/`DELETE`, `…/metadata/{key}` and `…/keys` (sorted, prefix, cursor, expiration) (`kv_values.go`) |
| WR-D1-EXECUTE | `POST …/query` executes constant `SELECT`s (numbers, strings, NULL, TRUE/FALSE, parameters, aliases; several statements; `batch`) and answers 400/99999 "not emulated" for anything else (`d1_sql.go`). No SQL engine: a pure-Go SQLite driver would pull a large transpiled C runtime into the operator's module for a surface the operator does not use. |
| CFD-TEAMNET-ROUTES | `…/teamnet/routes` list (filters, paging; replays 0159 and 0222), create, get, delete (soft), `…/routes/ip/{ip}` (`teamnet_routes.go`) |
| SDK-VPC-LIST-PAGINATION | the VPC services list honors `page`/`per_page` (spec defaults 1 and 1000), so a page past the end is empty |
| FAKE-HANDLER-DETECTION | handler detection also finds methods and properties right after `{` or `,`: `export default { async fetch() {…} }` and esbuild's `var x_default = { async fetch(…` |

`harness.KnownSpecDefects` lists places where clients break the pinned spec but a recording proves the live API accepts the request. These are spec defects, not flarefake discrepancies:

- the KV DELETE body (0013);
- the D1 `database_id` oneOf (0020);
- a catch-all ingress rule without `hostname` (0045, 0046). cloudflare-go also marks this field required, which is `knownSDKDecodeGaps`.

The tests also list, next to the scenario, requests a client makes on every such call that break the pinned spec, so the live API must accept them (SOURCED, relies). flarefake journals them and answers normally:

- wrangler (`knownWranglerSpecViolations`): `GET`/`DELETE …/workers/services/{name}` (not in the spec); `kv key delete` without a body (the spec requires one); `kv key put --metadata` sending metadata as a plain form field (the spec's encoding wants `application/json`).
- cloudflared (`knownCloudflaredSpecViolation`): `route ip add` sends `tunnel_id`, which the spec both requires and marks readOnly in the request body.

What wrangler sends that flarefake accepts, useful as SOURCED evidence:

- The upload is `PUT …/scripts/{name}?excludeScript=true&bindings_inherit=strict`.
  - The module part is `application/javascript+module`.
  - Metadata includes `code_update_strategy: {mode: "deferred", max_delay: 300}`.
  - D1 bindings use `id`, not `database_id`.
- A redeploy uses `POST …/versions?bindings_inherit=strict`, then `POST …/deployments` and `PATCH …/script-settings`.
- workers.dev is toggled with `POST …/subdomain {"enabled":true|false}`.
- `GET …/queues?name=` is used for lookups; the queue create body and KV values are sent as `text/plain`.
- `GET …/versions?deployable=true`.
- Static assets: `POST …/scripts/{name}/assets-upload-session` with `Content-Type: application/json` on every deploy, bucket uploads as `multipart/form-data` parts named and file-named by the hash with the base64 content and the served Content-Type, `Authorization: Bearer <session jwt>`; the upload metadata carries `assets: {jwt, config}` and, for an assets-only site, no `main_module` and no module part.

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
6. Add the discrepancy to the table above. When flarefake is fixed, delete the entry and its shim, add a scenario step that asserts the fixed behavior, and move the ID to the "Fixed" table.
