#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Context cap (issue 843; 2026-11-01 retro, point 2). Context = the last main-thread assistant turn's
# input + cache read + cache write tokens, read from the transcript.
#   context-cap-check.sh hook     PostToolUse hook: past the cap, tells the session to compact (once per
#                                 50k-token step above the cap; the mark file is in ${TMPDIR:-/tmp})
#   context-cap-check.sh status   status line text: "ctx 205k/300k", red at or past the cap (not
#                                 registered by the repository; see docs/maintainers/hooks.md)
# The cap is 300000 tokens; KOINON_CONTEXT_CAP changes it. Needs only bash and jq.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

mode=${1:-hook}
cap=${KOINON_CONTEXT_CAP:-300000}
step=50000
in=$(cat)
transcript=$(jq -r '.transcript_path // ""' <<<"$in")
session=$(jq -r '.session_id // "unknown"' <<<"$in" | tr -c 'A-Za-z0-9_-\n' '_')
tokens=0
if [ -f "$transcript" ]; then
  tokens=$(tail -n 400 "$transcript" | jq -s '[.[] | select(.type == "assistant" and (.isSidechain | not)) | .message.usage
    | select(. != null) | (.input_tokens // 0) + (.cache_read_input_tokens // 0) + (.cache_creation_input_tokens // 0)] | last // 0' 2>/dev/null)
  tokens=${tokens:-0}
fi
k=$((tokens / 1000))
case "$mode" in
status)
  if [ "$tokens" -ge "$cap" ]; then
    printf '\033[31mctx %dk/%dk: compact\033[0m' "$k" "$((cap / 1000))"
  else
    printf 'ctx %dk/%dk' "$k" "$((cap / 1000))"
  fi
  ;;
hook)
  [ "$tokens" -lt "$cap" ] && exit 0
  bucket=$(((tokens - cap) / step))
  mark="${TMPDIR:-/tmp}/koinon-context-cap-$session"
  [ "$(cat "$mark" 2>/dev/null)" = "$bucket" ] && exit 0
  echo "$bucket" >"$mark"
  ctx="Context cap: this session is at ${k}k tokens, past the $((cap / 1000))k cap. Compact, do not start a fresh session: finish or park the current step, bring your session notes (in flight, next steps, what waits on the maintainer) up to date, then tell the maintainer in one line to run /compact with a one-line focus. Background loops and claims keep running across compaction. Do not start new work before it."
  jq -nc --arg c "$ctx" '{hookSpecificOutput:{hookEventName:"PostToolUse",additionalContext:$c}}'
  ;;
esac
exit 0
