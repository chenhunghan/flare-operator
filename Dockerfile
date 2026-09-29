# syntax=docker/dockerfile:1
# flare-operator manager image. Multi-arch: the build stage runs on the build platform and
# cross-compiles for TARGETOS/TARGETARCH, e.g.
#   docker buildx build --platform linux/amd64,linux/arm64 -t flare-operator:dev .
ARG GO_VERSION=1.26.1

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
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="flare-operator" \
      org.opencontainers.image.description="Kubernetes operator for Cloudflare resources" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
