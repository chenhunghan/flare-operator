# Spike results: 2026-09-29

These spikes ran against the user's own Cloudflare account, which is on the **Free plan**. Four spikes ran; roughly 330 API calls were made in total, well within the 1,200 per 5 minutes rate limit. The raw recordings are sanitized in `test/recordings/2026-09-29/`, with account ID, subdomain, email, IP and geolocation replaced and secrets redacted.

**Cleanup.** Everything we created was deleted and checked against the baseline inventory taken beforehand.
- **Exception:** the `default` virtual network, which Cloudflare created automatically when the first tunnel was made. It returned `400/1049` ("Cannot delete the Virtual Network because: it is the default virtual network"). It is free and empty, and left in place.
- **Left untouched:** the account's pre-existing alert policy.

**Not possible on this account:** Containers need the Workers Paid plan (`401`, "Deploying containers requires the Workers Paid plan"), so spikes S2 (exec), S4 (application scale) and S5 (Job exits), plus the container half of S1 (logs), are still open. R2 needs to be enabled in the dashboard (`403/10042`), and Pipelines returns `403/100`.

## 1. Worker logs, i.e. `kubectl logs` (S1 for Workers)

| kubectl | Mechanism | Works |
|---|---|---|
| `logs` | `POST /workers/observability/telemetry/query`, `view:"events"`, filter `$metadata.service eq <script>` | ✓ |
| `--since` | `timeframe.from/to` (ms). A 30-day window is accepted; how long the Free plan actually retains logs is unverified | ✓ |
| `--tail=N` | `limit:N` (maximum 2000; more returns a 400 `too_big`). Results are newest first, so the client reverses them | ✓ |
| more than 2000 lines | `offset:<last $metadata.id>` with `offsetDirection:"next"` | ✓ |
| `--timestamps` | `timestamp` in ms. It's frozen for the length of a request, so lines within one request are ordered by the `$metadata.id` suffix | partly |
| `-f` | **Legacy tail**: `POST /workers/scripts/{n}/tails` returns a `wss://` URL (subprotocol `trace-v1`, binary JSON frames, about 90 ms delivery, stayed up 250 s with no heartbeat). The newer `telemetry/live-tail` works, but sessions dropped after 95–163 s and each URL can only be used once | ✓ (legacy) |
| `-p` / `--previous` | filter `$workers.scriptVersion.id` | ✓ |
| `-c <container>` | no equivalent | ✗ |

- **Ingestion lag:** 15–30 s from request to queryable. So `kubectl logs` shows the recent past, and `-f` needs the tail.
- **Cost:** one `kubectl logs` is 1 API request (plus 1 per 2000-line page). A `-f` session is 2 requests (create + delete), with nothing charged while streaming.
- **Security:** tail and live-tail URLs are **capability URLs**, meaning anyone holding the URL can read the logs, with no other auth. They must be proxied by the virtual kubelet and never handed to users. **Deleting a tail doesn't disconnect existing WebSocket clients**: the URL still upgraded 7 minutes after DELETE. Close connections on our side.
- **Worker upload** implicitly creates 1 version plus a 100% deployment. The `deployment_id` in the upload response is actually the version ID with the dashes removed.
- **The `cf` CLI** has no tail command, only `observability telemetry {query,keys,values}`.

## 2. Private backend: Worker → Workers VPC → Tunnel → cloudflared

**It works end to end.** `env.PRIVATE.fetch()` reached a service listening on `127.0.0.1:18080` behind `cloudflared`, taking about 318 ms inside the Worker (Taiwan). `cloudflared` connected in about 1 s.

- **Hostnames resolve on the `cloudflared` side.** `host.hostname` combined with `resolver_network.resolver_ips` sends DNS queries to the resolver you specify: a local DNS server received the `*.svc.cluster.local` A and AAAA queries, and the request returned 200. *(Corrected by §2.1: without `resolver_ips`, `cloudflared` uses its **host's own resolver**, not a fixed 1.1.1.1/8.8.8.8. On the laptop that resolver didn't know the test names, hence `dns_error`.)*
- **Tunnel ingress rules and `warp-routing` don't limit VPC traffic**, and the host and port in the Worker's fetch URL are ignored (the service's configured host and port always win). Isolation has to come from a NetworkPolicy on the `cloudflared` pod.
- A `tcp` service with `app_protocol: postgresql` was accepted on the Free plan.
- **Runtime errors** the Worker can see: `dns_error`, `destination_unavailable` (tunnel down or deleted), `target_not_found` (service deleted), `port_not_open`.

### 2.1 `cloudflared` inside Kubernetes (k0s v1.36.4, arm64, kube-router CNI with NetworkPolicy enforced)

**It works.** A Worker reached `marker.flare-spike.svc.cluster.local:80` through Workers VPC → Tunnel → a `cloudflared` pod. Median latency was 314 ms inside the Worker (range 301–321 ms) and 732 ms client to client. The pod was Ready in 9 s including the image pull, with 4 QUIC connections within 3 s. It used 4–11m CPU and about 19 Mi of memory.

| Variant | Service host | Result |
|---|---|---|
| A | FQDN with `resolver_ips: [kube-dns 10.96.0.10]` | **200** |
| no resolver_ips | FQDN | **200**: `cloudflared` uses the pod's `/etc/resolv.conf` (kube-dns) |
| short names | `marker.flare-spike` / `marker` (with or without resolver_ips) | `dns_error`: **search domains never apply, so always send FQDNs** |
| ClusterIP | `ipv4: <Service ClusterIP>` | **200** |
| pod IP | `ipv4: <pod IP>` | **200** |

**A NetworkPolicy on the `cloudflared` pod is a real isolation boundary.** A Service not allowed by the policy fails in the Worker with `connection_refused`, and `cloudflared` logs `unable to dial tcp to origin … originService=warp-routing`. The minimal egress policy needs three rules:
- DNS to kube-dns (UDP/TCP 53). The tunnel itself needs DNS: it discovers the edge through an SRV lookup, and without DNS `cloudflared` crash-loops.
- The allowed backend pods, by pod selector.
- `0.0.0.0/0` except the pod and service CIDRs, on TCP/UDP **7844** (the Cloudflare edge). **Port 443 is not needed**; the api.cloudflare.com pre-check fails softly.

**Use pod/namespace selectors, not ClusterIP `ipBlock`s.** kube-router evaluates the policy after the Service address has been translated to a pod address, so an `ipBlock` for a ClusterIP breaks both DNS and the backends. Keep the readiness probe on `/ready` (port 2000): during a bad rollout it kept the old pod serving. `/metrics` exposes `cloudflared_tunnel_ha_connections` and other counters.

**Hyperdrive through a VPC service (stretch goal).** Hyperdrive `origin.service_id` pointing at a TCP VPC service for an in-cluster Postgres **tests the database connection through the tunnel when the config is created**:
- `2012` "Databases must be configured to support SSL/TLS."
- `2015` "Failed to connect … TLS handshake failed: cert verification failed" (self-signed certificate)
- `2007` "mtls cannot be used with service_id."
- 200 after setting `tls_settings.cert_verification_mode: disabled` on the VPC service. The first retry still returned 2015; about 45 s later it succeeded, which suggests the change takes time to propagate (UNVERIFIED).
- A GET on a deleted Hyperdrive config returns `404/2006`.

Postgres then showed `application_name=Cloudflare Hyperdrive` connections over TLS 1.3. Querying through Hyperdrive from a Worker is not yet tested.

Also corrected: **a VPC service does echo `tls_settings` when it is set** (0193). The earlier "not echoed" note came from 0052, where the field wasn't sent.

## 3. API behaviors the emulator must reproduce

| Resource | Behavior | Status / code |
|---|---|---|
| all | Creates return **200**, not 201. Envelope is `{result, success, errors[{code,message}], messages}`; some list endpoints add `result_info` (the VPC services list does not). | – |
| all | **`Ratelimit` headers do not track the 5-minute budget**: 130 of 131 responses said `r=1199;t=1` (once `r=1198`, right after another call in the same second). `Ratelimit-Policy: "default";q=1200;w=300`. **KV, D1 and Workflows send no rate-limit headers at all**, and `GET /zones` uses a separate `"list_zones"` policy. The operator must run its own rate limiter. | – |
| KV namespace | duplicate title, on create **or rename** | 400 / 10014 |
|  | not found (messages differ between GET and DELETE) | 404 / 10013 |
|  | list uses `page`/`per_page`, with `result_info{count,page,per_page,total_count,total_pages}`. **Default order is by ID ascending**, confirmed with a second recording (`kv-order-*`); `order=title&direction=asc` also works | – |
|  | legacy `/workers/namespaces/{id}` still answers (end of life 2026-10-15) | 200 |
| D1 | duplicate name | 400 / 7502 |
|  | invalid enum (location hint); the message lists the allowed values | 400 / 7400 |
|  | multiple statements combined with params | 400 / 7400 |
|  | not found | 404 / 7404 |
|  | **each endpoint returns a different shape**: create has `created_in_region`; get has `running_in_region`; list omits `read_replication` and `total_pages` | – |
| Queues | duplicate name | **409** / 11009 |
|  | invalid name (the message includes the regex) | 400 / 11003 |
|  | **PUT is a full replace: settings left out are reset to defaults**; PATCH merges | – |
|  | not found | 404 / 11000 |
| Tunnel | the **create response contains `token` and `credentials_file`** (secrets) | 200 |
|  | config PUT replaces everything and increments `version` | 200 |
|  | config with no catch-all rule, or with empty ingress | 400 / 1056 |
|  | delete while connected | 400 / 1022 |
|  | delete while a VPC service still references it | **200, allowed** (the reference is left dangling) |
|  | GET after delete: **soft delete** (`deleted_at` set) | 200 |
|  | GET config after delete | 404 / 1055 |
|  | **creating the first tunnel auto-creates a `default` virtual network**, which can't be deleted | 400 / 1049 |
| VPC service | IDs are UUIDv7. PUT is a full replace and **resets `created_at`**. `tls_settings` is echoed only when set | 200 |
|  | duplicate name, or unknown tunnel ID | 400 / 5101 |
|  | invalid JSON / missing field | 400 / 5102 |
|  | not found | 404 / 5104 |
|  | delete while a Worker still binds it | **200, allowed** |
| Worker | upload binding an unknown VPC service | 400 / 10180 |
|  | not found | 404 / 10007 |

**What this means for the operator:**
- Cloudflare **does not check dependencies when deleting** (only a connected tunnel is protected), so finalizers must enforce delete order.
- It **does check on create** (a Worker upload fails if a bound VPC service doesn't exist).
- **Never send a partial PUT.**
- Status codes and error codes **differ from product to product**, so the emulator needs a per-product error table.

## 4. `cf --local` as an emulator

**Not usable as a control-plane emulator.** Its "local explorer" is a Miniflare v5 worker whose routes cover only the **data plane**: KV values, R2 objects, workflow instances, plus list endpoints built from Worker bindings. It has no create/get/delete for namespaces, databases, buckets, queues, DNS, tunnels or Workers (`cf kv namespaces create --local` fails with "The local explorer API does not implement POST /storage/kv/namespaces").

**Ideas to take:**
- request validators generated from the real OpenAPI spec
- one shared helper for the response envelope
- its error-code table (e.g. `10001` validation, `10009` KV key not found)

It could optionally serve later as a check for KV and R2 data-plane behavior.

## 5. Still open

| Spike | Status |
|---|---|
| S1: container logs | Blocked on Workers Paid. The Worker log mechanism is known, so we need to confirm container stdout lands in the same telemetry dataset. |
| S2: exec | Blocked on Workers Paid |
| S3: router Worker → container app | Blocked on Workers Paid. The VPC and Tunnel path is proven. |
| S4: app create latency and limits | Blocked on Workers Paid |
| S5: Job exit detection | Blocked on Workers Paid |
| S6: image mirroring | Blocked on Workers Paid |
| `cloudflared` inside a real cluster, using kube-dns as `resolver_ips` | **Done**: see §2.1 |
