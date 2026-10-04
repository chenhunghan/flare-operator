# `kubectl logs` for Workers: a virtual kubelet on the Free plan

Status: **design, 2026-10-04**. Contracts frozen in `internal/workerlogs`, `internal/vk`,
`internal/vk/standin` and `api/workers/v1alpha1/standinpod.go`. Nothing here needs Cloudflare
Containers or the Workers Paid plan. Parent design: [virtual-kubelet-design.md](virtual-kubelet-design.md).

## 1. What users get

```sh
helm upgrade flare-operator charts/flare-operator --reuse-values --set workersLogs.enabled=true
kubectl get pods -l flare.dev/stand-in=worker           # one Pod per WorkerScript, NODE cf-workers
kubectl logs api-worker                                 # recent logs from Workers Logs
kubectl logs api-worker --since=10m --timestamps
kubectl logs -l flare.dev/workerscript=api --tail=50
kubectl logs -f api-worker                              # live, through the legacy tail
kubectl exec api-worker -- sh                           # error: only `kubectl logs` is available
```

- **Opt-in.** Chart value `workersLogs.enabled`, default `false`. Per WorkerScript, the annotation
  `flare.dev/stand-in-pod: "false"` opts out.
- **Scope.** WorkerScripts only, including observe-only ones. Every other resource stays a CRD.
- **No change to the manager.** The manager's binary, flags, RBAC and metrics stay as they are.

## 2. Architecture

```
kubectl logs api-worker
   │
   ▼
kube-apiserver ── authorizes pods/log in the namespace (the user's RBAC)
   │  then dials node cf-workers: status.addresses[InternalIP]:daemonEndpoints.kubeletEndpoint.Port
   │  over TLS, presenting its kubelet client certificate (--kubelet-client-certificate)
   ▼
workers-vk Deployment (release namespace, 1 active replica)            ┌─► POST …/workers/observability/telemetry/query
   ├─ kubelet API (HTTPS :10250): mTLS authn + SubjectAccessReview      │   (history: logs, --since, --tail)
   │   └─ /containerLogs/{ns}/{pod}/worker → PodResolver → Streamer ───┤
   ├─ node controller + node Lease (virtual-kubelet node.NodeController)│   POST …/workers/scripts/{s}/tails → wss:// (trace-v1)
   ├─ pod controller (virtual-kubelet node.PodController): Pod status  └─► (live: -f), proxied, DELETE …/tails/{id}
   └─ stand-in controller: one Pod per WorkerScript (controller-runtime)
        reads WorkerScripts, CloudflareAccounts (cache) and token Secrets (uncached get)
```

## 3. How kube-apiserver reaches a kubelet (decision a)

**Observed on the local k0s cluster** (a local single-node Lima VM, k0s v1.36.4+k0s.1, `k0s controller --single=true`,
2026-10-04, read-only):

| Fact | Evidence |
|---|---|
| The apiserver verifies kubelet serving certificates | `kube-apiserver --kubelet-certificate-authority=/var/lib/k0s/pki/ca.crt` |
| It authenticates to kubelets with a client certificate `CN=apiserver-kubelet-client, O=system:masters`, issued by `kubernetes-ca` | `--kubelet-client-certificate=/var/lib/k0s/pki/apiserver-kubelet-client.crt`; `openssl x509 -subject -issuer` |
| It dials `InternalIP` first | `--kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname` |
| Port comes from `status.daemonEndpoints.kubeletEndpoint.Port` | node `lima-<instance>`: `{"kubeletEndpoint":{"Port":10250}}`, addresses `InternalIP <node IP>`, `Hostname` |
| No konnectivity in single-node mode: the apiserver dials directly from the host, which routes Pod IPs (kube-router) | no `konnectivity-server` process; same host runs the CNI |
| The real kubelet's serving certificate is a CSR-issued `kubernetes.io/kubelet-serving` cert, `O=system:nodes, CN=system:node:lima-<instance>`, SANs `DNS:lima-<instance>, IP:<node IP>`, issuer `kubernetes-ca` | kubelet `serverTLSBootstrap: true`; `/var/lib/k0s/kubelet/pki/kubelet-server-current.pem` |
| kube-controller-manager signs with the same CA the apiserver trusts for kubelets | `--cluster-signing-cert-file=/var/lib/k0s/pki/ca.crt` |
| The cluster client CA is published | ConfigMap `kube-system/extension-apiserver-authentication`, keys `client-ca-file`, `requestheader-*` |

**In general:** `kubectl logs` → apiserver `pods/log` (authorized for the *user*) → the apiserver
looks up the Pod's node and dials `https://<preferred address>:<kubeletEndpoint.Port>/containerLogs/<ns>/<pod>/<container>?…`
as *itself*. The user's identity never reaches the kubelet. Whether the apiserver checks the
kubelet's certificate depends on `--kubelet-certificate-authority`. kubeadm leaves it unset
(no verification); k0s sets it. When the control plane uses konnectivity (k0s with separate
controller nodes, GKE, AKS), the connection is tunnelled to an agent inside the cluster network,
which then dials the address.

**Decision.** The virtual node advertises the **virtual kubelet Pod's IP** as `InternalIP`
(`AddressPodIP`) and port **10250** (the Pod has its own network namespace, so there's no clash),
plus `Hostname=cf-workers`. The leader writes its own Pod IP into the Node status when it takes
over. For control planes that reach node IPs but not Pod IPs (a managed control plane with an
overlay CNI), `workersLogs.hostNetwork=true` advertises the host IP on port **10260**
(`AddressHostIP`). Reachability of Pod IPs from managed control planes (EKS with the VPC CNI,
GKE, AKS) is UNVERIFIED; it is what metrics-server needs too.

## 4. Decisions b–d: authentication, TLS, packaging

### 4.1 Kubelet-API authn/authz (decision b)

- **Authentication:** virtual-kubelet's `nodeutil.WebhookAuth` (v1.14.0 `node/nodeutil/auth.go`)
  builds a `DelegatingAuthenticatorConfig`. We pass a **client-certificate CA provider**:
  `dynamiccertificates.NewDynamicCAFromConfigMapController("client-ca", "kube-system",
  "extension-apiserver-authentication", "client-ca-file", …)` from `k8s.io/apiserver`, so the
  apiserver's client certificate is verified against the cluster client CA and CA rotation is
  picked up. Bearer tokens are verified with **TokenReview** (in-cluster callers such as
  metrics-server). Anonymous access is off. TLS uses `ClientAuth: RequestClientCert`; the x509
  authenticator does the chain check.
- **Authorization:** a **SubjectAccessReview** per request (`nodeutil.NodeRequestAttr`):
  resource `nodes`, name `cf-workers`, subresource `log` for `/logs`, `stats`, `metrics`, and
  **`proxy` for everything else, including `/containerLogs`** (same mapping as the kubelet's
  `pkg/kubelet/server/auth.go`). On k0s the apiserver's identity is in `system:masters`; on
  kubeadm it is bound to `system:kubelet-api-admin`. Both allow `nodes/proxy`.
- **RBAC to make this work:** `create` on `tokenreviews` and `subjectaccessreviews`, and the
  built-in Role `kube-system/extension-apiserver-authentication-reader` bound to the VK's
  ServiceAccount.

### 4.2 Serving TLS (decision c)

A self-signed certificate **fails on k0s** (`--kubelet-certificate-authority` is set). Modes
(`vk.TLSMode`, chart `workersLogs.tls.mode`):

| Mode | How | Where it works |
|---|---|---|
| `csr` (default) | ECDSA P-256 key; CSR `signerName: kubernetes.io/kubelet-serving`, `CN=system:node:cf-workers, O=system:nodes`, SANs `cf-workers` + the advertised IP, usages `digital signature, server auth`, `expirationSeconds` 24h; **approves its own CSR** (`workersLogs.tls.csr.approve`, default true); kube-controller-manager signs; renew at 80% of the lifetime or when the address changes | k0s (verified path: KCM signs with the CA the apiserver trusts) and every cluster that does not verify kubelet certs. Clusters whose signer refuses non-node requesters: UNVERIFIED (EKS, GKE). |
| `selfSigned` | generated at start | only where the apiserver does not verify kubelet certificates (kubeadm default). metrics-server then needs `--kubelet-insecure-tls`. A user can work around it per call with `kubectl logs --insecure-skip-tls-verify-backend`. |
| `secret` | `tls.crt`/`tls.key` from a Secret, reloaded on change | anything; the admin issues the certificate (for example cert-manager with the cluster CA) |

**Why the VK must approve its own CSR on k0s.** k0s's CSR approver
(`k0sproject/k0s@v1.36.4+k0s.1:pkg/component/controller/csrapprover.go`) only approves a
kubelet-serving CSR whose **requesting user equals its CN** (`system:node:<name>`) and whose
SANs match the Node's `status.addresses`, after a SubjectAccessReview for `create`
`certificatesigningrequests`. Our requester is a ServiceAccount, so k0s leaves the CSR pending
(it never denies it). kube-controller-manager's `csrapproving` never approves kubelet-serving
CSRs. So the VK needs `create/get/list/watch` on `certificatesigningrequests`, `update` on
`certificatesigningrequests/approval` and `approve` on `signers` `kubernetes.io/kubelet-serving`.
§10 covers the risk. With `approve: false` an admin runs `kubectl certificate approve` for each
CSR (every Pod restart changes the Pod IP and needs a new CSR).

The node is `Ready` only once a certificate is served (`ServingCert.Ready`).

### 4.3 Packaging (decision d)

- **A separate Deployment `<release>-workers-vk`, a new binary `cmd/workers-vk` in the same
  image** (`/workers-vk` beside `/manager`; one image to build, sign and publish). Reasons:
  - the kubelet API is a network-facing server, and it needs node, CSR and Pod-binding
    privileges that the manager must not get;
  - a VK crash, or a bug in virtual-kubelet, can't stop reconciles;
  - virtual-kubelet's dependencies (`k8s.io/apiserver` authn, gorilla/mux) stay out of the manager;
  - the manager's flag and metrics tables (`TestFlagsDocumented`, `TestMetricsDocumented`) are untouched;
  - the parent design already makes the VK its own process (§10 there).
  Rejected: a `--mode` of the manager binary (it would mix both flag sets) and a Runnable inside
  the manager (it would share the rate limiter, but put kubelet-port exposure and node/CSR
  RBAC on the manager).
- **Replicas and identity.** `replicas: 1`, `strategy: Recreate`, plus controller-runtime leader
  election on the Lease `flare-operator-workers-vk.flare.dev`, so a stuck old Pod never serves
  beside a new one. Everything (node controller, Pod controller, stand-in controller, HTTPS
  server) runs only on the leader. The node identity is the node name, not the Pod: the leader
  writes its address into the Node status.
- **Node lease.** virtual-kubelet `node.WithNodeEnableLeaseV1` (Lease `kube-node-lease/cf-workers`,
  default 40 s). The node controller is built directly from `node.NewNodeController` and
  `node.NewPodController`, **not `nodeutil.NewNode`**: NewNode starts unfiltered informers on
  every Secret, ConfigMap and Service in the cluster (v1.14.0 `nodeutil/controller.go`, the
  `scmInformerFactory`), which needs list/watch on all Secrets and caches them. We pass
  informers filtered to nothing and set `SkipDownwardAPIResolution`.
- **Cloudflare budget.** The VK has its own per-token limiter in its own process: `--api-budget`
  requests per 5 minutes per token, default **120**. The manager's default of 1080 leaves exactly
  120 of Cloudflare's 1200. One `kubectl logs` costs 1 request per 2000 events; `-f` costs 2
  (create and delete).
- **Credentials.** The same resolution as controllers: `reconcile.Accounts` behind a reader that
  is *not* a `client.Writer`, so `Resolve` labels only in memory and the VK never writes
  WorkerScripts. CloudflareAccounts come from the cache; token Secrets come from uncached `get`
  calls (no Secret list/watch).

**RBAC** (ClusterRole `<release>-workers-vk` unless noted):

| Resource | Verbs | Why |
|---|---|---|
| `nodes` | `create`; `get, update, patch, delete` (resourceNames `cf-workers`); `list, watch` | node controller |
| `nodes/status` | `update, patch` (resourceNames) | node status |
| `coordination.k8s.io/leases` in `kube-node-lease` (Role) | `create`; `get, update` (resourceNames) | node lease |
| `leases` in the release namespace (Role) | as the manager's | leader election |
| `pods` | `get, list, watch, create, delete` | stand-in Pods; the Pod controller |
| `pods/status` | `update, patch` | Pod status |
| `events`, `events.k8s.io/events` | `create, patch` | events |
| `flare.dev` `workerscripts`, `cloudflareaccounts` | `get, list, watch` | read-only |
| `secrets` | `get` | tokens (no list/watch) |
| `namespaces` | `get, list, watch` | `namespaceSelector` |
| `rbac.authorization.k8s.io/clusterroles` | `get` (resourceNames: its own) | owner anchor of the Node (§5.3) |
| `certificates.k8s.io/certificatesigningrequests` | `create, get, list, watch` | `tls.mode=csr` |
| `…/certificatesigningrequests/approval` | `update` | `csr.approve` |
| `certificates.k8s.io/signers` | `approve` (resourceNames `kubernetes.io/kubelet-serving`) | `csr.approve` |
| `authentication.k8s.io/tokenreviews`, `authorization.k8s.io/subjectaccessreviews` | `create` | kubelet-API auth |
| RoleBinding to `kube-system/extension-apiserver-authentication-reader` | | client CA |

- **NetworkPolicy** (when `networkPolicy.enabled`):
  - **Ingress** to port 10250 from `networkPolicy.apiServer.cidrs` (already required by the chart)
    plus `workersLogs.networkPolicy.kubeletAPIFrom` (for example konnectivity-agent Pods).
  - **Egress:** DNS, the API server, Cloudflare (`networkPolicy.cloudflareAPI.cidrs`, which also
    covers `tail.developers.workers.dev`; UNVERIFIED that the tail host sits in the published
    ranges), and flarefake when it is enabled.
  - A NetworkPolicy does not always let traffic from the API server *host* through (only traffic
    from the Pod's own node is guaranteed), so the CIDR rule is required.
- **Chart values** (new; all under `workersLogs`, schema-validated):

```yaml
workersLogs:
  enabled: false
  nodeName: cf-workers
  namespaceSelector: {}          # matchLabels/matchExpressions; empty = all namespaces
  hostNetwork: false             # true: advertise the host IP on port 10260
  port: 10250
  tls:
    mode: csr                    # csr | selfSigned | secret
    secretName: ""
    csr: {approve: true, lifetime: 24h}
  podImage: registry.k8s.io/pause:3.10  # never pulled; fixed so releases don't touch the Pods
  podResources: {cpu: 1m, memory: 1Mi}
  podLabels: {}
  apiBudget: 120                 # Cloudflare requests per 5 minutes per token
  logs: {defaultWindow: 72h, maxEvents: 10000, maxFollowers: 100}
  networkPolicy: {kubeletAPIFrom: []}
  resources: {}
  nodeSelector: {}
  tolerations: []
  affinity: {}
```

## 5. Stand-in Pods (decision e)

**Who does what:** the **stand-in controller in the VK process** creates and deletes the Pods and
owns their metadata and spec. The **virtual-kubelet PodController** owns their status. Each field
has a single writer. The manager doesn't take part, so the feature lives in one place, and
turning it off removes the controller with the Deployment. Rejected: a controller in the manager
(that would mean new Pod and node RBAC on the manager, and Pods that exist while no kubelet
serves them).

### 5.1 The Pod

Built by `standin.Builder`; the contract (`internal/vk/standin/contract.go`) lists every field.
The main ones:
- **Name** `<ws>-worker`, hashed when that's over 63 characters.
- **Labels and owner:** labels `flare.dev/workerscript`, `flare.dev/workerscript-uid` and
  `flare.dev/stand-in=worker`; a controller `ownerReference` to the WorkerScript with
  `blockOwnerDeletion: false`.
- **Placement and lifecycle:** `nodeName: cf-workers` (the scheduler is bypassed),
  `terminationGracePeriodSeconds: 0`, `automountServiceAccountToken: false`.
- **Tolerations:** the provider taint, plus `not-ready` and `unreachable` without
  `tolerationSeconds`, so a VK outage never evicts.
- **The container:** one container `worker` with a placeholder image that is never pulled
  (`registry.k8s.io/pause:3.10` by default, fixed across releases; if an admission policy
  restricts registries, set `workersLogs.podImage` to an allowed image; an image change is
  patched into the Pods in place, not recreated); tiny requests equal to
  limits so that ResourceQuota and LimitRange accept the Pod.
- **Pod Security:** compliant with Pod Security **restricted**.

Pods that aren't stand-ins but get bound to the node (DaemonSets that tolerate everything) are set
to `Failed`, reason `UnsupportedOnVirtualNode`. The node has **no `kubernetes.io/os` or `arch`
label**, so DaemonSets that select `kubernetes.io/os=linux` (on k0s that's `kube-proxy` and
`kube-router`, which both tolerate every taint, as observed on the local k0s VM) never target it.

### 5.2 Status

`standin.StatusMapper` turns the WorkerScript's conditions into Pod status. The provider
implements `GetPodStatus` and `NotifyPods`: an informer on WorkerScripts calls the notifier, and
the PodController writes the status.

| WorkerScript | Pod phase | Container | Ready |
|---|---|---|---|
| Ready=True | Running | running | True |
| Ready≠True, never deployed (`atProvider.version_id` empty) | Pending | waiting, reason = the Ready reason (`Creating`, `AccountNotReady`, `NameConflict`, …) | False |
| Ready≠True after a deployment | Running | running | False (reason/message from Ready) |

The phase is **never Failed**: Kubernetes doesn't let a Pod leave a terminal phase, and a
WorkerScript recovers. `imageID` and `containerID` are `cloudflare-workers://<script>@<version>`,
so `kubectl describe` shows the deployed version. `podIP` and `hostIP` stay empty.

### 5.3 Deletion and garbage collection

| Event | What happens |
|---|---|
| WorkerScript deleted | GC deletes the Pod through its ownerReference. Grace 0, so the deletion is immediate and doesn't need the VK (UNVERIFIED that an apiserver delete with spec grace 0 skips Terminating; envtest test B3 checks it). |
| Annotation `stand-in-pod: "false"`, or the namespace stops matching | the stand-in controller deletes the Pod |
| `kubectl delete pod api-worker` | the PodController finishes the delete; the stand-in controller recreates the Pod (new UID), like a mirror Pod |
| Feature disabled, or release uninstalled | Helm deletes the ClusterRole `<release>-workers-vk`. The Node carries an **ownerReference to that ClusterRole**, so GC deletes the Node, and the pod GC controller then force-deletes every Pod bound to a node that no longer exists (after about 40 s of quarantine). A VK that is still shutting down can't keep the Node alive: any Node it recreates names an owner that is gone, so GC removes it again. No hook Job is needed. |
| VK down | the Node goes NotReady and gets the not-ready/unreachable taints. The Pods tolerate them, so they stay put with stale status; `kubectl logs` fails with a connection error. |
| Another release's VK owns a Node of the same name | the VK refuses to start (owner UID mismatch) |

## 6. One node per cluster (decision f)

One virtual node per release, `cf-workers`, serves every namespace and account. Credentials are
resolved per request: Pod → its controlling WorkerScript (same namespace, UID-checked) →
`spec.accountRef` → a Ready CloudflareAccount *in that namespace* → token. Namespaces stay the
tenancy boundary.

Rejected: one node per account.
- **Addressing.** Nodes are cluster-scoped, and the kubelet URL doesn't name the node, so each
  node would need its own address or port and its own CSR and lease.
- **Exposure.** Namespaced account names would show up in cluster-wide node names.
- **Capacity.** Doesn't matter here: `nodeName` bypasses the scheduler, so node capacity is only
  for display (`pods: 10000`).

## 7. Log lines and option semantics (decision g)

### 7.1 Sources

- **History:** `telemetry/query` with `view: events`, `dry: true`, `queryId:
  flare-operator-logs`, the filter `$metadata.service eq <script>`, and
  `timeframe {from: since, to: now}` in ms (recording 0054).
  - Results come newest first: page by passing the last `$metadata.id` as `offset` with
    `offsetDirection: next` (0093), with `limit` ≤ 2000 (a larger limit returns
    `400 too_big`, 0096, whose error body is not the v4 envelope).
  - Events with equal `timestamp` (one invocation) are ordered by `$metadata.id`.
  - Event shapes: `$metadata.type` `cf-worker` is a console call (`source.level`,
    `source.message`; `source.exception{name,stack}` for an exception); `cf-worker-event` is the
    invocation summary (`$workers.event.request{method,url}`, `$workers.event.response.status`,
    `$workers.outcome`, `wallTimeMs`, `cpuTimeMs`, `$metadata.rayId`).
- **Live:** a legacy tail.
  - Create it with `POST …/tails` and body `{}` (0064), then dial the `wss://` URL with
    subprotocol `trace-v1`, send `{"debug":false}`, and ping every 10 s (wrangler).
  - Frames are JSON `{outcome, eventTimestamp, event, logs[{level, message[], timestamp}],
    exceptions[{name, message, stack, timestamp}], scriptVersion, entrypoint, truncated}`.
  - The event kind is read the way wrangler's `printing.ts` reads it: `request`, `cron`,
    `queue`, `mailFrom`, `rpcMethod`, `scheduledTime` (alarm), `consumedEvents` (tail), or
    `{type, message}` (tail info, e.g. overload sampling).

### 7.2 Line format

One `Event` becomes one or more lines. Per invocation, its console lines come first, then its
exceptions, then the summary. All text is sanitized: C0 control characters other than tab, and
DEL, are written as `\xNN`, because request URLs are attacker-controlled and would otherwise let
someone inject terminal escapes into an operator's shell.

| Event | Line(s) |
|---|---|
| console call | `[<level>] <args joined by spaces; non-strings as compact JSON>` (level `log` when Cloudflare gives none); a multi-line message becomes several lines |
| exception | `[exception] <name>: <message>`, then each stack line verbatim |
| fetch | `[fetch] GET https://host/path?q -> 200 ok (wall 1ms, cpu 0ms) ray=a42919d6cd448b3c` (status, times and ray omitted when unknown; the tail has no ray) |
| scheduled / alarm | `[scheduled] "*/5 * * * *" -> ok` / `[alarm] -> ok` |
| queue / email / rpc | `[queue] jobs (3 messages) -> ok` / `[email] from:a@x to:b@y size:123 -> ok` / `[rpc] Api.get -> ok` |
| tail consumer / unknown | `[tail] tailing a,b -> ok` / `[event] -> unknown` |
| notice | `[notice] <Cloudflare's sampling message>`, or `[notice] flare-operator: tail reconnected; events between <t1> and <t2> may be missing` |
| truncated record | the line gets the suffix ` [truncated]` |

With `--timestamps`, each line starts with the event time in the kubelet's fixed-width UTC
format `2006-01-02T15:04:05.000000000Z` followed by a space (`workerlogs.TimestampFormat`).
Cloudflare's precision is milliseconds, and it is frozen per invocation.

### 7.3 kubectl options

| kubectl | Behavior |
|---|---|
| `logs` | the newest `maxEvents` (10000) events of the last `defaultWindow` (72 h, the Free plan's retention; DOCS workers-logs), oldest first; 1 API request per 2000 |
| `--tail=N` | `min(N, maxEvents)` newest events (each event is ≥ 1 line), then the last N **lines**. `--tail=0` prints no history. The VK serves `/containerLogs` with its own handler, because virtual-kubelet's parser turns an absent `tailLines` and `0` into the same value. |
| `--since`, `--since-time` | `timeframe.from` (a 30-day window is accepted, 0098; actual retention is 3 days Free, 7 days Paid) |
| `--timestamps` | §7.2 |
| `--limit-bytes` | the stream stops after N bytes |
| `-f` | the tail is opened **first**, then the history is read and printed, then live events stream. The history misses the last 15–30 s because of ingestion lag (spike §1), and the tail starts at its own creation, so events from about 15–30 s before the command can be missing (§9). |
| `-c worker` | the only container; any other name is a 404 |
| `-p/--previous` | 400 `ErrPreviousUnsupported` (a version-filtered query would be possible, 0122; deferred) |
| `-l selector`, `--all-containers`, `--prefix` | handled by kubectl, so they work |
| exec, attach, port-forward, cp, debug | 501 `ErrUnsupported`, with a message saying only `kubectl logs` is available |

## 8. Alternatives rejected

| Alternative | Why not |
|---|---|
| The newer `telemetry/live-tail` for `-f` | sessions dropped after 95–163 s, and each URL works once (spike §1) |
| Giving users the tail URL (e.g. a `kubectl flare tail` that prints it) | it is a capability URL that keeps working after DELETE (spike §1) |
| An aggregated API or a CRD subresource for logs | `kubectl logs` only speaks Pod → kubelet; a plugin breaks `-l`, `--all-containers` and IDE integrations |
| Stand-in Pods via a Deployment with `replicas: 1` | a ReplicaSet controller would fight over deletes and add nothing; the stand-in controller owns the Pod directly |
| `nodeutil.NewNode` | cluster-wide Secret, ConfigMap and Service informers (§4.3) |
| Authenticating the VK as a node (`system:node:cf-workers`) so k0s's approver signs it | it would need permission to mint `kube-apiserver-client-kubelet` certificates for any node name, which is far worse than approving kubelet-serving CSRs |
| A Helm hook Job to delete the Node on uninstall | it races the RBAC that Helm deletes at the same time; the ownerReference to the ClusterRole has no race |

## 9. Limits users will hit

- **Ingestion lag of 15–30 s:** a plain `kubectl logs` doesn't show the last half-minute; `-f` can
  miss events from just before it started (§7.3).
- **Sampling:**
  - Free plan: 200,000 log events per day per account. Past that, and past the account-wide
    5 billion per day, Cloudflare samples at 1%. Head sampling (`observability.head_sampling_rate`)
    drops events before they're stored. (DOCS workers-logs)
  - A busy Worker's tail goes into sampling mode and drops messages; the line `[notice] …`
    shows it.
  - At most 10 tail viewers per Worker, wrangler and dashboard sessions included. The VK uses one
    upstream tail per Worker, whatever the number of `-f` sessions. (DOCS real-time-logs)
  - A record over 256 KB is truncated (` [truncated]`).
- **Observability must be on** (`forProvider.observability.enabled` / `logs.enabled`), or the
  query finds nothing. The tail works regardless.
- **Retention:** 3 days (Free), 7 days (Paid). `--since=30d` doesn't bring back older logs.
- **No `--previous`, no exec, attach, port-forward, cp or top**; `kubectl top pod` shows nothing.
- **Log collectors don't see these logs.** Fluent Bit, Vector and Promtail read `/var/log/pods`
  on real nodes, and Prometheus kubelet scrapes see an empty node. Use Workers Logpush
  (`forProvider.logpush`) for shipping.
- **Quotas and admission:** each stand-in Pod counts against the namespace's `pods` quota (and
  1m CPU / 1Mi memory). Admission policies (Kyverno, Gatekeeper) can reject it, for example on an
  image allowlist, a missing probe or required labels. The WorkerScript then gets the event
  `StandInPodFailed`; its own status isn't affected. `workersLogs.podImage` and `podLabels`
  exist for these policies.
- **Token permissions:** the spec lists `Workers Observability Write` for `telemetry/query`
  (UNVERIFIED whether Read is enough) and `Workers Tail Read` (or `Workers Scripts Write`) for
  tails. A token without them gets a 403, which `kubectl logs` passes on.
- **API budget:** 120 VK requests per 5 minutes per token by default. A busy team can hit HTTP
  429; raise `apiBudget` and lower the account's `rateLimit` so the two add up to 1200 or less.
- **Tolerate-all DaemonSets without an OS selector** create Pods on the node that fail with
  `UnsupportedOnVirtualNode`. Give them a node affinity `type NotIn [virtual-kubelet]`.

## 10. Security analysis

- **Who can read which Worker's logs:** whoever has `get pods/log` on the stand-in Pod, that is,
  in the WorkerScript's namespace.
  - The built-in `view`, `edit` and `admin` ClusterRoles all include `pods/log`, so **namespace
    viewers can read Worker logs**. Today those logs need Cloudflare dashboard access, and they
    may contain client IPs, headers, URLs and anything the code logs.
  - This is why the feature is opt-in, has a per-script opt-out annotation and a
    `namespaceSelector`, and why the chart README and SECURITY.md must say so.
  - Reading logs also spends the namespace account's API budget, which the namespace already
    controls.
- **The kubelet API can't be used to read other namespaces.**
  - The apiserver authorizes the user. The VK accepts only callers that pass mTLS against the
    cluster client CA, or a TokenReview, *and* a SubjectAccessReview for `nodes/proxy` on
    `cf-workers` (a cluster-admin-level permission).
  - A request names namespace/pod. The VK resolves only Pods bound to its node that a WorkerScript
    *in the same namespace* controls (UID-checked), and uses only that namespace's
    CloudflareAccount.
  - Pod annotations are never trusted for the script name or the account. Someone who can
    create Pods could bind a fake "stand-in" to the node, but it would only resolve to a
    WorkerScript in their own namespace, whose logs `pods/log` already lets them read.
- **Capability URLs:**
  - The tail URL lives only in the follower's memory. It is not logged at any level, not put in
    errors (dial errors are reduced to scheme and host), metrics, events or status, and never
    sent to a kubelet-API client.
  - Sockets are closed by us, and the tail is deleted, when the last follower leaves, on expiry,
    and at shutdown. A DELETE alone leaves the URL working.
  - A crash leaves the tail until its `expires_at` (about 6 h, 0064). Nobody holds the URL, so
    this is a quota cost, not a leak.
  - A `ws://` URL is refused unless the account uses an allowed `spec.baseURL` override
    (flarefake), so an API response can't downgrade the transport.
  - Unit test: the URL never appears in captured logs or errors.
- **Self-approved CSRs** (`tls.csr.approve`): RBAC can't limit `approve` on
  `signers/kubernetes.io/kubelet-serving` to one CN.
  - A stolen VK ServiceAccount token could therefore get a serving certificate for **any node's
    name and IP**. With a network position to intercept apiserver→kubelet traffic, that allows
    impersonating that kubelet to the apiserver (exec and log streams of real Pods).
  - Mitigations:
    - the VK approves only CSRs it created (requester = its own ServiceAccount, CN = its node,
      SANs = its address);
    - the token is a short-lived projected token in a dedicated Deployment;
    - `approve: false` plus manual approval, or `tls.mode=secret`, for clusters that won't
      grant it.
  - The doc and chart README must state this trade-off.
- **Secrets:** `get` on Secrets cluster-wide is the same power the manager already has. The VK
  doesn't list or watch them, and keeps only the cached client keyed by token hash.
- **Terminal injection:** sanitized output (§7.2).
- **DoS:** `maxFollowers` limits concurrent follow sessions; the per-token budget limits
  Cloudflare calls. Query results are bounded by `maxEvents`.

## 11. Test plan

| Layer | Tests |
|---|---|
| Unit (A) | `ParseOptions` (the kubelet's rules plus absent vs `tailLines=0`); Formatter golden files from the events of recording 0054 (log, error, exception with stack, fetch summary) and from synthetic trace-v1 frames for every wrangler `printing.ts` branch; control-character escaping; ordering by `Seq`; Streamer: `--tail` line trimming, `--limit-bytes`, `--timestamps` format, history then follow; tail hub (two followers → one create, last close → socket close + DELETE); reconnect on a missed pong and on `expires_at` with a fake clock, plus the gap notice; **redaction**: the tail URL never appears in logs or errors |
| flarefake (A; endpoints from the parallel flarefake work) | Query against telemetry/query with more than 2000 injected events (paging by `offset`/`next`), `400 too_big` above 2000, window filtering; Follow against the fake trace-v1 WebSocket with events injected through `/_fake`; the tail is deleted and the socket closed when the session ends. TestConformance replays the logs recordings (0047–0141). |
| envtest (B) | B1 kubelet API: a client certificate from an unknown CA → 401; a bearer token without `nodes/proxy` → 403; with it → 200. B2 node: created with the taint, labels, address, ownerReference to the ClusterRole, and lease; refuses a Node with a foreign owner. B3 Pod controller: status written from StatusMapper; a foreign Pod → Failed; deleting a Pod with spec grace 0 is immediate. B4 `/containerLogs` end to end with a fake `workerlogs.Source`; exec → 501 with the message. |
| envtest (C) | the stand-in reconciler creates a Pod with the exact spec; Pod Security **restricted** is enforced in a labelled namespace and admits the Pod; opt-out annotation and `namespaceSelector` delete the Pod; conflict with a foreign Pod → event, untouched; StatusMapper table tests |
| Chart (C) | `test/chart`: values schema, rendering with `workersLogs.enabled` on and off, RBAC and NetworkPolicy contents, no `workers-vk` objects when off |
| e2e on k0s (C, run after merge) | chart with flarefake and `workersLogs.enabled=true`. Check that: the CSR gets approved and signed; the Node becomes Ready; the WorkerScript's Pod becomes Running. Inject events through `/_fake`, then run the **real** `kubectl logs`, `--tail=2`, `--timestamps`, `--since=1m`, `-l flare.dev/workerscript=…`, and `-f` in the background (inject, see the line, stop, check through `/_fake` that the tail was deleted and its socket closed). `kubectl exec` gives the clear error. A user with `view` in another namespace is refused, and one with `view` in the namespace is allowed. Deleting the WorkerScript removes the Pod. Disabling the feature removes the Node and Pods within 60 s. |
| Live (optional, user-run) | one `flare-spike-*` Worker on the Free plan; `kubectl logs` output compared with a direct telemetry query; cleanup by the CLAUDE.md rules |

## 12. Work breakdown (≤ 3 parallel workstreams)

The contracts are frozen. Each workstream owns its files exclusively.
- A and C don't depend on each other.
- B compiles against the contracts and wires up `workerlogs.NewStreamer` and
  `standin.NewBuilder`/`NewStatusMapper`, so merge A and C first, then B.
- Conflicts in `go.mod` and `go.sum` (A adds `gorilla/websocket`; B adds
  `virtual-kubelet v1.14.0` and makes `k8s.io/apiserver` direct) are resolved by `go mod tidy`.

| Workstream | Owns | Delivers |
|---|---|---|
| **A: log source** | `internal/workerlogs/**` (except `contract.go`, `doc.go`) | `ParseOptions`, telemetry `Source.Query` with paging, the tail `Source.Follow` (WebSocket, ping, reconnect, expiry, hub, DELETE, URL redaction), `Formatter`, `Streamer`; unit and flarefake tests |
| **B: virtual kubelet** | `internal/vk/*.go` (not `standin/`), `cmd/workers-vk/**`, `Dockerfile` (second binary), `Makefile` (build/test targets) | node and lease, Pod controller wiring with filtered informers, the provider (status via StatusMapper and NotifyPods, foreign Pods), `PodResolver` with read-only Accounts and its own budget, the HTTPS server (own `/containerLogs` handler; virtual-kubelet `api.PodHandler` for the rest; empty stats/metrics), WebhookAuth with the dynamic client CA, the TLS modes (CSR request/approve/renew), leader election, flags; envtest B1–B4 |
| **C: stand-in Pods, chart, e2e, docs** | `internal/vk/standin/**` (except `contract.go`), `charts/flare-operator/**` (workers-vk templates, values, schema, README), `test/chart/**`, `test/e2e/**` (workers logs), `README.md` / `SECURITY.md` / `docs/operations.md` / `docs/known-issues.md` sections | `Builder`, `StatusMapper`, `PodName`, the stand-in Reconciler; chart Deployment, ServiceAccount, RBAC, NetworkPolicy and values; e2e on k0s; user docs including the security trade-offs in §10 |

## 13. Open risks and UNVERIFIED

1. virtual-kubelet v1.14.0 pins k8s.io 0.36.4. A scratch build against our 0.37.1
   (`nodeutil`, `node`, `node/api`, `dynamiccertificates`) compiles (2026-10-04); runtime behavior
   on 0.37 is untested until B's envtest.
2. Managed clusters: whether their signers issue kubelet-serving certificates to a non-node
   requester, and whether their control planes reach Pod IPs (§3, §4.2). k0s is the only verified
   target.
3. The self-approval privilege (§10).
4. Immediate deletion with `terminationGracePeriodSeconds: 0` (B3), and the Node →
   ClusterRole ownerReference GC path (e2e).
5. No recorded trace-v1 frame: the frame shape is SOURCED from wrangler only. Telemetry shapes of
   non-fetch triggers and of object arguments to `console.log` have no recording, so the
   formatter must tolerate unknown fields.
6. The permission group for `telemetry/query` (spec: Observability *Write*).
7. The `-f` gap (§7.3). A later fix could re-query the gap window after the lag and drop
   duplicates by request.
8. The tail's lifetime (about 6 h by `expires_at`) and behavior past it; the 10-viewer limit
   across processes (wrangler users of the same Worker count too).

## 14. Evidence index

| Claim | Evidence |
|---|---|
| Query and paging shapes, 2000 limit, newest first, `$metadata.id` ordering | recordings 0054, 0093, 0096, 0098 (`test/recordings/2026-09-29/`) |
| Tail create/list/delete, `expires_at`, body `{}` | 0064, 0073, 0074, 0075 |
| Ingestion lag 15–30 s; tail URL survives DELETE; legacy tail stable 250 s, about 90 ms delivery | [spike-results-2026-09-29.md §1](spike-results-2026-09-29.md) |
| trace-v1, `{"debug":false}` on open, 10 s ping, reconnect back-off, frame fields, event kinds | SOURCED: cloudflare/workers-sdk@wrangler@4.143.0 `packages/wrangler/src/tail/{createTail,index,printing}.ts` (read in the published `wrangler-dist/cli.js` of the pinned differential wrangler) |
| Token groups | pinned spec `x-api-token-group`: telemetry `Workers Observability Write`; tails `Workers Tail Read`, `Workers Scripts Write` |
| Retention, 200k/day, 256 KB truncation, head sampling | DOCS: https://developers.cloudflare.com/workers/observability/logs/workers-logs/ |
| 10 tail viewers, sampling mode | DOCS: https://developers.cloudflare.com/workers/observability/logs/real-time-logs/ |
| apiserver flags, certificates, node addresses, DaemonSet selectors on k0s | observed on a local Lima k0s VM, k0s v1.36.4+k0s.1, 2026-10-04 (§3, §5.1) |
| k0s CSR approver rules | k0sproject/k0s@v1.36.4+k0s.1 `pkg/component/controller/csrapprover.go` |
| virtual-kubelet behavior | github.com/virtual-kubelet/virtual-kubelet@v1.14.0 `node/nodeutil/{auth,controller,tls,provider}.go`, `node/api/logs.go`, `node/node.go` |
