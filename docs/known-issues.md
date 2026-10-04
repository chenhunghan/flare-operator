# Known issues and limits

This page lists what users and contributors should know before relying on flare-operator: the
project's status, its known limits, behaviour that may surprise you, review findings that were
deferred, and the assumptions that are still UNVERIFIED against the live Cloudflare API. The
exhaustive, per-route list of emulator assumptions is in
[emulator-fidelity.md](emulator-fidelity.md); this page summarizes it.

Names below use the `flare.dev` API group (`apiVersion: flare.dev/v1alpha1`) and label and
annotation keys such as `flare.dev/worker-binding`.

## Status

- **Alpha.** Every kind is `v1alpha1` and may change without notice. There is no conversion
  webhook yet; [api-versioning.md](api-versioning.md) describes the path to `v1beta1`.
- **Kinds.** CloudflareAccount, KVNamespace, Queue, D1Database, VectorizeIndex, SecretsStore,
  AIGateway and R2Bucket (generated); Tunnel, VPCService, WorkerScript, PagesProject and
  PagesDeployment (hand-written). The virtual kubelet for Cloudflare Containers is designed, not
  implemented ([virtual-kubelet-design.md](virtual-kubelet-design.md)).
- **Emulator-verified, not live-verified.** Every controller is tested against `flarefake`, an
  in-memory emulator of the Cloudflare API, in envtest suites and in fault, crash-consistency and
  scale tests. Real Cloudflare clients (wrangler, cloudflared, cloudflare-go) run against
  flarefake in [differential tests](differential-testing.md). The chart's e2e suite and the
  upgrade e2e have passed on k0s with flarefake in the cluster (linux/arm64 only).
- **No live run of the operator yet.** The live smoke test (`test/live`, `make live`) exists but
  has not been run against a real account. The only real-API evidence is the recordings from the
  design spikes ([spike-results-2026-09-29.md](spike-results-2026-09-29.md)).
- **Zone-level kinds** (DNS records, custom domains, email sending domains) are deferred: none of
  the current kinds needs a zone, and none has been tested with one.
- **Containers** (and therefore the virtual kubelet) need the Workers Paid plan and have not been
  prototyped beyond the design.

## Limits

- Over 200 `UNVERIFIED` markers remain in the emulator's non-test code (`internal/fake`), mostly
  failure-path error codes and messages and response fields no client reads
  ([emulator-fidelity.md](emulator-fidelity.md)).
- `VectorizeIndex`, `SecretsStore`, `AIGateway` and `R2Bucket` are emulated by flarefake's
  generic profile from the spec. The only recordings are one list call each for Vectorize and
  Secrets Store; AI Gateway and R2 have none.
- Resource Tagging (the ownership tags) has no recording at all.
- D1 SQL is not really emulated: flarefake runs only constant `SELECT`s and answers other SQL
  with 400/99999.
- Two differential discrepancies remain, both client defects in cloudflare-go, not flarefake:
  `Workers.Scripts.Update` flattens the multipart form, and `ScriptAndVersionSettings.Get`
  panics decoding `"placement": {}` (which the live API also sends). Operator code must not use
  that SDK call.
- Cloudflare's granular Workers roles (2026-09-15) are newer than the pinned spec, which knows
  only the legacy groups. flarefake's permission checks and the README token table use the
  legacy names; creating or deleting Workers with the new roles needs **Admin at Workers product
  scope**. The spec should be re-pinned once it carries the new roles.
- The pinned spec contradicts the real API in a few places; these are documented exceptions in
  `internal/generic/descriptors/emulator_test.go` and allowlisted in the e2e journal check:
  D1 `primary_location_hint` enum case (recording 0019), the D1 `database_id` path parameter as
  an ambiguous oneOf (0020), and KV `DELETE` requiring a body in the spec while the API deletes
  without one (0013). The reconciler sends `DELETE` with no body.

## Behaviour that users will notice

- The chart requires `clusterName`; an install without it fails. Give every cluster that manages
  the same account its own name (it is part of the ownership tags).
- A same-named untagged resource gives `NameConflict` unless the external-id annotation pins it.
  This applies to the untaggable generated kinds (VectorizeIndex, SecretsStore, AIGateway), to
  every generated kind with `--ownership-tags=false`, and to Tunnel. The only exception is the
  object's own lost create, which the operator recognizes from its create-pending record.
- Tagged generated kinds (KVNamespace, Queue, D1Database) with tagging on still adopt and tag an
  untagged same-named resource. They default to `deletionPolicy: Orphan`.
- `spec.accountRef` is immutable once `status.id` is set, and `CloudflareAccount.spec.accountID`
  is always immutable (CEL).
- A `secret_text` binding needs a Secret labelled `flare.dev/worker-binding=true`; service
  account token Secrets are refused.
- VectorizeIndex and SecretsStore hold data but default to `deletionPolicy: Delete`. Set
  `Orphan` if deleting the object must not delete the index or store.
- CEL immutability rules let an immutable field be removed and then re-added with a different
  value; only the controller's check (`Synced=False`, reason `Immutable`) catches that. For
  Tunnel, the default case (no `forProvider.name`, falling back to `metadata.name`) is likewise
  caught only by the controller.
- A token Secret referenced by a CloudflareAccount carries a finalizer. Deleting it directly
  leaves it `Terminating` while the account exists, so rotating by delete-and-recreate (for
  example `kubectl replace --force`, some secret managers) fails with `AlreadyExists`. Secrets
  owned by other tools (External Secrets and similar) get this foreign finalizer too.
- After an operator restart, a deleting CloudflareAccount whose token Secret is already gone
  blocks until someone intervenes; the account's condition message explains how.
- `LateInitialize` is accepted in `managementPolicies` but has no effect yet.
- Helm never upgrades CRDs in `crds/`: apply the new chart's CRDs before every `helm upgrade`
  ([operations.md](operations.md#upgrade)). Upgrade with `--reset-then-reuse-values`, not
  `--reuse-values`.
- CloudflareAccount `status.tokenID`, `tokenStatus` and `tokenExpiresOn` are deprecated; they are
  still filled in and are planned for removal in `v1beta1`.

## Deferred review findings

Found in reviews and not fixed yet:

- **Cache scope.** Secrets and ConfigMaps (other than Helm-release and service-account-token
  Secrets, whose data is dropped) are cached in full cluster-wide, and Deployments and Services
  are cached unfiltered. A label-filtered or metadata-only cache, or namespace scoping, is not
  implemented ([SECURITY.md](../SECURITY.md)).
- **Tagging for Vectorize and AI Gateway.** The spec's Resource Tagging enum has
  `vectorize_index` and `ai_gateway`, but `generator.yaml` sets no `tagResourceType` for them, so
  they are not tagged (live support is UNVERIFIED).
- The manager binary's `--cluster-name` still defaults to `default`; only the chart enforces a
  value.
- Smaller items: the write-only field hash is an unsalted SHA-256; the Tunnel NetworkPolicy can
  disclose Services in other namespaces; the cloudflared metrics port has no ingress policy;
  images are not pinned by digest and not signed; one shared per-token rate limiter can be
  retuned by any account using the token; metrics have no account label; generic kinds recreate
  a pinned resource that was deleted outside the operator; generated kind descriptions are
  generic text; unset name defaults are not validated; `zoneRef` is accepted but unused; the
  conformance guard does not cover generic-profile routes; the hand-written controller suites do
  not validate requests against the spec.
- A `WorkerScript` that never uploaded (`DependencyNotReady`) still blocks deletion of the KV,
  Queue, D1 or VPCService it references (conservative). Permanent 4xx upload errors are retried
  at resync or on a spec change, not with back-off.
- The WorkerScript ownership proof is keyed by script name: if the script is deleted outside the
  operator and someone recreates one with the same name, it is treated as owned.
- If the live `GET …/settings` echoes bindings differently from the upload, drift detection
  re-PATCHes settings once per resync (10 minutes). Needs a live recording.
- The generator does not support singletons, nested (parent path parameter) resources,
  upsert-style or non-JSON-body resources yet.
- Pinned spec defects block some generic kinds: valid Hyperdrive `PATCH` caching bodies match
  more than one oneOf branch. Turnstile is not generated because its `GET` returns the
  server-generated secret.
- While a CloudflareAccount is being deleted, an object can still resolve the account just
  before the account's finalizer is removed, because the cache may not show the deletion yet
  (the window is narrowed, not closed).
- Test suite: a few timing-sensitive tests can fail under heavy machine load (for example
  `TestLimiterSharedPerToken` in `internal/cfclient`); e2e still has sleep-based waits.

## UNVERIFIED against the live API

Each item is emulated from the spec, a client's source or the docs, not from a recording. Details
are in [emulator-fidelity.md](emulator-fidelity.md).

- **Accounts and tokens:** the status and error code for an invalid token or another account's
  token at `/accounts/{id}/tokens/verify`; the fallback to `/user/tokens/verify` for user tokens.
- **Resource Tagging:** the error for a never-tagged resource, `DELETE` with no body, `412` on
  an `If-Match` mismatch and whether the value must be quoted, the etag format, whether the tag
  index lags writes or lists deleted resources, the `resource_type` values (`kv_namespace`,
  `queue`, `d1_database`, `worker`), the owner tag key `flare.dev/owner` with `/` in the value,
  and the status codes for a token without tagging permission.
- **Request bodies:** whether the live API accepts bodies built from `forProvider` filtered by
  the create and update fields, and whether every `atProvider` type decodes every live
  response.
- **Queues:** `jurisdiction` handling (stored and echoed only when set; marked immutable by
  override although the spec allows it in updates).
- **Tunnel and VPCService:** the cloudflared pod security context and default resources, the
  default `cloudflared` image tag (inferred from recordings), the `degraded` tunnel status,
  duplicate tunnel names, VPC service port defaults, and whether the generated NetworkPolicy is
  enforced by the CNI.
- **Workers:** the settings `PATCH` route (multipart, a new version per change, error codes),
  the `GET …/settings` echo format for most binding types and observability, per-script
  subdomain `GET` and the workers.dev default, version and deployment ordering, module content
  types for JSON modules, the etag derivation, and whether Cloudflare refuses to delete a KV,
  Queue or D1 that a Worker still binds (only the VPC case is recorded).
- **Workers static assets and Pages:** both upload protocols come from wrangler's source and
  pass real `wrangler deploy` and `wrangler pages deploy` against flarefake, but have not been
  checked live; bucket sizing, error codes and JWT claims are UNVERIFIED.
- **Generic-profile kinds:** error codes, duplicate-name rejection, ID shapes, timestamps, PUT
  and PATCH semantics, list order and paging defaults; whether the Free plan can create
  Vectorize, Secrets Store and AI Gateway resources; the Secrets Store one-store limit.
- **Kubernetes side:** Events (`ExternalResourceKept`, `ForeignOwnerTunnelKept`, `NameConflict`)
  and RBAC for the account in-use listing were checked in envtest, not in a real namespace
  deletion; real namespace-controller ordering with a `Terminating` token Secret.
- **Images:** only linux/arm64 images have run in e2e; linux/amd64 is built but not exercised.
