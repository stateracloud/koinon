#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/grilling-question-check.sh (issue 843). Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/grilling-question-check.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

json() { jq -nc --arg s "$1" '{tool_name:"Skill",tool_input:{skill:$s}}'; }

# Off unless KOINON_MAINTAINER_HOOKS=1: nothing, exit 0, even for the grilling skill.
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(json grilling | env -u KOINON_MAINTAINER_HOOKS "$hook") || rc=$?
  else
    out=$(json grilling | KOINON_MAINTAINER_HOOKS=$v "$hook") || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

export KOINON_MAINTAINER_HOOKS=1
for s in grilling mattpocock-skills:grilling; do
  out=$(json "$s" | "$hook")
  if jq -e '.hookSpecificOutput.hookEventName == "PreToolUse" and (.hookSpecificOutput.additionalContext | test("AskUserQuestion"))' <<<"$out" >/dev/null \
    && ! jq -e '.hookSpecificOutput.permissionDecision' <<<"$out" >/dev/null; then
    ok "$s: adds the rule, no decision"
  else
    bad "$s: $out"
  fi
done
for s in tdd "" grilling-notes; do
  out=$(json "$s" | "$hook")
  if [ -z "$out" ]; then ok "other skill '$s': silent"; else bad "other skill '$s': $out"; fi
done

exit "$fail"
