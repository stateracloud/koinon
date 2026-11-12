#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Print the image tag of the escape suite's probe image (#482): probe-<first 12 hex of a sha256>. The hash
# covers every tracked and untracked file of hack/spike/probe (name, executable bit, content) and the
# `ARG BASE=` line of harnesses/opencode/Dockerfile, the base the probe is built on (test/escape/kind.sh).
# Same inputs, same tag; any change gets its own tag. The ci.yml harness-publish job pushes the image
# under this tag, and the kind e2e pulls it (PROBE_PULL_REPO), so a run needs no apt mirror (the
# Dockerfile's apt step runs once, on main, not in every e2e). Same scheme as hack/harness-tag.sh.
# Usage: hack/probe-tag.sh [ROOT]   (ROOT: the tree to hash, default the repository of this script)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export LC_ALL=C # byte-order collation, as in harness-tag.sh (old issue 335)

cd "${1:-$(dirname "$0")/..}"
dir=hack/spike/probe
[ -d "$dir" ] || { echo "probe-tag.sh: no $dir" >&2; exit 2; }
base=$(sed -n 's/^ARG BASE=//p' harnesses/opencode/Dockerfile | head -n 1)
[ -n "$base" ] || { echo "probe-tag.sh: no ARG BASE= in harnesses/opencode/Dockerfile" >&2; exit 2; }

hash=$(
	printf 'base %s\n' "$base"
	git ls-files -z --cached --others --exclude-standard --deduplicate -- "$dir" | sort -z |
		while IFS= read -r -d '' f; do
			[ -f "$f" ] || continue # tracked but deleted
			mode=-
			[ -x "$f" ] && mode=x
			printf '%s %s %s\n' "$mode" "$(shasum -a 256 <"$f" | cut -d' ' -f1)" "$f"
		done
)
hash=$(shasum -a 256 <<<"$hash" | cut -d' ' -f1)
echo "probe-${hash:0:12}"
