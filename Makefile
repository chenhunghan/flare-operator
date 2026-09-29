.PHONY: test test-race test-short fake conformance classify spec-check fmt vet generate manifests envtest controller-gen setup-envtest run tools

# Operator binaries and tests build without cgo, as the container image does. This also avoids
# linking prometheus/client_golang's darwin cgo files, which fails with toolchains whose ld
# cannot read the installed macOS SDK (seen with Xcode ld-1230 + MacOSX27 SDK). The race
# detector works without cgo on darwin. Override with CGO_ENABLED=1 make ...
export CGO_ENABLED ?= 0

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

test-race: envtest ## all tests with the race detector (works without cgo on darwin)
	CGO_ENABLED=0 KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test -race ./... -count=1

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
	$(CONTROLLER_GEN) rbac:roleName=flare-operator-manager crd paths="./api/..." paths="./internal/controller/..." \
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
	gofmt -w api cmd internal

vet:
	go vet ./...

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
