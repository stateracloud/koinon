#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/migrate-lint.sh against a throwaway repository: a branch may add migrations and
# may be behind its base, but must not edit, delete or rename a merged one.
set -euo pipefail

lint=$(cd "$(dirname "$0")" && pwd)/migrate-lint.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
git init -q -b main
dir=internal/db/migrations
mkdir -p "$dir"
echo a >"$dir/00001_a.sql"
echo b >"$dir/00002_b.sql"
git add -A && git commit -qm base
git branch behind
echo c >"$dir/00003_c.sql"
git add -A && git commit -qm "merge 00003"

fail=0
expect() { # expect <exit code> <description>
  local got=0
  "$lint" main >/dev/null 2>&1 || got=$?
  if [[ $got -ne $1 ]]; then echo "FAIL: $2 (exit $got, want $1)"; fail=1; else echo "ok: $2"; fi
  git checkout -q main
}

git checkout -q behind
expect 0 "branch behind its base"

git checkout -qb add main
echo d >"$dir/00004_d.sql" && git add -A && git commit -qm add
expect 0 "adds a migration"

git checkout -qb edit main
echo x >>"$dir/00001_a.sql" && git commit -qam edit
expect 1 "edits a merged migration"

git checkout -qb delete main
git rm -q "$dir/00003_c.sql" && git commit -qm delete
expect 1 "deletes a merged migration"

git checkout -qb rename main
git mv "$dir/00003_c.sql" "$dir/00003_renamed.sql" && git commit -qm rename
expect 1 "renames a merged migration"

exit $fail
