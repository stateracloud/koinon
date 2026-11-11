#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Sets the mtime of every tracked file from its content (the git blob id), so a fresh checkout gives
# the same mtimes for the same content, on main and on a pull request (issue 43).
#
# Why: Go's test cache hashes a file a test opens by size, mode and mtime, not by content
# (cmd/go hashOpen). actions/checkout stamps every file with the checkout time, so without this a
# package that reads testdata, golden files or the chart never prints `(cached)` on a CI run. A fixed
# epoch would be wrong the other way round: a same-size edit of a testdata file would keep its hash
# and show a stale pass. The mtime here follows the blob id, so changed content gets another mtime.
# Used by the `go-test` and `warm-caches` jobs after checkout; run it only in a throw-away checkout.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail
cd "${1:-.}"
export TZ=UTC
n=0
while IFS= read -r -d '' rec; do
  meta=${rec%%$'\t'*}
  path=${rec#*$'\t'}
  read -r mode oid _ <<<"$meta"
  [ "$mode" != 160000 ] || continue # a submodule
  h=$((16#${oid:0:7}))
  stamp=$(printf '2020%02d%02d%02d%02d.%02d' $((h / 2419200 % 12 + 1)) $((h / 86400 % 28 + 1)) $((h / 3600 % 24)) $((h / 60 % 60)) $((h % 60)))
  touch -h -t "$stamp" -- "$path"
  n=$((n + 1))
done < <(git ls-files -s -z)
echo "ci-stamp-mtime: stamped $n files" >&2
