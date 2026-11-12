#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Stop hook (issue 843). When the main session's reply ends in a question to the maintainer and that
# turn made no AskUserQuestion call, it blocks the stop once and tells the agent to re-ask through the
# question tool. It never blocks twice in a row (stop_hook_active), so it cannot loop. It replaces the
# narrower nudge of grilling-question-check.sh for questions asked outside the grilling skill.
# Interim: remove it when two nightly retros show no prose questions.
# Needs only bash and jq.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
[[ $(jq -r '.stop_hook_active // false' <<<"$in") == true ]] && exit 0
tp=$(jq -r '.transcript_path // empty' <<<"$in")
[[ -n $tp && -r $tp ]] || exit 0

# This turn: every entry after the last genuine user message (a user entry whose content is text,
# not a tool_result). Skip sidechain (subagent) entries.
turn=$(jq -c 'select((.isSidechain // false) | not)' "$tp" 2>/dev/null | jq -sc '
  def human: .type == "user" and ((.message.content | type) == "string"
    or ([.message.content[]? | select(.type == "text")] | length > 0)
    and ([.message.content[]? | select(.type == "tool_result")] | length == 0));
  (map(human) | rindex(true)) as $i | if $i == null then . else .[$i + 1:] end') || exit 0

asked=$(jq '[.[] | select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "AskUserQuestion")] | length' <<<"$turn")
[[ ${asked:-0} -gt 0 ]] && exit 0

# The last assistant text, without code fences and the Insight block.
text=$(jq -r '[.[] | select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text] | last // ""' <<<"$turn")
[[ -n $text ]] || exit 0
last_lines=$(awk '
  /^```/ { fence = !fence; next }
  fence { next }
  /★ Insight/ { ins = 1; next }
  ins && /^`?─+`?$/ { ins = 0; next }
  ins { next }
  NF { print }' <<<"$text" | tail -n 3)

# A question to the maintainer: one of the last three lines ends in "?" (optionally followed by ** or a
# closing bracket). Questions inside quotes or tables earlier in the reply do not count.
if grep -qE '\?(\*\*|\)|"|”)?[[:space:]]*$' <<<"$last_lines"; then
  reason="Your reply ends in a question to the maintainer, and this turn made no AskUserQuestion call. Every choice goes through the question tool, with the recommended option first and plain-words context in the question. Re-ask it now with AskUserQuestion. If it was not really a question to the maintainer (rhetorical, or a quoted one), end the turn with a statement instead."
  jq -nc --arg r "$reason" '{decision: "block", reason: $r}'
fi
exit 0
