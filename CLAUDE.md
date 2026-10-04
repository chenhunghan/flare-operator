# flare-operator: notes for Claude and contributors

A Kubernetes-native interface to **all** Cloudflare resources and settings, in the style of AWS ACK or GCP Config Connector:
- one CRD per Cloudflare resource, generated from Cloudflare's OpenAPI spec
- a virtual kubelet that runs Pods on Cloudflare Containers (planned)

It is a standalone Go operator (module `github.com/chenhunghan/flare-operator`, API group `flare.dev`), packaged as one Helm chart, following Crossplane-style conventions.

Project status, known limits and UNVERIFIED assumptions are in `docs/known-issues.md`. Personal, machine-specific notes go in `CLAUDE.local.md` (gitignored), never in this file.

## Commits
Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, `feat!:` for breaking changes). release-please reads them to pick the next version and write CHANGELOG.md; non-conventional commits are left out of the changelog.

## Commands
```sh
make test            # all tests (the full run loads the 26 MB spec, ~3 s); make test-short skips that
make conformance     # replay test/recordings against flarefake
make test-race       # race detector (cgo off on darwin, on elsewhere); make ci runs everything
make fake            # emulator on 127.0.0.1:8787 (/client/v4 for the API, /_fake for control)
make spec-check      # spec/openapi.json.gz must match spec/LOCK
make differential    # real clients (wrangler, cloudflared, cloudflare-go) against flarefake
```

## Rules
- **Emulator fidelity:**
  - Every emulator behavior cites its evidence, strongest first:
    1. a real-API recording (`// 0029`)
    2. `// SOURCED: <repo>@<short-sha>:<path>#L<n>`: an official Cloudflare client or its test fixtures (cloudflare/workers-sdk (wrangler), cloudflare-go, terraform-provider-cloudflare, cloudflared)
    3. `// DOCS: <developers.cloudflare.com URL>`: a documented example
    4. otherwise `UNVERIFIED` (spec-only or inferred)
  - A recording wins over any other source when they conflict.
  - Live testing is optional. Trust comes from the evidence tiers above plus differential tests: real Cloudflare clients such as wrangler and cloudflare-go run against flarefake.
  - When you change a profile, keep `TestConformance` passing. Don't loosen its normalization to make a test pass.
  - Add new recordings to a scenario; the test fails if a recording hits an emulated route that no scenario replays.
- **Live Cloudflare account** (only your own test account; `CLOUDFLARE_ACCOUNT_ID` must be exported):
  - Take a baseline inventory first.
  - Name everything `flare-spike-*` and log each resource in a ledger as it's created.
  - Delete everything afterwards, then independently re-list and compare with the baseline.
  - Never touch pre-existing resources. The `default` virtual network can't be deleted (400/1049); leave it.
  - Never change the account's plan or enable paid products without the account owner's approval.
- **Recordings:**
  - Sanitize them with `hack/sanitize_recordings.py`. Its substitution list (your real account ID, subdomain, email and so on) lives outside the repo, by default in `~/.config/flare-operator/sanitize.json` (`--subs`); never commit personal values.
  - After sanitizing, scan for leaks: IDs, IPs, geolocation, hostnames, user tags, tunnel tokens, tail URLs.
- **Generated files:** don't hand-edit CRDs, deepcopy, descriptors, `docs/api-reference.md` or the chart's `crds/` and ClusterRoles; regenerate them (`make manifests generate-crds chart-sync api-docs`). `make verify-generated` and `make chart-check` catch drift.
