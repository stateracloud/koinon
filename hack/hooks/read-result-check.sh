#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Claude Code PreToolUse hook on the Read tool (old issue 534; docs/research/2026-11-token-tiers.md).
# A Bash result too large to show is saved under .../tool-results/ and agents then Read that file
# whole: half of all Read characters in the research window. This hook refuses a Read whose
# file_path contains /tool-results/ and that sets no limit. Exact match, no heuristics.
# On a violation it prints a PreToolUse "deny" decision with a one-line reason; otherwise nothing.
# Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
path=$(jq -r '.tool_input.file_path // ""' <<<"$in")
limit=$(jq -r '.tool_input.limit // ""' <<<"$in")

case "$path" in
  */tool-results/*) [ -n "$limit" ] && exit 0 ;;
  *) exit 0 ;;
esac

reason="A saved tool result is read whole: grep -n the file, or Read it with offset and limit (docs/agents/subagent-brief.md)."
jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
