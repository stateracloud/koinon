#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/prose-question-stop-check.sh against small fake transcripts (issue 843).
# Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/prose-question-stop-check.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/pqs.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
export KOINON_MAINTAINER_HOOKS=1

user() { jq -nc --arg t "$1" '{type:"user",message:{role:"user",content:$t}}'; }
toolres() { jq -nc '{type:"user",message:{role:"user",content:[{type:"tool_result",tool_use_id:"x",content:"ok"}]}}'; }
say() { jq -nc --arg t "$1" '{type:"assistant",message:{role:"assistant",content:[{type:"text",text:$t}]}}'; }
tool() { jq -nc --arg n "$1" '{type:"assistant",message:{role:"assistant",content:[{type:"tool_use",id:"x",name:$n,input:{}}]}}'; }
side() { jq -nc --arg t "$1" '{type:"assistant",isSidechain:true,message:{role:"assistant",content:[{type:"text",text:$t}]}}'; }

# run <name> <want: block|pass> <stop_hook_active: true|false> <transcript lines...>
run() {
  local name=$1 want=$2 active=$3 got out
  shift 3
  printf '%s\n' "$@" >"$tmp/t.jsonl"
  out=$(jq -nc --arg p "$tmp/t.jsonl" --argjson a "$active" '{transcript_path:$p,stop_hook_active:$a}' | "$hook")
  got=pass
  [[ $out == *'"block"'* ]] && got=block
  if [[ $got == "$want" ]]; then ok "$name"; else bad "$name (want $want, got $got)"; fi
}

run "prose question at the end" block false "$(user 'Quick wins')" "$(say $'Two are done.\n\nApply all four?')"
run "bold question at the end" block false "$(user 'go')" "$(say $'Table.\n\n**Which group next?**')"
run "question before an insight block" block false "$(user 'go')" "$(say $'Apply these?\n\n`★ Insight ─────`\n- a point.\n`─────────`')"
run "question after an insight block" block false "$(user 'go')" "$(say $'`★ Insight ─────`\n- a point.\n`─────────`\nShould I merge it?')"
run "insight block closed by a bare rule" block false "$(user 'go')" "$(say $'`★ Insight ─────`\n- a point.\n─────\nShould I merge it?')"
for loc in C C.UTF-8; do
  printf '%s\n' "$(user 'go')" "$(say $'`★ Insight ─────`\n- a point.\n`─────`\nShould I merge it?')" >"$tmp/t.jsonl"
  out=$(jq -nc --arg p "$tmp/t.jsonl" '{transcript_path:$p,stop_hook_active:false}' | LC_ALL=$loc "$hook")
  if [[ $out == *'"block"'* ]]; then ok "question after an insight block, LC_ALL=$loc"; else bad "question after an insight block, LC_ALL=$loc: $out"; fi
done
# Only the end of a long transcript is read; the turn at its end is still judged.
{ for _ in $(seq 2000); do user 'old'; say 'Old question?'; done; user 'go'; say 'Merge it?'; } >"$tmp/long.jsonl"
out=$(jq -nc --arg p "$tmp/long.jsonl" '{transcript_path:$p,stop_hook_active:false}' | "$hook")
if [[ $out == *'"block"'* ]]; then ok "a long transcript: its last turn is judged"; else bad "long transcript: $out"; fi
# A turn that starts before the 400-line window is not judged: its AskUserQuestion may lie outside it.
{ user 'go'; tool AskUserQuestion; toolres; for _ in $(seq 250); do tool Bash; toolres; done; say 'Merge it?'; } >"$tmp/longturn.jsonl"
out=$(jq -nc --arg p "$tmp/longturn.jsonl" '{transcript_path:$p,stop_hook_active:false}' | "$hook")
if [[ -z $out ]]; then ok "a turn longer than the window: not judged"; else bad "long turn: $out"; fi
run "AskUserQuestion this turn" pass false "$(user 'go')" "$(tool AskUserQuestion)" "$(toolres)" "$(say 'Should I go on?')"
run "AskUserQuestion in an older turn" block false "$(user 'a')" "$(tool AskUserQuestion)" "$(toolres)" "$(user 'b')" "$(say 'Merge it?')"
run "statement at the end" pass false "$(user 'go')" "$(say $'Is it done? Yes.\n\nMerged as 60b79419.')"
run "question only inside a code fence" pass false "$(user 'go')" "$(say $'Run:\n```\nread -p "ok?"\n```\nDone.')"
run "already blocked once" pass true "$(user 'go')" "$(say 'Apply these?')"
run "subagent text is ignored" pass false "$(user 'go')" "$(say 'Merged.')" "$(side 'Should I?')"
run "tool results do not end the turn" pass false "$(user 'go')" "$(tool AskUserQuestion)" "$(toolres)" "$(tool Bash)" "$(toolres)" "$(say 'Want more?')"
run "no transcript" pass false

# Off unless KOINON_MAINTAINER_HOOKS=1: nothing, exit 0, for a reply it would block.
printf '%s\n' "$(user 'go')" "$(say 'Apply these?')" >"$tmp/t.jsonl"
in=$(jq -nc --arg p "$tmp/t.jsonl" '{transcript_path:$p,stop_hook_active:false}')
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(env -u KOINON_MAINTAINER_HOOKS "$hook" <<<"$in") || rc=$?
  else
    out=$(KOINON_MAINTAINER_HOOKS=$v "$hook" <<<"$in") || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

exit "$fail"
