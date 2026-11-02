#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds and pushes the OpenCode flavor image for a release (plan item REL1, ADR 0059) for
# linux/amd64 and linux/arm64, tagged <version> and, for a final release, latest. Prints
# "ref=<registry>/flavor-opencode@sha256:<hex>" for the release workflow's sign job. Needs a buildx
# builder that can build both platforms and a docker login to the registry.
#
#   hack/release-flavor.sh 0.1.0-rc.1 ghcr.io/stateracloud <commit>
set -euo pipefail

if (($# != 3)); then
	echo "usage: $0 <version> <registry> <commit>" >&2
	exit 2
fi
version=${1#v}
repo=${2%/}/flavor-opencode
commit=$3
cd "$(dirname "$0")/.."

[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	{ echo "version '$1' is not X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }

tags=(--tag "$repo:$version")
[[ $version == *-rc.* ]] || tags+=(--tag "$repo:latest") # latest moves for final releases only, like GoReleaser's

meta=$(mktemp "${TMPDIR:-/tmp}/release-flavor.XXXXXX")
trap 'rm -f "$meta"' EXIT
docker buildx build --push --platform linux/amd64,linux/arm64 \
	--build-context common=flavors/common "${tags[@]}" \
	--label org.opencontainers.image.source=https://github.com/stateracloud/koinon \
	--label org.opencontainers.image.version="$version" \
	--label org.opencontainers.image.revision="$commit" \
	--label org.opencontainers.image.licenses=Apache-2.0 \
	--sbom=true --provenance=mode=min \
	--metadata-file "$meta" flavors/opencode >&2

digest=$(jq -r '."containerimage.digest" // empty' "$meta")
[[ $digest == sha256:* ]] || { echo "no image digest in the buildx metadata" >&2; exit 1; }
echo "ref=$repo@$digest"
