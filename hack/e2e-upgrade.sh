#!/usr/bin/env bash
# Upgrade e2e (make e2e-upgrade): install the chart and manager of a previous git ref, create
# objects, `helm upgrade` to this checkout, check the objects survived without a Cloudflare
# write, then run a smoke subset of TestEndToEnd against the upgraded install. Everything runs
# against flarefake in the chart; nothing talks to the Cloudflare API.
#
# Steps:
#   1. wait (up to E2E_BUSY_WAIT seconds) while E2E_NAMESPACE exists: another e2e run owns it.
#      The check is repeated right before the install (after minutes of image builds), where
#      `kubectl create namespace` claims E2E_NAMESPACE atomically: only a run that created it
#      tears it down
#   2. build the previous ref's manager image (git archive of E2E_UPGRADE_FROM) as
#      flare-operator:E2E_PREV_TAG, and this checkout's images (make e2e-images); load them
#   3. helm install the previous chart (flarefake runs this checkout's flarefake image, so the
#      emulator, which holds its state in memory, is not replaced by the upgrade)
#   4. go test -run TestUpgradePre  (E2E_UPGRADE_PHASE=pre)
#   5. make crds-apply, helm upgrade to this checkout's chart
#   6. go test -run TestUpgradePost (E2E_UPGRADE_PHASE=post), then the TestEndToEnd smoke subset
#   7. always: when this run claimed E2E_NAMESPACE, make e2e-uninstall (release, CRDs,
#      namespace, E2E_IMAGE_REMOVE of the loaded images, plus the previous-ref image). When it
#      did not (it skipped, or failed before the claim), it touches no Kubernetes object: it
#      removes only the previous-ref image, plus the :E2E_TAG images when E2E_NAMESPACE is
#      absent (otherwise another run's pods may use them). The scratch directory always goes
#
# Environment (defaults match the Makefile): MAKE, HELM, KUBECTL, CONTAINER_TOOL, PLATFORM,
# CHART, E2E_NAMESPACE, E2E_RELEASE, E2E_TAG, E2E_PULL_POLICY, E2E_CLUSTER_NAME,
# E2E_IMAGE_LOAD, E2E_IMAGE_REMOVE, E2E_IMAGE_LIST, E2E_LOCAL_RMI (see the Makefile), and
#   E2E_UPGRADE_FROM    git ref to upgrade from (default: the latest tag before HEAD, else the
#                       merge base with main when HEAD is on a branch, else HEAD~1)
#   E2E_PREV_TAG        image tag for the previous manager (e2e-prev)
#   E2E_UPGRADE_SMOKE   TestEndToEnd steps to run after the upgrade (a -run regexp)
#   E2E_BUSY_WAIT       seconds to wait for a busy E2E_NAMESPACE before skipping (900)
#   E2E_UPGRADE_NAMESPACE  namespace of the objects kept across the upgrade (flare-e2e-upgrade)
set -euo pipefail

MAKE=${MAKE:-make}
HELM=${HELM:-helm}
KUBECTL=${KUBECTL:-kubectl}
CONTAINER_TOOL=${CONTAINER_TOOL:-docker}
PLATFORM=${PLATFORM:-linux/$(go env GOARCH)}
CHART=${CHART:-charts/flare-operator}
E2E_NAMESPACE=${E2E_NAMESPACE:-flare-system}
E2E_RELEASE=${E2E_RELEASE:-flare-operator}
E2E_TAG=${E2E_TAG:-e2e}
E2E_PULL_POLICY=${E2E_PULL_POLICY:-Never}
E2E_CLUSTER_NAME=${E2E_CLUSTER_NAME:-flare-e2e}
E2E_IMAGE_LOAD=${E2E_IMAGE_LOAD:-}
E2E_IMAGE_REMOVE=${E2E_IMAGE_REMOVE:-}
E2E_IMAGE_LIST=${E2E_IMAGE_LIST:-}
E2E_LOCAL_RMI=${E2E_LOCAL_RMI:-}
E2E_PREV_TAG=${E2E_PREV_TAG:-e2e-prev}
E2E_UPGRADE_SMOKE=${E2E_UPGRADE_SMOKE:-'^TestEndToEnd$/^(RBAC|Account|CreateResources|Idempotency|Update|ManagerHealth)$'}
E2E_BUSY_WAIT=${E2E_BUSY_WAIT:-900}
E2E_UPGRADE_NAMESPACE=${E2E_UPGRADE_NAMESPACE:-flare-e2e-upgrade}
export E2E_UPGRADE_NAMESPACE

log() { printf '\n=== e2e-upgrade: %s\n' "$*"; }

: "${KUBECONFIG:?KUBECONFIG must point at the test cluster}"

# 1. Another e2e run owns the namespace: wait, then skip.
waited=0
while "$KUBECTL" get namespace "$E2E_NAMESPACE" >/dev/null 2>&1; do
	if [ "$waited" -ge "$E2E_BUSY_WAIT" ]; then
		log "SKIPPED: namespace $E2E_NAMESPACE still exists after ${E2E_BUSY_WAIT}s (another e2e run?)"
		exit 0
	fi
	[ "$waited" -eq 0 ] && log "namespace $E2E_NAMESPACE exists; waiting up to ${E2E_BUSY_WAIT}s"
	sleep 30
	waited=$((waited + 30))
done

head_sha=$(git rev-parse HEAD)
from=${E2E_UPGRADE_FROM:-}
if [ -z "$from" ]; then
	from=$(git describe --tags --abbrev=0 HEAD~1 2>/dev/null || true)
fi
if [ -z "$from" ]; then
	mb=$(git merge-base HEAD main 2>/dev/null || true)
	if [ -n "$mb" ] && [ "$mb" != "$head_sha" ]; then from=$mb; else from=HEAD~1; fi
fi
from_sha=$(git rev-parse --verify "$from^{commit}")
from_desc=$(git describe --tags --always "$from_sha")
log "upgrade $from_desc ($from_sha) -> $(git describe --tags --always --dirty) ($head_sha)"

work=$(mktemp -d "${TMPDIR:-/tmp}/flare-e2e-upgrade.XXXXXX")
prev_img=flare-operator:$E2E_PREV_TAG
# 1 once `kubectl create namespace` below succeeded: this run owns E2E_NAMESPACE, its release
# and the CRDs, and may tear them down.
owned=0

cleanup() {
	rc=$?
	log "cleanup (exit $rc)"
	img_vars=(E2E_TAG="$E2E_TAG" E2E_IMAGE_REMOVE="$E2E_IMAGE_REMOVE" E2E_IMAGE_LIST="$E2E_IMAGE_LIST"
		E2E_LOCAL_RMI="$E2E_LOCAL_RMI")
	if [ "$owned" = 1 ]; then
		"$KUBECTL" delete namespace "$E2E_UPGRADE_NAMESPACE" --ignore-not-found --wait --timeout=3m || true
		"$MAKE" e2e-uninstall E2E_NAMESPACE="$E2E_NAMESPACE" E2E_RELEASE="$E2E_RELEASE" CHART="$CHART" \
			E2E_EXTRA_IMAGES="$prev_img" "${img_vars[@]}" || true
	elif "$KUBECTL" get namespace "$E2E_NAMESPACE" >/dev/null 2>&1; then
		log "namespace $E2E_NAMESPACE belongs to another run: leaving it, its release, the CRDs and the :$E2E_TAG images"
		# Only the previous-ref image is this run's alone.
		"$MAKE" e2e-rmi E2E_REMOVE_IMAGES="$prev_img" "${img_vars[@]}" || true
	else
		log "nothing was installed: removing the images only"
		"$MAKE" e2e-rmi E2E_EXTRA_IMAGES="$prev_img" "${img_vars[@]}" || true
	fi
	# Without E2E_LOCAL_RMI the previous-ref image is still removed locally: nothing else uses it.
	[ -n "$E2E_LOCAL_RMI" ] || "$CONTAINER_TOOL" rmi "$prev_img" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$rc"
}
trap cleanup EXIT

# 2. Images.
mkdir -p "$work/prev"
git archive "$from_sha" | tar -x -C "$work/prev"
log "build $prev_img from $from_desc"
"$CONTAINER_TOOL" build --platform="$PLATFORM" --build-arg VERSION="$from_desc" --build-arg COMMIT="$from_sha" \
	-f "$work/prev/Dockerfile" -t "$prev_img" "$work/prev"
if [ -n "$E2E_IMAGE_LOAD" ]; then
	"$CONTAINER_TOOL" save "$prev_img" | sh -c "$E2E_IMAGE_LOAD"
fi
"$MAKE" e2e-images E2E_TAG="$E2E_TAG" E2E_IMAGE_LOAD="$E2E_IMAGE_LOAD"

helm_args=(-n "$E2E_NAMESPACE" --wait --timeout 5m --set clusterName="$E2E_CLUSTER_NAME"
	--set image.pullPolicy="$E2E_PULL_POLICY"
	--set flarefake.image.tag="$E2E_TAG" --set flarefake.image.pullPolicy="$E2E_PULL_POLICY")

# 3. Claim the namespace atomically (another run may have started during the image builds),
# then install the previous chart (its CRDs come from its crds/ on this first install).
if ! "$KUBECTL" create namespace "$E2E_NAMESPACE"; then
	if "$KUBECTL" get namespace "$E2E_NAMESPACE" >/dev/null 2>&1; then
		log "SKIPPED: namespace $E2E_NAMESPACE appeared during the image builds (another e2e run?)"
		exit 0
	fi
	log "FAIL: cannot create namespace $E2E_NAMESPACE"
	exit 1
fi
owned=1
log "helm install the chart of $from_desc"
"$HELM" install "$E2E_RELEASE" "$work/prev/$CHART" "${helm_args[@]}" \
	-f "$work/prev/$CHART/ci/flarefake-values.yaml" --set image.tag="$E2E_PREV_TAG"
"$HELM" -n "$E2E_NAMESPACE" list

e2e_env=(E2E_OPERATOR_NAMESPACE="$E2E_NAMESPACE" E2E_RELEASE="$E2E_RELEASE" E2E_STUB_IMAGE="cloudflared-stub:$E2E_TAG"
	E2E_STUB_PULL_POLICY="$E2E_PULL_POLICY")

# 4. Objects under the previous manager.
log "pre-upgrade objects"
env "${e2e_env[@]}" E2E_UPGRADE_PHASE=pre go test -tags e2e ./test/e2e/ -count=1 -v -timeout 10m -run '^TestUpgradePre$'

# 5. CRDs first (Helm never upgrades crds/), then the chart.
log "apply this checkout's CRDs, helm upgrade"
"$MAKE" crds-apply CHART="$CHART" KUBECTL="$KUBECTL"
"$HELM" upgrade "$E2E_RELEASE" "$CHART" "${helm_args[@]}" -f "$CHART/ci/flarefake-values.yaml" --set image.tag="$E2E_TAG"
"$HELM" -n "$E2E_NAMESPACE" history "$E2E_RELEASE"

# 6. Checks after the upgrade, then the smoke subset.
log "post-upgrade checks"
env "${e2e_env[@]}" E2E_UPGRADE_PHASE=post E2E_UPGRADE_IMAGE="flare-operator:$E2E_TAG" \
	go test -tags e2e ./test/e2e/ -count=1 -v -timeout 15m -run '^TestUpgradePost$'
log "smoke subset: $E2E_UPGRADE_SMOKE"
env "${e2e_env[@]}" go test -tags e2e ./test/e2e/ -count=1 -v -timeout 20m -run "$E2E_UPGRADE_SMOKE"
log "PASS"
