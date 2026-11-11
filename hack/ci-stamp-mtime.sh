#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Sets the mtime of every tracked file and directory from its content (the git blob or tree id), so a
# fresh checkout gives the same mtimes for the same content, on main and on a pull request (issue 43).
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
# stamp OID PATH: an mtime in 2020 from the first 28 bits of OID (older than Go's 2-second "too new" cutoff).
stamp() {
  local h=$((16#${1:0:7}))
  touch -h -t "$(printf '2020%02d%02d%02d%02d.%02d' $((h / 2419200 % 12 + 1)) $((h / 86400 % 28 + 1)) $((h / 3600 % 24)) $((h / 60 % 60)) $((h % 60)))" -- "$2"
}
n=0
while IFS= read -r -d '' rec; do
  read -r mode oid _ <<<"${rec%%$'\t'*}"
  [ "$mode" != 160000 ] || continue # a submodule
  stamp "$oid" "${rec#*$'\t'}"
  n=$((n + 1))
done < <(git ls-files -s -z)
# Directories too, after the files (touching a file does not change its directory's mtime, creating one
# does): hashOpen hashes an opened directory's own stat and the stat of every entry, and Glob, ReadDir
# and WalkDir open directories. The tree id changes when anything below changes. HEAD's trees, so run it
# on a clean checkout. A directory a step later writes into (the root, when go-test.sh makes reports/)
# gets a new mtime, and untracked entries (.git, reports/) are hashed too: a package that opens such a
# directory only misses the cache, it never reuses a stale result.
d=0
while IFS= read -r -d '' rec; do
  read -r _ _ oid _ <<<"${rec%%$'\t'*}"
  stamp "$oid" "${rec#*$'\t'}"
  d=$((d + 1))
done < <(git ls-tree -r -d -z HEAD)
stamp "$(git rev-parse 'HEAD^{tree}')" .
echo "ci-stamp-mtime: stamped $n files and $((d + 1)) directories" >&2
