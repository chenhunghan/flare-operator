# Changelog

All notable changes to flare-operator are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Every API is `v1alpha1`: until 1.0, a minor version
may change the API. Chart versions are listed with the release they belong to. Before a release,
the chart version in `charts/flare-operator/Chart.yaml` is bumped on every chart change.

## [Unreleased]

Nothing has been released yet. This section collects what the first release will contain.

### Added
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
- flarefake emulates the Workers assets upload session, the bucket uploads (session JWTs) and
  `metadata.assets` / `keep_assets`; `wrangler deploy` of an assets site runs against it
  (`make differential`).
- Crossplane-style `deletionPolicy`, `managementPolicies` (including observe-only),
  adoption through `cloudflare.flare.dev/external-id`, and ownership tags
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
  images and the packaged chart. Publishing is disabled until the repository has a permanent
  home.
- `make lint-static` (staticcheck) and `make vulncheck` (govulncheck), in `make ci` and the CI
  workflow. `make crds-apply` and `make crds-diff` for CRD upgrades. `make e2e-upgrade` tests
  an upgrade from a previous git ref.
- `SECURITY.md` and `docs/operations.md` (install, upgrade, uninstall semantics, metrics,
  leader election, network policy, rate limiting, troubleshooting, backup and restore,
  multi-account setup).

### Changed
- The Go toolchain is pinned to go1.26.8 (`toolchain` directive, Dockerfile `GO_VERSION`),
  which fixes the standard-library vulnerabilities govulncheck reported for go1.26.1.
- The hand-written API packages register their kinds with apimachinery's
  `runtime.SchemeBuilder`, like the generated ones, instead of controller-runtime's deprecated
  `scheme.Builder`.

### Upgrade notes
- Helm does not upgrade CRDs. Run `make crds-apply` (or
  `kubectl apply --server-side --force-conflicts -f charts/flare-operator/crds/`) before
  `helm upgrade`. See [docs/operations.md](docs/operations.md#upgrade).
