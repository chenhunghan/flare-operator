# flare-operator

flare-operator lets you manage Cloudflare from Kubernetes. It follows the style of AWS ACK and
GCP Config Connector, and uses Crossplane-style conventions (`forProvider` / `atProvider`,
`managementPolicies`, `deletionPolicy`, `Ready` / `Synced` conditions):

- **One CRD per Cloudflare resource.** The kinds are generated from Cloudflare's OpenAPI spec,
  which is pinned in `spec/`. A generic reconciler drives the generated kinds. Kinds that need
  more than CRUD are written by hand.
- **A virtual kubelet that runs Pods on Cloudflare Containers** is *planned, not implemented*
  (see [docs/virtual-kubelet-design.md](docs/virtual-kubelet-design.md)).

It ships as one Go manager binary and one Helm chart (`charts/flare-operator`).

## Status

**Alpha.** Every API is `v1alpha1` and may change without notice. There are no releases and no
published images, so you build the images yourself. The Go module path `flare.dev/operator` is a
placeholder until the repository has a permanent home.

| Kind | API group | How it is built | Default `deletionPolicy` |
|---|---|---|---|
| `CloudflareAccount` | `cloudflare.flare.dev/v1alpha1` | hand-written | (not applicable) |
| `KVNamespace` | `kv.cloudflare.flare.dev/v1alpha1` | generated (`generator.yaml`) | `Orphan` |
| `Queue` | `queues.cloudflare.flare.dev/v1alpha1` | generated | `Orphan` |
| `D1Database` | `d1.cloudflare.flare.dev/v1alpha1` | generated | `Orphan` |
| `Tunnel` | `tunnels.cloudflare.flare.dev/v1alpha1` | hand-written; also runs `cloudflared` and an egress NetworkPolicy | `Delete` |
| `VPCService` | `workersvpc.cloudflare.flare.dev/v1alpha1` | hand-written (Workers VPC) | `Delete` |
| `WorkerScript` | `workers.cloudflare.flare.dev/v1alpha1` | hand-written (Workers scripts: modules, bindings, workers.dev) | `Delete` |

Every kind is namespaced and belongs to the `cloudflare` category, so `kubectl get cloudflare`
lists all of them. The generated kinds are tested against the `flarefake` emulator and against
recordings of the real API. The chart has not yet been exercised end to end on a real cluster.
Open issues and unverified assumptions are tracked in [docs/STATUS.md](docs/STATUS.md).

## Quickstart

You need a Kubernetes cluster (1.30 or newer), `helm`, `docker` (or another builder, set with
`CONTAINER_TOOL`), Go 1.26, and a Cloudflare account with an API token.

### 1. Build the images and install the chart

```sh
make docker-build docker-build-fake      # flare-operator:dev and flarefake:dev for linux/$(go env GOARCH)
```

Your cluster's nodes must be able to get these images. You can push them to a registry and
set `image.repository` and `image.tag`, or import them into each node's container runtime. On
k0s, for example, run `docker save flare-operator:dev | sudo k0s ctr -n k8s.io images import -`
on each node. The chart's default `pullPolicy` is `IfNotPresent`, so imported images work.

```sh
helm install flare-operator charts/flare-operator -n flare-system --create-namespace \
  --set image.tag=dev --set clusterName=my-cluster
```

Give every cluster that manages the same Cloudflare account its own `clusterName`. The name
becomes part of the ownership tags (see [Ownership tags](#ownership-tags)). For the other
values, see [charts/flare-operator/README.md](charts/flare-operator/README.md) and
`charts/flare-operator/values.yaml`.

Helm installs the CRDs from `crds/` on the first install only. When you upgrade, apply them
yourself: `kubectl apply --server-side -f charts/flare-operator/crds/`.

### 2. Add a token and a CloudflareAccount

```sh
kubectl create namespace demo
kubectl -n demo create secret generic cloudflare-token --from-literal=token="$CLOUDFLARE_API_TOKEN"
kubectl -n demo apply -f - <<'EOF'
apiVersion: cloudflare.flare.dev/v1alpha1
kind: CloudflareAccount
metadata:
  name: main
spec:
  accountID: <your 32-character account ID>
  tokenSecretRef:
    name: cloudflare-token     # key defaults to "token"
EOF
kubectl -n demo get cfaccount main      # READY True, TOKEN active
```

The account controller verifies the token. It tries `GET /accounts/{id}/tokens/verify` first for
account-owned tokens, then falls back to `GET /user/tokens/verify` plus `GET /accounts/{id}` for
user tokens. It reports the result in `status` (`tokenStatus`, `tokenType`, `tokenExpiresOn`)
and in the `Ready` condition. Failure reasons include `SecretNotFound`, `TokenInvalid`,
`TokenExpired`, `AccountMismatch` and `BaseURLNotAllowed`.

### 3. Create your first KVNamespace

```sh
kubectl -n demo apply -f - <<'EOF'
apiVersion: kv.cloudflare.flare.dev/v1alpha1
kind: KVNamespace
metadata:
  name: sessions
spec:
  accountRef:
    name: main
  forProvider:
    title: sessions
EOF
kubectl -n demo wait kvnamespace/sessions --for=condition=Ready --timeout=2m
kubectl -n demo get kvnamespace sessions -o yaml    # status.id, status.atProvider
```

`KVNamespace` defaults to `deletionPolicy: Orphan`, so `kubectl delete` keeps the namespace in
Cloudflare. To delete it in Cloudflare too, switch the policy to `Delete` first:

```sh
kubectl -n demo patch kvnamespace sessions --type merge -p '{"spec":{"deletionPolicy":"Delete"}}'
kubectl -n demo delete kvnamespace sessions
```

[examples/](examples/) has an annotated manifest for every kind. `go test ./examples/` (part of
`make test`; it needs the envtest binaries from `make envtest`) validates each manifest against
the CRDs on an envtest API server, checking the schema, the CEL rules and strict field
validation.

### Trying it without a Cloudflare account

The chart can run the `flarefake` emulator next to the manager:

```sh
helm install flare-operator charts/flare-operator -n flare-system --create-namespace \
  --set image.tag=dev -f charts/flare-operator/ci/flarefake-values.yaml --set flarefake.image.tag=dev
```

Then point a CloudflareAccount at the emulator with
`spec.baseURL: http://flare-operator-flarefake.flare-system.svc:8787/client/v4`. The install
notes print the exact URL. In its default open mode, flarefake accepts any token and any 32-hex
account ID. Never enable flarefake in a cluster that manages a real Cloudflare account.

### Token permissions

Give the token only what the kinds you use need. For an `Observe`-only object, the Read variant
is enough. The permission-group names below come from the `x-api-token-group` extension of the
pinned OpenAPI spec. Rows marked UNVERIFIED are ones where the spec lists no group.

| Kind | Permission group (spec `x-api-token-group`) |
|---|---|
| `CloudflareAccount` | None needed for `tokens/verify` (UNVERIFIED: the spec lists no group). A **user** token also calls `GET /accounts/{id}`, which accepts any account-level group, for example `Account Settings Read`. |
| `KVNamespace` | `Workers KV Storage Write` (`… Read` for Observe) |
| `Queue` | `Queues Write` (`Queues Read` for Observe); the spec also accepts `Workers Scripts Write` |
| `D1Database` | `D1 Write` (`D1 Read` for Observe) |
| `Tunnel` | `Cloudflare Tunnel Write` (`Cloudflare Tunnel Read` for Observe); the spec also accepts `Cloudflare One Connector: cloudflared Write`. Fetching the connector token needs Write. |
| `VPCService` | UNVERIFIED: the spec lists no group for `/connectivity/directory/services`. [docs/cloudflare-service-catalog.md](docs/cloudflare-service-catalog.md) calls it the "Connectivity Directory" permission. |
| Ownership tags (on by default) | UNVERIFIED: the spec lists no group for `/accounts/{id}/tags`. Grant Resource Tagging access, or install with `ownershipTags=false`. |

## Concepts

Every managed kind has this shape (the kind-specific fields live under `forProvider` and
`atProvider`):

```yaml
spec:
  accountRef: {name: main}          # a CloudflareAccount in the same namespace
  deletionPolicy: Orphan            # or Delete; default is per kind
  managementPolicies: ["*"]         # default; ["Observe"] = read-only
  forProvider: {...}                # Cloudflare API fields, JSON names exactly as in the API
status:
  id: <Cloudflare ID>
  atProvider: {...}                 # the resource as last read from the API
  conditions: [Ready, Synced]
  observedGeneration: 3
```

### accountRef and CloudflareAccount

`spec.accountRef.name` names a `CloudflareAccount` in the **same namespace**. Namespaces are the
tenancy boundary: an object cannot use another namespace's credentials. Each object is labelled
`cloudflare.flare.dev/account=<name>`. Two finalizers protect the credentials while they are
still needed:

- `cloudflare.flare.dev/account-in-use` keeps the account until no object in the namespace uses
  it.
- `cloudflare.flare.dev/account-token` keeps the token Secret until the account is released.

Because of these, deleting a namespace still cleans up in Cloudflare before the token disappears.

Every CloudflareAccount that uses the same token shares one client-side rate limiter:
`spec.rateLimit`, default 1080 requests per 5 minutes with a burst of 20. Cloudflare's limit is
1200 requests per 5 minutes per token.

`spec.baseURL` redirects API calls, for example to flarefake. The operator ignores it unless it
runs with `--allow-base-url-override` or lists the exact URL in `--allowed-base-url` (chart
values `baseURLOverride.allowAny` and `baseURLOverride.allowed`), because an override sends the
token to that URL.

### deletionPolicy

`deletionPolicy` decides what happens to the Cloudflare resource when you delete the object:

- `Delete` deletes the resource in Cloudflare.
- `Orphan` keeps the resource and releases the ownership tag, so another object can adopt it.

The per-kind defaults are in the table above. Kinds that hold data (KV, Queues, D1) default to
`Orphan`. New generated kinds default to `Delete` unless `generator.yaml` says otherwise.

Even with `Delete`, the operator deletes only resources it can **prove** it owns:

- the object created the resource, or confirmed its owner tag (recorded in the
  `cloudflare.flare.dev/ownership-proof` annotation, value `<uid>/<id>`);
- a readable owner tag names the object; or
- ownership tagging is off and the object pins the ID with the external-id annotation.

If none of these holds, the finalizer is removed, the resource is kept, and a Warning event
(`ExternalResourceKept`) says why.

Some kinds delete in several steps:

- **Tunnel:** the controller first scales `cloudflared` to zero, because Cloudflare refuses to
  delete a connected tunnel. It also waits for the VPCServices that reference the tunnel.

### managementPolicies

`managementPolicies` lists what the operator may do. The actions are `Observe`, `Create`,
`Update`, `Delete`, `LateInitialize` and `*`. The default, an empty list or `["*"]`, allows
everything.

- `["Observe"]` makes the object **read-only**. The operator never writes to Cloudflare: no
  create, update, delete or tagging. It only fills `status.atProvider`, and `Synced` reports
  reason `ObserveOnly`. Fields that are required for creation, such as `KVNamespace`
  `forProvider.title`, may be omitted. If the resource does not exist, `Ready=False` with reason
  `ExternalNotFound`.
- A subset such as `["Observe", "Create", "Update"]` suppresses the missing actions. Without
  `Delete`, the resource is never deleted, whatever the `deletionPolicy`.
- `LateInitialize` is accepted, but no kind fills unset spec fields from the observed state yet.

### Adoption: `cloudflare.flare.dev/external-id`

The annotation `cloudflare.flare.dev/external-id: <Cloudflare ID>` pins an object to an existing
resource and adopts it. The operator also writes the annotation itself right after a create, so
a crash cannot orphan a new resource.

Without the annotation, what happens depends on the kind:

- **Generated kinds and `Tunnel`** adopt a resource whose name matches `forProvider` (`title`,
  `queue_name` or `name`). If several resources match, the object reports an error.
- **`VPCService`** never adopts by name. VPC services carry no ownership tag, so a name match
  gives `Synced=False` with reason `NameConflict` until you set the annotation.

A resource whose owner tag names a different object is never touched.

### Ownership tags

With `--ownership-tags` (the default; chart value `ownershipTags`), the operator tags every
resource it manages through Cloudflare's Resource Tagging API with
`flare.dev/owner=<clusterName>/<namespace>/<name>`. It reads the existing tags, merges its own,
and writes them back with `If-Match`, so other tags are kept. The tag is how two objects, or two
clusters, avoid managing and deleting the same resource. The tag's `resource_type` values are
`kv_namespace`, `queue`, `d1_database` and `cloudflared_tunnel`. `VPCService` is not tagged.

### Conditions

Each object reports two conditions. Both are stamped with `metadata.generation`, and
`status.observedGeneration` tells you which spec they describe.

| Condition | Meaning | Common reasons |
|---|---|---|
| `Ready` | The Cloudflare resource exists and is usable | `Available`, `Creating`, `Deleting`, `Unavailable`, `ExternalNotFound`, `AccountNotReady` |
| `Synced` | The last reconcile applied the spec | `ReconcileSuccess`, `ObserveOnly`, `ReconcileError`, `Immutable` (a create-only field changed; nothing is written), `AccountNotReady`, `NameConflict` (VPCService) |

Generated kinds are re-read every 5 minutes to detect drift. Some fields are write-only:
Cloudflare never returns them, for example `Queue` `settings.delivery_paused` and `D1Database`
`primary_location_hint`. For those fields, the operator detects changes through
`status.writeOnlyHash`.

### Workers scripts: WorkerScript

A `WorkerScript` uploads a Cloudflare Workers script ([examples/workerscript.yaml](examples/workerscript.yaml))
and completes the private-backend path Worker → `vpc_service` binding → `VPCService` → `Tunnel` →
Kubernetes Service.

- **Source.** Either inline `forProvider.modules` (module name → `type` `esm`, `cjs`, `text`,
  `json` or `wasm-base64`, and `content`) or `forProvider.sourceRef`, a ConfigMap whose keys are
  the modules. `main_module` names the entry module. The script name is `forProvider.script_name`
  (immutable), else `metadata.name`.
- **Bindings.** `plain_text`, `secret_text` (`secretKeyRef`), `json`, `kv_namespace`, `queue`,
  `d1`, `vpc_service` and `service`. Each takes either the raw API value (`namespace_id`,
  `queue_name`, `database_id`, `service_id`, `service`) or a reference to an object in the same
  namespace (`kvNamespaceRef`, `queueRef`, `d1DatabaseRef`, `vpcServiceRef`, `serviceRef`).
  Until every referenced object is Ready, nothing is uploaded and `Synced` is `False` with
  reason `DependencyNotReady`.
- **Updates.** Cloudflare does not return script content, so the operator stores a hash of the
  modules in `status.contentHash`. A content change is one multipart upload, which creates a new
  version and deploys it at 100%. A change that only touches settings (bindings, compatibility
  date or flags, observability, logpush, or a Secret value) is one `PATCH …/settings`. If
  someone else deploys a different version, the next reconcile uploads the spec again. An
  unchanged object makes no writes.
- **workers.dev.** `forProvider.workersDev.enabled` turns the route on or off.
  `status.atProvider.url` shows the URL.
- **Ownership and deletion.** The operator manages an existing script only when this object
  created it, the external-id annotation pins it, or its `flare.dev/owner` tag names this object
  (Resource Tagging `resource_type` `worker`). Otherwise the object reports `NameConflict` and
  writes nothing, because an upload would replace someone else's code.
- **Delete order.** Cloudflare lets you delete a KV namespace, queue, D1 database or VPC service
  that a Worker still binds (recording 0091). The operator therefore makes a `KVNamespace`,
  `Queue`, `D1Database` or `VPCService` that would delete its Cloudflare resource wait, with
  `Ready=False` and reason `DependencyNotReady`, until no `WorkerScript` binds it. The same
  applies to a `WorkerScript` bound by another script's `serviceRef`.

## Local development

The Makefile exports `CGO_ENABLED=0`. The manager image builds without cgo too, and on some Macs
cgo linking fails: Xcode's `ld` cannot read the MacOSX 27 SDK `.tbd` files. If you run `go`
directly, set `CGO_ENABLED=0` yourself, for example
`CGO_ENABLED=0 go test -race ./internal/fake/`. The race detector works without cgo on darwin.

| Target | What it does |
|---|---|
| `make ci` | Runs everything CI runs: `fmt-check`, `vet`, `spec-check`, `test`, `test-race`, `verify-generated`, `chart-check`, `conformance`. `verify-generated` compares against `HEAD`, so commit generated files first. |
| `make test` | All tests, including envtest suites (a real kube-apiserver and etcd) and the ones that load the 26 MB spec |
| `make test-short` | Skips the spec-loading tests |
| `make test-race` | All tests with `-race` (cgo off on darwin, on elsewhere; override with `RACE_CGO_ENABLED`) |
| `make conformance` | Replays the real-API recordings against flarefake (`TestConformance`) |
| `make fake` | Runs flarefake on `127.0.0.1:8787` with request validation against the pinned spec |
| `make run` | Runs the manager against the current kubeconfig (install the CRDs first) |
| `make e2e` | End-to-end suite against the cluster in your kubeconfig, using the chart with flarefake. *In progress: the target is being added and may not exist on your branch yet.* |
| `make generate-crds` / `make generate-check` | Runs flaregen: writes generated kinds, or fails if they are stale |
| `make generate manifests` | controller-gen: deepcopy for `api/`, CRDs into `config/crd/bases`, RBAC into `config/rbac` |
| `make chart-sync` / `make chart-check` | Copies the CRDs and RBAC into the chart, or fails if the chart is stale |
| `make helm-lint` | `helm lint`, `helm template` and, if installed, `kubeconform` |
| `make docker-build` / `make docker-build-fake` | Builds the images (`IMG`, `FAKE_IMG`, `PLATFORM`, `CONTAINER_TOOL`) |
| `make spec-check` | Checks that `spec/openapi.json.gz` matches `spec/LOCK` |
| `make envtest` | Fetches the envtest binaries into `~/.cache/flare-operator/envtest`, shared by all checkouts |

After changing API types or RBAC markers, run `make manifests generate generate-crds chart-sync`
and commit the result.

To run the manager on your machine against flarefake, start the emulator with `make fake`, then
in a second terminal:

```sh
kubectl apply --server-side -f config/crd/bases
CGO_ENABLED=0 go run ./cmd/manager --allowed-base-url=http://127.0.0.1:8787/client/v4
# CloudflareAccount spec.baseURL: http://127.0.0.1:8787/client/v4
```

Manager flags: `--cluster-name`, `--ownership-tags`, `--controller` (repeatable; the names are
`cloudflareaccount`, `kvnamespace`, `queue`, `d1database`, `tunnel` and `vpcservice`),
`--allow-base-url-override`, `--allowed-base-url` (repeatable), `--leader-elect`,
`--leader-election-namespace`, `--metrics-bind-address`, `--health-probe-bind-address`,
`--user-agent`, and the zap logging flags (`--zap-log-level`, `--zap-encoder`, and so on).

## flarefake: the Cloudflare API emulator

`flarefake` (`internal/fake`, `cmd/flarefake`) is a stateful, in-memory emulator of the parts of
the Cloudflare API this project uses:

- KV, Queues and D1
- tunnels and Workers VPC services
- Workers scripts
- token verify
- Resource Tagging

It returns Cloudflare's envelope, error codes, pagination and rate-limit headers. With `-spec`,
it checks every request against the pinned OpenAPI spec. Schema violations are journaled, or
answered with 400 when you pass `-reject-schema-violations`. Tests use it in-process (see
`internal/testenv`), and it also runs standalone:

```sh
make fake
curl -s -H 'Authorization: Bearer x' -H 'Content-Type: application/json' \
  -X POST localhost:8787/client/v4/accounts/0123456789abcdef0123456789abcdef/queues -d '{"queue_name":"demo"}'
curl -s localhost:8787/_fake/journal     # every request, with any schema violation
```

**Fidelity rules.** The emulator is only useful if it behaves like the real API:

- Every emulated behavior cites the recording it came from (`// 0029`), or is marked
  `UNVERIFIED`.
- Recordings of the real API live in `test/recordings/`. They are sanitized with
  `hack/sanitize_recordings.py` and scanned for leaks before they are committed.
- `make conformance` replays the recordings and requires the same status, envelope, errors,
  result and headers. Don't loosen its normalization to make a test pass.
- Every new recording must be added to a conformance scenario. The test fails if a recording
  hits an emulated route that no scenario replays.

**Control API** (`/_fake/…`, not part of Cloudflare's API):

| Endpoint | Purpose |
|---|---|
| `POST /_fake/reset` | Drop all state |
| `GET` / `DELETE /_fake/journal` | Read or clear the request journal (time, method, path, status, schema violation, injected fault) |
| `POST /_fake/clock` | `{"set":"<RFC3339>"}`, `{"advance":"90s"}` or `{"real":true}` |
| `POST /_fake/ids` | `{"ids":["…"]}`: queue the IDs the next creates return |
| `POST` / `DELETE /_fake/faults` | `{"method":"POST","path_regex":"/queues$","status":500,"code":10001,"message":"…","times":1}` fails matching requests (`times` ≤ 0: until cleared) |
| `POST` / `DELETE /_fake/tokens` | Register tokens (`{"token":"…","kind":"account","account_id":"…"}`), which switches token checks to strict mode, or clear them |
| `POST /_fake/accounts/{account}/tunnels/{id}/connect` / `…/disconnect` | Simulate `cloudflared` connecting (`{"replicas":1,"connections":4}`) or disconnecting |

## The CRD generator (flaregen)

`cmd/flaregen` reads the pinned spec and `generator.yaml`. It groups operations by
`x-fern-sdk-group-name` and classifies them by `x-fern-sdk-method-name` into CRUD resources and
singletons. For every configured kind, it writes:

- `api/<product>/v1alpha1/`: the Go types and deepcopy
- `config/crd/bases/<group>_<plural>.yaml`: a structural CRD schema, with CEL rules
- `internal/generic/descriptors/zz_generated.go`: the descriptor the generic reconciler runs
- `internal/generic/kinds/zz_generated.rbac.go`: the kind's RBAC markers

The manager registers every generated kind automatically; `cmd/manager` needs no change. The
flattening rules (OpenAPI to structural schema) and the derivation of immutable, write-only, ID
and name fields are documented in `internal/flaregen/doc.go`.

To add a kind:

1. Find the resource: `go run ./cmd/flaregen -list | grep <group>`. It must say `[ok]`. Nested
   resources, upsert-style resources and non-JSON bodies are not supported yet.
2. Add an entry to `generator.yaml`:
   - `fernGroup` (plus `path` if the group holds several resources) and `kind`
   - `defaultDeletionPolicy: Orphan` if the resource holds data
   - `tagResourceType`, taken from the spec's tags-set enum
   - any overrides (`immutable`, `writeOnly`, `fields`, …), each explained under `why:` with a
     recording number or `UNVERIFIED`
3. Run `make generate-crds manifests chart-sync`.
4. Teach flarefake the resource, with behavior backed by new recordings in a conformance
   scenario. Add a sample to `TestDescriptorsAgainstFlarefake`
   (`internal/generic/descriptors/emulator_test.go`); the test fails without one.
5. Add a manifest to `examples/`. `TestEveryKindHasAnExample` fails without one.
6. Run `make ci`.

## Repository layout

```
api/                     API types: common/ (shared, frozen contract), cloudflare/ (CloudflareAccount),
                         tunnels/, workersvpc/ (hand-written), kv/, queues/, d1/ (generated by flaregen)
cmd/manager/             the operator binary
cmd/flaregen/            the CRD generator
cmd/flarefake/           the emulator as a standalone server (API at /client/v4, control at /_fake)
internal/cfclient/       Cloudflare API client: per-token rate limiter, retries, list cache
internal/reconcile/      shared reconcile helpers: policies, finalizers, conditions, accounts, tags, ownership
internal/generic/        the generic reconciler, generated descriptors and kind registration
internal/controller/     hand-written controllers: account, tunnel, vpcservice (+ tunnelnet)
internal/flaregen/       generator implementation
internal/fake/           flarefake
internal/testenv/        envtest + in-process flarefake test harness
config/crd/bases/        generated CRDs;  config/rbac/  generated RBAC
charts/flare-operator/   Helm chart (crds/ and ClusterRoles synced by make chart-sync)
examples/                one annotated manifest per kind, validated by go test ./examples/
spec/                    pinned Cloudflare OpenAPI spec (gzipped) + LOCK
test/recordings/         sanitized real-API recordings replayed by make conformance
hack/                    chartsync, classify_api.py (coverage map), sanitize_recordings.py
docs/                    design docs, status
```

## Documentation

| Doc | What |
|---|---|
| [docs/STATUS.md](docs/STATUS.md) | Build status, open issues, UNVERIFIED assumptions |
| [docs/cloudflare-service-catalog.md](docs/cloudflare-service-catalog.md) | Every Cloudflare product: its API, the Kubernetes analogy, the tier |
| [docs/cloudflare-api-coverage.md](docs/cloudflare-api-coverage.md) | All API operations, classified (generated by `make classify`) |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Emulator, recordings, API versioning |
| [docs/spike-results-2026-09-29.md](docs/spike-results-2026-09-29.md) | What we learned from the real API (tunnels, Workers VPC, `cloudflared` in Kubernetes) |
| [docs/virtual-kubelet-design.md](docs/virtual-kubelet-design.md) | Planned: Pods on Cloudflare Containers |
| [docs/plan-parallel.md](docs/plan-parallel.md) | How the work is split into workstreams |
| [charts/flare-operator/README.md](charts/flare-operator/README.md) | Chart install, values, e2e with flarefake |
