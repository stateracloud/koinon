#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks goose migrations (ADR 0047):
#   - file names are NNNNN_snake_case.sql, so goose and sqlc agree on order
#   - numbers are unique and contiguous from 00001
#   - with a base ref argument: migrations present on the base are unchanged
#     (forward-only, ADR 0027)
set -euo pipefail

dir=internal/db/migrations
base=${1:-}
fail=0

expected=1
for f in $(ls "$dir"/*.sql | sort); do
  name=$(basename "$f")
  if [[ ! $name =~ ^([0-9]{5})_[a-z0-9_]+\.sql$ ]]; then
    echo "bad migration name: $name (want NNNNN_snake_case.sql)"; fail=1; continue
  fi
  num=$((10#${BASH_REMATCH[1]}))
  if (( num != expected )); then
    echo "migration $name: expected number $(printf %05d "$expected")"; fail=1
  fi
  expected=$((num + 1))
done

if [[ -n $base ]]; then
  # Compare with the merge base, not the base tip: migrations the base gained since the
  # branch point are not this change's. The directory pathspec (not a glob of the files
  # present here) is what lets deletions and renames show up.
  changed=$(git diff --name-only --diff-filter=MDR "$(git merge-base "$base" HEAD)" -- "$dir" || true)
  if [[ -n $changed ]]; then
    echo "merged migrations must not change:"; echo "$changed"; fail=1
  fi
fi

exit $fail
