#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/hooks/sleep-chain-check.sh (issue 843). Needs only bash, sed, awk and jq.
# shellcheck disable=SC2016 # the commands under test are literal strings; nothing should expand

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
hook=$here/sleep-chain-check.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
export KOINON_MAINTAINER_HOOKS=1

# case_ <want: deny|allow> <command> [background: true|false]
case_() {
  local want=$1 cmd=$2 bg=${3:-false} json out got
  json=$(jq -nc --arg c "$cmd" --argjson b "$bg" '{tool_name:"Bash",tool_input:{command:$c,run_in_background:$b}}')
  out=$("$hook" <<<"$json")
  if [ -z "$out" ]; then got=allow; else got=$(jq -r '.hookSpecificOutput.permissionDecision' <<<"$out"); fi
  if [ "$got" = "$want" ]; then ok "$want: $cmd (bg=$bg)"; else bad "$cmd (bg=$bg): got $got, want $want; output: $out"; fi
}

case_ deny 'sleep 5'
case_ deny 'sleep 30 && gh api repos/x/y'
case_ deny 'git push || { sleep 8; git push; }'
case_ deny 'sleep 1m'
case_ deny 'sleep 2; sleep 3'
case_ deny 'sleep 1 2 2'
case_ deny 'for i in 1 2 3; do sleep 10; done'
case_ deny 'while true; do check || break; sleep 6; done'
case_ deny 'time sleep 9'
case_ deny 'if x; then sleep 0.5h; fi'
case_ allow 'sleep 4'
case_ allow 'sleep 2; sleep 2'
case_ allow 'sleep 4.9'
case_ allow 'sleep 60' true
case_ allow 'sleep 600; echo check-in' true
case_ allow 'sleep "$delay"'
case_ allow 'sleep $d'
case_ allow 'git commit -m "wait: sleep 30 is gone"'
case_ allow "echo 'sleep 30'"
case_ allow 'grep -n sleep 30 file.txt'
case_ allow 'make test'
case_ allow ''
case_ deny 'FOO=1 sleep 7'
case_ deny 'echo `sleep 6`'
case_ deny 'command sleep 10'
case_ deny 'env sleep 10'
case_ deny 'timeout 20 sleep 10'
case_ deny 'sleep infinity'
case_ deny "echo \"it's\"; sleep 9; echo 'x'"
case_ deny 'sleep 10 &'
case_ deny $'cat > f <<EOF\nx\nEOF\nsleep 9'
case_ deny $'grep x <<<word\nsleep 9'
case_ allow $'git commit -m "fix: x\n\nsleep 30 was removed"'
case_ allow $'cat > f <<EOF\nsleep 60\nEOF'
case_ allow $'cat > f <<-\'EOF\'\n\tsleep 60\n\tEOF'
case_ allow $'gh api x -f body="$(cat <<\'EOF\'\nwait\nsleep 10\nEOF\n)"'
case_ deny 'echo \"; sleep 9'
case_ allow 'for i in 1 2 3; do git push && break; sleep 2; done'
case_ allow 'echo $((1<<3)); sleep 1'

# The reason is one line and names the legal routes.
out=$("$hook" <<<'{"tool_input":{"command":"sleep 10"}}')
reason=$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")
if [ "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$out")" = PreToolUse ] && [ "$(printf '%s\n' "$reason" | wc -l)" -eq 1 ] \
  && grep -q 'Sleep chain blocked' <<<"$reason" && grep -q run_in_background <<<"$reason"; then
  ok "the deny names PreToolUse and the routes in one line"
else
  bad "the deny output: $out"
fi

# Off unless KOINON_MAINTAINER_HOOKS=1: no output and exit 0 for a command it would refuse, also under
# a POSIX sh (the opt-in line comes before any bash 4+ syntax).
rc=0
out=$(env -u KOINON_MAINTAINER_HOOKS sh "$hook" <<<'{"tool_input":{"command":"sleep 99"}}') || rc=$?
if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off under sh"; else bad "off under sh: rc=$rc output: $out"; fi
for v in unset '' 0 true yes; do
  rc=0
  if [ "$v" = unset ]; then
    out=$(env -u KOINON_MAINTAINER_HOOKS "$hook" <<<'{"tool_input":{"command":"sleep 99"}}') || rc=$?
  else
    out=$(KOINON_MAINTAINER_HOOKS=$v "$hook" <<<'{"tool_input":{"command":"sleep 99"}}') || rc=$?
  fi
  if [ -z "$out" ] && [ "$rc" -eq 0 ]; then ok "off with KOINON_MAINTAINER_HOOKS=$v"; else bad "KOINON_MAINTAINER_HOOKS=$v: rc=$rc output: $out"; fi
done

exit "$fail"
