#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the value of `--build-context flavor=<value>` for the substrate overlay (cmd/koinon-guest-launcher/Dockerfile)
# from a harness image in the local Docker daemon. The image goes into an OCI layout in <dir> and the build reads it
# from there, so the build needs no registry: a docker-container buildx builder (setup-buildx-action in CI, a local
# builder) resolves "localhost" inside its own container and cannot pull from a registry on the host.
#
#   --build-context "flavor=$(hack/harness-flavor-context.sh koinon/sandbox:dev-abc build/flavor-oci/opencode)"
#
# <dir> must not exist or must be empty; the script never removes anything. The release builds on a digest
# reference in a real registry and passes docker-image://<ref> itself.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

(($# == 2)) || { echo "usage: $0 <image in the local daemon> <directory for the OCI layout (absent or empty)>" >&2; exit 2; }
image=$1
dir=$2

docker image inspect "$image" >/dev/null 2>&1 || { echo "harness-flavor-context.sh: $image is not in the local daemon" >&2; exit 1; }
if [[ -e $dir ]]; then
  [[ -d $dir && -z $(ls -A "$dir") ]] || { echo "harness-flavor-context.sh: $dir exists and is not an empty directory; remove it or pick another" >&2; exit 1; }
fi
mkdir -p "$dir"
# Docker 25 and later write an OCI layout from `docker save`.
docker save "$image" | tar -x -C "$dir"
[[ -f $dir/oci-layout && -f $dir/index.json ]] || { echo "harness-flavor-context.sh: docker save of $image is not an OCI layout (needs Docker 25 or newer)" >&2; exit 1; }
digest=$(jq -r '.manifests[0].digest // empty' "$dir/index.json")
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "harness-flavor-context.sh: no manifest digest in $dir/index.json (got '$digest')" >&2; exit 1; }
echo "oci-layout://$(cd "$dir" && pwd)@$digest"
