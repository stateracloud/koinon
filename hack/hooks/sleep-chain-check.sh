#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Claude Code PreToolUse hook on the Bash tool (issue 843; docs/agents/guards.md, "No sleep of 5 s or
# more"). It replaces the maintainers' earlier local copy. A foreground `sleep`
# blocks the session and burns the turn while it waits; waits belong in `hack/pr-wait.sh`,
# `run_in_background` or a Monitor call (docs/agents/subagent-brief.md, "No standalone sleep").
# It refuses a foreground Bash command whose literal `sleep` durations add up to 5 seconds or more
# (`sleep 5`, `sleep 1m`, `sleep 2; sleep 3`, `sleep 1 2 2`, `FOO=1 sleep 9`, `timeout 20 sleep 9`,
# `sleep infinity`). A background call (`run_in_background: true`) is allowed, and so is a `sleep`
# whose duration is not a literal (`sleep "$d"`): the hook cannot know it. Quoted text (a commit
# message, over several lines too) and heredoc bodies are not commands. A `sleep 10 &` inside a
# foreground call is refused too: a later `wait`, or the shell, can still hold the call. Not seen:
# a sleep inside `bash -c "..."` or `eval`. The same wait in other spellings is refused as well (a
# perl `select` with undef handles, `tail -f /dev/null`, `timeout N tail -f`, `read -t N`, python
# `time.sleep`); those are matched on the raw command, so quoted text can trip them.
# On a violation it prints a PreToolUse "deny" decision with a one-line reason; otherwise nothing.
# Needs only bash, awk and jq.

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

# One awk pass over the whole command (RS that never matches, so multi-line quotes are seen whole):
#   1. drop heredoc bodies (the lines after `<<WORD` up to WORD), outside quotes;
#   2. drop # comments, replace quoted text with Q, honouring backslashes outside single quotes, and turn newlines,
#      backticks and ; & | ( ) { } into separators;
#   3. add up the literal arguments of every `sleep` that starts a simple command. Before it may come
#      do/then/else/!/time, NAME=value words, command, env, exec, nohup, and `timeout DURATION`.
#      GNU sleep sums its arguments.
total=$(printf '%s\n' "$cmd" | LC_ALL=C awk -v sq="'" '
BEGIN { RS = "\001" }
function sep() { out = out " ; " }
{
  # 1. heredoc bodies, line by line, with a rough quote state per line.
  nl = split($0, L, "\n"); text = ""; tag = ""
  for (k = 1; k <= nl; k++) {
    line = L[k]
    if (tag != "") {
      t = line; if (strip) sub(/^\t+/, "", t)
      if (t == tag) tag = ""
      text = text "\n"
      continue
    }
    text = text line "\n"
    if (match(line, /<<-?[ \t]*["\047]?[A-Za-z_][A-Za-z0-9_]*["\047]?/)) {
      hs = RSTART; hl = RLENGTH; pre = substr(line, 1, hs - 1)
      # Not a here-string (<<<word), not a shift inside $(( )), and not inside quotes. An odd count
      # of " is fine when a $( follows the last one: "$(cat <<EOF", the usual commit message.
      dq = pre; ndq = gsub(/"/, "\"", dq); lastdq = 0
      for (q = length(pre); q > 0; q--) if (substr(pre, q, 1) == "\"") { lastdq = q; break }
      inq = (ndq % 2 == 1 && index(substr(pre, lastdq + 1), "$(") == 0)
      arith = (match(pre, /\$\(\([^)]*$/) > 0)
      if (substr(pre, length(pre)) != "<" && !arith && gsub(sq, sq, pre) % 2 == 0 && !inq) {
        h = substr(line, hs, hl)
        strip = (h ~ /^<<-/)
        sub(/^<<-?[ \t]*/, "", h); gsub(/["\047]/, "", h); tag = h
      }
    }
  }
  # 2. quotes and separators, character by character.
  out = ""; st = 0; n = length(text)
  for (k = 1; k <= n; k++) {
    c = substr(text, k, 1)
    if (st == 1) { if (c == sq) st = 0; continue }
    if (st == 2) {
      if (c == "\\") { k++; continue }
      if (c == "\"") st = 0
      continue
    }
    if (c == "\\") { k++; out = out "x"; continue }
    if (c == sq) { st = 1; out = out "Q"; continue }
    if (c == "\"") { st = 2; out = out "Q"; continue }
    if (c == "#" && (out == "" || substr(out, length(out)) ~ /[ \t]/)) {
      while (k < n && substr(text, k + 1, 1) != "\n") k++
      continue
    }
    if (c == "\n" || c == "`" || c ~ /[;&|(){}]/) { sep(); continue }
    out = out c
  }
  # 3. the sleeps.
  n = split(out, w, /[ \t]+/); start = 1
  for (i = 1; i <= n; i++) {
    if (w[i] == "") continue
    if (w[i] == ";") { start = 1; continue }
    if (start && (w[i] ~ /^(do|then|else|!|time|command|env|exec|nohup)$/ || w[i] ~ /^[A-Za-z_][A-Za-z0-9_]*=/)) continue
    if (start && w[i] == "timeout") { i++; continue }
    if (start && w[i] == "sleep") {
      for (j = i + 1; j <= n && w[j] != ";"; j++) {
        if (w[j] == "infinity") sum += 86400
        else if (w[j] ~ /^[0-9]+(\.[0-9]+)?[smhd]?$/) {
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

# Other spellings of a wait of 5 s or more, matched line by line on the raw command.
n='([5-9]|[1-9][0-9]+)'
start='(^|[;&|(]|do|then)[[:space:]]*'
other_pats=(
  "select\\([[:space:]]*undef[[:space:]]*,[[:space:]]*undef[[:space:]]*,[[:space:]]*undef[[:space:]]*,[[:space:]]*$n"
  "${start}tail[[:space:]]+([^|;&]*[[:space:]])?(-[a-zA-Z]*[fF][a-zA-Z0-9]*|--follow[^[:space:]]*)[[:space:]]+([^|;&]*[[:space:]])?/dev/null"
  "${start}timeout[[:space:]]+(-[^[:space:]]+[[:space:]]+)*${n}[smhd]?[[:space:]]+tail[[:space:]]+([^|;&]*[[:space:]])?(-[a-zA-Z]*[fF][a-zA-Z0-9]*|--follow)"
  "${start}read[[:space:]]+(-[a-zA-Z]+[[:space:]]+)*-[a-zA-Z]*t[[:space:]]*$n"
  "python[0-9.]*[[:space:]].*time\\.sleep\\([[:space:]]*$n(\\.[0-9]*)?[[:space:]]*\\)"
)
other=0
for p in "${other_pats[@]}"; do
  if grep -Eq -- "$p" <<<"$cmd"; then other=1; break; fi
done

if [ "${total:-0}" -lt "$max_seconds" ]; then
  [ "$other" = 1 ] || exit 0
  reason="Sleep chain blocked: a foreground wait of ${max_seconds}s or more (perl select, tail -f /dev/null, read -t, python time.sleep). Wait with hack/pr-wait.sh <PR> --checks, run_in_background, or a Monitor call (docs/agents/subagent-brief.md)."
  jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
  exit 0
fi

reason="Sleep chain blocked: ${total}s of foreground sleep. Wait with hack/pr-wait.sh <PR> --checks, run_in_background, or a Monitor call (docs/agents/subagent-brief.md)."
jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
