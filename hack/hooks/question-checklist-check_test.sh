#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/question-checklist-check.sh (issue 843). Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/question-checklist-check.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

in='{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Merge it?"}]}}'

# Off unless KOINON_MAINTAINER_HOOKS=1: nothing, exit 0.
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(env -u KOINON_MAINTAINER_HOOKS "$hook" <<<"$in") || rc=$?
  else
    out=$(KOINON_MAINTAINER_HOOKS=$v "$hook" <<<"$in") || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

out=$(KOINON_MAINTAINER_HOOKS=1 "$hook" <<<"$in")
if jq -e '.hookSpecificOutput.hookEventName == "PreToolUse" and (.hookSpecificOutput.additionalContext | test("Question checklist"))' <<<"$out" >/dev/null \
  && ! jq -e '.hookSpecificOutput.permissionDecision' <<<"$out" >/dev/null; then
  ok "on: adds the checklist, no decision"
else
  bad "on: $out"
fi

exit "$fail"
