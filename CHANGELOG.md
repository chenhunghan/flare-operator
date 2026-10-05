# Changelog

All notable changes to flare-operator are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Every API is `v1alpha1`: until 1.0, a minor version
may change the API. The chart version equals the release version.

From the first release on, [release-please](https://github.com/googleapis/release-please) writes
this file from Conventional Commit messages; new entries are added above the pre-release notes.

## 0.1.0 (2026-10-05)


### Features

* kubectl logs for Workers via a virtual kubelet ([11988b6](https://github.com/chenhunghan/flare-operator/commit/11988b6ba98dc1014774570802410d8a7dcecc79))
* kubectl logs for Workers via a virtual kubelet ([7da295f](https://github.com/chenhunghan/flare-operator/commit/7da295ffcd89f27557aa96ae16b291b251357555))
* release with release-please; publish images and the Helm chart to GHCR ([161e9e9](https://github.com/chenhunghan/flare-operator/commit/161e9e9410e938e6ad96df08dc8a21a9795cc638))


### Bug Fixes

* back off per object on repeated status write conflicts ([b44fc57](https://github.com/chenhunghan/flare-operator/commit/b44fc573403a55b7f075242401fc0472a54e60a9))
* confirm a finalizer's CloudflareAccount uncached before reaching Cloudflare ([66a3b15](https://github.com/chenhunghan/flare-operator/commit/66a3b1588d55f08f8a786bde661fb8a8b45984b2))
* **fake:** register a tail connection before answering the WebSocket handshake ([433aab8](https://github.com/chenhunghan/flare-operator/commit/433aab8f079b3a2f5b5b068db5c51a1059df7022))
* never build a status from a cached copy older than the last write ([8cfce82](https://github.com/chenhunghan/flare-operator/commit/8cfce8289a7da30bf30ab492d3dd2be01bd95b1d))
* never build a status from a cached copy older than the last write ([4912f7a](https://github.com/chenhunghan/flare-operator/commit/4912f7a5d88f5743eda3bfb824901bb7cc7c313f))
* optimistically lock every status write instead of re-reading uncached ([893e7b9](https://github.com/chenhunghan/flare-operator/commit/893e7b901333dbb656f7c79289aecafdae096e79))
* optimistically lock every status write instead of re-reading uncached ([7a7ba82](https://github.com/chenhunghan/flare-operator/commit/7a7ba825898dc801e48b814b8691abbfa8b2d9a3))
* **vk:** serialize stand-in Pod status syncs ([bdd29d0](https://github.com/chenhunghan/flare-operator/commit/bdd29d0705893e321f6b41de1895fb99a217c9f5))

## [Pre-release] development history

Everything below was built before the first public release, 0.1.0, and ships in it.

### Added
- `examples/fullstack`: a complete notes app for `kubectl apply -k`. One Worker serves the
  static-assets SPA and an API bound to D1, R2, KV and a `secret_text` Secret, with workers.dev
  on. A Pages variant binds the same data. Its README covers token permissions, deploying,
  applying the D1 schema with wrangler, status fields and teardown per `deletionPolicy`.
  `go test ./examples/` validates every file and the kustomize output. `TestFullStack` applies
  it against flarefake and checks readiness, wiring, zero writes on re-reconcile and teardown,
  and the e2e suite has a `FullStack` step that does the same.
- Kinds: `CloudflareAccount` (token verification, rate-limit settings, usage protection),
  generated `KVNamespace`, `Queue` and `D1Database`, and hand-written `Tunnel` (with a managed
  `cloudflared` Deployment and an egress NetworkPolicy) and `VPCService`.
- `WorkerScript` full-stack fields: static assets (`forProvider.assets`: files from an artifact,
  `html_handling`, `not_found_handling`, `run_worker_first`, `base_path`, `_headers`,
  `_redirects`, `.assetsignore`) uploaded with the Workers assets upload flow, only when they
  change (`status.assetsHash`), with wrangler's manifest rules (`.assetsignore` matched
  case-insensitively) and Content-Types (the pinned wrangler's mime table, `make asset-mime`);
  assets-only Workers (no `main_module`); modules from an artifact (`forProvider.moduleSource`,
  `moduleTypes`); `assets`, `r2_bucket` and `send_email` bindings. `main_module` is now optional (required unless the Worker is assets-only).
- Kinds: hand-written `PagesProject` (deployment configs, environment variables, bindings) and
  `PagesDeployment` (Direct Upload of an artifact with wrangler's hashing, bucketing and
  Content-Types; artifacts needing a build, a `_worker.js` directory or a Pages Functions
  `functions` directory without `_worker.js`, are refused with `InvalidArtifact`). flarefake
  emulates the Pages projects, deployments and asset upload API; `wrangler pages deploy` runs
  against it (`make differential`).
- flarefake emulates the Workers assets upload session, the bucket uploads (session JWTs) and
  `metadata.assets` / `keep_assets`; `wrangler deploy` of an assets site runs against it
  (`make differential`).
- Crossplane-style `deletionPolicy`, `managementPolicies` (including observe-only),
  adoption through `flare.dev/external-id`, and ownership tags
  (`flare.dev/owner=<clusterName>/<namespace>/<name>`).
- `flarefake`, a Cloudflare API emulator validated against recordings of the real API
  (`make conformance`).
- Helm chart `flare-operator` (chart 0.2.0):
  - `values.schema.json`: unknown keys and wrong types fail `helm install`, `upgrade` and
    `lint`.
  - Optional PodDisruptionBudget (rendered when `replicas > 1`), `topologySpreadConstraints`
    (the manager's selector is filled in), `priorityClassName`.
  - Optional manager NetworkPolicy (`networkPolicy.enabled`): DNS, API server, HTTPS to
    Cloudflare's published ranges, and metrics ingress.
  - Optional prometheus-operator ServiceMonitor (`metrics.serviceMonitor.enabled`).
  - NOTES with the image, the version and the CRD upgrade step.
- Version stamping: `manager --version` and `flarefake -version`, a startup log line with
  version, commit and build date, and OCI image labels (`org.opencontainers.image.*`).
- Multi-arch images (`make docker-buildx`, linux/amd64 and linux/arm64) with BuildKit SBOM and
  provenance attestations. `make sbom` writes SPDX SBOMs.
- Release configuration (`.goreleaser.yaml`): binaries, archives, checksums, SBOMs, multi-arch
  images and the packaged chart. A `v*` tag runs `.github/workflows/release.yml`, which publishes
  the images to `ghcr.io/chenhunghan`, the GitHub release, and the chart to
  `oci://ghcr.io/chenhunghan/charts`.
- Apache License 2.0 (`LICENSE`, `NOTICE`); the pinned Cloudflare OpenAPI schema keeps its
  BSD-3-Clause license (`spec/LICENSE`). The chart's default image repositories are
  `ghcr.io/chenhunghan/flare-operator` and `ghcr.io/chenhunghan/flarefake`.
- `make lint-static` (staticcheck) and `make vulncheck` (govulncheck), in `make ci` and the CI
  workflow. `make crds-apply` and `make crds-diff` for CRD upgrades. `make e2e-upgrade` tests
  an upgrade from a previous git ref.
- Generated kind `R2Bucket` (default `deletionPolicy: Orphan`):
  jurisdiction (the `cf-r2-jurisdiction` header, immutable), location hint, storage class and
  CORS policy. It comes with three `generator.yaml` extensions for generated kinds:
  `requestHeaders`, `observedAs` and `subResources` (docs/generator-scaleout.md). A refused
  delete (an R2 bucket that is not empty) is reported as `Synced=False`, reason `DeleteFailed`.
  `cors: {}` removes the bucket's CORS policy. A create that the API refuses for good (a 4xx
  other than 400, 408, 409, 429; e.g. R2 not enabled) drops the object's create-pending record,
  so a same-named bucket created later by someone else is a NameConflict, not adopted.
  Emulated by flarefake's generic profile; not verified against the live API.
- `SECURITY.md` and `docs/operations.md` (install, upgrade, uninstall semantics, metrics,
  leader election, network policy, rate limiting, troubleshooting, backup and restore,
  multi-account setup).

### Changed
- **Breaking (API group rename, before the first release).** Every kind is now in one API
  group, `flare.dev` (`apiVersion: flare.dev/v1alpha1`), instead of one group per product
  (`kv.cloudflare.flare.dev`, `queues.cloudflare.flare.dev`, ..., and `cloudflare.flare.dev`
  for `CloudflareAccount`). CRD names follow (`kvnamespaces.flare.dev`; files
  `flare.dev_<plural>.yaml`), and the chart's RBAC rules collapse to `apiGroups: [flare.dev]`.
  Labels, annotations and finalizers move from `cloudflare.flare.dev/<key>` to
  `flare.dev/<key>` (`external-id`, `account`, `finalizer`, `account-in-use`, `account-token`,
  `artifact`, `worker-binding`, `create-pending`, `created-by-uid`, `ownership-proof`,
  `tunnel`, `tunnel-id`, `spec-hash`), and the leader-election Lease is
  `flare-operator.flare.dev`. The Go module path is `github.com/chenhunghan/flare-operator`.
  Because all kinds share the group, `flaregen` fails when two kinds (generated or
  hand-written) share a kind, plural, singular or short name; the fix is an explicit `kind:` and
  `plural:` for the new kind in `generator.yaml` (the per-kind `group:` key, which named the
  Go package and category, is now `product:`; the top-level `groupSuffix:` is now `group:`).
  Nothing is renamed automatically. There is no migration: objects and CRDs of a pre-rename
  build cannot be upgraded in place (delete them with `deletionPolicy: Orphan`, install this
  release, re-create them with the `flare.dev/external-id` annotation to adopt the Cloudflare
  resources), and `make e2e-upgrade` refuses a pre-rename `E2E_UPGRADE_FROM`.
- The Go toolchain is pinned to go1.26.8 (`toolchain` directive, Dockerfile `GO_VERSION`),
  which fixes the standard-library vulnerabilities govulncheck reported for go1.26.1.
- The hand-written API packages register their kinds with apimachinery's
  `runtime.SchemeBuilder`, like the generated ones, instead of controller-runtime's deprecated
  `scheme.Builder`.

### Upgrade notes
- Helm does not upgrade CRDs. Run `make crds-apply` (or
  `kubectl apply --server-side --force-conflicts -f charts/flare-operator/crds/`) before
  `helm upgrade`. See [docs/operations.md](docs/operations.md#upgrade).
