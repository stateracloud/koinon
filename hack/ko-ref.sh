#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Reads the stdout of `ko build` on stdin and prints "<repository>@sha256:<digest>", with no tag.
# ko prints "<repository>@sha256:<digest>", and "<repository>:<tag>@sha256:<digest>" when `--tags` names
# one (the release candidate build of ate-env-guest, #1219). Both forms are accepted. When ko prints more
# than one line, the last line that holds a digest reference is used. Anything else fails with exit 1.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

input=$(cat)
ref=
# <name>[:<tag>]@sha256:<64 lower-case hex>; the name has one or more path elements, a registry may carry a port.
re='^([a-z0-9][a-z0-9./:-]*)(@sha256:[0-9a-f]{64})$'
while IFS= read -r line; do
	if [[ $line =~ $re ]]; then
		repo=${BASH_REMATCH[1]}
		# Only a colon in the last path element is a tag; a colon before it is a registry port.
		[[ ${repo##*/} == *:* ]] && repo=${repo%:*}
		ref=$repo${BASH_REMATCH[2]}
	fi
done <<<"$input"

if [[ -z $ref ]]; then
	echo "ko-ref: no '<repository>[:<tag>]@sha256:<64 hex>' line in the ko output: ${input:-<empty>}" >&2
	exit 1
fi
echo "$ref"
