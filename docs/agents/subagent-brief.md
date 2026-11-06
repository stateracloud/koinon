# Subagent brief

How a subagent works in this repository. A parent session points its subagents here ("Follow
docs/agents/subagent-brief.md") instead of retyping the same rules in every prompt; the prompt then
carries only the task, the worktree and the scope. The shell tool is `Bash`.

## Which model
Parents pick the model by the work; it applies to cloud lanes and `SendMessage` briefs too (paste or link this section there, the spawn hook does not see them).
- haiku: mechanical, checkable work: test and log runs, no product code. It never fixes code: on a failure it stops and reports the evidence.
- sonnet: implementation of settled designs, tests from a spec, conflict merges, doc and CI fixes.
- opus: design, research against source, security, hard bugs, review.
- Verify every success claim a subagent makes before you repeat it.

## Find before you read
- Code questions go to CodeGraph first: the `codegraph_explore` MCP tool, or `codegraph explore
  "<symbol names or a question>"` in a shell. One call returns the relevant symbols' source, their
  callers and what depends on them. Do not start with `grep -rn` or `cat` on whole files.
- When you must read a file, read the part you need: `sed -n 'A,Bp'` (at most about 120 lines) or the
  Read tool with an offset and limit. Never `cat` a large file or a log whole; filter it first.
- One file and one range per call: no `cat a b c`, no several `sed -n` in one command, no loop
  that prints files. Bundled reads were 20% of all subagent output (docs/research/2026-11-token-tiers.md).
- Output too large and saved to a file: grep that file or Read it with offset and limit, never whole.
- `git diff --stat` before `git diff`; then one path at a time, `| head -150` when it is long.
- Edit with the Edit tool (exact string replacement), not with Python or sed heredocs. Before an
  Edit, read the lines with the Read tool or `codegraph_explore` (`sed`/`cat` output does not
  count). New files: the Write tool, not a heredoc.
- Read logs and task output with the Read tool on the absolute path, not `cat`/`tail` through
  `Bash`, and one file per call (no bundled reads).
- Research spawns: `codegraph_explore` first for repo questions, about 40 tool calls at most, and a
  hand-back of at most 60 lines that cites its sources.

## Work in your worktree
- `hack/wt.sh new` uses `/tmp/kw` regardless of the parent's or lane's `TMPDIR` and prints the
  canonical absolute path (`/private/tmp/kw/...` on macOS). The repository's Claude sandbox settings
  allow writes to this root. Parents pass the printed path verbatim to the lane. An explicit
  `KOINON_WT_ROOT` overrides the root; add that location to `sandbox.filesystem.allowWrite` in your
  local Claude settings before starting lanes there. Keep the sandbox enabled.
- The parent gives you a worktree. Run every command against it: `git -C <worktree> …`, or absolute
  paths. The shell's directory resets between calls.
- Never call `hack/claim.sh`: the parent owns claims (one lane per subagent, ADR 0082).
- No cluster: `KUBECONFIG=/dev/null` for every test. CI never calls paid models; neither do you.
- Scripts need bash 5 (ADR 0090; the PATH has Homebrew's on a Mac): run them and their tests with `bash` or directly, not `/bin/bash` (3.2, only to prove a guard fires). Use `/bin/ls`, not `ls`.
- Call hack scripts by absolute path (`<worktree>/hack/x.sh`), never `cd X && hack/x.sh`.
- No standalone `sleep`, and no `until … sleep` loop in the foreground. To wait on a pull request use
  `hack/pr-wait.sh`. For anything else: `run_in_background`, then keep working or end the turn; wait
  on a condition with Monitor and an until-loop that tests it (the Monitor tool needs a
  `description`). Never sleep-then-read, never Monitor a bare sleep, and never `pgrep`/`pkill`
  another lane's processes. Never `pkill`/`pgrep` by script name (`pkill -f hack/preflight.sh` kills
  every lane's run): stop your own preflight with `hack/preflight.sh --stop`, which uses the pid file
  of your worktree's log directory.
- Remove worktrees only through `hack/wt.sh rm <name>` (or `hack/wt.sh gc`), never `git worktree remove` or `rm -rf`: it refuses a worktree that has a live claim, a process in it, uncommitted changes or unpushed commits, because another session may be using it. `--force` is for your own worktree only.
- Never `git stash`: it is shared across worktrees. Commit, or leave the change in place.

## Check and ship
- `hack/preflight.sh` runs the gates your change touches (lint with gofumpt, vet, tests, tidy,
  generate, migrations, ADRs, docs, chart, web, shellcheck, gitleaks) and prints one line per gate.
  It must pass before you hand back.
- If the issue names a gate (escape, e2e, chart smoke), run it and hand back its real result. If it
  cannot run here, hand back a static stand-in (`helm template`) and say it is only that.
- Commit with `git commit -s` using the repository's git identity, in Conventional Commits. Do not
  push or open pull requests unless the prompt says so; the parent ships with `hack/ship.sh`.
- Fake secrets in tests are obviously fake from the first commit (`koinon_test_not-a-real-token`).

## Review threads from bots
- The ruleset requires every review thread resolved; DeepSource and CodeRabbit leave inline comments that
  block the merge with every check green (#187). `hack/pr-wait.sh <PR> --checks` and `hack/pr-merge.sh`
  list them (`UNRESOLVED path:line author id=PRRT_...`) and exit 7; `hack/pr-threads.sh <PR>` lists them alone.
- A lane fixes the real findings inside its fix rounds. The parent, before the merge, replies to every
  remaining bot thread (`fixed in <sha>`, or why not) and resolves it: GraphQL
  `addPullRequestReviewThreadReply`, then `resolveReviewThread` (commands in docs/dev/ci.md "Review
  threads"). A transient `Something went wrong` on resolve is retried with backoff. Never resolve a thread
  unread, and never from a lane that does not own the PR's replies.

## Hand back
- At most 40 lines: what changed (files grouped), the commits, each check with its real result,
  what you could not run and why, and where you deviated from the prompt.
- Return the content in the hand-back itself. Write a report file only when the prompt names the path.
- If a tool call is refused, stop and say which one and why. Do not look for another way to do the
  same thing.

## For parents
- Every spawn prompt names this brief, a model, the worktree and the scope.
- Spawns are checked by `hack/hooks/agent-spawn-check.sh` (a PreToolUse hook in `.claude/settings.json`): a model and this brief are required (koinon-private#397).
- Reads are checked too (koinon-private#534): `hack/hooks/read-result-check.sh` refuses a Read of `tool-results/` without `limit`; `hack/hooks/bash-read-check.sh` refuses a Bash call that prints several files, loops over files or cats a whole source file.
- No third fix round when round 2 still turns up a new class of finding: stop and ask the human through the question tool, with research as the recommended option.
- One fix round = one fresh agent, given the worktree, the findings and the head SHA. Never resume an agent past one compaction or about 120k context.
