#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/agent-spawn-check.sh (old issue 397). Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/agent-spawn-check.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
export KOINON_MAINTAINER_HOOKS=1

# run <name> <want: deny|allow> <subagent_type|-> <model|-> <prompt>
run() {
  local name=$1 want=$2 type=$3 model=$4 prompt=$5 json out got
  json=$(jq -nc --arg t "$type" --arg m "$model" --arg p "$prompt" \
    '{tool_name:"Agent",tool_input:({prompt:$p}
      + (if $t == "-" then {} else {subagent_type:$t} end)
      + (if $m == "-" then {} else {model:$m} end))}')
  out=$("$hook" <<<"$json")
  if [ -z "$out" ]; then got=allow; else got=$(jq -r '.hookSpecificOutput.permissionDecision' <<<"$out"); fi
  if [ "$got" = "$want" ]; then ok "$name"; else bad "$name (got $got, want $want; output: $out)"; fi
}

brief="Follow docs/agents/subagent-brief.md. Task: x"
run "no model denies" deny general-purpose - "$brief"
run "no brief denies" deny general-purpose sonnet "Task: x"
run "model and brief allow" allow general-purpose sonnet "$brief"
run "default type with model and brief allows" allow - sonnet "$brief"
run "fork allows without model or brief" allow fork - "Task: x"
run "Explore with model and no brief allows" allow Explore haiku "Find x"
run "Explore without model denies" deny Explore - "Find x"

out=$("$hook" <<<'{"tool_input":{"subagent_type":"general-purpose","prompt":"x"}}')
if [ "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$out")" = PreToolUse ] \
  && [ -n "$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")" ]; then
  ok "deny output shape"
else
  bad "deny output shape ($out)"
fi

# Off unless KOINON_MAINTAINER_HOOKS=1 (issue 843): no output and exit 0 for a spawn it would refuse.
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(env -u KOINON_MAINTAINER_HOOKS "$hook" <<<'{"tool_input":{"prompt":"x"}}') || rc=$?
  else
    out=$(KOINON_MAINTAINER_HOOKS=$v "$hook" <<<'{"tool_input":{"prompt":"x"}}') || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

exit "$fail"
