#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Claude Code PreToolUse hook on the Bash tool (issue 843; docs/agents/guards.md, "No sleep of 5 s or
# more"). It replaces the owner-local ~/.config/koinon/hooks/sleep-chain.sh. A foreground `sleep`
# blocks the session and burns the turn while it waits; waits belong in `hack/pr-wait.sh`,
# `run_in_background` or a Monitor call (docs/agents/subagent-brief.md, "No standalone sleep").
# It refuses a foreground Bash command whose literal `sleep` durations add up to 5 seconds or more
# (`sleep 5`, `sleep 1m`, `sleep 2; sleep 3`, `sleep 1 2 2`). A background call
# (`run_in_background: true`) is allowed, and so is a `sleep` whose duration is not a literal
# (`sleep "$d"`): the hook cannot know it. Text inside quotes is not a command (a commit message).
# On a violation it prints a PreToolUse "deny" decision with a one-line reason; otherwise nothing.
# Needs only bash, sed, awk and jq.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
[ "$(jq -r '.tool_input.run_in_background // false' <<<"$in")" = true ] && exit 0
cmd=$(jq -r '.tool_input.command // ""' <<<"$in")
[ -n "$cmd" ] || exit 0
[ ${#cmd} -gt 65536 ] && exit 0

# The seconds of foreground sleep that the hook allows in one call.
max_seconds=5

# Drop quoted text, then add up the literal arguments of every `sleep` that starts a simple command
# (after start of line, ; & | ( { or a do/then/else keyword). GNU sleep sums its arguments.
total=$(printf '%s\n' "$cmd" | sed -e "s/'[^']*'/Q/g" -e 's/"[^"]*"/Q/g' | awk '
{
  gsub(/[;&|(){}]/, " ; ")
  n = split($0, w, /[[:space:]]+/)
  start = 1
  for (i = 1; i <= n; i++) {
    if (w[i] == "") continue
    if (w[i] == ";") { start = 1; continue }
    if (start && (w[i] == "do" || w[i] == "then" || w[i] == "else" || w[i] == "!" || w[i] == "time")) continue
    if (start && w[i] == "sleep") {
      for (j = i + 1; j <= n && w[j] != ";"; j++) {
        if (w[j] ~ /^[0-9]+(\.[0-9]+)?[smhd]?$/) {
          v = w[j]; u = substr(v, length(v)); m = 1
          if (u == "m") m = 60; else if (u == "h") m = 3600; else if (u == "d") m = 86400
          sub(/[smhd]$/, "", v)
          sum += v * m
        }
      }
      i = j - 1
    }
    start = 0
  }
}
END { printf "%d\n", sum }')

[ "${total:-0}" -lt "$max_seconds" ] && exit 0

reason="Sleep chain blocked: ${total}s of foreground sleep. Wait with hack/pr-wait.sh <PR> --checks, run_in_background, or a Monitor call (docs/agents/subagent-brief.md)."
jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
