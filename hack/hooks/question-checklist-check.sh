#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# PreToolUse hook on the AskUserQuestion tool (issue 843; 2026-11-02 retro, point 3). It adds a
# checklist to the context before every question to the maintainer. It never refuses; it reminds.
# Needs only bash and jq.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

cat >/dev/null
ctx="Question checklist (retro 2026-11-02): before this AskUserQuestion goes out, check: (a) is it already answered by a standing rule (AGENTS.md, an ADR, a memory note)? Then act and say so instead; (b) a policy or design question, or one about how the agent itself works, needs research and review first; (c) can the maintainer answer it cold: plain words, no lane, gate or issue jargon they have not seen, every premise checked (a domain, an earlier approval); (d) did you first answer the question that was actually asked? If any check fails, fix the question or skip it."
jq -nc --arg c "$ctx" '{hookSpecificOutput:{hookEventName:"PreToolUse",additionalContext:$c}}'
