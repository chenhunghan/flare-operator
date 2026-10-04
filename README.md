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

## Status and production readiness

**Alpha.** Read this before you point the operator at an account you care about.

- **API.** Every kind is `v1alpha1` and may change without notice. There is no conversion
  webhook; [docs/api-versioning.md](docs/api-versioning.md) describes the path to `v1beta1`.
- **Releases.** There are no releases and no published images, so you build the images
  yourself. The Go module path is `github.com/chenhunghan/flare-operator`.
- **How it is verified.** Every controller is tested against `flarefake`, an in-memory
  emulator of the Cloudflare API, in envtest suites and in fault, crash and scale tests. The
  chart has an e2e suite (`test/e2e`) that runs on a real cluster with flarefake in the
  cluster; it last passed on k0s on 2026-09-29, before some of the current kinds existed
  ([docs/STATUS.md](docs/STATUS.md)). How far the emulator can be trusted rests on evidence,
  strongest first:
  1. replayed recordings of the real API;
  2. behavior that Cloudflare's own clients (wrangler, cloudflare-go, cloudflared, the
     Terraform provider) rely on or show in their fixtures;
  3. Cloudflare's docs.

  Real clients also run against flarefake in [differential tests](#differential-testing).
  Every flarefake response is validated against the pinned spec in tests.
- **What is UNVERIFIED.** Emulator behavior with none of that evidence is marked `UNVERIFIED`
  in the code and listed in [docs/emulator-fidelity.md](docs/emulator-fidelity.md). The largest
  gaps are:
  - `VectorizeIndex`, `SecretsStore`, `AIGateway` and `R2Bucket`: flarefake's generic profile
    emulates them from the spec. The only recordings are one list call each for Vectorize and
    Secrets Store (0154, 0155); AI Gateway and R2 have none (R2's "bucket not found" code 10006
    is taken from wrangler's source). R2 needs to be enabled on the account (possibly with a
    payment method); the operator only surfaces the API error when it is not;
  - Resource Tagging (the ownership tags): no recording at all;
  - error codes and messages for invalid input;
  - several Workers details (versions, settings, subdomains);
  - list ordering, and eventual consistency after a create;
  - the token permissions marked UNVERIFIED [below](#token-permissions).
- **Live testing is optional** and has not been done for the operator. The `test/live` smoke
  test (`make live`) exists but has not run against a real account. The only real-API evidence
  is the recordings made during the design spikes
  ([docs/spike-results-2026-09-29.md](docs/spike-results-2026-09-29.md)).

Open issues are tracked in [docs/STATUS.md](docs/STATUS.md).

## Kinds

Every kind is in one API group, `flare.dev`, at version `v1alpha1` (`apiVersion: flare.dev/v1alpha1`).

| Kind | Product category | Short names | Own printer columns (`-o wide` in italics) | Default `deletionPolicy` | How it is built |
|---|---|---|---|---|---|
| `CloudflareAccount` | (none) | `cfaccount`, `cfacct` | `TOKEN`, `REASON` | (none) | hand-written |
| `KVNamespace` | `kv` | `cfkv` | `TITLE` | `Orphan` | generated (`generator.yaml`) |
| `Queue` | `queues` | `cfqueue`, `cfq` | `QUEUE`, *`CONSUMERS`* | `Orphan` | generated |
| `D1Database` | `d1` | `cfd1` | `DATABASE`, *`VERSION`* | `Orphan` | generated |
| `VectorizeIndex` | `vectorize` | `cfvec` | `DIMENSIONS`, `METRIC` | `Delete` | generated; emulated by the generic profile (mostly UNVERIFIED) |
| `SecretsStore` | `secretsstore` | `cfstore` | `STORE` | `Delete` | generated; emulated by the generic profile (mostly UNVERIFIED) |
| `AIGateway` | `aigateway` | `cfaigw` | `COLLECT-LOGS`, *`CACHE-TTL`* | `Delete` | generated; emulated by the generic profile (mostly UNVERIFIED) |
| `R2Bucket` | `r2` | `cfr2`, `cfbucket` | `LOCATION`, `STORAGE-CLASS`, *`JURISDICTION`* | `Orphan` | generated, with `generator.yaml` extensions (jurisdiction header, CORS sub-resource); emulated by the generic profile (mostly UNVERIFIED) |
| `Tunnel` | `tunnels` | `cftunnel`, `cftun` | `STATUS`, `CONNECTORS` | `Delete` | hand-written; also runs `cloudflared` and an egress NetworkPolicy |
| `VPCService` | `workersvpc` | `cfvpcsvc`, `cfvpc` | `TYPE`, `TUNNEL` | `Delete` | hand-written (Workers VPC) |
| `WorkerScript` | `workers` | `cfworker`, `cfscript` | `URL`, *`VERSION`* | `Delete` | hand-written (Workers scripts: modules, static assets, bindings, workers.dev) |
| `PagesProject` | `pages` | `cfpages`, `cfpp` | `URL`, `BRANCH`, *`LIVE`* | `Delete` | hand-written (Cloudflare Pages projects: deployment configs, bindings) |
| `PagesDeployment` | `pages` | `cfpagesdeploy`, `cfpd` | `PROJECT`, `ENV`, `STAGE`, *`URL`* | `Delete` | hand-written (Pages Direct Upload of an artifact) |

Every kind prints `READY`, `SYNCED` and `EXTERNAL-ID` (`status.id`) first, then its own
columns, then `AGE`. Every kind is namespaced and belongs to the `cloudflare` category, so
`kubectl get cloudflare` lists all of them. `kubectl get managed` lists the managed kinds, that
is, all but `CloudflareAccount`. Each managed kind is also in the category of its product
(`kubectl get vectorize`). [docs/api-reference.md](docs/api-reference.md) lists every field,
validation rule, printer column JSONPath and condition reason.

`VectorizeIndex` and `SecretsStore` hold data but default to `Delete` (`generator.yaml` sets no
`defaultDeletionPolicy` for them). Set `deletionPolicy: Orphan` on them if deleting the object
must not delete the index or store.

`R2Bucket` notes:

- `forProvider.name` is the bucket name and its ID.
- `forProvider.jurisdiction` is sent as the `cf-r2-jurisdiction` header on every request. It
  selects where the bucket lives, so once the bucket exists it cannot be set, changed or removed
  (a CEL rule, and the controller's own check).
- `forProvider.storageClass` is changed with a bodiless `PATCH` carrying `cf-r2-storage-class`,
  and read back as `status.atProvider.storage_class`. `locationHint` is create-only.
- `forProvider.cors` manages the bucket's CORS policy (`PUT …/cors`, which replaces the whole
  policy: the policy read back must equal `cors` exactly, so a rule narrowed in `cors` is
  narrowed on Cloudflare too); leaving it out leaves the policy alone, and `cors: {}` (or `cors: {rules: []}`) removes it (`DELETE …/cors`).
- With `deletionPolicy: Delete`, Cloudflare refuses to delete a bucket that still holds objects.
  The object then reports `Synced=False` with reason `DeleteFailed` and keeps its finalizer.
- R2 must be enabled on the account first (in the dashboard, possibly with a payment method).
  Until then the create fails and `Synced=False` shows the API error. This has not been checked
  against the live API.

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
  --set image.tag=dev --set clusterName=my-cluster \
  --set reconcile.pollInterval=10m \
  --set reconcile.maxConcurrentReconciles=1 \
  --set reconcile.timeout=5m \
  --set reconcile.cloudflareRequestTimeout=60s
```

- Give every cluster that manages the same Cloudflare account its own `clusterName`. The name
  becomes part of the ownership tags (see [Ownership tags](#ownership-tags)).
- The `reconcile.*` values are optional. `reconcile.pollInterval=10m` polls every kind for
  drift every 10 minutes, which halves the steady-state API use of the generated kinds. Left
  empty (the chart default), each controller keeps its own interval: 5m for generated kinds,
  10m for the hand-written kinds (Tunnel, VPCService, WorkerScript, PagesProject,
  PagesDeployment). The other three values above are the defaults,
  spelled out. Size them with
  [the API budget](docs/operations.md#reconcile-tuning-and-the-api-budget).
- Every value is listed in [charts/flare-operator/README.md](charts/flare-operator/README.md#values).

**Upgrading.** Helm installs the CRDs from `crds/` on the first install only and never
upgrades them. Before every `helm upgrade`, apply the new CRDs from a checkout of the version
you are upgrading to:

```sh
make crds-diff        # optional: kubectl diff --server-side against the cluster
make crds-apply       # kubectl apply --server-side --force-conflicts --field-manager=flare-operator-crds -f charts/flare-operator/crds/
helm upgrade flare-operator charts/flare-operator -n flare-system --reset-then-reuse-values --set image.tag=<new tag>
```

Use `--reset-then-reuse-values` (Helm 3.14 or later) or `-f <your values file>`, not
`--reuse-values`: that flag drops the defaults of every value a newer chart adds.

[docs/operations.md](docs/operations.md#upgrade) explains why this is a manual step, covers
Flux and Argo CD, and describes the tested upgrade path (`make e2e-upgrade`).

### 2. Add a token and a CloudflareAccount

```sh
kubectl create namespace demo
kubectl -n demo create secret generic cloudflare-token --from-literal=token="$CLOUDFLARE_API_TOKEN"
kubectl -n demo apply -f - <<'EOF'
apiVersion: flare.dev/v1alpha1
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
user tokens. It reports the result in `status` and in the `Ready` condition:

- `id`: the verified account ID;
- `atProvider`: the token's `id`, `status` and `expires_on`;
- `tokenType`.

Failure reasons include `SecretNotFound`, `TokenInvalid`, `TokenExpired`, `AccountMismatch` and
`BaseURLNotAllowed` ([troubleshooting](docs/operations.md#troubleshooting-by-condition-reason)).

### 3. Create your first KVNamespace

```sh
kubectl -n demo apply -f - <<'EOF'
apiVersion: flare.dev/v1alpha1
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

### Examples

[examples/](examples/) has an annotated manifest for every kind. `go test ./examples/` (part
of `make test`; it needs the envtest binaries from `make envtest`) validates each manifest
against the CRDs on an envtest API server: schema, CEL rules and strict field validation.
`TestEveryKindHasAnExample` fails if a kind in `config/crd/bases` has no example.

| File | Shows |
|---|---|
| [cloudflareaccount.yaml](examples/cloudflareaccount.yaml) | the token Secret and the `CloudflareAccount` that every other example refers to as `main` |
| [kvnamespace.yaml](examples/kvnamespace.yaml) | a `KVNamespace`, plus an observe-only one pinned with the external-id annotation |
| [queue.yaml](examples/queue.yaml) | a `Queue` with settings |
| [d1database.yaml](examples/d1database.yaml) | a `D1Database` |
| [vectorizeindex.yaml](examples/vectorizeindex.yaml) | a `VectorizeIndex` (dimensions, metric), with `deletionPolicy: Orphan` |
| [secretsstore.yaml](examples/secretsstore.yaml) | a `SecretsStore`, with `deletionPolicy: Orphan` |
| [aigateway.yaml](examples/aigateway.yaml) | an `AIGateway` (caching, logs, rate limiting) |
| [r2bucket.yaml](examples/r2bucket.yaml) | an `R2Bucket` with a location hint, storage class and CORS policy |
| [tunnel.yaml](examples/tunnel.yaml) | a `Tunnel` with its managed `cloudflared` Deployment and egress NetworkPolicy |
| [vpcservice.yaml](examples/vpcservice.yaml) | two `VPCService`s behind the Tunnel: an HTTP Service by hostname, a TCP backend by IP |
| [workerscript.yaml](examples/workerscript.yaml) | a `WorkerScript` with inline modules and `*Ref` bindings, one with modules from a ConfigMap, and an observe-only one |
| [workerscript-fullstack.yaml](examples/workerscript-fullstack.yaml) | a static site with an API Worker (static assets, an `assets`, an `r2_bucket` and a `send_email` binding), an `R2Bucket` bound through `r2BucketRef`, an assets-only site from an archive, and modules from an OCI image |
| [pagesproject.yaml](examples/pagesproject.yaml) | a `PagesProject` with environment variables (one from a Secret) and `*Ref` bindings, and an observe-only one |
| [pagesdeployment.yaml](examples/pagesdeployment.yaml) | a production `PagesDeployment` from ConfigMaps, a preview one from an HTTPS archive, and one observing the live deployment |
| [fullstack/](examples/fullstack/README.md) | a complete notes app for `kubectl apply -k`: one Worker with static assets (SPA) and an API bound to D1, R2, KV and a secret, plus a Pages variant of it. Its README covers token permissions, deploying, the D1 schema, checking status and teardown. `TestFullStack` runs it against flarefake. |

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

## Token permissions

Give the token only what the kinds you use need. For an `Observe`-only object, the Read variant
is enough. The permission-group names come from the `x-api-token-group` extension of the pinned
OpenAPI spec. In the dashboard's token editor a group appears as its product under
**Account**, with the level **Read** or **Edit**; the spec's `… Write` is the dashboard's Edit.
That mapping and the dashboard names are Cloudflare's naming convention, not something this
repository checked in a live dashboard (UNVERIFIED), except for the Workers names cited in
[Workers roles](#workers-roles-legacy-and-granular). Rows marked UNVERIFIED are ones where the
spec lists no group.

| Kind | Spec group (Write; Read for Observe) | Dashboard: Account › product › level |
|---|---|---|
| `CloudflareAccount` | None listed for `tokens/verify` (UNVERIFIED: the spec gives no group for `/accounts/{id}/tokens/verify` or `/user/tokens/verify`). A **user** token also calls `GET /accounts/{id}`. The spec lists a fixed set of 29 groups for it, among them `Account Settings Read`, `Workers KV Storage Read`/`Write` and `Workers Scripts Read`/`Write`, but not the D1, Queues or Cloudflare Tunnel groups. | For a user token: Account Settings › Read (UNVERIFIED against the live API) |
| `KVNamespace` | `Workers KV Storage Write` (`… Read`) | Workers KV Storage › Edit (Read) |
| `Queue` | `Queues Write` (`Queues Read`); the spec also accepts the legacy `Workers Scripts Write` | Queues › Edit (Read) |
| `D1Database` | `D1 Write` (`D1 Read`) | D1 › Edit (Read) |
| `VectorizeIndex` | `Vectorize Write` (`Vectorize Read`) | Vectorize › Edit (Read) |
| `SecretsStore` | `Secrets Store Write` (`Secrets Store Read`) | Secrets Store › Edit (Read) |
| `AIGateway` | `AI Gateway Write` (`AI Gateway Read`) | AI Gateway › Edit (Read) |
| `R2Bucket` | `Workers R2 Storage Write` (`Workers R2 Storage Read`). The spec lists the group for the bucket list, create and delete only; for `GET`/`PATCH` of a bucket and its `cors` it lists none (UNVERIFIED: assumed to be the same group). | Workers R2 Storage › Edit (Read) |
| `Tunnel` | `Cloudflare Tunnel Write` (`Cloudflare Tunnel Read`). The spec also accepts `Cloudflare One Connector: cloudflared Write`/`Read` and `Cloudflare One Connectors Write`/`Read`. Fetching the connector token for `cloudflared` needs Write. | Cloudflare One Connector: cloudflared › Edit (Read); formerly "Cloudflare Tunnel" |
| `WorkerScript` | Legacy `Workers Scripts Write` (`Workers Scripts Read`). The spec also accepts `Workers Tail Read` for reading a script, its settings, deployments and subdomain, but not for `GET /accounts/{id}/workers/subdomain` (the workers.dev URL), so Observe needs `Workers Scripts Read`. Static assets: the upload session needs `Workers Scripts Write`; the file uploads authenticate with the session's JWT, not the token. Whether an `r2_bucket` or `send_email` binding also needs an R2 or Email Routing permission is UNVERIFIED. | Legacy: Workers Scripts › Edit (Read). Granular roles: **Admin at Workers product scope** to create or delete scripts; per-Worker Editor only for an adopted Worker; Content Read-Only for Observe. See [below](#workers-roles-legacy-and-granular). |
| `VPCService` | UNVERIFIED: the spec lists no group for `/connectivity/directory/services`. | Connectivity Directory (UNVERIFIED) |
| `PagesProject`, `PagesDeployment` | `Pages Write` (`Pages Read`). A deployment also needs Write for `GET …/upload-token`; the asset calls (`/pages/assets/*`) authenticate with that upload token, not with the API token. A `PagesProject` binding to a KV namespace, D1 database, queue or Worker needs no permission on those. | Cloudflare Pages › Edit (Read) |
| Ownership tags (on by default) | UNVERIFIED: the spec lists no group for `/accounts/{id}/tags`. Or install with `ownershipTags=false`. | Tag, formerly "Resource Tagging" (UNVERIFIED) |

### Workers roles: legacy and granular

On 2026-09-15 Cloudflare added granular Workers roles: **Metadata Read-Only**, **Content
Read-Only**, **Editor** and **Admin**. Each can be granted for the whole Workers product or for
one Worker
([Workers authorization](https://developers.cloudflare.com/workers/authorization/workers/),
[changelog](https://developers.cloudflare.com/changelog/post/2026-09-15-granular-worker-permissions/)).
The account-level legacy permissions still work, and Cloudflare has announced no deprecation date.
The pinned spec only knows the legacy names, so the table above and flarefake use those. The
legacy permissions map to the new roles at Workers product scope:

| Legacy permission (dashboard name; spec `x-api-token-group`) | Granular role at Workers product scope |
|---|---|
| Workers Scripts Edit (`Workers Scripts Write`) | Editor |
| Workers Scripts Read (`Workers Scripts Read`) | Content Read-Only |
| Workers Tail Read (`Workers Tail Read`) | Metadata Read-Only |

Editor can't create or delete Workers. If you use the granular roles and the operator creates
or deletes scripts, give the token **Admin at Workers product scope**. A per-Worker Editor grant
only works when the Worker already exists and the object adopts it.

### Renamed products in the dashboard

The dashboard now uses new names for some products this operator manages. The API paths haven't
changed.

| Former name | Current dashboard name | API path |
|---|---|---|
| Cloudflare Tunnel | Cloudflare One Connector: cloudflared | `/accounts/{id}/cfd_tunnel` |
| Virtual networks | Cloudflare One Networks | `/accounts/{id}/teamnet/virtual_networks` |
| VPC services | Connectivity Directory | `/accounts/{id}/connectivity/directory/services` |
| Resource Tagging | Tag | `/accounts/{id}/tags` |

The pinned spec carries both `Cloudflare Tunnel …` and `Cloudflare One Connector: cloudflared …`
groups for tunnel routes, and both `Cloudflare Tunnel …` and `Cloudflare One Networks …` for
virtual networks.

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
`flare.dev/account=<name>`. Two finalizers protect the credentials while they are
still needed:

- `flare.dev/account-in-use` keeps the account until no object in the namespace uses
  it.
- `flare.dev/account-token` keeps the token Secret until the account is released.

Because of these, deleting a namespace still cleans up in Cloudflare before the token disappears.

`spec.accountRef` cannot change once the resource exists (`status.id` is set), and a
CloudflareAccount's `spec.accountID` cannot change at all: the resource lives in that account,
and a switch would create a second, empty one in the new account and leave the first one
unmanaged. To move a resource between accounts, delete the object (with `deletionPolicy:
Orphan` to keep the old resource) and create a new one. The token Secret can be changed.

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

The per-kind defaults are in the [Kinds](#kinds) table. KV, Queues, D1 and R2 default to `Orphan`.
Generated kinds default to `Delete` unless `generator.yaml` sets `defaultDeletionPolicy`, which
it does not yet for `VectorizeIndex` and `SecretsStore`.

Even with `Delete`, the operator deletes only resources it can **prove** it owns:

- the object created the resource, or confirmed its owner tag (recorded in the
  `flare.dev/ownership-proof` annotation, value `<uid>/<id>`);
- a readable owner tag names the object; or
- the resource cannot be tagged (or ownership tagging is off) and the object pins the ID with
  the external-id annotation.

The last case matters for the kinds without an owner tag (`VectorizeIndex`, `SecretsStore`,
`AIGateway`, `R2Bucket` and `VPCService`). None of them adopts an existing resource by name: only a pin you
set yourself, or the operator's record of its own create, lets `Delete` delete the resource (see
[Adoption](#adoption-cloudflareflaredevexternal-id)).

Tagged kinds (`KVNamespace`, `Queue`, `D1Database`) with tagging on still adopt an untagged
resource of the same name and tag it, so with `Delete` deleting the object deletes that
resource too. They default to `Orphan`.

If none of these holds, the finalizer is removed, the resource is kept, and a Warning event
(`ExternalResourceKept`) says why.

Some kinds delete in several steps:

- **Tunnel:** the controller first scales `cloudflared` to zero, because Cloudflare refuses to
  delete a connected tunnel. It also waits for the VPCServices that reference the tunnel.
- **Resources bound by a Worker:** see [Delete order](#workers-scripts-workerscript).

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

### Adoption: `flare.dev/external-id`

The annotation `flare.dev/external-id: <Cloudflare ID>` pins an object to an existing
resource and adopts it. The operator also writes the annotation itself right after a create, and
a `flare.dev/create-pending` record right before it, so a crash between the create
and the status write cannot orphan or duplicate a resource ([Crash consistency](docs/operations.md#crash-consistency)).

Without the annotation, what happens depends on the kind:

- **Generated kinds with an owner tag** (`KVNamespace`, `Queue`, `D1Database`, with tagging on)
  adopt a resource whose name matches `forProvider` (`title`, `queue_name` or `name`) unless
  its owner tag names another object. If several resources match, the object reports an error.
- **Generated kinds without an owner tag** (`VectorizeIndex`, `SecretsStore`, `AIGateway`,
  `R2Bucket`, and every generated kind with `--ownership-tags=false`) never adopt by name (or,
  for `AIGateway`, by `id`): a match gives `Synced=False` with reason `NameConflict` until you set
  the annotation, unless the create-pending record shows it is the object's own lost create.
  Nothing else could prove that the object owns the resource, and `Delete` would delete it.
- **`Tunnel`** adopts a same-named tunnel only when its owner tag already names this object
  (with tagging on), or when the create-pending record shows it is the object's own lost create.
  A tunnel without that tag, such as one made with `cloudflared` or the dashboard, gives
  `NameConflict`: running connectors on it would take a share of its traffic.
- **`VPCService`** never adopts by name. VPC services carry no ownership tag, so a name match
  gives `Synced=False` with reason `NameConflict` until you set the annotation (unless the
  create-pending record shows it is the object's own lost create).
- **`WorkerScript`** adopts an existing script only when its owner tag names this object or the
  annotation pins it; otherwise `NameConflict`.
- **`PagesProject`** adopts an existing project only when its owner tag names this object, the
  annotation pins it, or the create-pending record shows it is the object's own lost create;
  otherwise `NameConflict`.
- **`PagesDeployment`** never adopts: a deployment's ID is Cloudflare's. It manages only the
  deployments it made (found again after a restart by the commit hash it derives from the
  object, when `forProvider.commit_hash` is unset) and, when observing, the one the annotation
  pins. A user-set `commit_hash` is a git commit that other deployments may carry too, so a
  deployment lost to a restart with it is not looked up: a Warning event
  `ExternalResourceKept` says it may be left in Cloudflare.

A resource whose owner tag names a different object is never touched.

### Ownership tags

With `--ownership-tags` (the default; chart value `ownershipTags`), the operator tags every
resource it manages through Cloudflare's Resource Tagging API with
`flare.dev/owner=<clusterName>/<namespace>/<name>`. It reads the existing tags, merges its own,
and writes them back with `If-Match`, so other tags are kept. The tag is how two objects, or two
clusters, avoid managing and deleting the same resource. The tag's `resource_type` values are
`kv_namespace`, `queue`, `d1_database`, `cloudflared_tunnel`, `worker` and `pages_project`
(its `resource_id` being the project's UUID is UNVERIFIED). Six kinds are not tagged:

- `SecretsStore`, `VPCService` and `PagesDeployment`: the spec's tags `resource_type` enum has no
  value for them.
- `VectorizeIndex`, `AIGateway` and `R2Bucket`: the enum has `vectorize_index`, `ai_gateway`
  and `r2_bucket`, but `generator.yaml` sets no `tagResourceType` for them yet (live support is
  UNVERIFIED; an R2 bucket name is unique only per jurisdiction).

### Conditions

Each object reports two conditions. Both are stamped with `metadata.generation`, and
`status.observedGeneration` tells you which spec they describe.

| Condition | Meaning | Common reasons |
|---|---|---|
| `Ready` | The Cloudflare resource exists and is usable | `Available`, `Creating`, `Deleting`, `Unavailable`, `ExternalNotFound`, `AccountNotReady`, `DependencyNotReady` |
| `Synced` | The last reconcile applied the spec | `ReconcileSuccess`, `ObserveOnly`, `ReconcileError`, `RateLimited`, `Immutable` (a create-only field changed; nothing is written), `AccountNotReady`, `DependencyNotReady`, `NameConflict`, `DeleteFailed` (Cloudflare refused the delete of a deleted object's resource) |

Every reason, per kind, is in [docs/api-reference.md](docs/api-reference.md#conditions), and
what to do about each one in [docs/operations.md](docs/operations.md#troubleshooting-by-condition-reason).

In-sync objects are re-read every 5 minutes (generated kinds) or 10 minutes (Tunnel, VPCService,
WorkerScript, PagesProject, PagesDeployment) to detect drift; `--poll-interval` overrides both. Some fields are
write-only: Cloudflare never returns them, for example `Queue` `settings.delivery_paused` and
`D1Database` `primary_location_hint`. For those fields, the operator detects changes through
`status.writeOnlyHash`.

### Workers scripts: WorkerScript

A `WorkerScript` uploads a Cloudflare Workers script ([examples/workerscript.yaml](examples/workerscript.yaml))
and completes the private-backend path Worker → `vpc_service` binding → `VPCService` → `Tunnel` →
Kubernetes Service.

- **Source.** One of inline `forProvider.modules` (module name → `type` `esm`, `cjs`, `text`,
  `json` or `wasm-base64`, and `content`), `forProvider.sourceRef`, a ConfigMap whose keys are
  the modules, or `forProvider.moduleSource`, an [artifact](docs/artifacts.md) (labelled
  ConfigMaps, an OCI image or an HTTPS archive) whose files are the modules, typed by extension
  or `moduleTypes`. `main_module` names the entry module. The script name is
  `forProvider.script_name` (immutable), else `metadata.name`.
- **Static assets.** `forProvider.assets.source` is an artifact of files that Cloudflare serves
  in front of the code, configured by `assets.config` (`html_handling`, `not_found_handling`,
  `run_worker_first` or `run_worker_first_paths`, `base_path`). As with wrangler, the root
  files `_headers` and `_redirects` become the header and redirect rules, and `.assetsignore`
  leaves files out. An `assets` binding lets the code fetch them. Without `main_module` and
  modules the Worker is assets-only. The operator uploads only new and changed files, and runs
  the upload only when the files or the config change (`status.assetsHash`): a code change with
  unchanged assets keeps them (`keep_assets`) without an asset call.
- **Bindings.** `plain_text`, `secret_text` (`secretKeyRef`), `json`, `kv_namespace`, `queue`,
  `d1`, `vpc_service`, `service`, `r2_bucket` (`bucket_name`, `jurisdiction`), `send_email`
  (`destination_address` or `allowed_destination_addresses`, `allowed_sender_addresses`; it
  needs Email Routing on a zone of the account, with the addresses verified there) and `assets`.
  Each takes either the raw API value (`namespace_id`,
  `queue_name`, `database_id`, `service_id`, `service`, `bucket_name`) or a reference to an object in the same
  namespace (`kvNamespaceRef`, `queueRef`, `d1DatabaseRef`, `vpcServiceRef`, `serviceRef`,
  `r2BucketRef`). An `r2BucketRef` binds the `R2Bucket`'s bucket name and its jurisdiction
  (none for `default`), so it takes no `jurisdiction` of its own.
  Until every referenced object is Ready, nothing is uploaded and `Synced` is `False` with
  reason `DependencyNotReady`. A `secret_text` Secret must carry the label
  `flare.dev/worker-binding=true` and must not be a service account token: the
  Worker's code can return the value, so only Secrets opted in for Workers are read (see
  [SECURITY.md](SECURITY.md#api-tokens)).
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
- **Delete order.** Cloudflare lets you delete a VPC service that a Worker still binds
  (recording 0091; for KV namespaces, queues, D1 databases and R2 buckets this is UNVERIFIED).
  The operator therefore makes a `KVNamespace`, `Queue`, `D1Database`, `VPCService` or
  `R2Bucket` that would delete its Cloudflare resource wait, with `Ready=False` and reason
  `DependencyNotReady`, until no `WorkerScript` binds it. The same applies to a `WorkerScript`
  bound by another script's `serviceRef`.

### Cloudflare Pages: PagesProject and PagesDeployment

> For a **new** site, Cloudflare recommends Workers with static assets rather than Pages
> ([migration guide](https://developers.cloudflare.com/workers/static-assets/migration-guides/migrate-from-pages/));
> wrangler 4.143 even redirects an AI agent's new static Pages project there. These kinds exist for sites
> that are on Pages.

A `PagesProject` ([examples/pagesproject.yaml](examples/pagesproject.yaml)) is a Pages project;
`PagesDeployment`s ([examples/pagesdeployment.yaml](examples/pagesdeployment.yaml)) deploy files
to it with Pages Direct Upload.

- **Project.** `forProvider.name` (immutable; default `metadata.name`) is the Cloudflare ID and
  the `<name>.pages.dev` subdomain. `production_branch` marks production deployments.
  `deployment_configs.production` and `.preview` hold compatibility settings, environment
  variables (`plain_text`, or `secret_text` from a Secret labelled
  `flare.dev/worker-binding=true`, tracked in `status.writeOnlyHash`) and bindings:
  `kv_namespaces`, `d1_databases`, `r2_buckets`, `queue_producers` and `services`, each by raw
  value or by a `kvNamespaceRef`, `d1DatabaseRef`, `r2BucketRef` (its bucket name and
  jurisdiction), `queueRef` or `serviceRef` (a `WorkerScript`); a referenced object waits for
  the binding to go away before its own Cloudflare delete. A set config is authoritative for its variables and bindings:
  ones that Cloudflare has and the config lacks are removed. A change is one `PATCH`; an
  unchanged object makes no writes. `source` (a GitHub or GitLab repository) is passed through
  as it is: authorizing the repository is done in the dashboard.
- **Deployment.** `forProvider.projectRef` names the `PagesProject`; `branch` (empty: the
  production branch) and `source`, an [artifact source](docs/artifacts.md) (labelled
  ConfigMaps, an OCI image or an HTTPS archive). The operator runs wrangler's Direct Upload
  flow: an upload token, `check-missing`, the missing files in buckets, `upsert-hashes`, then
  the deployment with its manifest. Files are hashed exactly as wrangler hashes them, so files
  Cloudflare already has are not uploaded again, and each file's stored content type (which
  Pages serves) is the one wrangler sends: the `mime` 3.0.0 type of its extension, else
  `application/octet-stream`. Root files `_headers`, `_redirects` and
  `_routes.json` go as the deployment's routing files and `_worker.js` as its advanced-mode
  Worker (not bundled). An artifact with a `_worker.js` directory is refused with
  `InvalidArtifact`, and so is one with a Pages Functions `functions` directory but no
  `_worker.js` file (compile the Functions into `_worker.js`; like wrangler, a `_worker.js`
  file wins and the directory is ignored). A new deployment is made only when the artifact's digest or the branch
  changes (`status.deployedHash`). The object is `Ready` once the deploy stage succeeds
  (`Deploying` until then; `DeploymentFailed`, not retried, when it fails).
  `status.atProvider` has the deployment's `url`, `aliases` (a preview's branch alias), stage
  and whether it is the live production deployment.
- **History.** Earlier deployments stay in the project's history (roll back in the dashboard);
  `status.id` is the newest one this object made.
- **Deletion.** Deleting a `PagesDeployment` deletes its deployment, a preview's alias with it
  (`force=true`). Cloudflare refuses to delete the project's live production deployment: that
  one is kept, with a Warning event `ExternalResourceKept`, and the object goes away. Deleting a
  `PagesProject` waits, with `DependencyNotReady`, until this namespace's `PagesDeployment`s of
  it are gone, then deletes the project and every deployment in it.
- **Observe.** With `managementPolicies: ["Observe"]`, a `PagesDeployment` reports the
  deployment the external-id annotation pins, else the live production deployment (no branch)
  or the newest one of its branch.

## Metrics

The manager serves Prometheus metrics on `--metrics-bind-address` (default `:8080`; chart
`metrics.*`): controller-runtime's standard set plus the metrics below. Labels never carry
Cloudflare IDs or names: `route_template` is the pinned spec's path template, or `other`.
`TestMetricsDocumented` (`cmd/manager`) fails when a metric defined in the code is missing here,
or a metric listed here is not defined.

<!-- metrics:begin -->
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `cloudflare_api_requests_total` | counter | `method`, `route_template`, `code` | Cloudflare API HTTP attempts, retries included; `code` is the HTTP status, or `error` for a transport failure. |
| `cloudflare_api_request_duration_seconds` | histogram | `method`, `route_template` | Latency of one attempt, until the body is read. |
| `cloudflare_api_rate_limit_wait_seconds` | histogram | `reason` (`limiter`, `retry_after`) | Time a call waited before being sent: the client-side token bucket, or a 429 back-off of the token. |
| `cloudflare_api_throttled_total` | counter | `source` (`api`, `client`) | HTTP 429 answers from the API, and calls refused locally while the token backs off. |
| `cloudflare_api_retries_total` | counter | `method`, `route_template`, `reason` (`429`, `5xx`, `transport`) | Retries. |
| `cloudflare_api_list_cache_hits_total` | counter | | Collection GETs answered from the list cache (`spec.rateLimit.listCacheTTL`). |
| `cloudflare_api_list_cache_misses_total` | counter | | Lists fetched from the API because the list cache had no fresh entry. |
| `flare_managed_sync_failures_total` | counter | `kind`, `reason` | `Synced=False` conditions set on managed objects, by kind and condition reason (`RateLimited`, `ReconcileError`, ...). |
<!-- metrics:end -->

Useful controller-runtime series: `controller_runtime_reconcile_total{controller,result}`,
`controller_runtime_reconcile_errors_total`, `controller_runtime_reconcile_timeouts_total`
(`--reconcile-timeout`), `controller_runtime_reconcile_time_seconds`, `workqueue_depth`,
`workqueue_retries_total` and `leader_election_master_status`. The controller label is the
controller name (see `--controller` below). [docs/operations.md](docs/operations.md#health-metrics-and-logs)
suggests alerts.

## Manager flags

`manager -h` prints them; the chart sets them from its values. `TestFlagsDocumented`
(`cmd/manager`) fails when the table below and the binary's flags or defaults disagree.

<!-- manager-flags:begin -->
| Flag | Default | Chart value | Meaning |
|---|---|---|---|
| `--metrics-bind-address` | `:8080` | `metrics.enabled`, `metrics.port` | Metrics endpoint address; `0` disables it. |
| `--health-probe-bind-address` | `:8081` | `probes.port` | `/healthz` and `/readyz` address. |
| `--leader-elect` | `false` | `leaderElection.enabled` (chart default `true`) | Leader election; required with more than one replica. |
| `--leader-election-namespace` | `""` | (the release namespace) | Namespace of the Lease `flare-operator.flare.dev` (empty: the in-cluster namespace). |
| `--cluster-name` | `default` | `clusterName` | Cluster identity in ownership tags. |
| `--ownership-tags` | `true` | `ownershipTags` | Tag managed resources with `flare.dev/owner`. |
| `--user-agent` | `flare-operator` | `userAgent` | User-Agent of Cloudflare API calls. |
| `--controller` | `[]` | `controllers` | Run only this controller (repeatable; default: all). Names: `cloudflareaccount`, `kvnamespace`, `queue`, `d1database`, `vectorizeindex`, `secretsstore`, `aigateway`, `tunnel`, `vpcservice`, `workerscript`. |
| `--allow-base-url-override` | `false` | `baseURLOverride.allowAny` | Honour any CloudflareAccount `spec.baseURL`. |
| `--allowed-base-url` | `[]` | `baseURLOverride.allowed` (and flarefake's URL when `flarefake.enabled`) | Honour exactly this `spec.baseURL` (repeatable; trailing slash ignored). |
| `--poll-interval` | `0s` | `reconcile.pollInterval` | Drift-poll interval of in-sync objects. `0`: 5m for generated kinds, 10m for Tunnel, VPCService, WorkerScript, PagesProject and PagesDeployment. Minimum `10s`. |
| `--max-concurrent-reconciles` | `1` | `reconcile.maxConcurrentReconciles` | Parallel reconciles per controller. |
| `--reconcile-timeout` | `5m` | `reconcile.timeout` | Context deadline of one reconcile (`0` disables it). |
| `--cloudflare-request-timeout` | `60s` | `reconcile.cloudflareRequestTimeout` | Timeout of one Cloudflare API HTTP request. |
| `--artifact-max-bytes` | `64Mi` | `artifacts.maxBytes` | Largest total size of one artifact's files (a quantity); held in memory. See [docs/artifacts.md](docs/artifacts.md). |
| `--artifact-max-files` | `20000` | `artifacts.maxFiles` | Most files in one artifact. |
| `--artifact-max-archive-bytes` | `64Mi` | `artifacts.maxArchiveBytes` | Largest download of one artifact (a url archive, or an image's compressed layers). |
| `--artifact-max-expanded-bytes` | `256Mi` | `artifacts.maxExpandedBytes` | Most bytes decompressed from one archive or image (zip/tar bomb limit). |
| `--artifact-max-compression-ratio` | `100` | `artifacts.maxCompressionRatio` | Largest decompressed/compressed ratio (zip/tar bomb limit; checked after the first 1Mi). |
| `--artifact-cache-bytes` | `128Mi` | `artifacts.cacheBytes` | Memory for cached OCI and url artifacts, by digest; `0` disables it. |
| `--artifact-allowed-cidr` | `[]` | `artifacts.allowedCIDRs` | A non-public range url and ociRef sources may connect to (repeatable). Loopback, private, link-local and metadata addresses are refused otherwise. |
| `--version` | `false` | | Print the version, commit and build date, and exit. |
| `--kubeconfig` | `""` | | Kubeconfig path, only needed out of cluster (controller-runtime). |
| `--zap-log-level` | | `logging.level` | `debug`, `info`, `error`, `panic`, or an integer verbosity (zap). |
| `--zap-encoder` | | `logging.encoder` | `json` or `console` (zap). |
| `--zap-devel` | `false` | | zap development defaults (console encoder, debug level). |
| `--zap-stacktrace-level` | | | Level at and above which stack traces are logged (zap). |
| `--zap-time-encoding` | | | `epoch`, `millis`, `nano`, `iso8601`, `rfc3339` or `rfc3339nano` (zap). |
<!-- manager-flags:end -->

## Local development

The Makefile exports `CGO_ENABLED=0`. The manager image builds without cgo too, and on some Macs
cgo linking fails: Xcode's `ld` cannot read the MacOSX 27 SDK `.tbd` files. If you run `go`
directly, set `CGO_ENABLED=0` yourself, for example
`CGO_ENABLED=0 go test -race ./internal/fake/`. The race detector works without cgo on darwin.

| Target | What it does |
|---|---|
| `make ci` | Runs everything CI runs: `fmt-check`, `vet`, `spec-check`, `lint-static`, `vulncheck` (needs network), `test`, `test-race`, `verify-generated`, `helm-lint`, `release-check`, `conformance`. `verify-generated` compares against `HEAD`, so commit generated files first. |
| `make test` | All tests, including envtest suites (a real kube-apiserver and etcd) and the ones that load the 26 MB spec |
| `make test-short` | Skips the spec-loading tests |
| `make test-race` | All tests with `-race` (cgo off on darwin, on elsewhere; override with `RACE_CGO_ENABLED`) |
| `make conformance` | Replays the real-API recordings against flarefake (`TestConformance`) |
| `make differential-tools` / `make differential` | Installs the pinned wrangler and cloudflared, then runs them and cloudflare-go against flarefake ([Differential testing](#differential-testing)) |
| `make fake` | Runs flarefake on `127.0.0.1:8787` with request validation against the pinned spec |
| `make run` | Runs the manager against the current kubeconfig (install the CRDs first) |
| `make build` | Version-stamped `manager` and `flarefake` binaries in `./bin` |
| `make e2e-images e2e-install e2e e2e-uninstall` | End-to-end suite (`test/e2e`) against the cluster in your kubeconfig, using the chart with flarefake; see the Makefile for `E2E_IMAGE_LOAD` |
| `make e2e-upgrade` | Installs a previous git ref's chart, upgrades to this checkout, checks that objects survive with no Cloudflare writes |
| `make live` | The `test/live` smoke test against the real API; skips unless `FLARE_LIVE=1` |
| `make generate-crds` / `make generate-check` | Runs flaregen: writes generated kinds, or fails if they are stale |
| `make generate manifests` | controller-gen: deepcopy for `api/`, CRDs into `config/crd/bases`, RBAC into `config/rbac` |
| `make api-docs` / `make api-docs-check` | Renders `docs/api-reference.md` from the CRDs and `hack/apidocs/reasons.yaml` |
| `make chart-sync` / `make chart-check` | Copies the CRDs and RBAC into the chart, or fails if the chart is stale |
| `make helm-lint` | `helm lint`, `helm template` and, if installed, `kubeconform` |
| `make crds-apply` / `make crds-diff` | Server-side apply (or diff) of the chart's CRDs, before `helm upgrade` |
| `make docker-build` / `make docker-build-fake` | Builds the images (`IMG`, `FAKE_IMG`, `PLATFORM`, `CONTAINER_TOOL`); `docker-buildx` for multi-arch |
| `make lint-static` / `make vulncheck` | staticcheck and govulncheck |
| `make spec-check` | Checks that `spec/openapi.json.gz` matches `spec/LOCK` |
| `make envtest` | Fetches the envtest binaries into `~/.cache/flare-operator/envtest`, shared by all checkouts |

After changing API types or RBAC markers, run `make manifests generate generate-crds api-docs chart-sync`
and commit the result.

To run the manager on your machine against flarefake, start the emulator with `make fake`, then
in a second terminal:

```sh
kubectl apply --server-side -f config/crd/bases
CGO_ENABLED=0 go run ./cmd/manager --allowed-base-url=http://127.0.0.1:8787/client/v4
# CloudflareAccount spec.baseURL: http://127.0.0.1:8787/client/v4
```

## flarefake: the Cloudflare API emulator

`flarefake` (`internal/fake`, `cmd/flarefake`) is a stateful, in-memory emulator of the parts of
the Cloudflare API this project uses:

- KV, Queues and D1
- tunnels, virtual networks and Workers VPC services
- Workers scripts
- token verify
- Resource Tagging
- a generic, spec-driven profile for simple CRUD resources (Vectorize, Secrets Store, AI
  Gateway)

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

**Fidelity.** The emulator is only useful if it behaves like the real API.
[docs/emulator-fidelity.md](docs/emulator-fidelity.md) has the evidence rules, how responses are
validated and what remains UNVERIFIED. In short:

- Every emulated behavior cites its evidence, strongest first:
  - a recording (`// 0029`);
  - `// SOURCED:` an official Cloudflare client or its fixtures;
  - `// DOCS:` developers.cloudflare.com;
  - otherwise it is marked `UNVERIFIED`.
- Recordings of the real API live in `test/recordings/`. They are sanitized with
  `hack/sanitize_recordings.py` and scanned for leaks before they are committed.
- `make conformance` replays the recordings and requires the same status, envelope, errors,
  result and headers. Don't loosen its normalization to make a test pass. Every new recording
  must be added to a conformance scenario; the test fails if a recording hits an emulated
  route that no scenario replays.
- In tests, every emulated response is validated against the pinned spec's response schema
  (strict mode). A violation fails the run unless an allowlist entry explains it; most entries
  cite a recording in which the real API violates the spec the same way.

**Control API** (`/_fake/…`, not part of Cloudflare's API):

| Endpoint | Purpose |
|---|---|
| `POST /_fake/reset` | Drop all state |
| `GET` / `DELETE /_fake/journal` | Read or clear the request journal (time, method, path, status, schema violation, injected fault) |
| `GET /_fake/response_violations` | Emulated responses that violate the spec's response schemas (with `-spec`) |
| `POST /_fake/clock` | `{"set":"<RFC3339>"}`, `{"advance":"90s"}` or `{"real":true}` |
| `POST /_fake/ids` | `{"ids":["…"]}`: queue the IDs the next creates return |
| `POST` / `DELETE /_fake/faults` | `{"method":"POST","path_regex":"/queues$","status":500,"code":10001,"message":"…","times":1}` fails matching requests (`times` ≤ 0: until cleared) |
| `POST` / `DELETE /_fake/tokens` | Register tokens (`{"token":"…","kind":"account","account_id":"…"}`), which switches token checks to strict mode, or clear them |
| `POST /_fake/accounts/{account}/tunnels/{id}/connect` / `…/disconnect` | Simulate `cloudflared` connecting (`{"replicas":1,"connections":4}`) or disconnecting |
| `GET /_fake/accounts/{account}/workers/{script}/assets` | The deployed version's static-assets manifest (path to hash) and assets config, which the API cannot read back |

### Differential testing

The differential tests run Cloudflare's own clients against an in-process flarefake: pinned
versions of wrangler, cloudflared (management commands only) and the cloudflare-go SDK. They
check that each client works end to end and that what it reads back decodes. They make no
Cloudflare API calls. A known mismatch is a skipped subtest `discrepancy/<ID>` whose message
cites the client's source line; when flarefake is fixed, the subtest fails with "no longer
reproduces" so the entry gets removed.

```sh
make differential-tools   # once: wrangler (npm ci) and cloudflared (sha256-checked) into ~/.cache/flare-operator/differential
make differential         # wrangler + cloudflared, then cloudflare-go
```

`make ci` does not run the clients (they need network installs); it vets `test/differential`
and runs the harness unit tests. Coverage, pinned versions and the list of known discrepancies
are in [docs/differential-testing.md](docs/differential-testing.md).

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
and name fields are documented in `internal/flaregen/doc.go`, and scaling out to more kinds in
[docs/generator-scaleout.md](docs/generator-scaleout.md).

To add a kind:

1. Find the resource: `go run ./cmd/flaregen -list | grep <group>`. It must say `[ok]`. Nested
   resources, upsert-style resources and non-JSON bodies are not supported yet.
2. Add an entry to `generator.yaml`:
   - `fernGroup` (plus `path` if the group holds several resources) and `kind`
   - `defaultDeletionPolicy: Orphan` if the resource holds data
   - `tagResourceType`, taken from the spec's tags-set enum
   - `shortNames` and `printColumns`
   - any overrides (`immutable`, `writeOnly`, `fields`, …), each explained under `why:` with a
     recording number or `UNVERIFIED`
3. Run `make generate-crds manifests api-docs chart-sync`.
4. Teach flarefake the resource (a hand-written profile, or `emulate: generic`), with behavior
   backed by evidence. Add a sample to `TestDescriptorsAgainstFlarefake`
   (`internal/generic/descriptors/emulator_test.go`); the test fails without one.
5. Add a manifest to `examples/` and a row to the [Examples](#examples) and [Token permissions](#token-permissions)
   tables. `TestEveryKindHasAnExample` fails without the manifest.
6. Run `make ci`.

## Repository layout

```
api/                     API types: common/ (shared, frozen contract), cloudflare/ (CloudflareAccount),
                         tunnels/, workersvpc/, workers/ (hand-written),
                         kv/, queues/, d1/, vectorize/, secretsstore/, aigateway/ (generated by flaregen)
cmd/manager/             the operator binary
cmd/flaregen/            the CRD generator
cmd/flarefake/           the emulator as a standalone server (API at /client/v4, control at /_fake)
internal/cfclient/       Cloudflare API client: per-token rate limiter, retries, list cache, metrics
internal/reconcile/      shared reconcile helpers: policies, finalizers, conditions, accounts, tags, ownership
internal/generic/        the generic reconciler, generated descriptors and kind registration
internal/controller/     hand-written controllers: account, tunnel, vpcservice (+ tunnelnet), workerscript
internal/resilience/     fault, crash-consistency, scale and metrics tests
internal/flaregen/       generator implementation
internal/fake/           flarefake
internal/testenv/        envtest + in-process flarefake test harness
config/crd/bases/        generated CRDs;  config/rbac/  generated RBAC
charts/flare-operator/   Helm chart (crds/ and ClusterRoles synced by make chart-sync)
examples/                one annotated manifest per kind, validated by go test ./examples/;
                         fullstack/ is a whole app for kubectl apply -k
spec/                    pinned Cloudflare OpenAPI spec (gzipped) + LOCK
test/recordings/         sanitized real-API recordings replayed by make conformance
test/chart/              chart rendering and values-table tests
test/differential/       real Cloudflare clients against flarefake
test/e2e/, test/live/    e2e on a cluster (tag e2e); live smoke test (tag live)
hack/                    chartsync, apidocs, classify_api.py (coverage map), sanitize_recordings.py
docs/                    design docs, runbook, status
```

## Documentation

| Doc | What |
|---|---|
| [docs/STATUS.md](docs/STATUS.md) | Build status, open issues, UNVERIFIED assumptions |
| [docs/api-reference.md](docs/api-reference.md) | Every kind's fields, validation rules, printer columns and condition reasons (generated) |
| [docs/api-versioning.md](docs/api-versioning.md) | The path from `v1alpha1` to `v1beta1` |
| [docs/operations.md](docs/operations.md) | Runbook: install, upgrade (CRDs), reconcile tuning and API budget, crash consistency, uninstall semantics, metrics, HA, network policy, rate limits, troubleshooting, backup |
| [docs/resilience.md](docs/resilience.md) | Crash consistency, fault behavior, measured API-call costs, metrics, fuzzing |
| [docs/emulator-fidelity.md](docs/emulator-fidelity.md) | flarefake's evidence tiers, response validation, what remains UNVERIFIED |
| [docs/differential-testing.md](docs/differential-testing.md) | wrangler, cloudflared and cloudflare-go against flarefake; known discrepancies |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Emulator, recordings, API versioning |
| [docs/generator-scaleout.md](docs/generator-scaleout.md) | Generating more kinds; the generic flarefake profile |
| [docs/cloudflare-service-catalog.md](docs/cloudflare-service-catalog.md) | Every Cloudflare product: its API, the Kubernetes analogy, the tier |
| [docs/cloudflare-api-coverage.md](docs/cloudflare-api-coverage.md) | All API operations, classified (generated by `make classify`) |
| [docs/spike-results-2026-09-29.md](docs/spike-results-2026-09-29.md) | What we learned from the real API (tunnels, Workers VPC, `cloudflared` in Kubernetes) |
| [docs/virtual-kubelet-design.md](docs/virtual-kubelet-design.md) | Planned: Pods on Cloudflare Containers |
| [docs/plan-parallel.md](docs/plan-parallel.md) | How the work is split into workstreams |
| [charts/flare-operator/README.md](charts/flare-operator/README.md) | Chart install, every value, e2e with flarefake |
| [SECURITY.md](SECURITY.md) | Token handling, in-cluster privileges, supply chain |
| [CHANGELOG.md](CHANGELOG.md) | Changes per release |
