# Operating flare-operator

This is the runbook for installing, upgrading, running and removing flare-operator. For
concepts (deletionPolicy, managementPolicies, adoption, ownership tags), see the
[README](../README.md#concepts). For tokens and privileges, see [SECURITY.md](../SECURITY.md)
and the README's [token permissions](../README.md#token-permissions). Every chart value is
listed in [charts/flare-operator/README.md](../charts/flare-operator/README.md#values) and
validated by `values.schema.json`; every manager flag in the README's
[flags table](../README.md#manager-flags).

- [Install](#install)
- [Upgrade](#upgrade)
- [Uninstall and what happens to Cloudflare resources](#uninstall-and-what-happens-to-cloudflare-resources)
- [Health, metrics and logs](#health-metrics-and-logs)
- [High availability and leader election](#high-availability-and-leader-election)
- [Network policy](#network-policy)
- [Reconcile tuning and the API budget](#reconcile-tuning-and-the-api-budget)
- [Rate limiting and HTTP 429](#rate-limiting-and-http-429)
- [Crash consistency](#crash-consistency)
- [Troubleshooting by condition reason](#troubleshooting-by-condition-reason)
- [Backup and restore of the custom resources](#backup-and-restore-of-the-custom-resources)
- [Several Cloudflare accounts, several clusters](#several-cloudflare-accounts-several-clusters)
- [Releases](#releases)

## Install

1. **Images.** Releases publish multi-arch images (linux/amd64 and linux/arm64) to
   `ghcr.io/chenhunghan/flare-operator` and `ghcr.io/chenhunghan/flarefake`; the chart uses them
   by default. To run your own build, use `make docker-build` (host platform) or
   `make docker-buildx` (linux/amd64 and linux/arm64, with SBOM and provenance attestations;
   `BUILDX_OUTPUT=--push IMG=<registry>/flare-operator:<tag>` pushes the multi-arch index), and
   set `image.repository` and `image.tag`. The nodes must be able to pull the image.
2. **Install the chart** into its own namespace, and give the cluster a unique `clusterName`:

   ```sh
   helm install flare-operator oci://ghcr.io/chenhunghan/charts/flare-operator --version <version> \
     -n flare-system --create-namespace \
     --set clusterName=<unique-cluster-name> \
     --set reconcile.pollInterval=10m      # optional; see "Reconcile tuning and the API budget"
   kubectl -n flare-system rollout status deploy/flare-operator
   kubectl -n flare-system logs deploy/flare-operator | head -3   # "flare-operator" version line
   ```

   The first install also creates the CRDs from `crds/`. `values.schema.json` makes
   `helm install` fail on an unknown key or a wrong type, so a typo such as `replica: 2` is an
   error rather than being silently ignored.
3. **Pod Security.** The manager pod meets the `restricted` Pod Security Standard. Label the
   namespace to enforce it:
   `kubectl label ns flare-system pod-security.kubernetes.io/enforce=restricted`.
4. **A token and a CloudflareAccount** per namespace that manages resources. See the README
   quickstart. The account must report `Ready=True` before its objects do anything.

`manager --version` prints the version, commit and build date. The same values appear in the
first log line (`"msg":"flare-operator","version":…`) and in the image labels
(`org.opencontainers.image.version`, `.revision`, `.created`).

## Upgrade

**Helm never upgrades or deletes the CRDs in `crds/`.** A `helm upgrade` alone therefore runs
the new manager against the old CRD schemas. Always apply the new CRDs first, from a checkout of
the version you are upgrading to:

```sh
make crds-diff                        # optional: kubectl diff --server-side against the cluster
make crds-apply                       # kubectl apply --server-side --force-conflicts -f charts/flare-operator/crds/
helm upgrade flare-operator charts/flare-operator -n flare-system --reset-then-reuse-values \
  --set image.tag=<new tag>
kubectl -n flare-system rollout status deploy/flare-operator
```

With the published chart, take the CRDs from the new chart version instead of a checkout:

```sh
helm pull oci://ghcr.io/chenhunghan/charts/flare-operator --version <new> --untar --untardir /tmp/flare-chart
kubectl apply --server-side --force-conflicts --field-manager=flare-operator-crds -f /tmp/flare-chart/flare-operator/crds/
helm upgrade flare-operator oci://ghcr.io/chenhunghan/charts/flare-operator --version <new> \
  -n flare-system --reset-then-reuse-values
```

Use `--reset-then-reuse-values` (Helm 3.14 or later), or pass your own values file with `-f`.
**Do not use `--reuse-values`**: it replaces the new chart's defaults with the old release's
values, so every key a newer chart adds is missing. Upgrading from chart 0.1.0 with
`--reuse-values`, for example, dropped the 0.2.0 defaults for `reconcile`,
`podDisruptionBudget`, `networkPolicy`, `topologySpreadConstraints` and
`metrics.serviceMonitor`. `--reset-then-reuse-values` starts from the new chart's defaults and
reapplies only the values you set.

`--server-side --force-conflicts` takes field ownership from the Helm install that first
created the CRDs, under the field manager `flare-operator-crds`. Apply the CRDs before the
manager rolls, so the new manager never sees an old schema. v1alpha1 CRDs have a single
version and no conversion webhook, so applying them is the whole CRD upgrade. A field removed
from a CRD disappears from stored objects the next time they are written.

**Why not a Helm hook Job?** We considered an opt-in `pre-upgrade` hook Job that applies the
CRDs, and chose the documented `make crds-apply` step instead:

- The hook Job would need write access to `customresourcedefinitions` cluster-wide, a
  permission the manager itself does not have and should not have.
- It would need a pinned `kubectl` image, one more image to mirror in air-gapped clusters.
- It would need the CRDs shipped in a ConfigMap. The CRDs are generated for every Cloudflare
  resource and will grow past the 1 MiB ConfigMap limit.
- A failed hook leaves the release in a failed state that is harder to recover than a
  failed `kubectl apply`.

GitOps tools need their own settings for CRD upgrades (per their docs; not tested here,
UNVERIFIED):

- **Flux**: helm-controller creates CRDs on install but skips them on upgrade by default
  (`spec.install.crds: Create`, `spec.upgrade.crds: Skip`). Set
  `spec.upgrade.crds: CreateReplace` on the HelmRelease, or apply `crds/` with a separate
  Kustomization.
- **Argo CD**: it renders the chart and applies `crds/` with the other manifests on sync, but
  client-side apply stores the whole object in the `last-applied-configuration` annotation,
  which fails once a CRD passes the 256 KiB annotation limit. Enable the `ServerSideApply=true`
  sync option for the Application.
- **Argo CD and deleting the Application.** Deleting an Application with cascade deletes every
  resource it tracks, CRDs included, and deleting a CRD deletes every object of that kind. If
  the manager is still running, it then finalizes those objects, and each `Delete`-policy
  resource is **deleted in Cloudflare** (see the table under
  [Uninstall](#uninstall-and-what-happens-to-cloudflare-resources)). The chart's CRDs therefore
  carry `argocd.argoproj.io/sync-options: Delete=false,Prune=false`, so Argo CD never deletes or
  prunes them. Keep that annotation if you apply the CRDs some other way. Better still, manage
  the CRDs in a separate Application (for example from `charts/flare-operator/crds/`), and set
  `skipCrds: true` in the operator Application's `helm` source. Either way, follow the uninstall
  steps below before you delete the Application.

**Tested path.** `make e2e-upgrade` installs the chart and manager of a previous git ref
(default: the latest tag; without one, the merge base with `main` when HEAD is on another
branch, else `HEAD~1`, so on `main` it tests an upgrade from the previous commit only), creates
an account, a KVNamespace, a Queue, a D1Database and a Tunnel, runs `make crds-apply` and the
documented `helm upgrade --reset-then-reuse-values` to the current
checkout, and checks:

- the objects stay Ready with the same Cloudflare IDs;
- the new manager reads every resource and makes **no** Cloudflare write, so there is no
  duplicate create and no drift from the version change, also after a second restart;
- then a smoke subset of `TestEndToEnd` runs.

Everything runs against flarefake in the cluster. See `hack/e2e-upgrade.sh`.

Set the previous ref with `make e2e-upgrade E2E_UPGRADE_FROM=<ref>`. Refs from before
publication used one API group per product and cannot be upgraded from (there is no
migration; see the CHANGELOG); the script refuses any ref whose chart CRDs are not in the
`flare.dev` group. The proposed default once the first release is tagged is that tag (the
"latest tag" rule above picks it up); until then, use the commit that introduced the
`flare.dev` group or a later one.

**Rollback.** `helm rollback flare-operator <revision>` rolls back the manager. It does not
roll back the CRDs. Stay on the newer CRDs unless [CHANGELOG.md](../CHANGELOG.md) says
otherwise: an older manager ignores fields it does not know, while an older CRD would prune
fields that newer objects already store.

## Uninstall and what happens to Cloudflare resources

What happens in Cloudflare is decided by each object's `deletionPolicy` when **the object**
is deleted, and only while the manager runs. Removing the operator never deletes anything in
Cloudflare by itself.

| Action | Effect in Cloudflare |
|---|---|
| `kubectl delete` an object, manager running | `Delete` policy: the resource is deleted (only if the operator can prove it owns it). `Orphan`: kept, and the owner tag released. |
| `helm uninstall` | Nothing. Manager, RBAC and Services go away; the CRDs, the objects and their finalizers stay. |
| Delete the CRDs **while the manager runs** | Every object is deleted, so every `Delete`-policy resource **is deleted in Cloudflare**. |
| Delete the CRDs or objects after `helm uninstall` | Nothing in Cloudflare. The objects hang in `Terminating` on their finalizers until you remove them. |
| Delete a Tunnel object, whatever its `deletionPolicy` | Its cloudflared Deployment, token Secret and NetworkPolicy are owned by the Tunnel, so the garbage collector deletes them too, and **the tunnel stops serving traffic**. `Orphan` keeps the tunnel in Cloudflare, not its connectors. Delete with `--cascade=orphan` to keep the connectors running. |

To remove the operator and **keep** everything in Cloudflare:

```sh
# 1. Make every managed object orphan its resource (or set managementPolicies: ["Observe"]).
kubectl get cloudflare -A -o json |
  jq -r '.items[] | select(.kind != "CloudflareAccount") | "\(.kind).\(.apiVersion | split("/")[0]) \(.metadata.namespace) \(.metadata.name)"' |
  while read -r kind ns name; do
    kubectl -n "$ns" patch "$kind" "$name" --type merge -p '{"spec":{"deletionPolicy":"Orphan"}}'
  done
# 2. Delete the objects while the manager still runs (the finalizers release the owner tags).
#    --cascade=orphan keeps the objects the operator created in the cluster: without it, the
#    garbage collector deletes every Tunnel's cloudflared Deployment, and the tunnels stop
#    serving traffic even though they stay in Cloudflare.
kubectl delete cloudflare -A --all --cascade=orphan
# 3. Remove the release, then the CRDs.
helm uninstall flare-operator -n flare-system
kubectl delete -f charts/flare-operator/crds/
```

The cloudflared Deployments, token Secrets and NetworkPolicies of the Tunnels are left running
and unmanaged. Delete them yourself (`kubectl delete deploy,secret,networkpolicy -n <ns>
-l flare.dev/tunnel=<tunnel name>`) once the tunnels are served some other way.

To remove the operator and **delete** what it created, delete the objects with
`deletionPolicy: Delete` while the manager runs, wait until they are gone, then uninstall.

If the manager is already gone and objects are stuck in `Terminating`, remove their finalizers
(`flare.dev/finalizer`, and `flare.dev/account-in-use` on accounts). The
Cloudflare resources stay as they are:

```sh
kubectl patch <kind> <name> -n <ns> --type merge -p '{"metadata":{"finalizers":null}}'
```

Token Secrets carry `flare.dev/account-token` while an account uses them. The
manager removes it when the account is deleted. Without the manager, patch it away in the same
way, or the namespace deletion waits for it.

`make e2e-uninstall` is the test-cluster version of this: `helm uninstall`, delete the CRDs,
delete the namespace.

## Health, metrics and logs

| Endpoint | Port (chart value) | Content |
|---|---|---|
| `/healthz` | 8081 (`probes.port`) | Liveness: the process serves HTTP (controller-runtime ping). |
| `/readyz` | 8081 | Readiness: ping. A standby replica is Ready too; only the leader reconciles. |
| `/metrics` | 8080 (`metrics.port`) | Prometheus text format, plain HTTP, no authentication. |

The operator's own metrics (Cloudflare API calls, latency, rate-limit waits, 429s, retries,
list cache, sync failures) are listed in the README's [metrics table](../README.md#metrics),
with their labels; [resilience.md §5](resilience.md#5-metrics) explains how they are counted.
Next to them are controller-runtime's standard series. What to watch:

| Question | Series |
|---|---|
| Is the token near Cloudflare's limit? | `sum(rate(cloudflare_api_requests_total[5m]))` against 4/s (1200 per 5 minutes per token); `rate(cloudflare_api_throttled_total{source="api"}[5m]) > 0` means Cloudflare is answering 429 |
| Are calls queueing behind the client-side limiter? | `histogram_quantile(0.9, rate(cloudflare_api_rate_limit_wait_seconds_bucket{reason="limiter"}[5m]))`; seconds here mean the poll load is too high for the budget (see [Reconcile tuning](#reconcile-tuning-and-the-api-budget)) |
| Is Cloudflare failing? | `cloudflare_api_requests_total{code=~"5..\|error"}`, `cloudflare_api_retries_total{reason=~"5xx\|transport"}`, `cloudflare_api_request_duration_seconds` |
| Which objects fail, and why? | `flare_managed_sync_failures_total{kind,reason}`; it also counts failures that controllers report through a condition and a timed requeue, which `controller_runtime_reconcile_errors_total{controller}` misses |
| Do reconciles hang? | `controller_runtime_reconcile_timeouts_total{controller}` (`reconcile.timeout`), `controller_runtime_reconcile_time_seconds`, `workqueue_longest_running_processor_seconds` |
| Is work piling up? | `workqueue_depth{name}`, `workqueue_retries_total{name}` |
| Which replica leads? | `leader_election_master_status{name="flare-operator.flare.dev"}`: 1 on the leader |

`rest_client_requests_total{code}` counts Kubernetes API calls, not Cloudflare calls. The
`controller` and `name` labels are the controller names (`kvnamespace`, `tunnel`, ...; the
README's [`--controller`](../README.md#manager-flags) row lists them).

`metrics.serviceMonitor.enabled=true` creates a prometheus-operator ServiceMonitor for the
metrics Service. It needs the `monitoring.coreos.com` CRDs. Add the label your Prometheus
selects with `metrics.serviceMonitor.labels`.

Logs are zap JSON on stderr (`logging.encoder=console` for development). `logging.level=debug`
adds the controllers' debug messages. Reconcile log lines carry `controller`, `namespace`,
`name` and `reconcileID`, so `kubectl logs deploy/flare-operator | jq 'select(.name=="sessions")'` follows
one object. With `replicas` above 1, `kubectl logs deploy/…` picks an arbitrary pod, often the
standby, which logs nothing but leader election. Read the leader's logs instead: its pod name
is the Lease holder (`kubectl -n flare-system get lease flare-operator.flare.dev -o
jsonpath='{.spec.holderIdentity}'`, up to the first `_`), or use
`kubectl -n flare-system logs -l app.kubernetes.io/component=manager --prefix`. Events are the other half: `kubectl get events -n <ns> --field-selector
involvedObject.name=<name>` shows `ExternalResourceKept` and `ForeignOwnerTunnelKept` warnings.

## High availability and leader election

- Leader election is on by default (`leaderElection.enabled`). The Lease is
  `flare-operator.flare.dev` in the release namespace:
  `kubectl -n flare-system get lease flare-operator.flare.dev -o jsonpath='{.spec.holderIdentity}'`.
- `replicas: 2` gives one standby. The chart then also renders a PodDisruptionBudget
  (`minAvailable: 1`, or `podDisruptionBudget.maxUnavailable`). Spread the replicas with
  `topologySpreadConstraints`; an entry without a `labelSelector` gets the manager's labels.
  With one replica no PDB is rendered, because it would block every node drain.
- On SIGTERM the manager stops its controllers, waits up to 5 s for running reconciles,
  releases the Lease and exits. `terminationGracePeriodSeconds` is 10. A standby therefore takes
  over within about a second instead of waiting for the lease to expire (15 s). A crashed
  leader is replaced after the lease expires.
- Only the leader calls Cloudflare, so extra replicas don't use more of the API rate budget.
- `priorityClassName` keeps the manager from being preempted before your workloads.

## Network policy

`networkPolicy.enabled=true` (off by default) renders a NetworkPolicy for the manager pods:

| Direction | Peer | Port |
|---|---|---|
| Egress | cluster DNS (`networkPolicy.dns`: kube-system / `k8s-app: kube-dns`) | 53 UDP+TCP |
| Egress | `networkPolicy.apiServer.cidrs` (**required**) | `apiServer.ports` (6443) |
| Egress | `networkPolicy.cloudflareAPI.cidrs` (default: Cloudflare's published IPv4+IPv6 ranges) | 443 |
| Egress | the flarefake pods, when `flarefake.enabled` | `flarefake.port` |
| Egress | `networkPolicy.extraEgress` (raw rules) | |
| Ingress | `networkPolicy.metricsFrom` peers (empty: any) | metrics |

Things to know:

- **The API server.** NetworkPolicy matches addresses after the Service DNAT, so list the
  API server's endpoint addresses, not the `kubernetes` Service ClusterIP:
  `kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[*].endpoints[*].addresses}'`.
  The chart refuses to render without them.
- **FQDN egress.** Plain NetworkPolicy cannot say "api.cloudflare.com". The default allows 443
  to all of Cloudflare's published ranges (<https://www.cloudflare.com/ips/>, copied
  2026-09-29), which api.cloudflare.com resolves into, but which also covers every site behind
  Cloudflare. For a tighter rule, use a CNI with FQDN policies (Cilium `toFQDNs`, Calico
  domain-based policy, …): set `networkPolicy.cloudflareAPI.cidrs: []` and add the FQDN rule
  through your CNI's policy kind. Check the published ranges again when you upgrade.
- **Probes.** The kubelet's health probes come from the node, which NetworkPolicy always
  allows, so there is no ingress rule for the health port.
- **CNI support.** The policy only has an effect with a CNI that enforces NetworkPolicy
  (k0s's default kube-router does).
- **Symptoms of a wrong policy.** An API server block shows as a manager that never becomes
  leader, or logs cache-sync timeouts. A Cloudflare block shows as CloudflareAccounts with
  `Synced=False`/`ReconcileError` and a dial or TLS timeout in the message.
- **cloudflared.** The Tunnel controller writes its own egress NetworkPolicy for each
  `cloudflared` Deployment (see the README). This chart value does not affect it.

## Reconcile tuning and the API budget

Four knobs set how hard the manager works and how much of the Cloudflare budget it spends.
They apply to every controller:

| Chart value | Flag | Default | Raise it when | Lower it when |
|---|---|---|---|---|
| `reconcile.pollInterval` | `--poll-interval` | empty / `0`: 5m for generated kinds, 10m for Tunnel, VPCService and WorkerScript (minimum `10s`) | polling crowds out real work (the budget below) | you need drift found faster, and the budget has room |
| `reconcile.maxConcurrentReconciles` | `--max-concurrent-reconciles` | `1` per controller | many objects wait in `workqueue_depth` while the limiter has room | rarely; each worker still waits for the token's shared limiter |
| `reconcile.timeout` | `--reconcile-timeout` | `5m` | reconciles of big WorkerScripts or large accounts hit `controller_runtime_reconcile_timeouts_total` | you want a hung call to free its worker sooner |
| `reconcile.cloudflareRequestTimeout` | `--cloudflare-request-timeout` | `60s` | large uploads time out on a slow link | you want a stalled request to fail sooner (it is retried if idempotent) |

Per account, `spec.rateLimit` on the CloudflareAccount sets the client-side limiter of its
token: `requestsPerFiveMinutes` (default 1080), `burst` (20), `maxRetries` (4) and
`listCacheTTL` (off).

**Budget math.** Cloudflare allows 1200 requests per 5 minutes per token. The client limits
itself to 90 % of that, 3.6 requests/s, which is **12 960 requests per hour** per token.
Measured costs (flarefake journal, `TestScale` in `internal/resilience`, ownership tags on;
details in [resilience.md §3](resilience.md#3-api-call-budget-and-polling-cost)):

| Operation | Cloudflare calls |
|---|---|
| Drift poll of an in-sync tagged generated object (KVNamespace, Queue, D1Database) | 2 (GET, owner-tag read) |
| Drift poll of an in-sync untagged generated object (VectorizeIndex, SecretsStore, AIGateway) | 1 (R2Bucket: 2, the bucket and its CORS policy) |
| Create of a tagged generated kind | up to 11, plus one list page per 20 (KV and generic kinds) or 100 (Queues, D1) existing resources of the kind |
| Create of an untagged generated kind | up to 7, plus the list pages |
| Token re-verification per CloudflareAccount, every 10 minutes | 1 (account-owned token) or 3 (user token: the account verify fails, then user verify and `GET /accounts/{id}`) |
| Drift poll of a Tunnel, VPCService or WorkerScript | not measured (UNVERIFIED): several GETs, plus the tag read for Tunnel and WorkerScript |

Steady-state cost per hour of N in-sync objects polled every P seconds is
`N × callsPerPoll × 3600 / P`. For tagged generated kinds (2 calls per poll):

| Objects per token | P = 5m (default) | P = 10m | P = 15m |
|---|---|---|---|
| 100 | 2 400/h (19 % of 12 960) | 1 200/h (9 %) | 800/h (6 %) |
| 300 | 7 200/h (56 %) | 3 600/h (28 %) | 2 400/h (19 %) |
| 500 | 12 000/h (93 %) | 6 000/h (46 %) | 4 000/h (31 %) |

Keep steady state well below 100 %: creates, updates and deletes need the rest, and a token
that other tools share (CI, Terraform, wrangler) shares Cloudflare's 1200, not the operator's
bucket. Above about 300 tagged objects per token at the 5-minute default, do one of these:

- raise `reconcile.pollInterval`;
- split the objects across CloudflareAccounts with different tokens (each token has its own
  budget);
- raise `spec.rateLimit.requestsPerFiveMinutes`, but only if nothing else uses the token;
- set `spec.rateLimit.listCacheTTL` to cut the list calls of creates in large accounts.

The 500-object scale run converged in 22 minutes, bound by the rate limit (about 4 760 calls at
3.6/s), and settled at about 8 000 calls per hour with a mix of tagged and untagged kinds.
`--max-concurrent-reconciles` does not raise throughput past the limiter: all workers of a
token wait for the same bucket. Polls of generated kinds carry up to 10 % jitter, so objects
created together do not poll in lockstep.

## Rate limiting and HTTP 429

How the client behaves at the limit (tested in `internal/cfclient` and `internal/resilience`,
see [resilience.md §2](resilience.md#2-faults)):

- **One client-side token bucket per API token**, shared by every CloudflareAccount that uses
  that token (3.6 requests/s and a burst of 20 by default). The most recently configured
  account using a token sets the shared values.
- **On a 429**, the whole token pauses (all accounts and controllers using it) for
  `Retry-After`, or for an exponential back-off (250 ms doubling, capped at 10 s, jittered)
  when there is no header. The same call is retried up to 8 times.
- **A reconcile never sleeps more than 30 s** (`cfclient.MaxInlineWait`). A longer
  `Retry-After` returns the 429 to the reconciler: the object reports `Synced=False` with
  reason **`RateLimited`** and the wait in the message, and is requeued after `Retry-After`
  (at least 1 s), not in a hot retry loop. Until then, other objects that use the token are
  refused locally without calling the API (error code 971, "token is backing off after HTTP
  429"; `cloudflare_api_throttled_total{source="client"}`).
- **5xx and transport errors** are retried only for idempotent calls (GET, HEAD, PUT, DELETE),
  up to `spec.rateLimit.maxRetries` times (default 4). A POST or PATCH that failed is not
  retried inside the client; the next reconcile looks the resource up first
  ([Crash consistency](#crash-consistency)), so a create that did succeed is adopted, not
  repeated.

## Crash consistency

A create is two steps: the Cloudflare call, then writing the new ID to the object. A manager
killed, evicted or cut off from the API server between the two must never leave a second
resource or lose one. What the operator guarantees (details, residual risks and the tests in
[resilience.md §1](resilience.md#1-crash-consistency-create-then-record)):

- **Before a create**, the object gets a `flare.dev/create-pending: <uid>/<key>`
  record (key: the name, or the client-chosen id of AIGateway and R2Bucket). **After it**, the external-id and
  ownership-proof annotations are written.
- **On the next reconcile**, a resource matching the record is the object's own lost create
  and is adopted, not created again. The generated kinds and a tagged Tunnel also find it by
  name; AIGateway and R2Bucket by their id (for R2 the bucket name).
- **An object deleted while the manager was down**, between the create and the record, is
  still cleaned up: the finalizer resolves the create-pending record first and deletes the
  resource under `deletionPolicy: Delete`, through the normal ownership check.
- **A lost POST answer** (timeout, reset connection) is not retried by the client; the next
  reconcile's lookup adopts what it created.
- **A failed page of a paginated list** fails the whole lookup, so a partial list is never
  taken as "not found".

`TestCrashBetweenCreateAndRecord` and `TestCrashThenDeleteBeforeRestart` check every kind for
exactly one Cloudflare resource and one create call. Known residual risks, by design:

- While creates keep failing transiently, a same-named resource that someone else creates in
  that window is adopted as the object's own.
- If a list lags a create (eventual consistency; UNVERIFIED for every product), a duplicate
  is possible for Tunnel names, whose uniqueness is UNVERIFIED.
- If the resource behind a known ID was found gone and its recreate's answer is lost, an object
  deleted before the next reconcile deletes only the old ID.

## Troubleshooting by condition reason

Start with `kubectl get cloudflare -A`: every kind is in the `cloudflare` category, and the
READY/SYNCED columns come from the conditions. Then run `kubectl describe` on the object and
read the condition message, which carries the Cloudflare error code and message.
[api-reference.md](api-reference.md#conditions) defines every reason and the kinds that use it
(generated from `hack/apidocs/reasons.yaml`); the tables below say what to do about each.

**CloudflareAccount, `Ready=False`:**

| Reason | Meaning | Fix |
|---|---|---|
| `SecretNotFound`, `SecretKeyMissing` | `spec.tokenSecretRef` names a missing Secret or key (default key `token`). | Create or restore the Secret. The account is re-checked when the Secret changes. |
| `TokenInvalid` | Cloudflare rejected the token (401/403 on verify). | Rotate the token in the Secret. |
| `TokenDisabled`, `TokenExpired`, `TokenNotYetValid` | The token status or validity window. | Re-enable or rotate the token, or wait for `not_before`. |
| `AccountMismatch` | The token is valid but cannot read `spec.accountID`. | Check the account ID, or the token's account resources. |
| `BaseURLNotAllowed` | `spec.baseURL` is set and the manager does not allow it. | Remove it, or allow it with `baseURLOverride.allowed` (test clusters). |
| `ClientError` | No API client could be built (for example a malformed base URL). | Fix the spec. |
| `Unavailable` (with `Synced=False`, `ReconcileError`) | The first verification failed transiently: 5xx, 429, timeout. A previous `Ready=True` is kept on later blips. | Usually clears by itself; check egress and Cloudflare status. |

A deleting account that is not Ready blocks its objects' cleanup, and its message says how to
unblock it. A deleting account still used by managed objects reports `DependencyNotReady`
until they are gone. `Synced=False` with `TokenSecretUpdateFailed` means the `account-token`
finalizer could not be added to or removed from the Secret (check the manager's RBAC and the
Secret).

**Managed kinds (KVNamespace, Queue, D1Database, VectorizeIndex, SecretsStore, AIGateway, R2Bucket,
Tunnel, VPCService, WorkerScript, PagesProject, PagesDeployment):**

| Condition / reason | Meaning | Fix |
|---|---|---|
| `Ready=False` `AccountNotReady` (also on Synced) | The referenced CloudflareAccount is missing or not Ready. Retried every 15 s. | Fix the account first. |
| `Ready=False` `DependencyNotReady` (also on Synced) | A referenced object is not Ready yet (a WorkerScript or PagesProject binding's `*Ref`, a VPCService's `tunnelRef`, a PagesDeployment's `projectRef` or artifact ConfigMap), or a deletion waits for referrers (a Tunnel still used by VPCServices; a KVNamespace, Queue, D1Database, VPCService or WorkerScript still bound by a WorkerScript or PagesProject; a PagesProject that still has PagesDeployments). The message names the object. | Make the referenced object Ready, or delete or change the referrer first. |
| `Ready=False` `Creating` | A create was sent; the resource has not been read back yet. | Wait. |
| `Ready=False` `Unavailable` | The resource exists but is not usable yet (a Tunnel whose `cloudflared` replicas are not ready or not connected). | Check the `cloudflared` pods, their logs and their egress. |
| `Ready=False` `ExternalNotFound` | The resource is gone from Cloudflare, or an `Observe` object's target does not exist. | Recreate it by removing the external-id annotation, or fix the name or ID. |
| `Ready=False` `Deleting` | The finalizer is running (Tunnel: scaling `cloudflared` to zero, waiting for VPCServices). The message says what it waits for. | Wait, or resolve what the message names. |
| `Synced=False` `RateLimited` | Cloudflare answered 429 with a long `Retry-After`, or the token is still backing off. The object is requeued after the wait. | Nothing, if it clears. If it persists, lower the load: [Reconcile tuning](#reconcile-tuning-and-the-api-budget). |
| `Synced=False` `ReconcileError` | The last API call failed; the message has the Cloudflare code. 5xx and transport errors are retried with back-off. It also covers a difference the policies or the API do not allow to fix (no update operation, `Update` not in `managementPolicies`). | 403 → [token permissions](../README.md#token-permissions). 400 → a spec value the API rejects. Timeouts → egress, or `reconcile.cloudflareRequestTimeout`. |
| `Synced=False` `Immutable` | A create-only field changed; nothing was written. | Revert the field, or delete and recreate the object. |
| `Synced=False` `DeleteFailed` (generated kinds) | The object is being deleted with `deletionPolicy: Delete` and Cloudflare refused the DELETE; the message has the API error. The finalizer stays and retries with back-off. R2Bucket: the bucket still holds objects (the error code is UNVERIFIED). | Empty the bucket (or fix what the message names), or set `deletionPolicy: Orphan` to keep the resource. |
| `Synced=False` `NameConflict` (Tunnel, VPCService, WorkerScript, PagesProject; VectorizeIndex, SecretsStore, AIGateway, R2Bucket; any generated kind with tagging off) | A same-named resource exists and cannot be proven to be this object's. | Set `flare.dev/external-id` to adopt it, or rename. |
| `Synced=False` `InvalidHostname` (VPCService) | `host.hostname` looks like a short in-cluster name; `cloudflared` never applies DNS search domains. | Use the fully qualified name. |
| `Synced=False` `InvalidScriptName` (WorkerScript) | `forProvider.script_name` (or `metadata.name`) is not a valid Workers script name. | Set a valid `script_name`. |
| `Synced=False` `InvalidSpec` (WorkerScript) | The modules cannot be uploaded: a bad module name or type, content that is not base64 for `wasm-base64`, a `main_module` that is not a module, an unusable `sourceRef` ConfigMap. | Fix `forProvider` or the ConfigMap. |
| `Synced=False` `InvalidSpec` (PagesProject) | The project name (`forProvider.name`, or `metadata.name`) is not a valid Pages name, or one binding name is used twice in a deployment config. | Set a valid `name`, or rename the binding. |
| `Ready=False` `Deploying` (PagesDeployment) | The deployment was made; its deploy stage has not succeeded yet. Re-read every few seconds (every minute after 5 minutes). | Wait. |
| `Ready=False` `DeploymentFailed` (PagesDeployment, also on Synced) | The deploy stage failed. Not retried. | Read the deployment's logs in the dashboard; change the artifact or branch to deploy again. |
| `Synced=False` `InvalidArtifact` (PagesDeployment) | The artifact cannot be deployed: a bad source, a loader limit or safety rule, a file over 25 MiB, too many files, a `_worker.js` directory or a top-level `functions` directory without a `_worker.js` file (Pages Functions: compile them into `_worker.js`). | Fix the source or its content. |
| `Synced=False` `ArtifactUnavailable` (PagesDeployment) | The registry or download server failed or could not be reached; retried with back-off. | Check the URL or image, the pull Secret, egress and `--artifact-allowed-cidr`. |
| `Ready=False` `LiveDeploymentKept` (PagesDeployment, while deleting) | Its deployment is the project's live production deployment, which Cloudflare never deletes; it is kept and the object goes. | Nothing; deploy another production deployment or delete the project to remove it. |
| `Synced=True` `ObserveOnly` | `managementPolicies: ["Observe"]`: read-only, as intended. | |
| Warning event `ExternalResourceKept` | Deletion kept the Cloudflare resource: no ownership proof, a permanent error reading the tags, or the CloudflareAccount is gone. | Delete it in Cloudflare by hand if it should go. |
| Warning event `ForeignOwnerTunnelKept` (Tunnel) | The tunnel's owner tag names another object or cluster, so it was kept. | Delete it by hand if it should go. |

When an object never changes: check that its `status.observedGeneration` matches
`metadata.generation`. If it doesn't, the manager has not processed the latest spec; check
that the manager is the leader and its logs for the object.

## Backup and restore of the custom resources

The Cloudflare state lives in Cloudflare; the objects are the desired state plus the link to
it. Back up the objects and the token Secrets together (Velero, or plain YAML):

```sh
kubectl get cloudflare -A -o yaml > flare-objects.yaml     # every flare.dev kind
# The token Secrets the accounts reference. This file contains the tokens: store it encrypted.
kubectl get cloudflareaccounts -A -o json |
  jq -r '.items[] | "\(.metadata.namespace) \(.spec.tokenSecretRef.name)"' | sort -u |
  while read -r ns s; do kubectl -n "$ns" get secret "$s" -o yaml; echo ---; done > tokens.yaml
```

What makes a restore safe:

- Keep the annotation **`flare.dev/external-id`**. It pins each object to its
  Cloudflare resource, so the restored object adopts it instead of creating a second one.
  `status` is not needed; the manager fills it again.
- Restore **with the same `clusterName`**, so the owner tags (`<clusterName>/<ns>/<name>`)
  still name the restored objects. `flare.dev/ownership-proof` contains the old
  object UID and no longer matches after a restore. The owner tag, which is read again, is
  the proof from then on. With `ownershipTags=false`, the external-id annotation alone is the
  proof.
- Restore the Secrets and CloudflareAccounts first, or together with the objects. The objects
  wait (`AccountNotReady`, retried every 15 s) until their account is Ready.
- **Never run two clusters with the same `clusterName` against the same account at the same
  time.** Stop or scale down the old manager before restoring into a new cluster.
- Strip `metadata.uid`, `resourceVersion`, `creationTimestamp`, `managedFields`,
  `deletionTimestamp` and `finalizers` from plain-YAML backups before applying them.

This procedure follows from the adoption and ownership rules. It is not exercised by an
automated test yet (UNVERIFIED as a whole; the pieces, adoption by external-id and by owner
tag, are covered by the controller tests and e2e).

## Several Cloudflare accounts, several clusters

- **Several accounts in one cluster.** Create one CloudflareAccount (and token Secret) per
  Cloudflare account in each namespace that uses it. Objects refer to an account in their
  own namespace by `spec.accountRef.name`. Accounts are independent: separate tokens have
  separate rate limits, while accounts that share a token share one limiter. A typical layout
  is one namespace per team or environment, each with its own least-privilege token.
- **One account, several clusters.** Give every cluster a distinct `clusterName`. The owner
  tag keeps each cluster from adopting or deleting another cluster's resources. The objects
  concerned report `NameConflict`, a foreign-owner error, or `ForeignOwnerTunnelKept`, instead
  of fighting. Give each cluster its own token, so one cluster's 429s don't throttle the other.
- **One cluster, several operator installs** is not supported. Each manager watches every
  namespace and would reconcile the same objects, and the CRDs are cluster-wide anyway.
  (Splitting kinds between installs with the `controllers` value is possible in principle but
  untested: UNVERIFIED.)

## Releases

`.goreleaser.yaml` (checked with `make release-check`) builds the release artifacts:

- manager (linux) and flarefake (linux, darwin) binaries for amd64 and arm64, stamped with
  version, commit and date, in archives with checksums and SPDX SBOMs;
- multi-arch images `$IMAGE_REGISTRY/flare-operator` and `$IMAGE_REGISTRY/flarefake`, with OCI
  labels and SBOM attestations;
- the chart as `flare-operator-<version>.tgz`, with chart version = appVersion = the release
  version.

`make release-snapshot` builds all of it into `dist/` and `bin/chart/`, and publishes nothing.
In a checkout without a git remote, goreleaser cannot read the git state for a snapshot: it
stamps commit `none` and date `0001-01-01T00:00:00Z`, and names the artifacts
`<version>-snapshot.none`. Treat such a snapshot as a local build without provenance; a
release build (`goreleaser release`, from a tagged clone with a remote) stamps both.

**Publishing.** Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`. It logs in to
`ghcr.io` with the workflow's `GITHUB_TOKEN` (`packages: write`), runs `goreleaser release`
(images to `ghcr.io/chenhunghan`, archives, checksums, SBOMs and the chart `.tgz` on the GitHub
release), then pushes the chart to `oci://ghcr.io/chenhunghan/charts/flare-operator`. A tag with
a pre-release suffix (`v0.2.0-rc.1`) is published as a pre-release and does not move `:latest`.
The images carry `org.opencontainers.image.source`, which links each GHCR package to the
repository. GHCR creates new packages as **private**: after the first release, make the
`flare-operator`, `flarefake` and `charts/flare-operator` packages public once, in each
package's settings. Record changes in [CHANGELOG.md](../CHANGELOG.md).
