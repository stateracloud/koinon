#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# UserPromptSubmit wrapper around `codegraph prompt-hook` (issue 843). It skips the code-snippet
# injection when that only costs tokens:
#   - messages from other sessions, subagent hand-backs and task notifications;
#   - short prompts (under KOINON_CODEGRAPH_MIN_WORDS words, default 8): questions and replies, not
#     code tasks;
#   - the main checkout ($CLAUDE_PROJECT_DIR), where the main session orchestrates and does not read
#     code; a worktree and other projects still get it.
# Everything else goes to codegraph unchanged. It fails open: any error prints nothing and exits 0.
# Needs bash, jq and (for the injection) the codegraph CLI.

# Maintainer hook: does nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md). POSIX
# syntax, so it runs before the bash 5 check and a contributor without the variable sees nothing.
[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

min_words=${KOINON_CODEGRAPH_MIN_WORDS:-8}
main_checkout=${CLAUDE_PROJECT_DIR:-}

input=$(cat 2>/dev/null) || exit 0
command -v codegraph >/dev/null 2>&1 || exit 0
command -v jq >/dev/null 2>&1 || { printf '%s' "$input" | codegraph prompt-hook 2>/dev/null; exit 0; }

prompt=$(printf '%s' "$input" | jq -r '.prompt // ""' 2>/dev/null)
cwd=$(printf '%s' "$input" | jq -r '.cwd // ""' 2>/dev/null)

# 1. Not typed by the maintainer.
case "$prompt" in
*'<cross-session-message'* | *'<agent-message'* | *'<task-notification>'* | *'[SYSTEM NOTIFICATION'* | *'[Subagent hand-back]'*) exit 0 ;;
esac

# 2. Short prompt.
words=$(printf '%s' "$prompt" | wc -w | tr -d ' ')
[ "${words:-0}" -lt "$min_words" ] && exit 0

# 3. Main checkout.
[ -n "$main_checkout" ] && [ "${cwd%/}" = "${main_checkout%/}" ] && exit 0

printf '%s' "$input" | codegraph prompt-hook 2>/dev/null
exit 0
