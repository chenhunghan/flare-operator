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
`spec.baseURL: http://<fullname>-flarefake.<ns>.svc:8787/client/v4` in a CloudflareAccount, where
`<fullname>` is `<release>-flare-operator`, or just `<release>` when the release name already
contains `flare-operator` (release `flare-operator` in `flare-system`:
`http://flare-operator-flarefake.flare-system.svc:8787/client/v4`). The install NOTES print the
exact value. Never enable it in a cluster that
manages a real Cloudflare account.

`make e2e-images e2e-install e2e e2e-uninstall` builds the images (plus the cloudflared stand-in
`test/e2e/cloudflared-stub`), installs this chart with flarefake, runs `test/e2e` and removes the
release, its CRDs and the namespace again. The images are loaded into the node's container
runtime with `E2E_IMAGE_LOAD` and removed again by `e2e-uninstall` with `E2E_IMAGE_REMOVE` (for
k0s: `sudo k0s ctr -n k8s.io images rm`); see the Makefile and `test/e2e/doc.go`.

## Values

See `values.yaml`. Common ones: `image.*`, `clusterName`, `ownershipTags`, `controllers`,
`baseURLOverride.{allowAny,allowed}`, `leaderElection.enabled`, `replicas`, `metrics.*`,
`resources`, `nodeSelector`, `tolerations`, `affinity`, `extraArgs`, `extraEnv`,
`rbac.aggregateToDefaultRoles`, `flarefake.*`.
