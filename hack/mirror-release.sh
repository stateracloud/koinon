#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Copies one release (plan item REL2) from ghcr.io/stateracloud/koinon to another registry: the images
# api, reconciler, agentd and sandbox with every platform (BuildKit's
# attestation manifests included), their cosign signatures, attestations and SBOM tags when present
# (not referrers-API-only bundles), and the Helm chart. It needs crane and jq and reads credentials
# from the docker config (crane uses the same keychain as docker); it never prints them.
#
#   hack/mirror-release.sh 0.1.0-rc.1 registry.example.com/koinon
#
# The images land at <registry>/<name>:<version> and the chart at <registry>/charts/koinon:<version>,
# so install with --set image.registry=<registry>. SOURCE overrides the source registry (tests).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if (($# != 2)); then
	echo "usage: $0 <version> <registry>   (for example 0.1.0-rc.1 registry.example.com/koinon)" >&2
	exit 2
fi
version=${1#v}
dest=${2%/}
src=${SOURCE:-ghcr.io/stateracloud/koinon}

[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	{ echo "version '$1' is not X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is not installed" >&2; exit 1; }
command -v crane >/dev/null || { echo "crane is not installed (https://github.com/google/go-containerregistry)" >&2; exit 1; }

# copy_one <source repo> <destination repo>: the version tag, then cosign's tags for every digest
# the tag references (the index and its platform manifests): sha256-<hex>.sig, .att and .sbom (cosign
# 2 and ko) and the bare sha256-<hex> fallback tag that cosign 3 uses for its bundles. Bundles that
# exist only through the registry's referrers API, without that tag, are not copied.
copy_one() {
	local from=$1 to=$2 digests d hex suffix
	echo "copy $from:$version -> $to:$version"
	crane copy "$from:$version" "$to:$version"
	digests=$(crane digest "$from:$version")
	digests+=$'\n'$(crane manifest "$from:$version" | jq -r '.manifests[]?.digest')
	for d in $digests; do
		hex=${d#sha256:}
		for suffix in "" .sig .att .sbom; do
			if crane manifest "$from:sha256-$hex$suffix" >/dev/null 2>&1; then
				crane copy "$from:sha256-$hex$suffix" "$to:sha256-$hex$suffix"
			fi
		done
	done
}

for name in api reconciler agentd sandbox; do
	copy_one "$src/$name" "$dest/$name"
done
copy_one "$src/charts/koinon" "$dest/charts/koinon"
echo "mirrored $version to $dest"
