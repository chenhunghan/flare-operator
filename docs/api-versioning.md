# API versioning: v1alpha1 → v1beta1

Status: plan (2026-09-29). Nothing described under "Mechanics" is implemented yet; in
particular there is no conversion webhook.

## Where we are

- Every kind is served at `v1alpha1`, which is also the storage version. Each CRD has exactly
  one version (`TestCRDConventions` in `internal/apivalidation` checks this).
- Every kind is in one API group, `flare.dev`. Versions are per CRD, not per group, so each kind
  can still graduate on its own (`flare.dev/v1beta1` serving some kinds while others stay at
  `v1alpha1`).
- Generated kinds take their version from `version:` in `generator.yaml` (one value for all of
  them); hand-written kinds (`CloudflareAccount`, `Tunnel`, `VPCService`, `WorkerScript`) from
  their Go package under `api/<product>/v1alpha1`.
- The reference for the current schemas is [api-reference.md](api-reference.md), generated from
  the CRDs.

`v1alpha1` means: the shape is Crossplane-style and meant to last (`spec.accountRef`,
`spec.forProvider`, `spec.deletionPolicy`, `spec.managementPolicies`, `status.id`,
`status.conditions`, `status.observedGeneration`, `status.atProvider`), but fields of individual
kinds may still change incompatibly between minor releases, following the deprecation policy
below.

## Graduation criteria (per kind)

A kind moves to `v1beta1` when all of these hold:

1. **Evidence.** Every Cloudflare route the kind's controller calls on its normal path (create,
   read, update, delete, adopt, ownership tagging) is emulated by flarefake with evidence of tier
   recording or SOURCED (CLAUDE.md, "Emulator fidelity"), not DOCS or UNVERIFIED. The kind's
   UNVERIFIED items in `docs/known-issues.md` (e.g. Queue `jurisdiction` read-back, the Resource Tagging
   `resource_type` values) are resolved or explicitly accepted as documented limitations.
2. **Differential tests.** The real Cloudflare clients (wrangler, cloudflare-go) agree with
   flarefake on the kind's routes (`test/differential`).
3. **Conformance suite.** `internal/generic/kindsuite` (generated kinds) or the controller's
   envtest suite (hand-written kinds) covers create, idempotency, update, immutable changes
   (API server rejection and the controller's `Immutable` reason), external deletion,
   observe-only adoption and both deletion policies; `make e2e` passes on a real cluster.
4. **Stable schema.** No incompatible change to the kind's `spec` or `status` for one minor
   release, and none planned. Numeric types match what the API returns (e.g. the spec's `number`
   fields that are really integers are fixed through `generator.yaml` `fields` overrides before
   graduation, not after).
5. **Validation.** Immutable fields have CEL transition rules; required, mutually exclusive and
   enum fields are enforced by the schema; `internal/apivalidation` has cases for each.
6. **Status and conditions.** The kind follows the status conventions and uses only condition
   reasons documented in the reasons table of the API reference (`TestReasonsDocumented`).
7. **Upgrade test.** Objects created at `v1alpha1` by the previous release reconcile after the
   upgrade with zero Cloudflare writes (a new e2e step that installs the previous chart,
   creates objects, upgrades and checks the flarefake journal).

`CloudflareAccount` graduates first or together with the first managed kind, because every
managed kind refers to it.

## Known changes planned for v1beta1

- `CloudflareAccount`: remove `status.tokenID`, `status.tokenStatus` and
  `status.tokenExpiresOn`, deprecated since they were duplicated by `status.atProvider`
  (`id`, `status`, `expires_on`). `status.tokenType` stays (it is not an API field).
- Generated kinds: fix `number` fields that are integers (e.g. Queue
  `atProvider.consumers_total_count`), decided per kind from recordings.

Anything else found while checking the criteria is listed here before the release that
deprecates it.

## Mechanics

### Serving and storage

1. **Release N**: add `v1beta1` as a served version next to `v1alpha1`; `v1alpha1` stays the
   storage version. `v1alpha1` is marked `deprecated: true` with a `deprecationWarning` naming
   `v1beta1`, so kubectl and client-go print a warning on every request.
2. **Release N+1**: make `v1beta1` the storage version. Objects are rewritten to it the next
   time they are written (every controller writes status regularly). Run a storage migration
   anyway (`kube-storage-version-migrator`, or `kubectl get <kind> -A -o json | kubectl replace -f -`
   documented in the upgrade notes), then remove `v1alpha1` from the CRD's
   `status.storedVersions`.
3. **Release N+1 + deprecation period** (below): stop serving `v1alpha1`, then drop it.

Helm does not upgrade CRDs under `crds/` (`charts/flare-operator/README.md`), so each of these
steps is applied with `kubectl apply -f charts/flare-operator/crds/` before `helm upgrade`, as
documented in the release notes.

### Conversion

- **No structural change** (the expected case: fields added, deprecated fields removed): use
  `conversion.strategy: None`. The API server then only rewrites `apiVersion`. Fields that exist
  only in one version are pruned when an object is read or written through the other one, which
  is acceptable for deprecated *status* fields (the controllers recompute status) and for new
  optional fields (a `v1alpha1` client simply does not see them). This needs no webhook.
- **Structural change** (a field renamed, moved or retyped): a conversion webhook is required,
  because `None` would silently lose data. It would be served by the manager binary
  (controller-runtime's webhook server) with `v1beta1` as the hub (`conversion.Hub`) and
  `v1alpha1` implementing `conversion.Convertible`; the chart would add the Service, the
  webhook certificate (cert-manager, or a self-signed certificate the manager rotates) and the
  CRD's `conversion.webhook.clientConfig`. Round-trip fuzz tests (`v1alpha1 → hub → v1alpha1`)
  would cover every kind. None of this exists yet; the plan is to avoid structural changes so
  that it is never needed for `v1alpha1 → v1beta1`.

### Code generation

- `flaregen` needs per-kind versions: a `versions` list in each `generator.yaml` entry
  (`name`, `served`, `storage`, `deprecated`), emitting one CRD with several versions and one Go
  package per version (`api/<product>/v1alpha1`, `api/<product>/v1beta1`). The descriptors in
  `internal/generic/descriptors` and the generic controllers use the storage version only.
- Hand-written kinds copy their package to `v1beta1` and keep `v1alpha1` as a thin copy of
  the types (with `+kubebuilder:deprecatedversion`) until it is dropped; `v1beta1` gets
  `+kubebuilder:storageversion` in release N+1.
- Controllers watch and write the storage version only; the API server converts for clients
  using the other version.

## Deprecation policy

Modelled on the Kubernetes API deprecation policy.

- **What is API**: CRD group, version, kind and resource names; every `spec` and `status`
  field, its type, enum values, defaults and validation; annotations and labels the operator
  reads or writes (`flare.dev/external-id`, `flare.dev/account`, the
  finalizers); condition types and reasons (the reasons table in
  [api-reference.md](api-reference.md)); the default `deletionPolicy` of each kind.
- **Not API**: printer columns and their order, categories, Event messages and condition
  messages. Short names are kept once released, but may gain aliases.
- **How a deprecation is announced**: a `Deprecated:` paragraph in the field's description (so it
  shows in `kubectl explain` and in the API reference), a line in the release notes, and for a
  version `deprecated: true` plus a `deprecationWarning` on the CRD version.
- **How long deprecated things stay**:
  - `v1alpha1`: at least one minor release after the deprecation is announced.
  - `v1beta1`: at least three minor releases or nine months, whichever is longer.
  - `v1`: not removed within the major version.
- **Validation changes**: tightening validation (a new enum restriction, pattern or CEL rule)
  can make existing objects fail on their next update. New rules are therefore written so that
  CRD validation ratcheting (on by default since Kubernetes 1.30) lets unchanged fields keep
  their values, i.e. they do not use `oldSelf` unless they are transition rules on purpose, and
  the release notes list them. Loosening validation is always allowed.
- **Behaviour changes**: changing a default (such as a kind's default `deletionPolicy`) or what
  a condition reason means counts as an incompatible change and follows the same periods.
- **Adding** optional fields, enum values the API accepts, condition reasons, printer columns or
  kinds is compatible and needs no deprecation.
