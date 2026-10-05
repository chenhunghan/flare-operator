# Virtual kubelet: making Cloudflare compute look like Pods

Status: **draft, 2026-09-29**. Sections marked 🔬 depend on the spikes listed in §9.

## 1. Goal

Users should be able to run Cloudflare compute with ordinary Kubernetes workload objects:

```sh
kubectl create deployment api --image=docker.io/acme/api:1.4 --replicas=3
kubectl patch deployment api -p '{"spec":{"template":{"spec":{"nodeSelector":{"type":"virtual-kubelet"},"tolerations":[{"key":"virtual-kubelet.io/provider","value":"cloudflare","effect":"NoSchedule"}]}}}}'
kubectl get pods -o wide        # NODE = cf-enam, status from Cloudflare
kubectl logs -f api-7c9d-xk2  # 🔬
kubectl exec -it api-7c9d-xk2 -- sh   # 🔬
```

**Scope:**

- **Pods are the interface for compute** (Cloudflare Containers).
- **Every other Cloudflare resource stays a CRD**: R2, DNS, Access and the rest are generated from OpenAPI, as described in the service catalog.
- **The virtual kubelet is one more component** of the standalone flare-operator.

## 2. Architecture

```
                 ┌──────────────── flare-operator (one Helm chart) ─────────────────┐
kube-apiserver ◄─┤ generated CRD controllers (R2, DNS, Access, …)                     │
   ▲   ▲         │ virtual-kubelet: nodes cf-enam, cf-weur, …  ── PodLifecycleHandler ─┼─► Containers API
   │   │         │ admission webhook (rejects Pod features CF can't honour)            │   Workers observability 🔬
   │   │         │ front-door controller (Service/HTTPRoute → generated Worker) 🔬     │   SSH/exec transport 🔬
   │   │         └─────────────────────────────────────────────────────────────────────┘
   │   └── kubectl logs / exec / port-forward → kubelet API on the virtual node → provider
   └────── Deployment / ReplicaSet / StatefulSet / Job controllers work unchanged
```

- **Library:** [`github.com/virtual-kubelet/virtual-kubelet`](https://github.com/virtual-kubelet/virtual-kubelet) (`node`, `nodeutil`).
- **Prior art:** the Azure Container Instances provider and interLink. The AWS Fargate provider is archived, and its known issues (pod networking, logs) apply here too.
- **Shared plumbing:** uses the same Cloudflare client, rate limiter and tagging as the CRD controllers.

## 3. Virtual nodes

- **One node per Cloudflare location region.** Instances report `location.region`, e.g. ENAM, WNAM, EEUR, APAC, AFR, ME. Topology spread and `nodeSelector` then work natively and translate to `constraints.regions`. There's also an optional catch-all node `cf-global` with no region constraint.
- **Labels:** `type=virtual-kubelet`, `kubernetes.io/arch=amd64` (Containers are amd64-only), `kubernetes.io/os=linux`, `topology.kubernetes.io/region=<cf-region>`, `flare.dev/jurisdiction=<eu|us|fedramp>` (optional).
- **Taint:** `virtual-kubelet.io/provider=cloudflare:NoSchedule`. Nothing lands on Cloudflare by accident.
- **Capacity:** taken from the account limits (1,500 vCPU, 6 TiB memory, 30 TB disk concurrently) and shared between the nodes. Node conditions report Ready only while the API is reachable and the token is valid.

## 4. Mapping a Pod to a Container application

**Decision: 1 Pod = 1 scheduler-backed Container application with `instances: 1`.**

The reason is that the Containers API can't delete or restart a *specific* instance; it can only change the instance count or run a rollout. But Kubernetes controllers choose *which* pod to remove (ReplicaSet scale-down, StatefulSet ordinals, evictions). With one application per Pod, both sides agree on identity. Deployment rolling updates then happen natively: a new ReplicaSet creates new Pods, which create new applications.

| Pod event | Provider action |
|---|---|
| CreatePod | Resolve env vars from ConfigMaps/Secrets, as kubelet does. Ensure registry credentials exist (`/containers/registries/{domain}/credentials`). `POST /containers/applications` with `scheduling_policy: default`, `instances: 1`, `max_instances: 1`, and tags `flare.dev/pod-uid`, `flare.dev/cluster`. |
| UpdatePod (image change, the only mutable container field) | `POST …/rollouts` with `strategy: new_instances`, `step_percentage: 100` |
| DeletePod | `DELETE /containers/applications/{id}` after `terminationGracePeriodSeconds`, which maps to `rollout_active_grace_period` where it applies |
| GetPodStatus / NotifyPods | One `instances-v2` LIST per application, batched and rate-limited, with a watch-style cache. The provider pushes status changes to Kubernetes. |

Application name: `<cluster>-<ns>-<pod>-<uid[:8]>`, truncated. The Cloudflare application ID is also written back as the Pod annotation `flare.dev/application-id`.

**Open cost questions** 🔬: how long an application takes to create, and whether many small applications per account hit an undocumented limit. The docs list no maximum.

### 4.1 Translating the Pod spec

| Pod field | Cloudflare | Notes |
|---|---|---|
| `containers[0].image` | `configuration.image` | Registries allowed: registry.cloudflare.com, Docker Hub, ECR, Google Artifact Registry. **ghcr.io and quay.io are not supported**, so they're rejected by the webhook, or mirrored to registry.cloudflare.com as an opt-in 🔬. |
| `command` / `args` | `entrypoint` / `command` | Kubernetes `command` overrides the ENTRYPOINT and `args` overrides CMD. |
| `env`, `envFrom` | `environment_variables` | ⚠ Values are stored **in plaintext** in the application config. The webhook warns when a Secret is referenced. |
| `resources.requests/limits` | `instance_type` | Smallest predefined type that fits (lite, basic, standard-1…4), otherwise a custom type (1–4 vCPU, ≤12 GiB, ≥3 GiB per vCPU). |
| `nodeSelector` / affinity / topology spread (region) | `constraints.regions` | Follows from the virtual node the Pod was scheduled to. |
| `imagePullSecrets` | registry credentials | |
| `restartPolicy: Always` | scheduler keeps 1 instance | |
| `restartPolicy: Never/OnFailure` (Jobs) | on `exit_code`, delete the application and set the Pod phase to Succeeded/Failed | The scheduler restarts exited instances, so there's a race 🔬. |
| `ports`, probes | none | There's no API field. Ports are used by the front door (§6). Readiness follows the instance `state`. |
| volumes | none | Only `emptyDir` is accepted (the container disk is ephemeral). An R2 FUSE mount via annotation may come later. |
| multiple containers, init containers, sidecars | none | Rejected by the webhook. |
| `hostNetwork`, `privileged`, `securityContext` capabilities | none | Rejected by the webhook. |

### 4.2 Translating status

| Instance `status.state` | Pod phase / container state |
|---|---|
| provisioning | Pending / Waiting (ContainerCreating) |
| running | Running / Running (Ready=True) |
| unhealthy | Running / Running (Ready=False) |
| stopping | Running, with a deletionTimestamp |
| stopped, `exit_code`=0 | Succeeded / Terminated(0) |
| failed or stopped, `exit_code`≠0 | Failed or CrashLoopBackOff / Terminated(code) |
| inactive, unknown | Unknown |

`startTime` comes from `started_at`. `status.hostIP` and `podIP` are empty (there's no routable IP). The resources come from `configuration.{vcpu,memory,disk}`, and the location is added as the annotation `flare.dev/location`.

## 5. kubectl support

| Command | Status | Mechanism |
|---|---|---|
| get, describe, delete, events, rollout, scale | ✓ | Kubernetes itself plus provider status |
| logs, `logs -f` | 🔬 | Workers observability `telemetry/query` / live-tail filtered by application or instance (the docs say logs are "automatically set up"; the query dimensions are unverified) |
| exec, attach | 🔬 | `wrangler containers ssh` exists, using `authorized_keys` and `wrangler_ssh` in the app config, but its transport isn't in the public REST spec. Fallback: a DO-mode helper that uses `ctx.container.exec()` |
| port-forward | 🔬 | Through the front-door Worker, or SSH forwarding |
| top | 🔬 | Containers metrics through GraphQL analytics (unverified) |
| cp | ✗ (v0) | Would need exec to work |

## 6. Networking

- **Inbound:** Cloudflare sends traffic to a container **only through a Worker or Durable Object**. The container is never directly exposed. The front-door controller watches Services of type LoadBalancer and HTTPRoutes whose selectors match Cloudflare Pods. For each one it generates:
  - a router Worker
  - a DO or application association that forwards requests to the selected Pods' applications on the Service `targetPort`
  - a Worker route or custom domain for the hostname

  Load balancing across the selected Pods happens in the router Worker. 🔬: how to address a scheduler-backed application from a Worker.
- **Inside the cluster:** ClusterIP Services that select Cloudflare Pods have no endpoints, because the Pods have no IP. The webhook warns about this. Workloads in the cluster reach Cloudflare Pods through the front-door hostname.
- **Outbound:** the public internet works. Reaching cluster or private services requires a Tunnel plus Workers VPC or Mesh, if Containers support it 🔬.

## 7. Admission webhook

A validating webhook applies only to Pods that tolerate `virtual-kubelet.io/provider=cloudflare`.

- **Rejects:** unsupported registries, more than one container, init containers or sidecars, volumes other than emptyDir, hostNetwork, privileged mode, non-amd64 image manifests (checked through the registry when possible), and resource requests larger than standard-4 or a custom type.
- **Warns:** Secret env vars (stored in plaintext), ClusterIP Services with no endpoints, Jobs (restart race).

## 8. How it relates to the `ContainerApp` CRD

- **`ContainerApp` (CRD):** Cloudflare's own model. Multi-instance applications with Cloudflare-managed rollouts, DO mode, and fine-grained options.
- **Pods on virtual nodes:** the familiar path, with one application per Pod.

Applications created by the virtual kubelet carry the tag `flare.dev/managed-by=virtual-kubelet`, and the CRD controllers ignore them.

Workers and Workflows aren't Pods in v0. For Workers, read-only stand-in Pods on a separate virtual node give `kubectl logs` without Containers (Free plan): see [workers-logs-design.md](workers-logs-design.md).

## 9. Spikes before committing

| # | Question | Pass criteria |
|---|---|---|
| S1 | Can container stdout/stderr be queried, and live-tailed, per application or instance through the observability API? | `kubectl logs -f` works end to end |
| S2 | Can we drive the SSH/exec transport used by `wrangler containers ssh` programmatically from Go? | `kubectl exec -it … -- sh` works |
| S3 | How does a Worker route HTTP to a scheduler-backed application, and to a specific one? | A generated router Worker serves a Pod on its `targetPort` |
| S4 | How long does creating and deleting an application take, and is there a limit on applications per account? | Under 60 s to Running; 200 applications without errors |
| S5 | Can a Job-style exit be detected before the scheduler restarts the instance? | Pod reaches Succeeded exactly once |
| S6 | Can images from unsupported registries be mirrored through `/containers/image-preparations` or a registry.cloudflare.com push? | ghcr.io image runs |

Spikes S1–S3 decide whether the design holds. If S2 fails, exec is dropped from v0. If S3 fails, the front door falls back to DO-mode applications, with one DO class per Service.

**Results from 2026-09-29** (see [spike-results-2026-09-29.md](spike-results-2026-09-29.md)):

- **Worker logs are solved.** `logs`, `--since`, `--tail` and `-p` work through `telemetry/query`, with 15–30 s ingestion lag. `-f` works through the legacy tail WebSocket, which must be proxied because anyone holding its URL can read the logs. The design that turns this into `kubectl logs` for WorkerScripts is [workers-logs-design.md](workers-logs-design.md).
- **The private-backend path is proven.** Worker → Workers VPC → Tunnel → `cloudflared` works, including `*.svc.cluster.local` hostnames resolved on the `cloudflared` side when `resolver_ips` is set.
- **All container spikes (S1 for containers, S2–S6) are blocked** because the test account is on the Free plan, and Containers requires Workers Paid.

## 10. Packaging

The virtual kubelet has to run as its own long-lived process with node leases, so flare-operator ships as a **standalone operator in a single Helm chart**. The chart contains the generated CRD controllers, the virtual kubelet, the webhook and the front-door controller, and it's paired with the `kubectl flare` plugin.

Crossplane conventions are kept: `providerConfigRef`, `deletionPolicy`, `managementPolicies`, `writeConnectionSecretToRef`, and `Synced`/`Ready` conditions. A Crossplane-packaged build of the CRD controllers can come later from the same code base.
