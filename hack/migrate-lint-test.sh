#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/migrate-lint.sh against a throwaway repository: a branch may add migrations and
# may be behind its base, but must not edit, delete or rename a merged one, except an edit that
# leaves a file listed in the lint's allowed_edits at exactly its allowed content. On a patch line
# (base ref release/*, ADR 0085) a new migration must also not rename, drop or retype.
set -euo pipefail

lint=$(cd "$(dirname "$0")" && pwd)/migrate-lint.sh
allowed=$(cd "$(dirname "$0")/.." && pwd)/internal/db/migrations/00002_agents.sql
work=$(mktemp -d "${TMPDIR:-/tmp}/migrate-lint.XXXXXX")
trap 'rm -rf "$work"' EXIT
cd "$work"

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
git init -q -b main
dir=internal/db/migrations
mkdir -p "$dir"
echo a >"$dir/00001_a.sql"
echo b >"$dir/00002_agents.sql"
git add -A && git commit -qm base
git branch behind
echo c >"$dir/00003_c.sql"
git add -A && git commit -qm "merge 00003"

fail=0
expect() { # expect <exit code> <description>
  local got=0
  "$lint" "${base:-main}" >/dev/null 2>&1 || got=$?
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

git checkout -qb allowed main
cp "$allowed" "$dir/00002_agents.sql" && git commit -qam allowed
expect 0 "edits an allowed migration to its allowed content"

git checkout -qb allowed-more main
cp "$allowed" "$dir/00002_agents.sql" && echo x >>"$dir/00002_agents.sql" && git commit -qam more
expect 1 "edits an allowed migration past its allowed content"

# Patch line: the base is a release branch, cut at the current main.
git branch release/v0.1 main
base=release/v0.1
patch() { # patch <branch> <migration body> <description> <exit code>
  git checkout -qb "$1" main
  printf '%b\n' "$2" >"$dir/00004_p.sql" && git add -A && git commit -qm "$1"
  expect "$4" "patch line: $3"
}
patch p-ok 'ALTER TABLE t ADD COLUMN c int;\nCREATE INDEX i ON t (c);' "adds a column and an index" 0
patch p-comment '-- we do not DROP COLUMN or RENAME anything here\nALTER TABLE t ADD COLUMN c int;' "mentions a ban only in a comment" 0
patch p-rename 'ALTER TABLE t RENAME TO u;' "renames a table" 1
patch p-rename-col 'ALTER TABLE t\n  RENAME COLUMN a TO b;' "renames a column over two lines" 1
patch p-dropcol 'alter table t drop column a;' "drops a column (lower case)" 1
patch p-droptable 'DROP TABLE t;' "drops a table" 1
patch p-droptype 'DROP TYPE s;' "drops a type" 1
patch p-retype 'ALTER TABLE t ALTER COLUMN a TYPE bigint;' "changes a column type" 1
patch p-retype2 'ALTER TABLE t\n  ALTER COLUMN a SET DATA TYPE text;' "changes a column type (SET DATA TYPE)" 1
patch p-default 'ALTER TABLE t ALTER COLUMN a SET DEFAULT 1;\nALTER TABLE t ALTER COLUMN a DROP NOT NULL;' "changes a default and drops NOT NULL" 0

# The same migration is fine on the next minor line (base main), and the env var forces the rule.
base=main
git checkout -qb next main
echo 'ALTER TABLE t RENAME TO u;' >"$dir/00004_p.sql" && git add -A && git commit -qm next
expect 0 "rename on main (next minor line)"
git checkout -q next
MIGRATE_LINT_PATCH_LINE=1 expect 1 "rename with MIGRATE_LINT_PATCH_LINE=1"

exit $fail
