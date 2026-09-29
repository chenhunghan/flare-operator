// Package differential drives real Cloudflare clients (wrangler, cloudflared) against an
// in-process flarefake and records every mismatch as a known discrepancy: a skipped subtest
// that carries its evidence. The client tests have the build tag "differential"; the
// cloudflare-go scenarios live in the separate module test/differential/go, so the operator's
// go.mod stays free of the SDK. Run them with `make differential`; see
// docs/differential-testing.md.
package differential

// Pinned client versions. `make differential-tools` installs exactly these into
// $(DIFF_CACHE) (default ~/.cache/flare-operator/differential); the Makefile's
// WRANGLER_VERSION and CLOUDFLARED_VERSION and test/differential/npm/package.json must match
// (TestPinnedVersionsMatch). cloudflare-go is pinned in test/differential/go/go.mod.
const (
	WranglerVersion    = "4.143.0"
	CloudflaredVersion = "2026.9.3"
)
