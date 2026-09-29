# flare-operator Helm chart

Installs the flare-operator manager, its CRDs (`crds/`), RBAC, and a metrics Service.

```sh
make docker-build docker-build-fake          # flare-operator:dev, flarefake:dev
helm install flare-operator charts/flare-operator -n flare-system --create-namespace \
  --set image.tag=dev --set clusterName=<unique-cluster-name>
```

## Generated files (do not edit)

`crds/*.yaml`, `templates/clusterrole-manager.yaml` and `templates/clusterrole-aggregate.yaml`
are produced by `make chart-sync` from `config/crd/bases` and `config/rbac/role.yaml`
(controller-gen / flaregen output). `make chart-check` fails when they are stale; run
`make manifests generate-crds chart-sync` after changing API types or RBAC markers.

Helm installs `crds/` on first install only and never upgrades or deletes them; apply
`charts/flare-operator/crds/` with `kubectl apply --server-side` when upgrading.

## e2e with flarefake

`--set flarefake.enabled=true --set flarefake.image.tag=dev` (or `-f ci/flarefake-values.yaml`)
runs the Cloudflare API emulator as a Deployment and Service in the release namespace and passes
the manager `--allowed-base-url` for that Service's URL only, in three spellings
(`<svc>`, `<svc>.<ns>.svc`, `<svc>.<ns>.svc.<clusterDomain>`). Use
`spec.baseURL: http://<release>-flare-operator-flarefake.<ns>.svc:8787/client/v4` in a
CloudflareAccount (the install NOTES print the exact value). Never enable it in a cluster that
manages a real Cloudflare account.

## Values

See `values.yaml`. Common ones: `image.*`, `clusterName`, `ownershipTags`, `controllers`,
`baseURLOverride.{allowAny,allowed}`, `leaderElection.enabled`, `replicas`, `metrics.*`,
`resources`, `nodeSelector`, `tolerations`, `affinity`, `extraArgs`, `extraEnv`,
`rbac.aggregateToDefaultRoles`, `flarefake.*`.
