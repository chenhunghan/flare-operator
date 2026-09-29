# flare-operator

A Kubernetes-native interface to all of Cloudflare: one CRD per Cloudflare resource (generated from
Cloudflare's OpenAPI spec), plus a virtual kubelet that runs Pods on Cloudflare Containers.
Status: **design + test infrastructure**; no controllers yet.

| Doc | What |
|---|---|
| [docs/cloudflare-service-catalog.md](docs/cloudflare-service-catalog.md) | Every Cloudflare product → API, k8s analogy, tier |
| [docs/cloudflare-api-coverage.md](docs/cloudflare-api-coverage.md) | All 3,620 API operations classified (generated) |
| [docs/virtual-kubelet-design.md](docs/virtual-kubelet-design.md) | Pods on Cloudflare Containers |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Emulator, recordings, API versioning |
| [docs/spike-results-2026-09-29.md](docs/spike-results-2026-09-29.md) | What we learned from the real API |

## Layout

```
spec/                     pinned Cloudflare OpenAPI spec (gzipped) + LOCK
internal/fake/            flarefake: stateful Cloudflare API emulator (profiles cite recordings)
cmd/flarefake/            emulator as a standalone server (API at /client/v4, control at /_fake)
test/recordings/          sanitized real-API recordings; replayed by `make conformance`
hack/                     classify_api.py (coverage map), sanitize_recordings.py
```

## Try it

```sh
make test          # unit + conformance tests
make fake          # run flarefake on http://127.0.0.1:8787
curl -s -H 'Authorization: Bearer x' -X POST localhost:8787/client/v4/accounts/abc/queues -d '{"queue_name":"demo"}'
```

The Go module path `flare.dev/operator` is a placeholder until the repository has a home.
