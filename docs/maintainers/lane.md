<!-- SPDX-License-Identifier: Apache-2.0 -->

# Lane procedure

**Maintainers only.** A lane is one agent that works one issue in its own worktree and hands the
result back to a parent session. This file is the procedure. It is plain text, so any tool can run it
(Claude Code, Codex, Cursor). A tool runner only automates the steps:
[`.claude/workflows/koinon-lane.js`](../../.claude/workflows/koinon-lane.js) is the opt-in runner for
Claude Code. The claim, ship and merge steps are in [agent-workflow.md](agent-workflow.md); the
rules for the lane agent are in [the subagent brief](../agents/subagent-brief.md).

## Roles

- **Parent:** takes the claim (`hack/claim.sh take N --lane L`), makes the worktree
  (`hack/wt.sh new`), starts the lane, and after the hand-back ships (`hack/ship.sh`) and merges.
- **Lane agent:** works only in its worktree. It never calls `claim.sh`, never pushes, never opens
  or edits a PR and never comments on the issue.
- **Reviewer:** a separate agent that did not write the change.

## Steps

1. **Claim and worktree.** The parent does both. The lane gets the issue text (read once, not again
   per stage), the worktree path and the branch.
2. **Implement.** Do the prior-art check first ([below](#prior-art-first)). Fix root causes. Update
   every test, doc and CI job list that mentions what you change. Add a test for each behaviour the
   issue's "done when" names. Commit with `git commit -s` in Conventional Commits, with
   `Closes #N` in the body of the main commit.
3. **Gates.** Run `hack/lane-gates.sh origin/main` once, in the foreground. It picks the gates the
   change touches and prints one `gate=... verdict=pass|fail|notrun` line each. `notrun` is not
   green. Also run directly any test that names a file or string the diff renames or removes.
4. **Review.** One independent review of `git diff origin/main...HEAD` against the issue: spec
   (everything in "done when", nothing outside it), ripple (what else mentions the change), repo
   rules from AGENTS.md, correctness. Findings are blocker, major or minor.
5. **Fix rounds.** At most 2. Each round is one fresh agent with the worktree, the findings and the
   head SHA, and one separate signed-off commit. Run steps 3 and 4 again after each round. If round
   2 still finds a new class of problem, stop and ask the human; do not start round 3.
6. **Hand back.** At most 40 lines: what changed, the commits, each gate with its real result, what
   did not run and why, where you deviated, and a "Prior art" paragraph. The change is ready when
   every gate is `pass` and no blocker or major finding remains. Minor findings go to the parent.
7. **Ship.** The parent only. The lane never pushes.

## Prior art first

For a feature, a mechanism or a process (not a plain bug fix in our own code), look at how
well-known projects solve the same problem before you design. Read their source or process files,
not only their docs, and cite a permalink with a commit SHA.

- **Quick** (about 15 minutes): for a small mechanism. One or two projects.
- **Full:** for an ADR, a new component, security or a product choice. Do not do it inside the
  lane. Stop and tell the parent that a full research pass is needed.

The PR body has a "Prior art" section:

```markdown
## Prior art

<what you borrow, from where (links with SHA), and why you differ>
or: none found (what you searched)
or: exempt: plain bug fix
```

Quick or full, the section says which one it was. A reviewer checks that the section exists and
that its links are real.

## Security work (auth, rate limits, keys)

See "Security work" in [the subagent brief](../agents/subagent-brief.md#security-work-auth-rate-limits-keys):
a short threat model and prior art before code, and a "Threat model" section in the PR.
