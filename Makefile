.PHONY: test test-race test-short fake conformance classify spec-check fmt vet generate manifests envtest controller-gen setup-envtest run tools
.PHONY: ci fmt-check verify-generated

# Operator binaries and tests build without cgo, as the container image does. This also avoids
# linking prometheus/client_golang's darwin cgo files, which fails with toolchains whose ld
# cannot read the installed macOS SDK (seen with Xcode ld-1230 + MacOSX27 SDK). The race
# detector works without cgo on darwin. Override with CGO_ENABLED=1 make ...
export CGO_ENABLED ?= 0

# test-race sets cgo on its own: darwin's race detector needs no cgo (and cgo linking is broken
# on some Macs, see above), but on every other OS `go test -race` requires cgo.
# Override with RACE_CGO_ENABLED=... make test-race.
ifeq ($(shell go env GOOS),darwin)
RACE_CGO_ENABLED ?= 0
else
RACE_CGO_ENABLED ?= 1
endif

## Tool binaries (installed into ./bin, which is gitignored)
LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest

# controller-tools v0.22 / controller-runtime v0.25 match k8s.io/* v0.37 in go.mod
# (controller-gen@latest failed with a klog error against apimachinery v0.37).
CONTROLLER_TOOLS_VERSION ?= v0.22.0
SETUP_ENVTEST_VERSION ?= v0.25.1
ENVTEST_K8S_VERSION ?= 1.37.0
# envtest assets live outside the checkout so every git worktree shares one download.
ENVTEST_DIR ?= $(HOME)/.cache/flare-operator/envtest

# envtest assets for tests: installed copy first, download if missing.
ENVTEST_ASSETS = $$($(SETUP_ENVTEST) use -i $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path 2>/dev/null || $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)

test: envtest     ## all tests (loads the pinned 26 MB spec once; runs envtest suites)
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test ./... -count=1

test-race: envtest ## all tests with the race detector (cgo off on darwin, on elsewhere; see RACE_CGO_ENABLED)
	CGO_ENABLED=$(RACE_CGO_ENABLED) KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test -race ./... -count=1

test-short: envtest ## skip spec-loading tests
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test ./... -short -count=1

conformance:     ## replay real-API recordings against flarefake
	go test ./internal/fake/ -run TestConformance -count=1 -v

fake:            ## run the emulator on 127.0.0.1:8787 with request validation
	go run ./cmd/flarefake -spec spec/openapi.json.gz

run: manifests   ## run the manager against the current kubeconfig
	go run ./cmd/manager

classify:        ## regenerate the API coverage map from the pinned spec
	gunzip -c spec/openapi.json.gz > /tmp/flare-openapi.json && python3 hack/classify_api.py /tmp/flare-openapi.json > docs/cloudflare-api-coverage.md

spec-check:      ## verify spec/openapi.json.gz matches spec/LOCK
	@test "$$(gunzip -c spec/openapi.json.gz | shasum -a 256 | cut -d' ' -f1)" = "$$(awk '/^sha256:/{print $$2}' spec/LOCK)" && echo "spec OK"

generate: controller-gen ## deepcopy methods for api/...
	$(CONTROLLER_GEN) object paths="./api/..."

manifests: controller-gen ## CRDs into config/crd/bases, RBAC into config/rbac
	$(CONTROLLER_GEN) rbac:roleName=flare-operator-manager crd paths="./api/..." paths="./internal/controller/..." paths="./internal/generic/kinds/..." \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

envtest: setup-envtest ## fetch envtest assets (kube-apiserver, etcd) into $(ENVTEST_DIR)/k8s
	@echo "envtest assets: $(ENVTEST_ASSETS)"

tools: controller-gen setup-envtest

controller-gen: | $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

setup-envtest: | $(LOCALBIN)
	$(call go-install-tool,$(SETUP_ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(SETUP_ENVTEST_VERSION))

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

fmt:
	gofmt -w api cmd internal test/e2e

vet:             ## go vet, including the e2e-tagged test/e2e package
	go vet ./...
	go vet -tags e2e ./test/e2e/...

fmt-check:       ## fail if gofmt would change anything in api, cmd, internal, test/e2e (the dirs `make fmt` rewrites)
	@out="$$(gofmt -l api cmd internal test/e2e)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi; echo "gofmt OK"

# Directories written by `make generate manifests` (controller-gen) and `make generate-crds` (flaregen).
GENERATED_PATHS ?= api config internal/generic

verify-generated: ## regenerate deepcopy/CRDs/RBAC, run flaregen -check, fail if GENERATED_PATHS differ from HEAD
	$(MAKE) generate manifests
	$(MAKE) generate-check
	@git diff --exit-code -- $(GENERATED_PATHS) || { echo "generated files differ from HEAD: run make generate manifests generate-crds and commit"; exit 1; }
	@untracked="$$(git ls-files --others --exclude-standard -- $(GENERATED_PATHS))"; if [ -n "$$untracked" ]; then echo "untracked generated files:"; echo "$$untracked"; exit 1; fi; echo "generated files OK"

# ci runs the checks of .github/workflows/ci.yml in the same order (lint, test, race,
# generate-check, conformance). verify-generated compares against HEAD, so commit local edits
# under GENERATED_PATHS first.
ci:
	$(MAKE) fmt-check
	$(MAKE) vet
	$(MAKE) spec-check
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) verify-generated
	$(MAKE) chart-check
	$(MAKE) conformance

# go-install-tool installs a versioned binary ($1-$3) and points $1 at it, so bumping a
# version re-installs.
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
	set -e; \
	echo "Installing $(2)@$(3)"; \
	rm -f $(1); \
	GOBIN=$(LOCALBIN) go install $(2)@$(3); \
	mv $(1) $(1)-$(3); \
}; \
ln -sf $(notdir $(1))-$(3) $(1)
endef

.PHONY: generate-crds generate-check

generate-crds:   ## regenerate api/<product>/v1alpha1, config/crd/bases and internal/generic/descriptors (cmd/flaregen)
	go run ./cmd/flaregen

generate-check:  ## fail if the generated CRD files are not up to date
	go run ./cmd/flaregen -check

## Packaging: images and the Helm chart (charts/flare-operator)
.PHONY: docker-build docker-build-fake chart-sync chart-check helm-lint

# Container CLI (docker, or nerdctl/podman with a compatible `build`).
CONTAINER_TOOL ?= docker
IMG ?= flare-operator:dev
FAKE_IMG ?= flarefake:dev
# One platform loads into the local image store; for several, use `docker buildx build --push`.
PLATFORM ?= linux/$(shell go env GOARCH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART ?= charts/flare-operator
HELM ?= helm
KUBECONFORM ?= $(shell command -v kubeconform 2>/dev/null)

docker-build:    ## manager image $(IMG) for $(PLATFORM)
	$(CONTAINER_TOOL) build --platform=$(PLATFORM) --build-arg VERSION=$(VERSION) -f Dockerfile -t $(IMG) .

docker-build-fake: ## flarefake image $(FAKE_IMG) for $(PLATFORM), with the pinned spec baked in
	$(CONTAINER_TOOL) build --platform=$(PLATFORM) --build-arg VERSION=$(VERSION) -f Dockerfile.flarefake -t $(FAKE_IMG) .

chart-sync:      ## copy config/crd/bases into the chart and render config/rbac/role.yaml into its ClusterRole
	go run ./hack/chartsync

chart-check:     ## fail if the chart's CRDs or ClusterRoles are out of sync with config/
	go run ./hack/chartsync -check

helm-lint: chart-check ## helm lint + helm template (default and flarefake values); kubeconform if installed
	$(HELM) lint --strict $(CHART)
	$(HELM) lint --strict $(CHART) -f $(CHART)/ci/flarefake-values.yaml
	@for v in $(CHART)/ci/*-values.yaml; do \
		echo "helm template -f $$v"; \
		$(HELM) template flare-operator $(CHART) -n flare-system --include-crds -f $$v > /dev/null || exit 1; \
		if [ -n "$(KUBECONFORM)" ]; then \
			$(HELM) template flare-operator $(CHART) -n flare-system --include-crds -f $$v \
				| $(KUBECONFORM) -strict -summary -ignore-missing-schemas || exit 1; \
		fi; \
	done
	@[ -n "$(KUBECONFORM)" ] || echo "kubeconform not installed; skipped schema validation"

## e2e on a real cluster (test/e2e): the chart with flarefake, driven by go test -tags e2e.
## Example for the Lima k0s VM:
##   export KUBECONFIG=~/.lima/<instance>/kubeconfig.yaml DOCKER_HOST=unix://$HOME/.lima/<instance>/docker.sock
##   make e2e-images E2E_IMAGE_LOAD='limactl shell <instance> sudo k0s ctr -n k8s.io images import -'
##   make e2e-install e2e e2e-uninstall E2E_IMAGE_REMOVE='limactl shell <instance> sudo k0s ctr -n k8s.io images rm'
.PHONY: e2e e2e-images e2e-install e2e-uninstall

E2E_TAG ?= e2e
E2E_NAMESPACE ?= flare-system
E2E_RELEASE ?= flare-operator
# Images are loaded into the node's runtime (E2E_IMAGE_LOAD), not pulled.
E2E_PULL_POLICY ?= Never
# A command that reads `docker save` output on stdin and imports it into the cluster's container
# runtime (empty: skip, e.g. for kind use `kind load` by hand or push to a registry).
E2E_IMAGE_LOAD ?=
# The counterpart of E2E_IMAGE_LOAD for e2e-uninstall: a command that removes the image
# references given as its arguments from the cluster's container runtime (empty: skip). It gets
# E2E_IMAGE_REFS, the names containerd records for the `docker save` archive that
# E2E_IMAGE_LOAD imported (docker.io/library/<name>:<tag>); `ctr images rm` only warns about a
# reference it does not have.
E2E_IMAGE_REMOVE ?=
E2E_IMAGES = flare-operator:$(E2E_TAG) flarefake:$(E2E_TAG) cloudflared-stub:$(E2E_TAG)
E2E_IMAGE_REFS = $(addprefix docker.io/library/,$(E2E_IMAGES))
E2E_CLUSTER_NAME ?= flare-e2e

e2e-images:      ## build flare-operator, flarefake and the cloudflared stub as :$(E2E_TAG) and load them (E2E_IMAGE_LOAD)
	$(MAKE) docker-build IMG=flare-operator:$(E2E_TAG)
	$(MAKE) docker-build-fake FAKE_IMG=flarefake:$(E2E_TAG)
	$(CONTAINER_TOOL) build --platform=$(PLATFORM) -t cloudflared-stub:$(E2E_TAG) test/e2e/cloudflared-stub
	@if [ -n "$(E2E_IMAGE_LOAD)" ]; then \
		echo "loading images with: $(E2E_IMAGE_LOAD)"; \
		$(CONTAINER_TOOL) save $(E2E_IMAGES) | $(E2E_IMAGE_LOAD); \
	else echo "E2E_IMAGE_LOAD is empty: images were not loaded into the cluster"; fi

e2e-install:     ## helm install the chart with flarefake into $(E2E_NAMESPACE) (CRDs from the chart)
	$(HELM) upgrade --install $(E2E_RELEASE) $(CHART) -n $(E2E_NAMESPACE) --create-namespace --wait --timeout 5m \
		-f $(CHART)/ci/flarefake-values.yaml --set clusterName=$(E2E_CLUSTER_NAME) \
		--set image.tag=$(E2E_TAG) --set image.pullPolicy=$(E2E_PULL_POLICY) \
		--set flarefake.image.tag=$(E2E_TAG) --set flarefake.image.pullPolicy=$(E2E_PULL_POLICY)

e2e:             ## run test/e2e against the installed chart (skips without KUBECONFIG)
	E2E_OPERATOR_NAMESPACE=$(E2E_NAMESPACE) E2E_RELEASE=$(E2E_RELEASE) E2E_STUB_IMAGE=cloudflared-stub:$(E2E_TAG) \
		E2E_STUB_PULL_POLICY=$(E2E_PULL_POLICY) go test -tags e2e ./test/e2e/ -count=1 -v -timeout 20m

e2e-uninstall:   ## helm uninstall, delete the chart's CRDs (Helm keeps them) and $(E2E_NAMESPACE), then remove the e2e images (E2E_IMAGE_REMOVE)
	-$(HELM) uninstall $(E2E_RELEASE) -n $(E2E_NAMESPACE) --wait
	kubectl delete -f $(CHART)/crds/ --ignore-not-found
	kubectl delete namespace $(E2E_NAMESPACE) --ignore-not-found --wait
	@if [ -n "$(E2E_IMAGE_REMOVE)" ]; then \
		echo "removing images with: $(E2E_IMAGE_REMOVE) $(E2E_IMAGE_REFS)"; \
		$(E2E_IMAGE_REMOVE) $(E2E_IMAGE_REFS); \
	else echo "E2E_IMAGE_REMOVE is empty: the images e2e-images loaded were left in the cluster ($(E2E_IMAGE_REFS))"; fi
