#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/migrate-renumber.sh in a throwaway repository (no `make generate`).
set -euo pipefail

tool=$(cd "$(dirname "$0")" && pwd)/migrate-renumber.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
git init -q -b main
dir=internal/db/migrations
mkdir -p "$dir"
echo a >"$dir/00001_a.sql"; echo b >"$dir/00002_b.sql"
git add -A && git commit -qm base
git checkout -q -b feature
echo c >"$dir/00003_mine.sql"; echo d >"$dir/00004_mine_too.sql"
git add -A && git commit -qm "branch migrations"
git checkout -q main
echo e >"$dir/00003_theirs.sql"
git add -A && git commit -qm "main gains 00003"
git checkout -q feature
git merge -q --no-edit main
fail=0
check() { if [[ $2 == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1 (got '$2', want '$3')"; fail=1; fi; }

check "collision before" "$(/bin/ls $dir | paste -sd, -)" "00001_a.sql,00002_b.sql,00003_mine.sql,00003_theirs.sql,00004_mine_too.sql"
"$tool" --no-generate main >/dev/null
check "branch migrations follow main's" "$(/bin/ls $dir | paste -sd, -)" "00001_a.sql,00002_b.sql,00003_theirs.sql,00004_mine.sql,00005_mine_too.sql"
check "contents kept" "$(cat $dir/00004_mine.sql)$(cat $dir/00005_mine_too.sql)" "cd"
check "main's file untouched" "$(cat $dir/00003_theirs.sql)" "e"
out=$("$tool" --no-generate main)
check "idempotent" "$out" "migrate-renumber: already in order after 00003"
git commit -qam renumbered
git checkout -q main
check "no added migrations" "$("$tool" --no-generate main)" "migrate-renumber: this branch adds no migrations"
exit "$fail"
