# syntax=docker/dockerfile:1
# cloudflared stand-in for e2e tests (see main.go). Build context: this directory
# (`make e2e-images`). Stdlib only, so no go.mod is copied.
ARG GO_VERSION=1.26.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/cloudflared-stub main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cloudflared-stub /cloudflared-stub
USER 65532:65532
EXPOSE 2000
ENTRYPOINT ["/cloudflared-stub"]
