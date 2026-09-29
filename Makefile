.PHONY: test test-short fake conformance classify spec-check fmt vet

test:            ## all tests (loads the pinned 26 MB spec once for schema-validation tests)
	go test ./... -count=1

test-short:      ## skip spec-loading tests
	go test ./... -short -count=1

conformance:     ## replay real-API recordings against flarefake
	go test ./internal/fake/ -run TestConformance -count=1 -v

fake:            ## run the emulator on 127.0.0.1:8787 with request validation
	go run ./cmd/flarefake -spec spec/openapi.json.gz

classify:        ## regenerate the API coverage map from the pinned spec
	gunzip -c spec/openapi.json.gz > /tmp/flare-openapi.json && python3 hack/classify_api.py /tmp/flare-openapi.json > docs/cloudflare-api-coverage.md

spec-check:      ## verify spec/openapi.json.gz matches spec/LOCK
	@test "$$(gunzip -c spec/openapi.json.gz | shasum -a 256 | cut -d' ' -f1)" = "$$(awk '/^sha256:/{print $$2}' spec/LOCK)" && echo "spec OK"

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

.PHONY: generate-crds generate-check

generate-crds:   ## regenerate api/<product>/v1alpha1, config/crd/bases and internal/generic/descriptors (cmd/flaregen)
	go run ./cmd/flaregen

generate-check:  ## fail if the generated CRD files are not up to date
	go run ./cmd/flaregen -check
