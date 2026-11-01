#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks goose migrations (ADR 0047):
#   - file names are NNNNN_snake_case.sql, so goose and sqlc agree on order
#   - numbers are unique and contiguous from 00001
#   - with a base ref argument: migrations present on the base are unchanged
#     (forward-only, ADR 0027), except the one-time edits listed in allowed_edits
#   - on a patch line (ADR 0085, decisions 5 and 6): migrations the branch adds must not contain
#     RENAME, DROP COLUMN, DROP TABLE, DROP TYPE or a column type change, so the previous patch
#     release's binaries keep working on the new schema (expand now, contract in the next minor).
#
# A patch line is a base ref named release/<anything> (for example origin/release/v0.1) or
# MIGRATE_LINT_PATCH_LINE=1. A base of main (the next minor line) may contain them. Patch releases
# are cut only from release/vX.Y branches, so the branch name is the marker.
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

# Statements that break the previous patch release's binaries. Matched case-insensitively on each
# statement with comments removed and whitespace collapsed.
contract_re='(^|[^a-z0-9_])(rename[[:space:]]|drop[[:space:]]+(column|table|type)[[:space:]]|alter[[:space:]]+column[[:space:]]+[^[:space:]]+[[:space:]]+(set[[:space:]]+data[[:space:]]+)?type[[:space:]])'
patch_line() {
  [[ ${MIGRATE_LINT_PATCH_LINE:-} == 1 ]] && return 0
  [[ $base == release/* || $base == */release/* ]]
}

if [[ -n $base ]] && patch_line; then
  added=$(git diff --name-only --diff-filter=A "$(git merge-base "$base" HEAD)" -- ":(glob)$dir/*.sql" || true)
  for f in $added; do
    # One statement per record: drop "--" comments, split on ";", collapse whitespace.
    hits=$(sed -E 's/--.*$//' "$f" | awk -v RS=';' '{ gsub(/[[:space:]]+/, " "); print }' |
      grep -iE "$contract_re" || true)
    if [[ -n $hits ]]; then
      echo "$f: breaks the previous patch release on a patch line (ADR 0085): expand now, contract in the next minor"
      echo "$hits" | cut -c1-120 | sed 's/^/    /'
      fail=1
    fi
  done
fi

exit $fail
