#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): builds the guest server for the harness image (make guest-launcher); it runs locally and in the release workflow.
#
# Builds the Substrate guest server `ate-env-guest` as a static binary for linux/amd64 and linux/arm64, at the commit
# ATE_ENV_VERSION of hack/versions.env (ADR 0119, issue #1087). The substrate overlay of a harness image carries it
# (cmd/koinon-guest-launcher/Dockerfile): upstream publishes no image, and the actor's process is the guest server.
#
#   hack/build-env-guest.sh [OUTDIR]     # default build/env-guest; writes OUTDIR/linux-<arch>/ate-env-guest
#
# The source comes from a clone and a checkout of the pinned commit (hack/pinned-download-lint.sh refuses archive
# downloads). ENV_SRC names an existing clone to use instead (offline work); it must be at the pinned commit.
# Needs git and a Go toolchain that satisfies the env module's go.mod (GOTOOLCHAIN=auto fetches one).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$root/build/env-guest}
mkdir -p "$out"
out=$(cd "$out" && pwd)

pin=$(sed -n 's/^ATE_ENV_VERSION=//p' "$root/hack/versions.env")
[[ $pin =~ ^[0-9a-f]{40}$ ]] || { echo "ATE_ENV_VERSION in hack/versions.env is not a 40-digit commit: '$pin'" >&2; exit 1; }

src=${ENV_SRC:-}
if [[ -z $src ]]; then
	work=$(mktemp -d "${TMPDIR:-/tmp}/env-guest.XXXXXX")
	trap 'rm -rf "$work"' EXIT
	src=$work/env-src
	git clone --quiet https://github.com/agent-substrate/env.git "$src"
	git -C "$src" checkout --quiet --detach "$pin"
fi
[[ $(git -C "$src" rev-parse HEAD) == "$pin" ]] || { echo "the env source is not at $pin" >&2; exit 1; }

for arch in amd64 arm64; do
	mkdir -p "$out/linux-$arch"
	(cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' \
		-o "$out/linux-$arch/ate-env-guest" ./cmd/ate-env-guest)
done
echo "built ate-env-guest at $pin into $out" >&2
