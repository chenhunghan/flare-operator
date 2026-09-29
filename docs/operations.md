# Operating flare-operator

This is the runbook for installing, upgrading, running and removing flare-operator. For
concepts (deletionPolicy, managementPolicies, adoption, ownership tags), see the
[README](../README.md#concepts). For tokens and privileges, see [SECURITY.md](../SECURITY.md).
Chart values are listed in `charts/flare-operator/values.yaml` and validated by
`values.schema.json`.

- [Install](#install)
- [Upgrade](#upgrade)
- [Uninstall and what happens to Cloudflare resources](#uninstall-and-what-happens-to-cloudflare-resources)
- [Health, metrics and logs](#health-metrics-and-logs)
- [High availability and leader election](#high-availability-and-leader-election)
- [Network policy](#network-policy)
- [Rate limiting and HTTP 429](#rate-limiting-and-http-429)
- [Troubleshooting by condition reason](#troubleshooting-by-condition-reason)
- [Backup and restore of the custom resources](#backup-and-restore-of-the-custom-resources)
- [Several Cloudflare accounts, several clusters](#several-cloudflare-accounts-several-clusters)
- [Releases](#releases)

## Install

1. **Images.** There are no published images yet. Build them with `make docker-build`
   (host platform) or `make docker-buildx` (linux/amd64 and linux/arm64, with SBOM and
   provenance attestations; `BUILDX_OUTPUT=--push IMG=<registry>/flare-operator:<tag>` pushes
   the multi-arch index). The nodes must be able to pull the image.
2. **Install the chart** into its own namespace, and give the cluster a unique `clusterName`:

   ```sh
   helm install flare-operator charts/flare-operator -n flare-system --create-namespace \
     --set image.repository=<registry>/flare-operator --set image.tag=<tag> \
     --set clusterName=<unique-cluster-name>
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
helm upgrade flare-operator charts/flare-operator -n flare-system --reuse-values \
  --set image.tag=<new tag>
kubectl -n flare-system rollout status deploy/flare-operator
```

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

GitOps tools (Argo CD, Flux) apply `crds/` on sync, so they need no extra step.

**Tested path.** `make e2e-upgrade` installs the chart and manager of a previous git ref
(default: the latest tag, else the merge base with `main`), creates an account, a KVNamespace,
a Queue, a D1Database and a Tunnel, runs `make crds-apply` and `helm upgrade` to the current
checkout, and checks:

- the objects stay Ready with the same Cloudflare IDs;
- the new manager reads every resource and makes **no** Cloudflare write, so there is no
  duplicate create and no drift from the version change, also after a second restart;
- then a smoke subset of `TestEndToEnd` runs.

Everything runs against flarefake in the cluster. See `hack/e2e-upgrade.sh`.

**Rollback.** `helm rollback flare-operator <revision>` rolls back the manager. It does not
roll back the CRDs. Stay on the newer CRDs unless the release notes say otherwise:
v1alpha1 changes so far only add fields, and an older manager ignores fields it does not know.

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

To remove the operator and **keep** everything in Cloudflare:

```sh
# 1. Make every object orphan its resource (or set managementPolicies: ["Observe"]).
for k in $(kubectl api-resources --categories cloudflare -o name); do
  kubectl get "$k" -A -o name | grep -v cloudflareaccount | while read -r o; do :; done
done
kubectl get cloudflare -A -o json | jq -r '.items[] | select(.kind!="CloudflareAccount") | "\(.kind).\(.apiVersion|split("/")[0]) -n \(.metadata.namespace) \(.metadata.name)"' |
  while read -r kind _ ns name; do kubectl patch "$kind" -n "$ns" "$name" --type merge -p '{"spec":{"deletionPolicy":"Orphan"}}'; done
# 2. Delete the objects while the manager still runs (the finalizers release the owner tags).
kubectl delete cloudflare -A --all
# 3. Remove the release, then the CRDs.
helm uninstall flare-operator -n flare-system
kubectl delete -f charts/flare-operator/crds/
```

To remove the operator and **delete** what it created, delete the objects with
`deletionPolicy: Delete` while the manager runs, wait until they are gone, then uninstall.

If the manager is already gone and objects are stuck in `Terminating`, remove their finalizers
(`cloudflare.flare.dev/finalizer`, and `cloudflare.flare.dev/account-in-use` on accounts). The
Cloudflare resources stay as they are:

```sh
kubectl patch <kind> <name> -n <ns> --type merge -p '{"metadata":{"finalizers":null}}'
```

Token Secrets carry `cloudflare.flare.dev/account-token` while an account uses them. The
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

The metrics are controller-runtime's standard set. The flare-operator-specific metrics (API
calls, latency, 429s) are planned in PR-3. Useful series today:

- `controller_runtime_reconcile_total{controller,result}` and
  `controller_runtime_reconcile_errors_total{controller}`: error rate per kind.
- `controller_runtime_reconcile_time_seconds`: reconcile latency. It includes the time spent
  waiting on the client-side Cloudflare rate limiter.
- `workqueue_depth{name}`, `workqueue_retries_total{name}`, `workqueue_longest_running_processor_seconds`:
  backlog and retry storms.
- `leader_election_master_status{name="flare-operator.cloudflare.flare.dev"}`: 1 on the leader.
- `rest_client_requests_total{code}`: Kubernetes API calls, not Cloudflare calls.

`metrics.serviceMonitor.enabled=true` creates a prometheus-operator ServiceMonitor for the
metrics Service. It needs the `monitoring.coreos.com` CRDs. Add the label your Prometheus
selects with `metrics.serviceMonitor.labels`.

Logs are zap JSON on stderr (`logging.encoder=console` for development). `logging.level=debug`
logs every reconcile. Each line carries `controller`, `namespace`, `name` and
`reconcileID`, so `kubectl logs deploy/flare-operator | jq 'select(.name=="sessions")'` follows
one object. Events are the other half: `kubectl get events -n <ns> --field-selector
involvedObject.name=<name>` shows `ExternalResourceKept` and `ForeignOwnerTunnelKept` warnings.

## High availability and leader election

- Leader election is on by default (`leaderElection.enabled`). The Lease is
  `flare-operator.cloudflare.flare.dev` in the release namespace:
  `kubectl -n flare-system get lease flare-operator.cloudflare.flare.dev -o jsonpath='{.spec.holderIdentity}'`.
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

## Rate limiting and HTTP 429

Cloudflare allows 1200 requests per 5 minutes per token (global limit). The operator:

- keeps **one client-side token bucket per API token**, shared by every CloudflareAccount
  that uses that token. The default is 3.6 requests/s (1080 per 5 minutes, 90 % of the limit),
  with a burst of 20. Tune it per account with `spec.rateLimit.requestsPerFiveMinutes` and
  `spec.rateLimit.burst`. The most recently configured account using a token sets the shared
  values;
- on a **429**, blocks the whole token (all accounts and controllers using it) for
  `Retry-After`, or for an exponential back-off (250 ms doubling, capped at 10 s, jittered)
  when there is no header, and retries the same call up to 8 times;
- does not sleep in a reconcile for more than 30 s (`MaxInlineWait`). A longer
  `Retry-After` returns the 429 to the reconciler. The object reports `Synced=False` /
  `ReconcileError` with the API error, and controller-runtime requeues it with per-object
  exponential back-off (5 ms doubling, up to about 16 minutes). Other objects that use the
  token wait out the block without calling the API (error code 971, "token is backing off
  after HTTP 429");
- retries **5xx and transport errors** only for idempotent calls (GET, PUT, DELETE, HEAD), up
  to 4 times (`spec.rateLimit.maxRetries`). A POST that failed with a 5xx is not retried
  blindly: the next reconcile first looks for the resource by name or owner tag and adopts it,
  so it is not created twice;
- polls in-sync objects every 5 minutes for drift, and re-verifies tokens every 10 minutes.
  About 300 in-sync objects per token therefore use one request per second at steady state.
  `spec.rateLimit.listCacheTTL` caches collection GETs, which the adoption and tag lookups
  use.

Sharing a token with other tools (CI, Terraform, wrangler) shares Cloudflare's budget, not the
operator's bucket. Give the operator its own token, or lower `requestsPerFiveMinutes`. The
fault-injection tests for 429 storms and a scale test under the limit are planned in PR-3; the
behavior above is covered by the `internal/cfclient` unit tests.

## Troubleshooting by condition reason

Start with `kubectl get cloudflare -A`: every kind is in the `cloudflare` category, and the
READY/SYNCED columns come from the conditions. Then run `kubectl describe` on the object and
read the condition message, which carries the Cloudflare error code and message.

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
unblock it. `Synced=False` with `TokenSecretUpdateFailed` means the `account-token` finalizer
could not be added to or removed from the Secret (check the manager's RBAC and the Secret).

**Managed kinds (KVNamespace, Queue, D1Database, Tunnel, VPCService, …):**

| Condition / reason | Meaning | Fix |
|---|---|---|
| `Ready=False` `AccountNotReady` (also on Synced) | The referenced CloudflareAccount is missing or not Ready. Retried every 15 s. | Fix the account first. |
| `Ready=False` `Creating` | Created, not yet usable (a Tunnel waiting for `cloudflared`). | Wait; for Tunnels, check the `cloudflared` pods. |
| `Ready=False` `ExternalNotFound` | The resource is gone from Cloudflare, or an `Observe` object's target does not exist. | Recreate it by removing the external-id annotation, or fix the name or ID. |
| `Ready=False` `Deleting` | The finalizer is running (Tunnel: scaling `cloudflared` to zero, waiting for VPCServices). The message says what it waits for. | Wait, or resolve what the message names. |
| `Synced=False` `ReconcileError` | The last API call failed; the message has the Cloudflare code. 429s and 5xx are retried with back-off. | 403 → token permissions (README table). 400 → a spec value the API rejects. 429 → see [Rate limiting](#rate-limiting-and-http-429). |
| `Synced=False` `Immutable` | A create-only field changed; nothing was written. | Revert the field, or delete and recreate the object. |
| `Synced=False` `NameConflict` (VPCService) | A same-named service exists and cannot be proven ours. | Set `cloudflare.flare.dev/external-id` to adopt it, or rename. |
| `Synced=False` `InvalidHostname` (VPCService) | The backend host is not usable. | Fix `spec`. |
| `Synced=True` `ObserveOnly` | `managementPolicies: ["Observe"]`: read-only, as intended. | |
| Warning event `ExternalResourceKept` | Deletion kept the Cloudflare resource: no ownership proof, or a permanent error reading the tags. | Delete it in Cloudflare by hand if it should go. |

When an object never changes: check that its `status.observedGeneration` matches
`metadata.generation`. If it doesn't, the manager has not processed the latest spec; check
that the manager is the leader and its logs for the object.

## Backup and restore of the custom resources

The Cloudflare state lives in Cloudflare; the objects are the desired state plus the link to
it. Back up the objects and the token Secrets together (Velero, or plain YAML):

```sh
kubectl get cloudflare -A -o yaml > flare-objects.yaml     # every flare.dev kind
kubectl get secret -A -o yaml ... > tokens.yaml            # the Secrets tokenSecretRef names; store encrypted
```

What makes a restore safe:

- Keep the annotation **`cloudflare.flare.dev/external-id`**. It pins each object to its
  Cloudflare resource, so the restored object adopts it instead of creating a second one.
  `status` is not needed; the manager fills it again.
- Restore **with the same `clusterName`**, so the owner tags (`<clusterName>/<ns>/<name>`)
  still name the restored objects. `cloudflare.flare.dev/ownership-proof` contains the old
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
  namespace and would reconcile the same objects. Use `controllers` to split kinds between
  installs only if they don't overlap.

## Releases

`.goreleaser.yaml` (checked with `make release-check`) builds the release artifacts:

- manager (linux) and flarefake (linux, darwin) binaries for amd64 and arm64, stamped with
  version, commit and date, in archives with checksums and SPDX SBOMs;
- multi-arch images `$IMAGE_REGISTRY/flare-operator` and `$IMAGE_REGISTRY/flarefake`, with OCI
  labels and SBOM attestations;
- the chart as `flare-operator-<version>.tgz`, with chart version = appVersion = the release
  version.

`make release-snapshot` builds all of it into `dist/` and `bin/chart/`, and publishes nothing.
Publishing stays disabled (`release.disable: true`, placeholder `IMAGE_REGISTRY`) until the
repository has a permanent home. Record changes in [CHANGELOG.md](../CHANGELOG.md).
