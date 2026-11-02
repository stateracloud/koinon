#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the GitHub handles in the maintainers table of a MAINTAINERS.md (default ./MAINTAINERS.md,
# or stdin with "-"), one per line: the "GitHub" column of the first table under "# Maintainers".
# Links elsewhere in the file (reviewers, emeritus, thanks) are not approvers. The dependency-age job
# (ADR 0079) calls this on the base branch's copy.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
src=${1:-MAINTAINERS.md}
[[ $src == - ]] && src=/dev/stdin
awk -F'|' '
  /^# Maintainers[[:space:]]*$/ { in_sec = 1; next }
  in_sec && /^#/ { exit }
  in_sec && /^\|/ {
    intable = 1
    if (!col) {
      for (i = 2; i < NF; i++) { h = $i; gsub(/^[ \t]+|[ \t]+$/, "", h); if (tolower(h) == "github") col = i }
      next
    }
    if ($2 ~ /^[ \t]*-+[ \t]*$/) next
    cell = $col
    if (match(cell, /@[A-Za-z0-9][A-Za-z0-9-]*/)) print substr(cell, RSTART + 1, RLENGTH - 1)
    next
  }
  intable { exit }
' "$src"
