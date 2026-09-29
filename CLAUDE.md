# flare-operator: working notes for Claude

A Kubernetes-native interface to **all** Cloudflare resources and settings, in the style of AWS ACK or GCP Config Connector:
- one CRD per Cloudflare resource, generated from Cloudflare's OpenAPI spec
- a virtual kubelet that runs Pods on Cloudflare Containers

It is a standalone Go operator, packaged as one Helm chart, following Crossplane-style conventions.

## Status (2026-09-30)
See `docs/STATUS.md` → "Current state". In short:
- **Done:** the operator (10 kinds), the flaregen generator, the flarefake emulator with evidence tiers and strict response validation, differential tests against real clients, resilience and crash-consistency tests, the Helm chart, CI config (not pushed), and e2e and upgrade-e2e on k0s. The production-readiness phase is complete.
- **Blocked on the user:** pushing to GitHub; a test zone; the Workers Paid plan (never upgrade without asking).
- **Paused:** the full-stack slice, generator singletons and nested resources, and Wave 1 kinds (the user asked to harden first; hardening is done).

## How to work in a long session
Follow `docs/plan-parallel.md`:
- **The main session orchestrates.** It writes the contracts, launches worktree-isolated agents (implementers on `opus`, reviewers on `sonnet`, at most 4 at a time), verifies with tests and review agents, merges, and keeps `docs/STATUS.md` current.
- **Delegate** file reading, writing and debugging to agents, and keep agent reports ≤ 400 words.
- **Read `docs/STATUS.md` first** in any new session.

## Commands
```sh
make test            # all tests (the full run loads the 26 MB spec, ~3 s); make test-short skips that
make conformance     # replay test/recordings against flarefake
make test-race       # race detector (CGO_ENABLED=0 on this Mac); make ci runs everything
make fake            # emulator on 127.0.0.1:8787 (/client/v4 for the API, /_fake for control)
make spec-check      # spec/openapi.json.gz must match spec/LOCK
```

## Rules
- **Emulator fidelity:**
  - Every emulator behavior cites its evidence, strongest first:
    1. a real-API recording (`// 0029`)
    2. `// SOURCED: <repo>@<short-sha>:<path>#L<n>`: an official Cloudflare client or its test fixtures (cloudflare/workers-sdk (wrangler), cloudflare-go, terraform-provider-cloudflare, cloudflared)
    3. `// DOCS: <developers.cloudflare.com URL>`: a documented example
    4. otherwise `UNVERIFIED` (spec-only or inferred)
  - A recording wins over any other source when they conflict.
  - Live testing is optional (2026-09-29 decision). Trust comes from the evidence tiers above plus differential tests: real Cloudflare clients such as wrangler and cloudflare-go run against flarefake.
  - When you change a profile, keep `TestConformance` passing. Don't loosen its normalization to make a test pass.
  - Add new recordings to a scenario; the test fails if a recording hits an emulated route that no scenario replays.
- **Live Cloudflare account** (the user's own; `cf` CLI logged in; `CLOUDFLARE_ACCOUNT_ID` must be exported):
  - Take a baseline inventory first.
  - Name everything `flare-spike-*` and log each resource in a ledger as it's created.
  - Delete everything afterwards, then independently re-list and compare with the baseline.
  - Never touch pre-existing resources. The `default` virtual network can't be deleted (400/1049); leave it.
  - A pre-existing `python -m http.server 8765` process is not ours.
- **Recordings:**
  - Sanitize them with `hack/sanitize_recordings.py`. Its substitution list is at `~/.config/flare-operator/sanitize.json`, outside the repo; never commit personal values.
  - After sanitizing, scan for leaks: IDs, IPs, geolocation, hostnames, user tags, tunnel tokens, tail URLs.
- **Local cluster:** a Lima k0s VM. The kubeconfig is at `~/.lima/<instance>/kubeconfig.yaml`. Instance names change when the user recreates the VM, so check `limactl list`.
- **Shell is zsh:**
  - An unquoted `$var` does not word-split; use `${=var}`.
  - Never name a variable `path`; it clobbers `PATH`.
  - A glob with no matches aborts the whole command line.
- **Subagents:** parallel agents are welcome, but their results must be verified by you or a peer-review agent before you report them to the user.
