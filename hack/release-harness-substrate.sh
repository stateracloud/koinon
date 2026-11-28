#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): pushes a release image, so it needs the registry's write rights (the release
# workflow). The image source label names the repository it runs in (hack/repo-lib.sh).
#
# Builds and pushes the Substrate overlay of the harness image (ADR 0119, issue #1087) for linux/amd64 and
# linux/arm64: the released harness image plus the guest front (#1309) and the guest launcher
# (cmd/koinon-guest-launcher/Dockerfile). Tagged <version> and, for a final release, latest, in the repository
# <registry>/sandbox-substrate. Prints "ref=<registry>/sandbox-substrate@sha256:<hex>" for the release workflow's sign job.
#
#   hack/release-harness-substrate.sh 0.1.0-rc.1 ghcr.io/stateracloud/koinon <commit> ghcr.io/stateracloud/koinon/sandbox@sha256:<hex>
#
# The base is the digest reference of the harness image of the same release (hack/release-harness.sh prints it), so
# the overlay never builds on a tag that can move. The overlay has no RUN step: both platforms build on one runner
# without emulation. It needs Go (the guest launcher and the guest front are built here), git, and a buildx builder
# with a docker login to the registry. The guest front embeds the upstream guest server package at the version in
# go.mod, which is the commit ATE_ENV_VERSION.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=repo-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/repo-lib.sh"

(($# == 4)) || { echo "usage: $0 <version> <registry> <commit> <harness image digest reference>" >&2; exit 2; }
version=${1#v}
repo=${2%/}/sandbox-substrate
commit=$3
base=$4
cd "$(dirname "$0")/.."
source_repo=$(koinon_repo)

[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	{ echo "version '$1' is not X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }
[[ $base =~ ^[a-z0-9][a-z0-9./:-]*@sha256:[0-9a-f]{64}$ ]] ||
	{ echo "the base '$base' is not a digest reference (name@sha256:<64 hex>)" >&2; exit 2; }

tags=(--tag "$repo:$version")
[[ $version == *-rc.* ]] || tags+=(--tag "$repo:latest") # latest moves for final releases only, like the harness image

epoch=$(git show -s --format=%ct "$commit")
[[ $epoch =~ ^[0-9]+$ ]] || { echo "no commit time for '$commit'" >&2; exit 1; }

make guest-launcher >&2

meta=$(mktemp "${TMPDIR:-/tmp}/release-harness-substrate.XXXXXX")
trap 'rm -f "$meta"' EXIT
docker buildx build --output "type=registry,rewrite-timestamp=true" --platform "linux/amd64,linux/arm64" "${tags[@]}" \
	--build-context "flavor=docker-image://$base" \
	--build-arg "SOURCE_DATE_EPOCH=$epoch" \
	--build-context launcher=build/guest-launcher \
	--label org.opencontainers.image.source="https://github.com/$source_repo" \
	--label org.opencontainers.image.version="$version" \
	--label org.opencontainers.image.revision="$commit" \
	--label org.opencontainers.image.licenses=LicenseRef-koinon-image-notices \
	--sbom=true --provenance=mode=min \
	--metadata-file "$meta" cmd/koinon-guest-launcher >&2

digest=$(jq -r '."containerimage.digest" // empty' "$meta")
[[ $digest == sha256:* ]] || { echo "no image digest in the buildx metadata" >&2; exit 1; }
echo "ref=$repo@$digest"
