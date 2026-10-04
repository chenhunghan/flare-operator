# Security

flare-operator holds Cloudflare API tokens and acts on a Cloudflare account with them. This page
says how the tokens are handled, what the operator is allowed to do in the cluster, and how to
keep both as small as possible. Operating procedures (install, upgrade, troubleshooting) are in
[docs/operations.md](docs/operations.md).

## Reporting a vulnerability

Report a vulnerability privately through GitHub's
[private vulnerability reporting](https://github.com/chenhunghan/flare-operator/security/advisories/new).
Don't open a public issue, and don't put a real token, account ID or recording in a report.

## Supported versions

The project is alpha. Only the latest release and the `main` branch get fixes.

## API tokens

- **Where a token lives.** Each `CloudflareAccount` names a Secret in its own namespace
  (`spec.tokenSecretRef`, key `token` by default). It never writes a token into a status, an
  event, a log line or another object. The operator reads the Secret values of:
  - the token Secrets that CloudflareAccounts reference;
  - the Secrets that WorkerScript `secret_text` bindings reference (`secretKeyRef`), and only
    those labelled `flare.dev/worker-binding=true` that are not service account
    tokens. The value is uploaded to Cloudflare as a Worker secret, and the Worker's code can
    return it, so never put that label on a token Secret;
  - the `<tunnel>-cloudflared-token` Secrets it writes itself for Tunnel connectors.

  It caches every Secret of the cluster, though (see [In-cluster privileges](#in-cluster-privileges)).
- **Use one token per purpose.** Use an account-owned API token scoped to the one account
  (`spec.accountID`), with only the permissions the kinds you create need. See
  [Least-privilege permissions](#least-privilege-permissions). Don't use a Global API Key: the
  operator supports only bearer tokens.
- **Where a token is sent.** A token goes only to `https://api.cloudflare.com/client/v4`,
  unless `spec.baseURL` overrides that. The manager ignores overrides unless you pass
  `--allow-base-url-override` (chart `baseURLOverride.allowAny`) or list the URL with
  `--allowed-base-url` (chart `baseURLOverride.allowed`). Otherwise the account reports
  `Ready=False` with reason `BaseURLNotAllowed`. An override sends the token to that URL, so
  allow one only in test clusters, and never enable `flarefake.enabled` in a cluster that
  manages a real account.
- **Rotation.** Update the Secret in place. The account controller watches its token Secret,
  verifies the new token, and managed objects use the new client from then on. The manager
  caches one API client per CloudflareAccount and rebuilds it when the token changes. The rate
  limiter is shared by every account that uses the same token, keyed by the token's SHA-256,
  so the token itself is not kept as a map key.
- **Verification.** Every token is verified on create, on change, and every 10 minutes
  (`GET /accounts/{id}/tokens/verify`, falling back to `GET /user/tokens/verify` plus
  `GET /accounts/{id}` for user tokens). A disabled, expired or not-yet-valid token turns the
  account `Ready=False`, and its managed objects then report `AccountNotReady` and stop calling
  Cloudflare.
- **Deletion protection.** A token Secret carries the finalizer
  `flare.dev/account-token` while an account uses it, and an account carries
  `flare.dev/account-in-use` while managed objects reference it. Together they keep
  a namespace deletion from removing the token before the objects have cleaned up in
  Cloudflare.
- **RBAC on Secrets.** Anyone who can read Secrets in a namespace can read its tokens. Anyone
  who can create a CloudflareAccount there can make the operator act with any token stored
  there. Grant `cloudflareaccounts` create/update like you grant Secret access. The chart's
  aggregated roles give `cloudflareaccounts` write access to `edit` and `admin`, matching those
  roles' Secret access.
- **RBAC on WorkerScripts and Tunnels.** Anyone who can create or update a WorkerScript can read,
  through a Worker they write, every Secret of the namespace labelled
  `flare.dev/worker-binding=true`: treat that label as granting Secret read access to
  WorkerScript authors. Anyone who can create or update a Tunnel can have the operator run a
  Deployment with any image (`spec.connector.image`) in that namespace: grant `tunnels` like you
  grant `deployments` create.

## Least-privilege permissions

The Cloudflare permission groups each kind needs are listed in one place, the README's
[Token permissions](README.md#token-permissions) table; this page does not repeat them. That
table also carries the note on Cloudflare's granular Workers roles (introduced 2026-09-15). In
short: creating or deleting Workers needs **Admin at Workers product scope**, Editor cannot
create or delete, and the legacy account-level "Workers Scripts Edit/Read" groups still work. For an object with
`managementPolicies: ["Observe"]`, the Read variant is enough. With ownership tags on (the
default), the token also needs Resource Tagging access; install with `ownershipTags=false` to
avoid granting it.

## In-cluster privileges

The manager runs as one Deployment with the ServiceAccount the chart creates:

| Grant | Why |
|---|---|
| ClusterRole `…-manager` (generated from the controllers' RBAC markers, `config/rbac/role.yaml`) | Watch and update the flare.dev kinds everywhere. Cluster-wide `get`/`list`/`watch` on ConfigMaps and Services, and `get`/`list`/`watch`/`create`/`update`/`patch`/`delete` on Secrets, Deployments and NetworkPolicies: the Tunnel controller creates the `cloudflared` Deployment, the token Secret and the egress NetworkPolicy next to each Tunnel, and the account controller adds and removes a finalizer on token Secrets. |
| Role `…-leader-election` in the release namespace | The leader-election Lease and its events. |
| ClusterRoles `…-aggregate-to-view` and `…-aggregate-to-edit` (the latter also aggregates to `admin`; optional, `rbac.aggregateToDefaultRoles`) | Let the default user roles work with the flare.dev kinds. |

The manager caches Secrets, ConfigMaps, Deployments and Services cluster-wide, so it can read
(and, for Secrets, Deployments and NetworkPolicies, write) every one of them. That is the cost of
supporting CloudflareAccounts and Tunnels in any namespace; the chart has no option yet to scope
the manager to some namespaces. Restricting where CloudflareAccounts, WorkerScripts and Tunnels may
be created (admission policy, or RBAC for tenants) limits what tenants can make it do, not what the
manager itself can reach. The cache drops managedFields and the data of Helm release and service
account token Secrets; size the manager's memory for the remaining Secrets and ConfigMaps.

Artifact sources (the deployable content of kinds that embed an `ArtifactSource`) are read only
from ConfigMaps and pull Secrets labelled `flare.dev/artifact=true`, over HTTPS, and
never from loopback, private, link-local (cloud metadata) or other non-public addresses unless
`--artifact-allowed-cidr` lists them; archives and images are unpacked under size, file-count
and compression-ratio limits with path-traversal and link checks. See
[docs/artifacts.md](docs/artifacts.md).

Pod hardening (chart defaults, checked by `test/chart`): non-root UID 65532, `runAsNonRoot`,
seccomp `RuntimeDefault`, no privilege escalation, a read-only root filesystem, all
capabilities dropped, and a distroless static base image. This meets the Kubernetes
**restricted** Pod Security Standard, so the release namespace can be labeled
`pod-security.kubernetes.io/enforce=restricted`.

Network: `networkPolicy.enabled=true` limits the manager to DNS, the API server, HTTPS to
Cloudflare's published ranges, and the metrics port inbound. The metrics endpoint is plain HTTP
without authentication; it exposes only controller-runtime metrics, no tokens. See
[docs/operations.md "Network policy"](docs/operations.md#network-policy).

## Supply chain

- `make vulncheck` runs govulncheck (pinned `golang.org/x/vuln` v1.8.0). `make lint-static`
  runs staticcheck (pinned v0.8.1, 2026.1.1). Both run in `make ci` and in the CI workflow. The
  Go toolchain is pinned by the `toolchain` directive in `go.mod` and by `GO_VERSION` in the
  Dockerfiles. Raise both when govulncheck reports a standard-library fix.
- Images are built on `gcr.io/distroless/static-debian12:nonroot` with `CGO_ENABLED=0`,
  `-trimpath`, and OCI labels (`org.opencontainers.image.*`: version, revision, created,
  source, base name). `make docker-buildx` adds a BuildKit SPDX SBOM and minimal provenance
  attestations. `make sbom` writes SPDX SBOMs, with syft when it is installed. The release
  config (`.goreleaser.yaml`) produces checksums and SBOMs for every archive and SBOM
  attestations for the images.
- Releases are built by `.github/workflows/release.yml` from a `v*` tag and published to GHCR
  (`ghcr.io/chenhunghan/flare-operator`, `ghcr.io/chenhunghan/flarefake`, and the chart at
  `oci://ghcr.io/chenhunghan/charts/flare-operator`). Images and binaries are not signed yet;
  signing (cosign keyless) is planned.
