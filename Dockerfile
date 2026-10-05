# syntax=docker/dockerfile:1
# flare-operator image: /manager and /workers-vk. Multi-arch: the build stage runs on the build platform and
# cross-compiles for TARGETOS/TARGETARCH, e.g.
#   docker buildx build --platform linux/amd64,linux/arm64 -t flare-operator:dev .
# Keep in step with the toolchain directive in go.mod (govulncheck: stdlib fixes).
ARG GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/chenhunghan/flare-operator/internal/version.Version=${VERSION} -X github.com/chenhunghan/flare-operator/internal/version.Commit=${COMMIT} -X github.com/chenhunghan/flare-operator/internal/version.Date=${BUILD_DATE}" \
    -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/chenhunghan/flare-operator/internal/version.Version=${VERSION} -X github.com/chenhunghan/flare-operator/internal/version.Commit=${COMMIT} -X github.com/chenhunghan/flare-operator/internal/version.Date=${BUILD_DATE}" \
    -o /out/workers-vk ./cmd/workers-vk

FROM gcr.io/distroless/static-debian13:nonroot
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
# org.opencontainers.image.source links a GHCR package to its repository.
ARG SOURCE_URL=https://github.com/chenhunghan/flare-operator
# OCI image annotations (https://github.com/opencontainers/image-spec/blob/main/annotations.md).
LABEL org.opencontainers.image.title="flare-operator" \
      org.opencontainers.image.description="Kubernetes operator for Cloudflare resources" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian13:nonroot"
COPY --from=build /out/manager /manager
# The Workers virtual kubelet (`kubectl logs` for WorkerScripts; chart workersLogs.enabled) runs
# from the same image as its own Deployment: command ["/workers-vk"].
COPY --from=build /out/workers-vk /workers-vk
USER 65532:65532
ENTRYPOINT ["/manager"]
