#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): runs in the upstream release workflow, before GoReleaser (#1224).
#
# Copies the pinned base image of the goreleaser kos pipe (.goreleaser.yaml base_image, kept equal to
# hack/base-images/Dockerfile by hack/versions-check.sh) to the release registry, and prints the reference of the
# copy. GoReleaser then pulls the base from the registry it pushes to: the layer blobs are mounted inside that
# registry, and GoReleaser does not contact gcr.io. A short fault of the source registry (HTTP 500, "Too many
# connections" on a blob GET) is waited out here, under push_retry, because GoReleaser itself cannot be repeated
# halfway (it may have created the draft release and pushed images).
#
# Usage: hack/release-base-mirror.sh REGISTRY
#   REGISTRY is ghcr.io/<owner>/<repo>. You must be logged in to it, and crane must be on the PATH.
#   stdout: <REGISTRY>/base/distroless-static@sha256:<hex> (the same digest as the source, so nothing changes in
#   the image that is built). The workflow puts it in KOINON_KOS_BASE_IMAGE, which .goreleaser.yaml reads.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

registry=${1:?usage: $0 REGISTRY}
registry=${registry%/}
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=hack/push-retry-lib.sh
. "$root/hack/push-retry-lib.sh"

# The source is the digest-pinned default that .goreleaser.yaml carries (inside its envOrDefault template).
src=$(grep -oE 'gcr\.io/distroless/static:nonroot@sha256:[0-9a-f]{64}' "$root/.goreleaser.yaml" | head -1)
[[ -n $src ]] || { echo "release base mirror: no digest-pinned gcr.io/distroless/static base in .goreleaser.yaml" >&2; exit 1; }
digest=${src#*@}
dst="$registry/base/distroless-static@$digest"

push_retry crane copy "$src" "$dst" >&2
echo "$dst"
