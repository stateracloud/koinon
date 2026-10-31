# Subagent brief

How a subagent works in this repository. A parent session points its subagents here ("Follow
docs/agents/subagent-brief.md") instead of retyping the same rules in every prompt; the prompt then
carries only the task, the worktree and the scope.

## Find before you read
- Code questions go to CodeGraph first: the `codegraph_explore` MCP tool, or `codegraph explore
  "<symbol names or a question>"` in a shell. One call returns the relevant symbols' source, their
  callers and what depends on them. Do not start with `grep -rn` or `cat` on whole files.
- When you must read a file, read the part you need: `sed -n 'A,Bp'` (at most about 120 lines) or the
  Read tool with an offset and limit. Never `cat` a large file or a log whole; filter it first.
- `git diff --stat` before `git diff`; diff one path at a time.
- Edit with the Edit tool (exact string replacement), not with Python or sed heredocs.

## Work in your worktree
- The parent gives you a worktree. Run every command against it: `git -C <worktree> …`, or absolute
  paths. The shell's directory resets between calls.
- Never call `hack/claim.sh`: the parent owns claims (one lane per subagent, ADR 0082).
- No cluster: `KUBECONFIG=/dev/null` for every test. CI never calls paid models; neither do you.
- macOS shells run bash 3.2: no `declare -A`, `mapfile` or `${v,,}`. Use `/bin/ls`, not `ls`.
- No standalone `sleep`: run long commands in the background, or poll inside one command with an
  `until … ; do sleep N; done` loop.

## Check and ship
- `hack/preflight.sh` runs the gates your change touches (lint with gofumpt, vet, tests, tidy,
  generate, migrations, ADRs, docs, chart, web, shellcheck, gitleaks) and prints one line per gate.
  It must pass before you hand back.
- Commit with `git commit -s` using the repository's git identity, in Conventional Commits. Do not
  push or open pull requests unless the prompt says so; the parent ships with `hack/ship.sh`.
- Fake secrets in tests are obviously fake from the first commit (`koinon_test_not-a-real-token`).

## Hand back
- At most 40 lines: what changed (files grouped), the commits, each check with its real result,
  what you could not run and why, and where you deviated from the prompt.
- Put anything longer in a file and give its path.
- If a tool call is refused, stop and say which one and why. Do not look for another way to do the
  same thing.
