#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/codegraph-prompt-check.sh with a stub `codegraph` (issue 843). Needs
# only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/codegraph-prompt-check.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cgp.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

# A stub codegraph that answers every prompt-hook call with one marker line.
mkdir "$tmp/bin"
printf '#!/bin/sh\ncat >/dev/null\necho CODEGRAPH-SNIPPETS\n' >"$tmp/bin/codegraph"
chmod +x "$tmp/bin/codegraph"
export PATH="$tmp/bin:$PATH"
export CLAUDE_PROJECT_DIR=$tmp/main

long='please explain how the scheduler picks the next run for an agent in this repository'
mkin() { jq -nc --arg p "$1" --arg c "$2" '{prompt:$p,cwd:$c}'; }

# Off unless KOINON_MAINTAINER_HOOKS=1: nothing, exit 0, for a prompt it would pass on.
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(mkin "$long" "$tmp/wt" | env -u KOINON_MAINTAINER_HOOKS "$hook") || rc=$?
  else
    out=$(mkin "$long" "$tmp/wt" | KOINON_MAINTAINER_HOOKS=$v "$hook") || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

export KOINON_MAINTAINER_HOOKS=1

# check <name> <want: pass|skip> <prompt> <cwd>
check() {
  local out got
  out=$(mkin "$3" "$4" | "$hook")
  got=skip
  [[ $out == *CODEGRAPH-SNIPPETS* ]] && got=pass
  if [ "$got" = "$2" ]; then ok "$1"; else bad "$1 (want $2, got $got)"; fi
}
check "long prompt in a worktree is passed on" pass "$long" "$tmp/wt"
check "short prompt is skipped" skip "ok go on" "$tmp/wt"
check "main checkout is skipped" skip "$long" "$tmp/main"
check "main checkout with a trailing slash is skipped" skip "$long" "$tmp/main/"
check "task notification is skipped" skip "<task-notification> $long" "$tmp/wt"
check "subagent hand-back is skipped" skip "[Subagent hand-back] $long" "$tmp/wt"

printf '#!/bin/sh\nexit 1\n' >"$tmp/bin/codegraph"
out=$(mkin "$long" "$tmp/wt" | "$hook") || bad "a failing codegraph must not fail the hook"
if [ -z "$out" ]; then ok "failing codegraph: silent, exit 0"; else bad "failing codegraph printed: $out"; fi

exit "$fail"
