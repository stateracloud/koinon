<!-- SPDX-License-Identifier: Apache-2.0 -->

# Guards around agents

A guard is anything that stops or redirects an agent: a hook, a script refusal, a CI check, the
GitHub ruleset, the Claude Code permission classifier or the sandbox. Guards come from different
people at different times. This table lists them together.

Rule: each guard has a legal route. When a guard refuses, the agent can reach the same goal with a
command or step that the other guards allow. A guard with no legal route is a defect (section 2).

Status: written 2026-11-09 from the files named in it. The live ruleset for `main` was read on 2026-11-09
with `gh api`; the ruleset rows match it. The classifier was not re-read from Claude Code.

## 1. The table

Owner: "repo" means a pull request changes it. "Owner" means only the human owner can change it.

| Rule | Guard, where | What the agent sees | Legal route | Owner |
|---|---|---|---|---|
| A spawn sets a model | `hack/hooks/agent-spawn-check.sh`, PreToolUse on Agent (`.claude/settings.json`). A second spawn hook, `~/.config/koinon/hooks/agent-spawn-check.sh`, is registered in the owner's local settings; both run on each spawn | deny: `set "model"` | Send the spawn again with `model` (`docs/agents/subagent-brief.md`, "Which model") | repo; the second hook: owner |
| A spawn names the brief | same two hooks | deny: `name docs/agents/subagent-brief.md in the prompt`; the owner-local hook adds the brief digest to the reason | Put the path in the prompt. Explore, claude-code-guide, web-fetch, statusline-setup, claude-security:* and fork need no path | repo; the second hook: owner |
| No `sleep` of 5 s or more in the foreground Bash call | `~/.config/koinon/hooks/sleep-chain.sh`, PreToolUse on Bash (not in the repository) | deny: `Sleep chain blocked` | `hack/pr-wait.sh <PR> --checks`; `run_in_background`; a Monitor call | owner |
| Read a saved tool result in small parts | `hack/hooks/read-result-check.sh` (Read of `tool-results/` needs `limit`) | deny with reason | Read with `offset` and `limit`; grep the saved file | repo |
| Do not print much of a file or of several files in one Bash call | `hack/hooks/bash-read-check.sh` | deny: `Prints too much of a file or of several files (...)` | Read with `offset` and `limit`, `grep -n`, codegraph, or `sed -n` ranges of at most 210 lines in total per call (`max_lines` in the script) | repo |
| Hooks need bash 5 | the bash 5 check near the top of each repository hook in `hack/hooks/`; the owner-local hooks have no such check (checked: no `BASH_VERSINFO` in `~/.config/koinon/hooks/`) | `systemMessage`: hooks skipped | `brew install bash` (ADR 0090). The hook is then off, not satisfied | repo |
| Only the parent takes claims | `docs/agents/subagent-brief.md` ("Work in your worktree"); `hack/claim.sh` exit 3 when the claim is not this lane's ("not yours") | text only; no hook | The parent runs `hack/claim.sh take/release` | repo |
| One claim per issue; a claim is a plain lock (no lease, no beat; ADR 0104) | `hack/claim.sh` (claim branches, ADR 0082); CI check `claim` (`.github/workflows/claims.yml`) | exit 2 (taken), exit 3 (not yours); `claim` red on the PR | `claim.sh next`, then `take`; `block`, or `release --note` to hand off; a PR closed without a merge needs `release-pr <PR>` or `release <N>` | repo |
| Sign-off on every commit | CI check `dco` (`.github/workflows/dco.yml`) | `dco` red | `git commit -s` | repo |
| No secret in any commit of a PR | CI check `gitleaks` (`.github/workflows/secrets.yml`) | `gitleaks` red; a later fix commit does not clear it | Fake secrets that look fake from the first commit; `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .` before the push | repo |
| A needs-e2e change has a green e2e on the queue commit | CI check `e2e result` (`.github/workflows/e2e-kind.yml`, ADR 0105); `hack/needs-e2e.sh` | the queue entry is dropped | Fix the failure; the `e2e` label forces the e2e; `hack/e2e-remote.sh` is dev feedback only | repo |
| A commit reached main without a queue build | `.github/workflows/route-guard.yml` (`hack/route-guard.sh`) | an issue labelled `break-glass` (none for an `--admin` merge of a `.github/**` PR with green checks) | Close it with a line saying what happened | repo |
| A draft PR is not merge evidence | `go` and `web` show red on a draft on purpose (`.github/workflows/ci.yml`); `hack/pr-merge.sh` exit 5 | red checks; exit 5 | `hack/ship.sh --ready` or `gh pr ready`, then wait. WIP feedback: `hack/preflight.sh` | repo |
| Merge only with every required context green | `hack/pr-merge.sh` (reads `hack/ci-required.txt` on the head); `hack/pr-wait.sh` exits 1 to 8 | exit 1 (not green), 2 (timeout), 3 (closed), 4 (conflict), 6 (stale head), 7 (open review threads), 8 (left the queue) | Fix the cause; for 7 reply to and resolve the threads (`hack/pr-threads.sh`, `docs/dev/ci.md` "Merging"); for 4 merge `origin/main` | repo |
| Merge by merge commit, through a queue | GitHub ruleset on `main`: merge queue on | PR waits in the queue | `hack/pr-merge.sh <PR>` (adds `--auto`) | owner |
| 14 required checks and resolved review threads | GitHub ruleset on `main` | PR `BLOCKED` | as the row above; `hack/ci-fail.sh <PR>` reads why a check failed | owner |
| One team approval for PRs that touch `.github/**` | GitHub ruleset on `main` (required reviewers on `.github/**` only) | PR `BLOCKED`, "required review" | A human approval, or the owner merges with `--admin` (only a PR with a `.github/` path) | owner |
| An admin merge is the exception | Claude Code permission classifier (outside the repository); `hack/pr-merge.sh --admin` | tool call denied | Today: the human owner runs `hack/pr-merge.sh <PR> --admin`. See section 2 | owner |
| A new `ci.yml` job is listed in `hack/ci-required.txt` | `hack/ci-required.txt` and its check `hack/ci-required-test.sh` (`docs/dev/ci.md` "required status checks") | the check fails | Add the job to the list, or mark it advisory, in the same PR | repo |
| Worktrees are removed only by `hack/wt.sh` | `hack/wt.sh rm` refuses a live claim, a process in the tree, uncommitted changes, unpushed commits | `wt: refusing to remove <name>: <reason>` | Commit, push or release first; `--force` for your own worktree only | repo |
| Ship from a feature branch, to a branch named for an issue | `hack/ship.sh` (refuses `main`, a merged or closed PR, a bad branch name) | `ship: FAIL ...` (exit 1) | Branch `<type>/<N>-<slug>` | repo |
| Commit and merge commit carry an allowed git identity | `hack/git-identity-check.sh` (used by `hack/ship.sh`, the `hack/githooks/` pre-commit and pre-merge-commit hooks, and CI); list: `hack/git-identities.allowed` | exit 1 with the identity named; `ship: FAIL identity` | Set the repository git identity; a new contributor adds a line to the list in a PR. In CI the allowed list is read from the base commit, so a new contributor's line counts from their next PR | repo |
| Migrations are numbered, forward-only and, on a patch line, expand-only | `hack/migrate-lint.sh` (ADR 0047, 0085, 0099) | the script exits 1 with the file and rule | Add a new migration instead of editing a merged one; `hack/migrate-renumber.sh` after merging main | repo |
| A dependency is at least 3 days old | `hack/dep-age-check.sh` (ADR 0079) | the script exits 1 with the version and its age | Pick an older version, or wait; a security fix follows the fast path in ADR 0079 | repo |
| Release tags are immutable | the `release tags: immutable` ruleset (`docs/dev/releasing.md`) | push of a moved or deleted tag is refused | Cut the next rc tag | owner |
| Write only where the sandbox allows | Claude Code sandbox: repository (`.claude/settings.json` entries) and owner (local settings); worktrees under `/tmp/kw` | `Operation not permitted` | `hack/wt.sh new`; `$TMPDIR`. `hack/lane-gates.sh` reports a sandbox denial as `notrun` | repo and owner |
| No cluster by default | rule in AGENTS.md "Working rules" (explicit `KUBECONFIG`); no hook | text only | `KUBECONFIG=/dev/null`, or an explicit file | repo |
| Do not relay the owner's approval to another session of the owner | `~/.config/koinon/hooks/relay-guard.sh`, PreToolUse on SendMessage and AskUserQuestion (read in full 2026-11-09); it also allows a message that carries only the request or the question | deny with a reason | The other session asks the owner itself | human owner only, not in the repository |
| Compact the main session past 300k context | `~/.config/koinon/hooks/context-cap.sh`, PostToolUse hook and status line (read in full 2026-11-09) | a message to compact, once per 50k step | Update the state files and ask the owner to run `/compact` | human owner only, not in the repository |
| Questions to the owner go through the question tool | `~/.config/koinon/hooks/grilling-question-tool.sh` (PreToolUse on Skill) and `prose-question-stop.sh` (Stop hook); read in full 2026-11-09 | the stop is blocked once, with a request to ask again through the tool | Ask through `AskUserQuestion` | human owner only, not in the repository |

Guards in the table: 29.

## 2. Guards with no legal route today

1. **Admin merge by an agent.** The ruleset needs one team approval for PRs that touch `.github/**`
   (see the ruleset row; before 2026-11-11 it covered the 15 sensitive path patterns, which 74% of merged PRs touched: 293 of 395 PR
   merges in the 30 days to 2026-11-09, from the diff of each first-parent merge). The permission
   classifier denies `--admin` from an agent (4 of 4 on 2026-11-08, from the audit record).
   `hack/pr-merge.sh` has `--admin`, but the agent cannot use it. Since 2026-11-11 only a PR with a `.github/` path needs `--admin` (ADR 0103 superseded); the owner runs the command; the agent hands over
   `hack/pr-merge.sh <PR> --admin --approved-at <time>` and the evidence.
2. **The hook set is split.** `sleep-chain.sh`, the second spawn hook and the digest live in
   `~/.config/koinon/hooks`, not in the repository, so a cloud lane or a second contributor has
   other guards than this table says. Smallest fix: move the repository-relevant hooks to
   `hack/hooks/` with tests, and register them in `.claude/settings.json`.
3. **Hooks run from the main checkout.** `.claude/settings.json` starts them from
   `$CLAUDE_PROJECT_DIR`, the main checkout, not from a lane's worktree. A lane gets the hook version
   of the main checkout, which can be many commits behind `origin/main`. Smallest fix: make "main
   checkout at `origin/main`" a session-start step, or resolve the hooks from the worktree.
4. **Missing bash 5 turns a repository hook off.** The hook prints a `systemMessage` and exits 0.
   The guard then does not exist, and nothing says so to the model. Smallest fix: fail closed for
   Agent spawns, or have `hack/preflight.sh` report the missing bash.

Guards with a legal route and a high false-refusal rate:
- The spawn hooks match the string `docs/agents/subagent-brief.md` in the prompt. The route exists
  (put the path in the prompt), but on 2026-11-09 the hook refused 3 of 4 reader agents of the audit.
  The repository hook prints no digest; the owner-local hook prints it and still denies. Smallest
  fix: section 3.

Already repaired or not a contradiction on this tree:
- `hack/lane-gates.sh` reported a sandbox denial as a failed gate. Fixed on 2026-11-08 or before
  (commits `4edea9286`, `6ec112458`, `e98781670`); it now prints `verdict=notrun` with the reason
  `sandbox denial`.
- The brief against `sleep-chain.sh`. The brief (`docs/agents/subagent-brief.md`, "Work in your
  worktree", the "No standalone `sleep`" bullet) forbids a foreground sleep loop and sends waits to
  `hack/pr-wait.sh`, `run_in_background` or a Monitor call. `sleep-chain.sh` is a PreToolUse hook on
  the Bash tool only, so it never sees a Monitor call. The two do not conflict.

Not checked against the live system: the hook refusal count (about 375 on 2026-11-08) and the
classifier record. They are from the audit.

## 3. Proposed hook change (text only)

Checked against https://code.claude.com/docs/en/hooks on 2026-11-09:
- `SubagentStart` exists. Its `hookSpecificOutput.additionalContext` is added to the subagent's
  context at the start of its conversation, before its first prompt. It cannot block a spawn. Its
  event fields are `agent_id` and `agent_type`; no prompt.
- PreToolUse `additionalContext` goes to the calling model, beside the tool result. It does not
  reach the new subagent.
- PreToolUse `updatedInput` is still supported. The page section "PreToolUse `updatedInput` in auto
  mode" says that without a `permissionDecision`, auto mode can deny the changed call, because the
  server reviewed the input that the model wrote. This matches the documented behaviour and the 18
  refusals on 2026-11-03 (the owner removed `updatedInput` on 2026-11-04). Whether
  `permissionDecision: "allow"` prevents the denial: not verified.

So the proposal is: the spawn hook denies only for a missing `model`, and a SubagentStart hook
injects the digest. That hook cannot see the prompt, so it injects the digest into every subagent.
This costs about the digest's size per spawn: `subagent-brief-digest.txt` is 1658 bytes, 255 words.

The proposal adds `subagent-brief-digest.txt` to `hack/hooks/` (today it exists only in
`~/.config/koinon/hooks/`), adds `hack/hooks/subagent-brief-inject.sh`, and registers a
SubagentStart entry for it in `.claude/settings.json`.

In `hack/hooks/agent-spawn-check.sh`, replace everything from `missing=()` to the end with the
snippet below. It prints exactly one JSON object; on a deny, `systemMessage` is in the same object
as `hookSpecificOutput`. `hack/hooks/agent-spawn-check_test.sh` must change with it.

```bash
missing=()
note=
[ -z "$model" ] && missing+=('set "model" (haiku, sonnet or opus)')
case "$type" in
  Explore | claude-code-guide | web-fetch | statusline-setup | claude-security:*) ;;
  *) grep -q 'docs/agents/subagent-brief.md' <<<"$prompt" ||
       note='No docs/agents/subagent-brief.md in the prompt: the SubagentStart hook adds the digest.' ;;
esac
if [ ${#missing[@]} -gt 0 ]; then
  reason="Spawn rule: $(IFS='; '; echo "${missing[*]}")."
  jq -nc --arg r "$reason" --arg n "$note" \
    '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}
     + (if $n == "" then {} else {systemMessage:$n} end)'
elif [ -n "$note" ]; then
  jq -nc --arg n "$note" '{systemMessage:$n}'
fi
```

`hack/hooks/subagent-brief-inject.sh` prints:

```bash
jq -nc --rawfile d "$(dirname "$0")/subagent-brief-digest.txt" \
  '{hookSpecificOutput:{hookEventName:"SubagentStart",additionalContext:$d}}'
```

## 4. Rule for maintainers

A new guard (hook, script refusal, required check, ruleset rule) comes with its row in section 1 and
its legal route, in the same PR. A PR that adds a guard without the row is not ready. When an agent
reports a refusal that has no route, add it to section 2 with the evidence and the smallest fix.
