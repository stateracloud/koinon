#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): runs in the upstream release workflow (job `test-images`).
#
# Publishes the TEST images of a release candidate to <registry> (ghcr.io/<owner>/<repo>), built from the
# tagged commit, for the lab drills (#928). The lab cannot build images; it mirrors these by digest:
#   <registry>/koinon-fakemodel   the fake model server (cmd/koinon-fakemodel; test/drill's fakeModelImage)
#   <registry>/koinon-fakemcp     the fake MCP server (cmd/koinon-fakemcp; test/drill's fakeMcpImage)
#   <registry>/rt1-*              the RT1 fixture images (hack/rt1-fixtures-build.sh, test/fixtures/rt1)
# They are not product images: nothing installs them, the chart does not reference them, and they carry
# no support promise. Tags are the release version without the "v" (the release images' rule).
#
# Usage: hack/release-test-images.sh REGISTRY TAG OUT
#   TAG is the git tag (v0.1.0-rc.2); OUT is a file that gets one "<name> <repository>@sha256:<hex>" line per
#   image, in the shape of hack/release-refs.sh, for the sign job. You must be logged in to the registry, and
#   ko, crane and docker buildx must be on the PATH (the workflow installs them).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

registry=${1:?usage: $0 REGISTRY TAG OUT}
git_tag=${2:?usage: $0 REGISTRY TAG OUT}
out=${3:?usage: $0 REGISTRY TAG OUT}
registry=${registry%/}
tag=${git_tag#v}
[[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || { echo "release test images: '$git_tag' is not a release tag" >&2; exit 1; }
root=$(cd "$(dirname "$0")/.." && pwd)
refs=$(mktemp "${TMPDIR:-/tmp}/release-test-images.XXXXXX") || exit 1
trap 'rm -f "$refs"' EXIT

# The fake servers the drills need, one ko image each (cmd/<name>). test/drill's Validate() requires an image
# for each of them; hack/release-test-images-test.sh checks that every image the topology requires is here.
fakes=(koinon-fakemodel koinon-fakemcp)

# Multi-arch ko images (the kind drill runs on either architecture).
for f in "${fakes[@]}"; do
	(cd "$root" && KO_DOCKER_REPO="$registry/$f" ko build --bare --platform linux/amd64,linux/arm64 --tags "$tag" "./cmd/$f") >&2
	digest=$(crane digest "$registry/$f:$tag")
	[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "release test images: no digest for $f:$tag" >&2; exit 1; }
	echo "$f $registry/$f@$digest" >>"$refs"
done

# The RT1 fixtures, under <registry>/rt1-* (RT1_REPO). The script prints a kustomize images block with a digest
# per image at the end; the refs are read from it.
fixtures=$(RT1_REPO=$registry "$root/hack/rt1-fixtures-build.sh" "$registry" "$tag") || exit 1
n=0
name=
while read -r key value; do
	case $key in
	-) name=${value#name: } ;;
	digest:)
		[[ -n $name && $value =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "release test images: unreadable fixtures output near '$name $value'" >&2; exit 1; }
		echo "${name##*/} $name@$value" >>"$refs"
		# The copies of upstream images keep their upstream tag; every image also gets the release version.
		crane tag "$name@$value" "$tag" >&2
		n=$((n + 1))
		name=
		;;
	esac
done <<<"$fixtures"
((n == 8)) || { echo "release test images: want 8 RT1 fixture images, got $n" >&2; exit 1; }

cp "$refs" "$out"
echo "release test images: $((n + ${#fakes[@]})) images for $tag in $out" >&2
