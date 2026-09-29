#!/usr/bin/env bash
# Installs the pinned clients the differential tests (test/differential) drive:
#   - wrangler, with npm ci from test/differential/npm/package-lock.json, into
#     $DIFF_CACHE/wrangler-$WRANGLER_VERSION
#   - cloudflared, the release binary for this OS/arch, checked against its GitHub asset
#     sha256 digest, into $DIFF_CACHE/cloudflared-$CLOUDFLARED_VERSION/cloudflared
# A client whose tooling is missing (npm, curl) is skipped with a message; the tests then skip
# that client. Nothing here talks to the Cloudflare API.
set -euo pipefail

: "${DIFF_CACHE:?}" "${WRANGLER_VERSION:?}" "${CLOUDFLARED_VERSION:?}"
root="$(cd "$(dirname "$0")/.." && pwd)"

# sha256 of the cloudflared $CLOUDFLARED_VERSION release assets (GitHub's asset "digest";
# the checksums in the release notes predate the macOS notarization and differ for darwin).
cloudflared_sha256() {
	case "$CLOUDFLARED_VERSION/$1" in
	2026.9.3/cloudflared-darwin-arm64.tgz) echo 587c2cfb1c230fe36c7fa7727da78be459dae028cabe8c001291999350f07095 ;;
	2026.9.3/cloudflared-darwin-amd64.tgz) echo d1155d0837487f261183b15c1eab6c4ebcad9dc49b94675f1524c3564cea3977 ;;
	2026.9.3/cloudflared-linux-arm64) echo aaeb2d7d0da3614634c7e03ab13487a1522c2e79165ed2929cfe23d5e95b326d ;;
	2026.9.3/cloudflared-linux-amd64) echo 77e26d8d900e0b8469f416239d14b5f296525fdf79fee6f511ef55609e3fbac2 ;;
	*) echo "" ;;
	esac
}

install_wrangler() {
	local dir="$DIFF_CACHE/wrangler-$WRANGLER_VERSION"
	if [ -x "$dir/node_modules/.bin/wrangler" ]; then
		echo "wrangler $WRANGLER_VERSION: already installed in $dir"
		return
	fi
	if ! command -v npm >/dev/null; then
		echo "wrangler: npm not found, skipped (the wrangler tests will skip)"
		return
	fi
	mkdir -p "$dir"
	cp "$root/test/differential/npm/package.json" "$root/test/differential/npm/package-lock.json" "$dir/"
	(cd "$dir" && npm ci --no-audit --no-fund)
	"$dir/node_modules/.bin/wrangler" --version
}

install_cloudflared() {
	local dir="$DIFF_CACHE/cloudflared-$CLOUDFLARED_VERSION"
	if [ -x "$dir/cloudflared" ]; then
		echo "cloudflared $CLOUDFLARED_VERSION: already installed in $dir"
		return
	fi
	if ! command -v curl >/dev/null; then
		echo "cloudflared: curl not found, skipped (the cloudflared tests will skip)"
		return
	fi
	local os arch asset sum
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$(uname -m)" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) echo "cloudflared: unsupported arch $(uname -m), skipped"; return ;;
	esac
	asset="cloudflared-$os-$arch"
	[ "$os" = darwin ] && asset="$asset.tgz"
	sum="$(cloudflared_sha256 "$asset")"
	if [ -z "$sum" ]; then
		echo "cloudflared: no pinned sha256 for $asset at $CLOUDFLARED_VERSION, skipped"
		return
	fi
	mkdir -p "$dir"
	local tmp="$dir/$asset.download"
	curl -fsSL -o "$tmp" "https://github.com/cloudflare/cloudflared/releases/download/$CLOUDFLARED_VERSION/$asset"
	if [ "$(shasum -a 256 "$tmp" | cut -d' ' -f1)" != "$sum" ]; then
		rm -f "$tmp"
		echo "cloudflared: sha256 mismatch for $asset" >&2
		exit 1
	fi
	if [ "$os" = darwin ]; then
		tar -xzf "$tmp" -C "$dir" cloudflared
		rm -f "$tmp"
	else
		mv "$tmp" "$dir/cloudflared"
	fi
	chmod +x "$dir/cloudflared"
	"$dir/cloudflared" --version
}

install_wrangler
install_cloudflared
