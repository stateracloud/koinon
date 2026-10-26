#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks goose migrations (ADR 0047):
#   - file names are NNNNN_snake_case.sql, so goose and sqlc agree on order
#   - numbers are unique and contiguous from 00001
#   - with a base ref argument: migrations present on the base are unchanged
#     (forward-only, ADR 0027), except the one-time edits listed in allowed_edits
set -euo pipefail

dir=internal/db/migrations
base=${1:-}
fail=0

# One-time exceptions to forward-only: "<file> <git blob of the allowed content>". A file passes
# only while its content is exactly that blob, so any further edit fails again.
#   - 00002_agents.sql: agent_flavor narrowed to ('opencode') before v0.1 shipped (ADR 0059).
allowed_edits=(
  "$dir/00002_agents.sql 7104fa8dc2cb5a64fbdc924da92feb49073cb1e1"
)
allowed() {
  local f=$1 entry
  for entry in "${allowed_edits[@]}"; do
    [[ $entry == "$f "* && -f $f && $(git hash-object "$f") == "${entry#* }" ]] && return 0
  done
  return 1
}

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
  # branch point are not this change's. A pathspec glob (not a list of the files present
  # here) is what lets deletions and renames show up. Only the .sql files are migrations;
  # the Go files next to them may change.
  changed=$(git diff --name-only --diff-filter=MDR "$(git merge-base "$base" HEAD)" -- ":(glob)$dir/*.sql" || true)
  changed=$(for f in $changed; do allowed "$f" || echo "$f"; done)
  if [[ -n $changed ]]; then
    echo "merged migrations must not change:"; echo "$changed"; fail=1
  fi
fi

exit $fail
