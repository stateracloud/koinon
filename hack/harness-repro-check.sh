#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks that HEAD builds one harness image twice with the same digest (#176): the harness's
# reproducibility check. Both builds run on a fresh builder with no layer cache, for one platform,
# with SOURCE_DATE_EPOCH set to the HEAD commit time and BuildKit's rewrite-timestamp, the same two
# settings hack/release-harness.sh uses. Attestations are off: provenance and SBOM carry build data
# by design, and the check compares the image itself. Nothing is pushed or loaded.
#
#   hack/harness-repro-check.sh [harness [platform]]     (opencode, host arch)
#
# The build reads the working tree, so the script refuses to run when harnesses/ has changes that HEAD
# does not have: the result would describe a tree that is not the commit.
# Needs docker with buildx. GITHUB_TOKEN, when set, is the BuildKit secret github_token (#145).
# Exit 0: equal digests. Exit 1: they differ (the two image layouts are kept and named, to diff).
# Exit 2: bad usage or a dirty harnesses/ tree.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if (($# > 2)); then
	echo "usage: $0 [harness [platform]]" >&2
	exit 2
fi
harness=${1:-opencode}
case $(uname -m) in
arm64 | aarch64) host=linux/arm64 ;;
*) host=linux/amd64 ;;
esac
platform=${2:-$host}
commit=HEAD
cd "$(dirname "$0")/.."

[[ -f harnesses/$harness/Dockerfile ]] || { echo "no harnesses/$harness/Dockerfile" >&2; exit 2; }
if [[ -n $(git status --porcelain harnesses/) ]]; then
	echo "harnesses/ has uncommitted or untracked changes; the build reads the working tree, so this would not check HEAD. Commit or remove them first." >&2
	exit 2
fi
epoch=$(git show -s --format=%ct "$commit")
[[ $epoch =~ ^[0-9]+$ ]] || { echo "no commit time for '$commit'" >&2; exit 2; }

secret=()
[[ -z ${GITHUB_TOKEN:-} ]] || secret=(--secret "id=github_token,env=GITHUB_TOKEN")

work=$(mktemp -d "${TMPDIR:-/tmp}/harness-repro.XXXXXX")
builder=harness-repro-$$
cleanup() {
	docker buildx rm --force "$builder" >/dev/null 2>&1 || true
	[[ -n ${keep:-} ]] || rm -rf "$work"
}
trap cleanup EXIT
# A builder of its own starts with an empty cache and does not depend on the caller's driver (the
# default docker driver cannot write an OCI layout).
docker buildx create --name "$builder" --driver docker-container >/dev/null

digests=()
for n in 1 2; do
	# No layer cache: --no-cache, and the builder is new. The same epoch both times.
	docker buildx build --builder "$builder" --no-cache --platform "$platform" \
		--build-arg "SOURCE_DATE_EPOCH=$epoch" --provenance=false --sbom=false \
		--build-context common=harnesses/common "${secret[@]}" \
		--output "type=oci,dest=$work/build$n.tar,rewrite-timestamp=true" "harnesses/$harness" >&2
	digest=$(tar -xOf "$work/build$n.tar" index.json | jq -r '.manifests[0].digest // empty')
	[[ $digest == sha256:* ]] || { echo "no image digest in build $n's OCI layout" >&2; exit 1; }
	echo "build $n: $digest" >&2
	digests+=("$digest")
done

if [[ ${digests[0]} != "${digests[1]}" ]]; then
	keep=1
	echo "harnesses/$harness at $commit ($platform, SOURCE_DATE_EPOCH=$epoch) is NOT reproducible:" >&2
	echo "  ${digests[0]}  $work/build1.tar" >&2
	echo "  ${digests[1]}  $work/build2.tar" >&2
	echo "Compare the layers (tar -tvf on each layer blob, or diffoci) to find the differing file." >&2
	exit 1
fi
# Equal: the layouts are no longer needed (they are large); free the disk now, not at exit.
rm -f "$work/build1.tar" "$work/build2.tar"
echo "harnesses/$harness at $commit ($platform, SOURCE_DATE_EPOCH=$epoch): ${digests[0]} twice" >&2
