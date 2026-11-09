<!-- SPDX-License-Identifier: Apache-2.0 -->

# Guards around agents

A guard is anything that stops or redirects an agent: a hook, a script refusal, a CI check, the GitHub
ruleset, the Claude Code permission classifier or the sandbox. Guards come from different people at
different times. This table is the one place that lists them together.

Rule: each guard has a legal route. When a guard refuses, the agent can reach the same goal with a
command or step that the other guards allow. A guard with no legal route is a defect (section 2).

Status of this table: written 2026-11-09 from the files named in it. Where a guard lives outside the
repository, the row says so; the live ruleset and the classifier were not re-read from GitHub or Claude
Code for this table.

## 1. The table

Owner: "repo" means a pull request changes it. "Owner" means only the human owner can change it.

| Rule | Guard, where | What the agent sees | Legal route | Owner |
|---|---|---|---|---|
| A spawn sets a model | `hack/hooks/agent-spawn-check.sh`, PreToolUse on Agent (`.claude/settings.json`); the owner's local copy is `~/.config/koinon/hooks/agent-spawn-check.sh` | deny: `set "model"` | Send the spawn again with `model` (rules: `docs/agents/subagent-brief.md`, "Which model") | repo (local copy: owner) |
| A spawn names the brief | same hook | deny: `name docs/agents/subagent-brief.md in the prompt`; the local copy adds the brief digest to the reason | Put the path in the prompt. Explore, claude-code-guide, web-fetch, statusline-setup, claude-security:* and fork need no path | repo (local copy: owner) |
| No `sleep` of 5 s or more in the foreground | `~/.config/koinon/hooks/sleep-chain.sh`, PreToolUse on Bash (not in the repository) | deny: `Sleep chain blocked` | `hack/pr-wait.sh <PR> --checks` for a PR; `run_in_background` for a long command; `hack/claim.sh wait-release` | owner |
| Read a file in small parts | `hack/hooks/read-result-check.sh` (Read of `tool-results/` needs `limit`) | deny with reason | Read with `offset` and `limit`; grep the saved file | repo |
| One file per Bash read, no loops that print files, at most 210 `sed -n` lines per call | `hack/hooks/bash-read-check.sh` | deny: `Prints several files or a whole source file` | One Read call per file; `codegraph_explore`; `grep -n` | repo |
| Hooks need bash 5 | first line of each hook in `hack/hooks/` | `systemMessage`: hooks skipped | `brew install bash` (ADR 0090). Note: the hook is then off, not satisfied | repo |
| Only the parent takes claims | `docs/agents/subagent-brief.md` ("Work in your worktree"); `hack/claim.sh` exit 3 when a claim is lost | text only; no hook | The parent runs `hack/claim.sh take/beat/review` | repo |
| One claim per issue; lease 2 h; beat every 30 min | `hack/claim.sh` (claim branches, ADR 0082); CI check `claim` (`.github/workflows/claims.yml`) | exit 2 (taken), exit 3 (lost); `claim` red on the PR | `claim.sh next`, then `take`; `block` or `release --note` to hand off | repo |
| Sign-off on every commit | CI check `dco` (`.github/workflows/dco.yml`) | `dco` red | `git commit -s` | repo |
| No secret in any commit of a PR | CI check `gitleaks` (`.github/workflows/secrets.yml`) | `gitleaks` red; a later fix commit does not clear it | Fake secrets that look fake from the first commit; `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .` before the push | repo |
| A needs-e2e PR has a green e2e | CI check `e2e result` (`.github/workflows/e2e-kind.yml`); `hack/needs-e2e.sh` | `e2e result` red without a green `e2e-lab` status or `quickstart` run | `hack/e2e-remote.sh` (lab), or `gh workflow run e2e-kind.yml --ref <branch>` | repo |
| A draft PR is not merge evidence | `go` and `web` show red on a draft on purpose (`.github/workflows/ci.yml`); `hack/pr-merge.sh` exit 5 | red checks; exit 5 | `hack/ship.sh --ready` or `gh pr ready`, then wait. WIP feedback: `hack/preflight.sh` | repo |
| Merge only with every required context green | `hack/pr-merge.sh` (reads `hack/ci-required.txt` on the head); `hack/pr-wait.sh` exits 1 to 8 | exit 1 (not green), 2 (timeout), 3 (closed), 4 (conflict), 6 (stale head), 7 (open review threads), 8 (left the queue) | Fix the cause; for 7 reply to and resolve the threads (`hack/pr-threads.sh`, `docs/dev/ci.md` "Merging"); for 4 merge `origin/main` | repo |
| Merge by merge commit, through a queue | GitHub ruleset on `main`: merge queue on | PR waits in the queue | `hack/pr-merge.sh <PR>` (adds `--auto`) | owner |
| 14 required checks and resolved review threads | GitHub ruleset on `main` | PR `BLOCKED` | as the row above; `hack/ci-fail.sh <PR>` reads why a check failed | owner |
| One approval by a team for 15 path patterns | GitHub ruleset on `main` (to be replaced by a `review` check, ADR 0103, in progress) | PR `BLOCKED`, "required review" | A human approval; the owner merges with `--admin` where the ruleset allows the bypass | owner |
| An admin merge is the exception | Claude Code permission classifier (outside the repository); `hack/pr-merge.sh --admin` | tool call denied | Today: the human owner runs `hack/pr-merge.sh <PR> --admin`. See section 2 | owner |
| A new `ci.yml` job is listed in `hack/ci-required.txt` | `hack/ci-required.txt` and its lint (`docs/dev/ci.md` "required status checks") | lint fails | Add the job to the list, or mark it advisory, in the same PR | repo |
| Worktrees are removed only by `hack/wt.sh` | `hack/wt.sh rm` refuses a live claim, a process in the tree, uncommitted changes, unpushed commits | `wt: refusing to remove <name>: <reason>` | Commit, push or release first; `--force` for your own worktree only | repo |
| Ship from a feature branch, with an allowed identity, to a branch named for an issue | `hack/ship.sh` (refuses `main`, a merged or closed PR, a bad branch name, a bad identity, a lost claim) | `ship: FAIL ...` (exit 1), exit 3 for a lost claim | Branch `<type>/<N>-<slug>`; set the repository identity; stop on exit 3 | repo |
| Write only where the sandbox allows | Claude Code sandbox (`.claude/settings.json` `sandbox` entries); worktrees under `/tmp/kw` | `Operation not permitted` | `hack/wt.sh new`; `$TMPDIR`. `hack/lane-gates.sh` reports a sandbox denial as `notrun` | owner |
| No cluster by default | rule in AGENTS.md "Working rules" (explicit `KUBECONFIG`); no hook | text only | `KUBECONFIG=/dev/null`, or an explicit file | repo |

Guards in the table: 22.

## 2. Guards with no legal route today

1. **Admin merge by an agent.** The ruleset needs an approval for most PRs. The permission classifier
   denies `--admin` from an agent (4 of 4 on 2026-11-08, from the audit record). `hack/pr-merge.sh`
   has `--admin`, but the agent cannot use it. Smallest fix: the `review` check of ADR 0103 (then the
   approval need goes away for these PRs). Until then, the owner runs the command; the agent hands
   over `hack/pr-merge.sh <PR> --admin --approved-at <time>` and the evidence.
2. **Spawn hook refuses on a string match.** Still true in `hack/hooks/agent-spawn-check.sh`: it greps
   for `docs/agents/subagent-brief.md`. On 2026-11-09 it refused 3 of 4 reader agents of the audit. The
   repository copy prints no digest at all; the local copy prints it but still denies. Smallest fix:
   section 3.
3. **Brief and hook disagree on waiting.** `docs/agents/subagent-brief.md` allows "Monitor and an
   until-loop". `~/.config/koinon/hooks/sleep-chain.sh` denies a foreground `sleep` of 5 s or more,
   also inside a loop. The audit counted up to 93.6M tokens in one day. The loop in a Monitor runs
   in the foreground command of the Monitor tool; the hook has no way to tell. Smallest fix: the
   brief says "`hack/pr-wait.sh`, or `run_in_background`" only; or `sleep-chain.sh` allows a Monitor
   call. Pick the first: it needs no change outside the repository.
4. **The hook set is split.** `sleep-chain.sh` and the digest live in `~/.config/koinon/hooks`, not
   in the repository, so a cloud lane or a second contributor has other guards than the table says.
   Smallest fix: move the repository-relevant hooks to `hack/hooks/` with tests, and register them in
   `.claude/settings.json`.
5. **bash 5 missing turns a hook off.** The hook prints a `systemMessage` and exits 0. The guard then
   does not exist, and nothing says so to the model. Smallest fix: fail closed for Agent spawns, or
   have `hack/preflight.sh` report the missing bash.

Already repaired:
- `hack/lane-gates.sh` reporting a sandbox denial as a failed gate: fixed on 2026-11-08 or before
  (commits `4edea9286`, `6ec112458`, `e98781670`); it now prints `verdict=notrun` with the reason
  `sandbox denial`.

Not checked against the live system: the hook refusal count (about 375 on 2026-11-08) and the
classifier record. They are from the audit.

## 3. Proposed hook change (text only)

Claude Code PreToolUse hooks can return `updatedInput`, but the owner removed it on 2026-11-04
(retro point 8): a rewritten prompt, changed after auto mode had judged it, made auto mode refuse the
call (18 refusals on 2026-11-03). `additionalContext` on a PreToolUse hook reaches the calling model,
not the subagent. So neither fits. The fit is a SubagentStart hook, which can return
`additionalContext` to the subagent itself, and a spawn hook that denies only for a missing model.
Check the SubagentStart output fields in the Claude Code hook documentation before applying.

In `agent-spawn-check.sh`, replace the brief case with:

```bash
case "$type" in
  Explore | claude-code-guide | web-fetch | statusline-setup | claude-security:*) ;;
  *) grep -q 'docs/agents/subagent-brief.md' <<<"$prompt" ||
       echo '{"systemMessage":"Spawn without docs/agents/subagent-brief.md in the prompt: the SubagentStart hook adds the digest."}' ;;
esac
```

Add a SubagentStart hook (`hack/hooks/subagent-brief-inject.sh`) that prints:

```bash
jq -nc --rawfile d "$(dirname "$0")/subagent-brief-digest.txt" \
  '{hookSpecificOutput:{hookEventName:"SubagentStart",additionalContext:$d}}'
```

The subagent then always gets the digest, and no spawn is refused for a missing string.

## 4. Rule for maintainers

A new guard (hook, script refusal, required check, ruleset rule) comes with its row in section 1 and
its legal route, in the same PR. A PR that adds a guard without the row is not ready. When an agent
reports a refusal that has no route, add it to section 2 with the evidence and the smallest fix.
