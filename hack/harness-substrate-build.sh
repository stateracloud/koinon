#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the substrate overlay of a harness image (cmd/koinon-guest-launcher/Dockerfile) and loads it into the local
# Docker daemon. `make harness-substrate-<name>` and test/e2e/kind/substrate.sh both call it, so both use the same builder.
#
#   hack/harness-substrate-build.sh <harness image in the local daemon> <overlay tag> [<platform>]
#
# Needs build/guest-launcher (`make guest-launcher`). The harness image goes in as an OCI layout (hack/harness-flavor-context.sh)
# because the build runs on a docker-container builder of its own (koinon-substrate): the default docker driver does not read an
# OCI layout context, and a container cannot pull from a registry on the host (localhost:5001). The layout is in a temporary
# directory that this script removes on exit.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

(($# >= 2 && $# <= 3)) || { echo "usage: $0 <harness image> <overlay tag> [<platform>]" >&2; exit 2; }
src=$1
tag=$2
platform=${3:-}
root="$(cd "$(dirname "$0")/.." && pwd)"
[[ -d $root/build/guest-launcher ]] || { echo "harness-substrate-build.sh: $root/build/guest-launcher is missing; run make guest-launcher" >&2; exit 1; }

builder=koinon-substrate
docker buildx inspect "$builder" >/dev/null 2>&1 || docker buildx create --name "$builder" --driver docker-container >&2

# Outside any output directory that CI uploads: the layout holds the whole harness image.
work=$(mktemp -d "${TMPDIR:-/tmp}/flavor-oci.XXXXXX")
trap 'rm -rf "$work"' EXIT

docker buildx build --builder "$builder" --load ${platform:+--platform "$platform"} \
  --build-context "flavor=$("$root/hack/harness-flavor-context.sh" "$src" "$work/layout")" \
  --build-context "launcher=$root/build/guest-launcher" \
  --tag "$tag" "$root/cmd/koinon-guest-launcher" >&2
