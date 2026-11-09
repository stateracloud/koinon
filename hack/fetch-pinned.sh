#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Download one pinned file and check its sha256 (#431). Every CI download of a pinned tool or
# manifest from a GitHub release goes through here (or the composite action
# .github/actions/fetch-pinned, which caches the result): github.com/.../releases/download answers
# 504 now and then, so curl retries 5 times, also on HTTP errors, and a file that is already there
# with the right sha256 (a cache hit) is not fetched again. A wrong sha256 is never retried.
#
# Usage: hack/fetch-pinned.sh URL SHA256 DEST

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

[ $# -eq 3 ] || { echo "usage: hack/fetch-pinned.sh URL SHA256 DEST" >&2; exit 2; }
url=$1 want=$2 dest=$3

sha_ok() { # sha_ok FILE: sha256 of FILE equals $want (GNU sha256sum, or shasum on macOS)
	if command -v sha256sum >/dev/null 2>&1; then
		echo "${want}  $1" | sha256sum -c - >/dev/null 2>&1
	else
		echo "${want}  $1" | shasum -a 256 -c - >/dev/null 2>&1
	fi
}

if [ -f "$dest" ] && sha_ok "$dest"; then
	echo "fetch-pinned: $dest already present with the pinned sha256"
	exit 0
fi

mkdir -p "$(dirname "$dest")"
tmp=$(mktemp "${dest}.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL --retry 5 --retry-all-errors --retry-delay "${FETCH_PINNED_RETRY_DELAY:-5}" -o "$tmp" "$url"
sha_ok "$tmp" || { echo "fetch-pinned: sha256 mismatch for $url (want $want)" >&2; exit 1; }
mv "$tmp" "$dest"
trap - EXIT
echo "fetch-pinned: fetched $url"
