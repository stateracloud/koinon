#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Print the image tag for a harness: dev-<first 12 hex of a sha256 over the harness's build context>.
# The context is harnesses/<harness> (its Dockerfile included) and harnesses/common, as they are in the
# working tree: tracked and untracked files, not ignored ones, with their executable bit. Two trees
# with the same context get the same tag; any difference gets its own, so worktrees building from
# different branches never overwrite each other's image. Used by the Makefile and test/harnesses.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

# Byte-order collation everywhere: sort (and every other comparison in the hash path) must not
# depend on the caller's locale, or a UTF-8 shell computes a different tag than CI (old issue 335).
export LC_ALL=C

harness=${1:?usage: harness-tag.sh <harness>}
cd "$(dirname "$0")/.."
[ -d "harnesses/$harness" ] || { echo "harness-tag.sh: no such harness: $harness" >&2; exit 2; }

paths=("harnesses/$harness")
[ -d harnesses/common ] && paths+=(harnesses/common)

hash=$(
	git ls-files -z --cached --others --exclude-standard --deduplicate -- "${paths[@]}" | sort -z |
		while IFS= read -r -d '' f; do
			[ -f "$f" ] || continue # tracked but deleted
			mode=-
			[ -x "$f" ] && mode=x
			printf '%s %s %s\n' "$mode" "$(shasum -a 256 <"$f" | cut -d' ' -f1)" "$f"
		done | shasum -a 256 | cut -d' ' -f1
)
echo "dev-${hash:0:12}"
