#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Old-issue reference lint (#336): the old private repository is gone, so nothing tracked may name it.
# An issue that lived there is written as plain text, "old issue 615", with no repo name and no link
# (an issue that moved gets its new number, "#122"). Scans every tracked file under the root; with a
# root that is not a git checkout it scans every file. The needle is built from two halves so this
# script and its test do not match themselves.
#
# Usage: hack/old-issue-refs-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"
needle="koinon-""private"

if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && [[ $(git rev-parse --show-toplevel) == "$(pwd -P)" ]]; then
	hits=$(git grep -nI -e "$needle" -- . || true)
else
	hits=$(grep -rnI -e "$needle" . || true)
fi

if [[ -n $hits ]]; then
	echo "FAIL: a reference to the old private repository is back; write it as plain text, 'old issue NNN' (or the new #N):" >&2
	printf '%s\n' "$hits" | head -50 >&2
	exit 1
fi
echo "old-issue refs ok"
