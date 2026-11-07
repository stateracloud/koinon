#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Claude Code PreToolUse hook on the Agent tool (2026-10-31 retro, point 6; old issue 397).
# Registered in .claude/settings.json. Reads the hook JSON on stdin and enforces the spawn rule:
#   - subagent_type "fork" is always allowed (it inherits the parent's context and model);
#   - every other spawn must set tool_input.model;
#   - every type except Explore, claude-code-guide, web-fetch, statusline-setup and
#     claude-security:* must name docs/agents/subagent-brief.md in its prompt.
# On a violation it prints a PreToolUse "deny" decision with a reason, so the caller fixes the call
# and sends it again. On success it prints nothing.
# Needs only bash and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
type=$(jq -r '.tool_input.subagent_type // "general-purpose"' <<<"$in")
model=$(jq -r '.tool_input.model // ""' <<<"$in")
prompt=$(jq -r '.tool_input.prompt // ""' <<<"$in")

[ "$type" = fork ] && exit 0

missing=()
[ -z "$model" ] && missing+=("set \"model\" (haiku: runs/mechanical; sonnet: specified implementation; opus: diagnosis/review)")
case "$type" in
  Explore | claude-code-guide | web-fetch | statusline-setup | claude-security:*) ;;
  *) grep -q 'docs/agents/subagent-brief.md' <<<"$prompt" || missing+=("name docs/agents/subagent-brief.md in the prompt (plus the worktree and scope)") ;;
esac

[ ${#missing[@]} -eq 0 ] && exit 0

reason="Spawn rule (retro 2026-10-31): $(IFS='; '; echo "${missing[*]}")."
jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
