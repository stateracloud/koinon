#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks the e2e command files (test/e2e/commands/*.e1) and the scripts that run them (ADR 0108). Needs
# bash, awk and grep; it never talks to a cluster. The docs site is in stateracloud/koinon-docs, which
# compares its pages with these files in its own CI; nothing here reads a docs page.
#
#   0. The offline tests of hack/docs-e1.sh (the reader of the command files) pass.
#   1. Every block of every command file is valid shell (bash -n).
#   2. test/e2e/run.sh runs every block of quickstart.e1, and only blocks that exist.
#   3. test/e2e/backup-restore.sh runs only blocks of backup-restore.e1, and every block it does not run
#      (the pipe blocks: they need a source and a target install) is run by test/drill/drill_test.go.
#   4. The guide files (test/docs-examples/*.e1, run by hack/docs-examples.sh against a build) parse,
#      and the offline tests of hack/docs-examples.sh pass. Running the guides is the `docs examples` job.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
cmds=test/e2e/commands
quickcmds=$cmds/quickstart.e1
backupcmds=$cmds/backup-restore.e1

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
ok() { echo "ok: $*"; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-commands-check.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# parses <command file>: every block is valid shell.
parses() {
  local file=$1 n list
  list=$(E1_FILE="$file" hack/docs-e1.sh names) || { fail "reading the e1 blocks of $file"; return; }
  for n in $list; do
    E1_FILE="$file" hack/docs-e1.sh show "$n" >"$tmp/$n.sh" || { fail "e1 block $n of $file is empty"; continue; }
    bash -n "$tmp/$n.sh" || fail "e1 block $n of $file is not valid shell"
  done
}

# 0. The offline tests of hack/docs-e1.sh.
if hack/docs-e1-test.sh >"$tmp/e1test.out" 2>&1; then
  ok "hack/docs-e1-test.sh passes"
else
  cat "$tmp/e1test.out" >&2
  fail "hack/docs-e1-test.sh fails"
fi

# 1. The command files parse.
names=$(E1_FILE="$quickcmds" hack/docs-e1.sh names) || { fail "reading the e1 blocks of $quickcmds"; names=""; }
[[ -n $names ]] || fail "$quickcmds has no e1 blocks"
gnames=$(E1_FILE="$backupcmds" hack/docs-e1.sh names) || { fail "reading the e1 blocks of $backupcmds"; gnames=""; }
[[ -n $gnames ]] || fail "$backupcmds has no e1 blocks"
parses "$quickcmds"
parses "$backupcmds"
[[ $failures -eq 0 ]] && ok "every e1 block of $quickcmds and $backupcmds is valid shell"

# 2. run.sh runs exactly the e1 blocks of the quickstart file.
used=$(grep -v '^[[:space:]]*#' test/e2e/run.sh | grep -oE '(\be1 |docs-e1\.sh" run )[a-z0-9-]+' | awk '{print $NF}' | sort -u)
for n in $used; do
  grep -qx "$n" <<<"$names" || fail "test/e2e/run.sh runs e1 block '$n', which $quickcmds does not have"
done
for n in $names; do
  grep -qx "$n" <<<"$used" || fail "test/e2e/run.sh does not run e1 block '$n'"
done
[[ $failures -eq 0 ]] && ok "test/e2e/run.sh runs every e1 block: $(tr '\n' ' ' <<<"$used")"

# 3. test/e2e/backup-restore.sh (the nightly step, #90) runs exactly the blocks of the backup file.
gused=$(grep -v '^[[:space:]]*#' test/e2e/backup-restore.sh | grep -oE '\bblk [a-z0-9-]+' | awk '{print $NF}' | sort -u)
for n in $gused; do
  grep -qx "$n" <<<"$gnames" || fail "test/e2e/backup-restore.sh runs e1 block '$n', which $backupcmds does not have"
done
for n in $gnames; do
  grep -qx "$n" <<<"$gused" && continue
  # Not run in place: the restore drill (ADR 0109) must run it, by name.
  grep -q "\"$n\"" test/drill/drill_test.go || fail "neither test/e2e/backup-restore.sh nor test/drill/drill_test.go runs e1 block '$n' of $backupcmds"
done
[[ $failures -eq 0 ]] && ok "every e1 block of the backup file is run by test/e2e/backup-restore.sh or the drill: $(tr '\n' ' ' <<<"$gused")"

# 4. The guide files parse; the runner's own tests pass.
for g in test/docs-examples/*.e1; do
  [[ -e $g ]] || { fail "test/docs-examples has no .e1 file"; break; }
  [[ -n $(E1_FILE="$g" hack/docs-e1.sh names) ]] || fail "$g has no e1 blocks"
  parses "$g"
done
if hack/docs-examples-test.sh >"$tmp/ex.out" 2>&1; then
  ok "hack/docs-examples-test.sh passes"
else
  cat "$tmp/ex.out" >&2
  fail "hack/docs-examples-test.sh fails"
fi

if [[ $failures -ne 0 ]]; then
  echo "e2e-commands-check: $failures problem(s)" >&2
  exit 1
fi
echo "e2e-commands-check: all checks passed"
