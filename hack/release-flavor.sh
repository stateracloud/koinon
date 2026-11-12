#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds and pushes the OpenCode flavor image for a release (plan item REL1, ADR 0059) for
# linux/amd64 and linux/arm64, tagged <version> and, for a final release, latest. Prints
# "ref=<registry>/flavors/opencode@sha256:<hex>" for the release workflow's sign job. Needs a buildx
# builder that can build both platforms and a docker login to the registry.
#
#   hack/release-flavor.sh 0.1.0-rc.1 ghcr.io/stateracloud/koinon <commit>

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if (($# != 3)); then
	echo "usage: $0 <version> <registry> <commit>" >&2
	exit 2
fi
version=${1#v}
repo=${2%/}/flavors/opencode
commit=$3
cd "$(dirname "$0")/.."

[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	{ echo "version '$1' is not X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }

tags=(--tag "$repo:$version")
[[ $version == *-rc.* ]] || tags+=(--tag "$repo:latest") # latest moves for final releases only, like GoReleaser's

# GITHUB_TOKEN authenticates the Dockerfile's GitHub release downloads (BuildKit secret, #145).
secret=()
[[ -z ${GITHUB_TOKEN:-} ]] || secret=(--secret "id=github_token,env=GITHUB_TOKEN")

# The commit time pins every timestamp (#176): SOURCE_DATE_EPOCH is the build arg BuildKit reads for
# the image's creation time and rewrite-timestamp clamps the layer files' mtimes to it, so one commit
# gives the same layers (hack/flavor-repro-check.sh checks it). The attestations still vary.
epoch=$(git show -s --format=%ct "$commit")
[[ $epoch =~ ^[0-9]+$ ]] || { echo "no commit time for '$commit'" >&2; exit 1; }

meta=$(mktemp "${TMPDIR:-/tmp}/release-flavor.XXXXXX")
trap 'rm -f "$meta"' EXIT
docker buildx build --output type=registry,rewrite-timestamp=true --platform linux/amd64,linux/arm64 \
	--build-arg "SOURCE_DATE_EPOCH=$epoch" \
	--build-context common=flavors/common "${secret[@]}" "${tags[@]}" \
	--label org.opencontainers.image.source=https://github.com/stateracloud/koinon \
	--label org.opencontainers.image.version="$version" \
	--label org.opencontainers.image.revision="$commit" \
	--label org.opencontainers.image.licenses=Apache-2.0 \
	--sbom=true --provenance=mode=min \
	--metadata-file "$meta" flavors/opencode >&2

digest=$(jq -r '."containerimage.digest" // empty' "$meta")
[[ $digest == sha256:* ]] || { echo "no image digest in the buildx metadata" >&2; exit 1; }
echo "ref=$repo@$digest"
