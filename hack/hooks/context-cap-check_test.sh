#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/context-cap-check.sh (issue 843). Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/context-cap-check.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ctxcap.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
export TMPDIR=$tmp

# A transcript whose last main-thread assistant turn uses $1 tokens (a sidechain turn after it is ignored).
transcript() {
  jq -nc --argjson t "$1" '{type:"assistant",message:{usage:{input_tokens:$t,cache_read_input_tokens:0,cache_creation_input_tokens:0}}}' >"$tmp/t.jsonl"
  jq -nc '{type:"assistant",isSidechain:true,message:{usage:{input_tokens:900000}}}' >>"$tmp/t.jsonl"
}
input() { jq -nc --arg p "$tmp/t.jsonl" --arg s "$1" '{transcript_path:$p,session_id:$s}'; }

# Off unless KOINON_MAINTAINER_HOOKS=1: nothing, exit 0, even past the cap.
transcript 400000
for v in unset '' 0 true; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(input off | env -u KOINON_MAINTAINER_HOOKS "$hook" hook) || rc=$?
  else
    out=$(input off | KOINON_MAINTAINER_HOOKS=$v "$hook" hook) || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

export KOINON_MAINTAINER_HOOKS=1

transcript 100000
out=$(input s1 | "$hook" hook)
if [ -z "$out" ]; then ok "below the cap: silent"; else bad "below the cap printed: $out"; fi

transcript 310000
out=$(input s2 | "$hook" hook)
if jq -e '.hookSpecificOutput.hookEventName == "PostToolUse" and (.hookSpecificOutput.additionalContext | test("310k"))' <<<"$out" >/dev/null; then
  ok "past the cap: asks to compact"
else
  bad "past the cap: $out"
fi
out=$(input s2 | "$hook" hook)
if [ -z "$out" ]; then ok "same 50k step: once"; else bad "repeated in the same step: $out"; fi
transcript 365000
out=$(input s2 | "$hook" hook)
if [ -n "$out" ]; then ok "next 50k step: again"; else bad "next step was silent"; fi

# The mark is per session: a second session in the same 50k step is still told, and nothing goes to
# stderr (a bad tr range once made every session share one mark file and print an error).
transcript 310000
err=$(input s4 | "$hook" hook 2>&1 >/dev/null)
out=$(input s5 | "$hook" hook 2>"$tmp/err")
if [ -n "$out" ] && [ -z "$err" ] && [ ! -s "$tmp/err" ]; then ok "two sessions in one step: both told, no stderr"; else bad "two sessions: out=$out err=$err $(cat "$tmp/err")"; fi
if [ -f "$tmp/koinon-context-cap-s4" ] && [ -f "$tmp/koinon-context-cap-s5" ] && [ ! -e "$tmp/koinon-context-cap-" ]; then
  ok "one mark file per session"
else
  bad "mark files: $(cd "$tmp" && echo koinon-context-cap-*)"
fi
out=$(jq -nc --arg p "$tmp/t.jsonl" '{transcript_path:$p,session_id:"../a b"}' | "$hook" hook)
if [ -n "$out" ] && [ -f "$tmp/koinon-context-cap-___a_b" ]; then ok "an odd session id is made safe"; else bad "odd session id: $(cd "$tmp" && echo koinon-context-cap-*)"; fi

transcript 205000
out=$(input s3 | "$hook" status)
if [ "$out" = "ctx 205k/300k" ]; then ok "status line"; else bad "status line: $out"; fi
out=$(input s3 | KOINON_CONTEXT_CAP=200000 "$hook" status)
if [[ $out == *"compact"* ]]; then ok "status line past a lowered cap"; else bad "status line past the cap: $out"; fi

exit "$fail"
