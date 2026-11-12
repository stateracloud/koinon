#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# PreToolUse hook on the Skill tool (issue 843; 2026-11-01 retro, point 1). When the grilling skill
# loads, it adds context: every question goes through the AskUserQuestion tool, not prose. It never
# refuses. The Stop hook prose-question-stop-check.sh covers the case where the rule is missed.
# Needs only bash and jq.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
skill=$(jq -r '.tool_input.skill // ""' <<<"$in")
case "$skill" in
grilling | *:grilling) ;;
*) exit 0 ;;
esac
ctx="Grilling rule (maintainer, retro 2026-11-01): ask every question through the AskUserQuestion tool, at most 4 questions per call, the recommended option first and marked (Recommended). No questions in prose. Each question stands on its own and names what it is about."
jq -nc --arg c "$ctx" '{hookSpecificOutput:{hookEventName:"PreToolUse",additionalContext:$c}}'
