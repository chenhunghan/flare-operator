# flare-operator Helm chart

Installs the flare-operator manager, its CRDs (`crds/`), RBAC, and a metrics Service.

```sh
make docker-build docker-build-fake          # flare-operator:dev, flarefake:dev
helm install flare-operator charts/flare-operator -n flare-system --create-namespace \
  --set image.tag=dev --set clusterName=<unique-cluster-name> \
  --set reconcile.pollInterval=10m --set reconcile.maxConcurrentReconciles=2
```

The `reconcile.*` values are optional; the defaults are in the table below, and
[docs/operations.md](../../docs/operations.md#reconcile-tuning-and-the-api-budget) explains how
to size them.

## Generated files (do not edit)

`crds/*.yaml`, `templates/clusterrole-manager.yaml` and `templates/clusterrole-aggregate.yaml`
are produced by `make chart-sync` from `config/crd/bases` and `config/rbac/role.yaml`
(controller-gen / flaregen output). `make chart-check` fails when they are stale; run
`make manifests generate-crds chart-sync` after changing API types or RBAC markers.

Helm installs `crds/` on first install only and never upgrades or deletes them. Before every
`helm upgrade`, run `make crds-apply`
(`kubectl apply --server-side --force-conflicts --field-manager=flare-operator-crds -f charts/flare-operator/crds/`).
[docs/operations.md](../../docs/operations.md#upgrade) explains why this is a documented step
rather than a hook Job. `make e2e-upgrade` tests the upgrade path from a previous git ref.

## e2e with flarefake

`--set flarefake.enabled=true --set flarefake.image.tag=dev` (or `-f ci/flarefake-values.yaml`)
runs the Cloudflare API emulator as a Deployment and Service in the release namespace and passes
the manager `--allowed-base-url` for that Service's URL only, in three spellings
(`<svc>`, `<svc>.<ns>.svc`, `<svc>.<ns>.svc.<clusterDomain>`). Use
`spec.baseURL: http://<fullname>-flarefake.<ns>.svc:8787/client/v4` in a CloudflareAccount, where
`<fullname>` is `<release>-flare-operator`, or just `<release>` when the release name already
contains `flare-operator` (release `flare-operator` in `flare-system`:
`http://flare-operator-flarefake.flare-system.svc:8787/client/v4`). The install NOTES print the
exact value. Never enable it in a cluster that manages a real Cloudflare account.

`make e2e-images e2e-install e2e e2e-uninstall` builds the images (plus the cloudflared stand-in
`test/e2e/cloudflared-stub`), installs this chart with flarefake, runs `test/e2e` and removes the
release, its CRDs and the namespace again. The images are loaded into the node's container
runtime with `E2E_IMAGE_LOAD` and removed again by `e2e-uninstall` with `E2E_IMAGE_REMOVE` (for
k0s: `sudo k0s ctr -n k8s.io images rm`); see the Makefile and `test/e2e/doc.go`.

## Values

`values.schema.json` validates the values: an unknown key or a wrong type fails `helm install`,
`upgrade`, `template` and `lint`. `go test ./test/chart/` (part of `make test`) checks that the
schema rejects typos. Its `TestValuesTableMatchesChart` checks the table below against
`values.yaml` and `values.schema.json`:

- every key in `values.yaml` has a row (a row for an object covers its fields);
- every row names a key of both files;
- the Type column is the schema's type;
- a Default written as code is the `values.yaml` default.

When you change `values.yaml`, update the schema and this table in the same change. The
toolchain has no helm-docs generator; the test takes its place.

<!-- values-table:begin -->
| Key | Type | Default | Description |
|---|---|---|---|
| `nameOverride` | string | `""` | Override the chart name used in resource names. |
| `fullnameOverride` | string | `""` | Override the full resource name prefix (default: `<release>-<chart>`, or `<release>` if it already contains the chart name). |
| `image.repository` | string | `"flare-operator"` | Manager image repository. Build it with `make docker-build`. |
| `image.tag` | string | `""` | Manager image tag (empty: the chart `appVersion`). |
| `image.pullPolicy` | string | `"IfNotPresent"` | `Always`, `IfNotPresent` or `Never`. |
| `imagePullSecrets` | array | `[]` | Pull secrets for the manager and flarefake pods. |
| `replicas` | integer | `1` | Manager replicas. More than one requires `leaderElection.enabled`; only the leader reconciles. |
| `leaderElection.enabled` | boolean | `true` | `--leader-elect`. The Lease lives in the release namespace (`--leader-election-namespace`). |
| `clusterName` | string | `""` | `--cluster-name`: the cluster identity in ownership tags (`flare.dev/owner=<clusterName>/<ns>/<name>`). Required: the install fails without it. Give every cluster that manages the same Cloudflare account a distinct name. |
| `ownershipTags` | boolean | `true` | `--ownership-tags`: tag managed Cloudflare resources through Resource Tagging. |
| `userAgent` | string | `""` | `--user-agent` for Cloudflare API calls (empty: the binary default, `flare-operator`). |
| `controllers` | array | `[]` | `--controller`, once per entry: run only these controllers. Empty runs all of them. |
| `baseURLOverride.allowAny` | boolean | `false` | `--allow-base-url-override`: honour any CloudflareAccount `spec.baseURL`. Test clusters only: an override sends the account's token to that URL. |
| `baseURLOverride.allowed` | array | `[]` | `--allowed-base-url`, once per entry: honour exactly these `spec.baseURL` values. |
| `reconcile.pollInterval` | string | `""` | `--poll-interval`, a Go duration, minimum `10s`. Empty keeps the controller defaults (5m for generated kinds, 10m for Tunnel, VPCService and WorkerScript). |
| `reconcile.maxConcurrentReconciles` | integer | `1` | `--max-concurrent-reconciles`: parallel reconciles per controller. All workers of a token share its rate limit. |
| `reconcile.timeout` | string | `"5m"` | `--reconcile-timeout`: context deadline of one reconcile. `"0"` (quoted, or `--set-string`) or `0s` disables it; empty keeps the manager default (5m). |
| `reconcile.cloudflareRequestTimeout` | string | `"60s"` | `--cloudflare-request-timeout`: timeout of one Cloudflare API HTTP request. |
| `artifacts.maxBytes` | string | `"64Mi"` | `--artifact-max-bytes`: largest total size of one artifact's files (ConfigMaps, OCI image, archive), a quantity. The tree is held in memory. |
| `artifacts.maxFiles` | integer | `20000` | `--artifact-max-files`: most files in one artifact. |
| `artifacts.maxArchiveBytes` | string | `"64Mi"` | `--artifact-max-archive-bytes`: largest download of one artifact (a url archive, or the sum of an image's compressed layers). |
| `artifacts.maxExpandedBytes` | string | `"256Mi"` | `--artifact-max-expanded-bytes`: most bytes decompressed from one archive or image, entries outside the selected path included (zip/tar bomb limit). |
| `artifacts.maxCompressionRatio` | integer | `100` | `--artifact-max-compression-ratio`: largest decompressed/compressed ratio, checked after the first 1Mi. |
| `artifacts.cacheBytes` | string | `"128Mi"` | `--artifact-cache-bytes`: memory for cached OCI and url artifacts, by digest; `"0"` (quoted, or `--set-string`) disables the cache. Byte sizes are quantity strings; empty keeps the manager default. |
| `artifacts.allowedCIDRs` | array | `[]` | `--artifact-allowed-cidr`, once per entry: non-public ranges url and ociRef sources may connect to (e.g. an in-cluster registry). Loopback, private, link-local (cloud metadata) and other special-purpose addresses are refused otherwise. |
| `logging.level` | string or integer | `"info"` | `--zap-log-level`: `debug`, `info`, `error`, `panic`, or an integer verbosity. |
| `logging.encoder` | string | `"json"` | `--zap-encoder`: `json` or `console`. |
| `extraArgs` | array | `[]` | Extra manager command-line arguments. |
| `extraEnv` | array | `[]` | Extra environment variables (EnvVar list) for the manager container. |
| `metrics.enabled` | boolean | `true` | Serve Prometheus metrics on `metrics.port` (`--metrics-bind-address`; `false` passes `0`). Plain HTTP without authentication; restrict it with `networkPolicy.metricsFrom`. |
| `metrics.port` | integer | `8080` | Metrics port. |
| `metrics.service.enabled` | boolean | `true` | A ClusterIP Service `<fullname>-metrics` for the metrics port. |
| `metrics.service.annotations` | object | `{}` | Annotations of the metrics Service. |
| `metrics.serviceMonitor.enabled` | boolean | `false` | A prometheus-operator ServiceMonitor. Needs the `monitoring.coreos.com/v1` CRDs, `metrics.enabled` and `metrics.service.enabled`. |
| `metrics.serviceMonitor.labels` | object | `{}` | Extra labels, e.g. the label your Prometheus selects ServiceMonitors by. |
| `metrics.serviceMonitor.annotations` | object | `{}` | Annotations of the ServiceMonitor. |
| `metrics.serviceMonitor.interval` | string | `""` | Scrape interval (empty: Prometheus' default). |
| `metrics.serviceMonitor.scrapeTimeout` | string | `""` | Scrape timeout (empty: Prometheus' default). |
| `metrics.serviceMonitor.relabelings` | array | `[]` | ServiceMonitor `relabelings`. |
| `metrics.serviceMonitor.metricRelabelings` | array | `[]` | ServiceMonitor `metricRelabelings`. |
| `probes.port` | integer | `8081` | `/healthz` and `/readyz` port (`--health-probe-bind-address`). |
| `serviceAccount.create` | boolean | `true` | Create the manager's ServiceAccount. |
| `serviceAccount.name` | string | `""` | ServiceAccount name (empty: the full name). |
| `serviceAccount.annotations` | object | `{}` | Annotations of the ServiceAccount. |
| `rbac.create` | boolean | `true` | Create the manager ClusterRole and binding, and the leader-election Role and binding. |
| `rbac.aggregateToDefaultRoles` | boolean | `true` | ClusterRoles that aggregate the flare-operator kinds into the default `view`, `edit` and `admin` roles. |
| `podAnnotations` | object | `{}` | Annotations of the manager pods. |
| `podLabels` | object | `{}` | Labels of the manager pods. |
| `priorityClassName` | string | `""` | PriorityClass of the manager pods, so the operator is not preempted before your workloads. |
| `topologySpreadConstraints` | array | `[]` | topologySpreadConstraints of the manager pods. An entry without `labelSelector` gets the manager's selector labels. |
| `podDisruptionBudget.enabled` | boolean | `true` | Render a PodDisruptionBudget, but only when `replicas` > 1 (with one replica it would block every node drain). |
| `podDisruptionBudget.minAvailable` | integer or string | `1` | `minAvailable` (integer or percentage). Ignored when `maxUnavailable` is set. |
| `podDisruptionBudget.maxUnavailable` | integer or string | `""` | `maxUnavailable` (integer or percentage); empty uses `minAvailable`. |
| `networkPolicy.enabled` | boolean | `false` | A NetworkPolicy for the manager pods ([docs/operations.md](../../docs/operations.md#network-policy)). |
| `networkPolicy.apiServer.cidrs` | array | `[]` | **Required when enabled**: the API server's endpoint addresses (not the `kubernetes` Service ClusterIP), e.g. `["192.168.5.15/32"]`. |
| `networkPolicy.apiServer.ports` | array | `[6443]` | API server ports. |
| `networkPolicy.cloudflareAPI.cidrs` | array | Cloudflare's published IPv4 and IPv6 ranges (22 CIDRs, copied 2026-09-29) | HTTPS egress for `api.cloudflare.com`. Set `[]` and use your CNI's FQDN policy for a tighter rule. |
| `networkPolicy.cloudflareAPI.ports` | array | `[443]` | Cloudflare API ports. |
| `networkPolicy.dns.namespaceSelector` | object | `{"kubernetes.io/metadata.name": "kube-system"}` | Namespace labels of the cluster DNS pods (UDP and TCP 53). |
| `networkPolicy.dns.podSelector` | object | `{"k8s-app": "kube-dns"}` | Labels of the cluster DNS pods. |
| `networkPolicy.metricsFrom` | array | `[]` | NetworkPolicyPeers allowed to scrape the metrics port; empty allows any source. |
| `networkPolicy.extraEgress` | array | `[]` | Extra NetworkPolicyEgressRules, e.g. for a `spec.baseURL` override target. |
| `podSecurityContext` | object | `{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}}` | Pod security context of the manager and flarefake pods (restricted Pod Security Standard). |
| `securityContext` | object | `{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "privileged": false, "capabilities": {"drop": ["ALL"]}}` | Container security context of the manager and flarefake containers. |
| `resources` | object | `{"requests": {"cpu": "50m", "memory": "128Mi"}, "limits": {"memory": "512Mi"}}` | Manager resources. The manager caches Secrets cluster-wide (for the token refs); size memory for that. |
| `nodeSelector` | object | `{}` | nodeSelector of the manager pods. |
| `tolerations` | array | `[]` | Tolerations of the manager pods. |
| `affinity` | object | `{}` | Affinity of the manager pods. |
| `clusterDomain` | string | `"cluster.local"` | Cluster DNS domain, used to build the flarefake Service URL. |
| `flarefake.enabled` | boolean | `false` | Run the flarefake emulator as a Deployment and Service, and pass the manager `--allowed-base-url` for its URL only. **Never in a cluster that manages a real Cloudflare account.** |
| `flarefake.image.repository` | string | `"flarefake"` | flarefake image repository. Build it with `make docker-build-fake`. |
| `flarefake.image.tag` | string | `""` | flarefake image tag (empty: the chart `appVersion`). |
| `flarefake.image.pullPolicy` | string | `"IfNotPresent"` | `Always`, `IfNotPresent` or `Never`. |
| `flarefake.port` | integer | `8787` | flarefake port. |
| `flarefake.validateRequests` | boolean | `true` | Validate requests against the pinned OpenAPI spec baked into the image (`-spec`). |
| `flarefake.rejectSchemaViolations` | boolean | `false` | Answer schema-invalid requests with 400 instead of only journaling them (`-reject-schema-violations`). |
| `flarefake.extraArgs` | array | `[]` | Extra flarefake arguments. |
| `flarefake.podAnnotations` | object | `{}` | Annotations of the flarefake pod. |
| `flarefake.resources` | object | `{"requests": {"cpu": "50m", "memory": "384Mi"}, "limits": {"memory": "1Gi"}}` | flarefake resources. The parsed 26 MB spec keeps an idle flarefake at about 320 MiB (measured on linux/arm64). |
| `flarefake.nodeSelector` | object | `{}` | nodeSelector of the flarefake pod. |
| `flarefake.tolerations` | array | `[]` | Tolerations of the flarefake pod. |
| `flarefake.affinity` | object | `{}` | Affinity of the flarefake pod. |
<!-- values-table:end -->

Production options (all off or empty by default):

| Value | Effect |
|---|---|
| `replicas: 2` + `podDisruptionBudget.*` | A standby manager. A PDB (`minAvailable: 1`, or `maxUnavailable`) is rendered only when `replicas > 1`. |
| `topologySpreadConstraints` | Spread the replicas; an entry without `labelSelector` gets the manager's selector. |
| `priorityClassName` | Keep the manager from being preempted. |
| `networkPolicy.enabled` | Manager NetworkPolicy: DNS, `networkPolicy.apiServer.cidrs` (required), HTTPS to `networkPolicy.cloudflareAPI.cidrs` (default: Cloudflare's published ranges; FQDN rules need a CNI that supports them), metrics ingress from `networkPolicy.metricsFrom`. |
| `metrics.serviceMonitor.enabled` | A prometheus-operator ServiceMonitor (needs the monitoring.coreos.com CRDs). |

`make helm-lint` lints the chart with `--strict` and renders every `ci/*-values.yaml`
(`ci/full-values.yaml` turns all of the above on) through kubeconform when it is installed.
`go test ./test/chart/` renders the chart and checks the objects. Both run in `make ci`.
Operating the chart (upgrade, uninstall semantics, troubleshooting) is covered in
[docs/operations.md](../../docs/operations.md).
