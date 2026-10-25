#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Reads GoReleaser's dist/digests.txt on stdin and prints "<name> <repository>@sha256:<hex>" per image,
# for the release workflow's sign job. A line is "<hex>  <repository>" for the final release's two
# tags and "<hex>  <repository>:<tag>" when ko has a single tag (a release candidate).
set -euo pipefail

while read -r hex repo; do
	[[ -n $hex ]] || continue
	ref=$repo
	[[ ${repo##*/} == *:* ]] && ref=${repo%:*} # only a colon in the last path element is a tag
	echo "${ref##*/} $ref@sha256:$hex"
done
