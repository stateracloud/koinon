#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): pushes release images, so it needs the registry's write rights (the release
# workflow). The image source label names the repository it runs in (hack/repo-lib.sh).
#
# Builds and pushes the OpenCode harness image for a release (plan item REL1, ADR 0059) for
# linux/amd64 and linux/arm64, tagged <version> and, for a final release, latest. Prints
# "ref=<registry>/sandbox@sha256:<hex>" for the release workflow's sign job. Needs a buildx
# builder that can build both platforms and a docker login to the registry.
#
#   hack/release-harness.sh 0.1.0-rc.1 ghcr.io/stateracloud/koinon <commit>
#
# Modes (the release workflow builds each platform on a native runner, then joins them):
#   hack/release-harness.sh <version> <registry> <commit>
#       both platforms in one buildx call, tags pushed (local use, the lab, docs/dev/releasing.md)
#   hack/release-harness.sh --platform linux/arm64 <version> <registry> <commit>
#       one platform, pushed by digest without a tag, with a registry build cache; prints "digest=sha256:<hex>"
#   hack/release-harness.sh --merge <version> <registry> <commit> <digest>...
#       joins the per-platform digests into one tagged manifest list; prints "ref=<registry>/sandbox@sha256:<hex>"

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=repo-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/repo-lib.sh"

mode=all
platform=
case ${1:-} in
--platform) mode=platform platform=${2:-}; shift 2 || { echo "--platform needs a value" >&2; exit 2; } ;;
--merge) mode=merge; shift ;;
esac
if [[ $mode == merge ]] && (($# < 4)); then
	echo "usage: $0 --merge <version> <registry> <commit> <digest>..." >&2
	exit 2
elif [[ $mode != merge ]] && (($# != 3)); then
	echo "usage: $0 [--platform <os/arch>] <version> <registry> <commit>" >&2
	echo "       $0 --merge <version> <registry> <commit> <digest>..." >&2
	exit 2
fi
version=${1#v}
repo=${2%/}/sandbox
commit=$3
cd "$(dirname "$0")/.."
source_repo=$(koinon_repo)

[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	{ echo "version '$1' is not X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }

tags=(--tag "$repo:$version")
[[ $version == *-rc.* ]] || tags+=(--tag "$repo:latest") # latest moves for final releases only, like GoReleaser's

if [[ $mode == merge ]]; then
	shift 3
	srcs=()
	for d in "$@"; do
		[[ $d =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "'$d' is not a sha256 digest" >&2; exit 2; }
		srcs+=("$repo@$d")
	done
	meta=$(mktemp "${TMPDIR:-/tmp}/release-harness.XXXXXX")
	trap 'rm -f "$meta"' EXIT
	docker buildx imagetools create "${tags[@]}" --metadata-file "$meta" "${srcs[@]}" >&2
	# imagetools create writes {"containerimage.descriptor": {..., "digest": ...}}, not buildx build's key.
	digest=$(jq -r '."containerimage.descriptor".digest // empty' "$meta")
	[[ $digest == sha256:* ]] || { echo "no manifest digest in the imagetools metadata" >&2; exit 1; }
	echo "ref=$repo@$digest"
	exit 0
fi

# GITHUB_TOKEN authenticates the Dockerfile's GitHub release downloads (BuildKit secret, #145).
secret=()
[[ -z ${GITHUB_TOKEN:-} ]] || secret=(--secret "id=github_token,env=GITHUB_TOKEN")

# The commit time pins every timestamp (#176): SOURCE_DATE_EPOCH is the build arg BuildKit reads for
# the image's creation time and rewrite-timestamp clamps the layer files' mtimes to it, so one commit
# gives the same layers (hack/harness-repro-check.sh checks it). The attestations still vary.
epoch=$(git show -s --format=%ct "$commit")
[[ $epoch =~ ^[0-9]+$ ]] || { echo "no commit time for '$commit'" >&2; exit 1; }

if [[ $mode == platform ]]; then
	[[ $platform =~ ^linux/(amd64|arm64)$ ]] || { echo "platform '$platform' is not linux/amd64 or linux/arm64" >&2; exit 2; }
	# Pushed by digest, no tag: the merge step tags the joined manifest list. The registry cache is one
	# ref per platform and is never a release artifact (nothing signs, pins or documents it; it may be
	# deleted at any time). A missing or unreadable cache only costs time: cache-from falls back to a
	# full build and ignore-error keeps a failed cache export from failing the release.
	cache="$repo:buildcache-${platform#linux/}"
	output=(--output "type=image,name=$repo,push-by-digest=true,name-canonical=true,push=true,rewrite-timestamp=true"
		--platform "$platform"
		--cache-from "type=registry,ref=$cache"
		--cache-to "type=registry,ref=$cache,mode=max,ignore-error=true")
else
	exporter="type=registry,rewrite-timestamp=true"
	output=(--output "$exporter" --platform "linux/amd64,linux/arm64" "${tags[@]}")
fi

meta=$(mktemp "${TMPDIR:-/tmp}/release-harness.XXXXXX")
trap 'rm -f "$meta"' EXIT
docker buildx build "${output[@]}" \
	--build-arg "SOURCE_DATE_EPOCH=$epoch" \
	--build-context common=harnesses/common "${secret[@]}" \
	--label org.opencontainers.image.source="https://github.com/$source_repo" \
	--label org.opencontainers.image.version="$version" \
	--label org.opencontainers.image.revision="$commit" \
	--label org.opencontainers.image.licenses=Apache-2.0 \
	--sbom=true --provenance=mode=min \
	--metadata-file "$meta" harnesses/opencode >&2

digest=$(jq -r '."containerimage.digest" // empty' "$meta")
[[ $digest == sha256:* ]] || { echo "no image digest in the buildx metadata" >&2; exit 1; }
if [[ $mode == platform ]]; then echo "digest=$digest"; else echo "ref=$repo@$digest"; fi
