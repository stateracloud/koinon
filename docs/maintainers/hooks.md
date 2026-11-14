<!-- SPDX-License-Identifier: Apache-2.0 -->

# Maintainer agent hooks

Koinon commits the Claude Code hooks that its maintainers run, so that a maintainer's laptop and a
maintainer's cloud session get the same guards (issue 843, tracking issue 841). They are
**opt-in**: each hook does nothing unless `KOINON_MAINTAINER_HOOKS=1` is set. A contributor who does
not set it sees no refusal, no message and no other change. The rules the hooks check are still the
project's rules (`AGENTS.md`, `docs/agents/subagent-brief.md`); for a contributor they are text, and
CI and the pull request checks enforce what matters.

Why opt-in: open-source projects do not control how a contributor works. They commit a short
tool-neutral `AGENTS.md`, enforce rules in CI and on the pull request, and keep maintainer tooling
public but labelled. A committed hook that refuses a contributor's tool call does not fit that.

## The hooks

All are in `hack/hooks/`, registered in `.claude/settings.json`. A PreToolUse hook that refuses prints a
`deny` decision with a reason and a legal route; the others add context or block a stop; otherwise a hook
prints nothing. `agent-spawn-check.sh` adds `subagent-brief-digest.txt` (the rules every subagent
follows; `docs/agents/subagent-brief.md` wins over it) to its deny reason.

| Hook | Tool | Refuses | Test |
|---|---|---|---|
| `agent-spawn-check.sh` | Agent | a spawn without `model`; a spawn whose prompt does not name `docs/agents/subagent-brief.md` (Explore, claude-code-guide, web-fetch, statusline-setup, claude-security:* and fork are exempt) | `agent-spawn-check_test.sh` |
| `read-result-check.sh` | Read | a Read of a saved `tool-results/` file without `limit` | `read-hooks_test.sh` |
| `bash-read-check.sh` | Bash | printing much of a file or of several files in one call (`cat` of two files, `sed -n` past 210 lines, ...) | `read-hooks_test.sh` |
| `sleep-chain-check.sh` | Bash | a foreground call whose literal `sleep`s add up to 5 s or more, or the same wait spelled as perl `select`, `tail -f /dev/null`, `timeout N tail -f`, `read -t N`, python `time.sleep`, `/bin/sleep` or a FIFO made with `mkfifo` and then read; it ignores the word sleep inside quoted text and data heredocs (a heredoc fed to python, perl, ruby or node still counts as code, and so does the quoted argument of `python -c`, `perl -e`/`-E` and `bash|sh|zsh -c`, which is scanned again for nested interpreter calls); `run_in_background` is allowed | `sleep-chain-check_test.sh` |
| `grilling-question-check.sh` | Skill | nothing; when the grilling skill loads it adds the rule that every question goes through `AskUserQuestion` | `grilling-question-check_test.sh` |
| `question-checklist-check.sh` | AskUserQuestion | nothing; it adds a checklist before each question (already answered by a rule? researched? answerable cold?) | `question-checklist-check_test.sh` |
| `prose-question-stop-check.sh` | Stop | blocks the stop once when the reply ends in a question and the turn made no `AskUserQuestion` call; never twice in a row | `prose-question-stop-check_test.sh` |
| `context-cap-check.sh` | PostToolUse | nothing; past 300k tokens of context (`KOINON_CONTEXT_CAP`) it adds a request to compact, once per 50k step. `context-cap-check.sh status` is a status-line text (not registered) | `context-cap-check_test.sh` |
| `codegraph-prompt-check.sh` | UserPromptSubmit | nothing; it wraps `codegraph prompt-hook` and skips it for short prompts, notifications and the main checkout | `codegraph-prompt-check_test.sh` |

Known gaps of `sleep-chain-check.sh` (not blocked, same as before): `echo ... | python3`, `cat <<EOF | python3`, `ruby -e 'sleep N'`, `node -e setTimeout`, `/usr/bin/env sleep N`, `eval "sleep N"`, and `time.sleep` inside a quoted plain echo.

The tests need only bash 5 and jq; `make adr-lint` and the `ci` workflow run them, and they check
both the refusals and that each hook is silent without the variable. The guards table,
`docs/agents/guards.md`, has a row for each hook with its legal route.

## Turning them on

The value must be exactly `1`. Any other value, or no variable, leaves the hooks off.

### On a Mac

1. Install bash 5 (`brew install bash`, ADR 0090). With the macOS bash 3.2 the hooks skip with a
   message.
2. Add the variable to your user settings, `~/.claude/settings.json`. Claude Code exports the `env`
   block to every session and its hooks, in the CLI and in the desktop app:

   ```json
   {
     "env": { "KOINON_MAINTAINER_HOOKS": "1" }
   }
   ```

   The variable is named for Koinon, so it changes nothing in other repositories. To limit it to
   one checkout instead, put the same block in that checkout's `.claude/settings.local.json` (not
   committed). An `export KOINON_MAINTAINER_HOOKS=1` in `~/.zshrc` reaches only a CLI started from
   that shell, not the desktop app.
3. Start a new session. Check: a foreground `sleep 10` in the Bash tool is refused with
   `Foreground wait blocked`.

### In a Claude Code cloud environment

1. On claude.ai/code, open the environment's settings and add the environment variable
   `KOINON_MAINTAINER_HOOKS` with the value `1`. It is not a secret. Set it on a personal
   environment; on an organization environment it turns the hooks on for everyone who uses it.
2. Start a new session in that environment: a running session keeps the variables it started with.
3. Check as on the Mac. The cloud image has bash 5 and jq.

## Kept outside the repository

- `relay-guard.sh` (PreToolUse on SendMessage and AskUserQuestion): it refuses relaying the owner's
  approval to a named private lab session. It only makes sense with that private setup, and the
  lab's details stay out of git.

A maintainer who still registers the owner-local copies of `agent-spawn-check.sh`, `sleep-chain.sh`,
`context-cap.sh`, `grilling-question-tool.sh`, `prose-question-stop.sh`, `question-checklist.sh` or
`codegraph-prompt.sh` in `~/.claude/settings.json` must remove those entries: the repository hooks
replace them, and both would run.

## Optional: the context status line

`hack/hooks/context-cap-check.sh status` prints `ctx 205k/300k` (red past the cap) for a Claude Code
status line. The repository does not register it, because a status line is a per-user setting. To use
it, point your user `statusLine` command at the script, with `KOINON_MAINTAINER_HOOKS=1` set.

## Adding a maintainer hook

A new hook comes, in the same pull request, with:

- the file `hack/hooks/<name>-check.sh`, whose first command is the opt-in line and whose second is
  the fail-open bash 5 guard (`hack/bash5-lint.sh` checks both, from its list of registered hooks):

  ```bash
  [ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }
  ```

- its offline test `hack/hooks/<name>-check_test.sh`, which covers the off case, listed in the
  `Makefile` (`adr-lint`) and in `.github/workflows/ci.yml`;
- its entry in `.claude/settings.json` and in the hook list of `hack/bash5-lint.sh`;
- a `script-check inputs=<paths> <cmd>` line for its test in `hack/paths.rules` (the paths the test reads; `inputs=guard` if it must always run);
- nothing personal and nothing from the private lab: no secrets, no host or cluster names, no
  absolute home paths (use `$HOME`, `${TMPDIR:-/tmp}` or repo-relative paths), no person's name (say
  "the maintainer");
- its row in `docs/agents/guards.md` and in the table above.

Hooks run from `$CLAUDE_PROJECT_DIR`, the main checkout, not from a lane's worktree
(`docs/agents/guards.md`, section 2).
