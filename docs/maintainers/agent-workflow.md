<!-- SPDX-License-Identifier: Apache-2.0 -->

# Maintainer workflow for agents

**Maintainers only.** This is how the maintainers and their agents take, ship, merge and release
work: claims, lanes, the merge queue, the lab and the release gate. Contributors do not need any of
it; [AGENTS.md](../../AGENTS.md) and [CONTRIBUTING.md](../../CONTRIBUTING.md) hold everything a
contribution needs. Agents working for a maintainer read AGENTS.md first, then this file. The rules
here moved out of AGENTS.md in issue 842; they are unchanged.

## Commands

- The maintainer scripts and the repository, board and worktree settings they read (`KOINON_REPO`,
  `KOINON_BOARD`, `KOINON_WT_ROOT`; a fork needs none): [hack/README.md](../../hack/README.md).
- Claims: `hack/claim.sh help`; `hack/claim_test.sh` (offline).
- Ship a branch with `hack/ship.sh` (sign-off, blob and gitleaks checks, merge of `origin/main`,
  push, PR, board, optional auto-merge). Wait on a PR with `hack/pr-wait.sh`
  (in the background) and merge it with `hack/pr-merge.sh <PR>` (refuses a draft, reads every
  required context on the head); read why CI failed with `hack/ci-fail.sh <PR>`. Worktrees:
  `hack/wt.sh`.
- Mark the PR ready (`hack/ship.sh --ready`, `gh pr ready`) before you wait on it with
  `hack/pr-wait.sh`: a draft runs nothing in CI (its `go` and `web` report "draft: not run") and its
  green is not merge evidence.
- `hack/e2e-remote.sh` (lab or `--local`) is dev feedback, not merge evidence:
  [testing layers](../dev/ci.md#testing-layers).
- When main's kind e2e goes red, fix forward within 2 hours or revert the merge (`hack/e2e-bisect.sh`
  opens an `e2e-regression` issue). RC and release tags are cut only from a commit whose `quickstart`
  job succeeded (`hack/release-gate.sh` checks it).

## Picking up work

Claim an issue before you work on it. The lock is the ref `claim/<N>` (ADR 0082, reduced by ADR
0104): creating it is a compare-and-set, so two lanes cannot hold one issue. A claim has no lease and
no heartbeat; it is held until a pull request that closes the issue merges, or you release it. Use
`hack/claim.sh` (gh and jq only). The holder is `<login>#<lane>`, so a restarted session keeps
its claims; the default lane is `main`.

1. `hack/claim.sh next` (or `next --label start-here`): Ready, `agent-ok`, current-milestone,
   unblocked issues that nobody holds, by Priority then age. Only humans move issues to Ready; never
   start an issue that is not listed unless your human names it. Then the overlap check (below).
2. `hack/claim.sh take <N>` (exit 2: pick another issue). One claim per lane; the per-human cap is
   off while one human works on the repository (ADR 0104 amendment); a PR that closes several issues
   takes them together, `take N M` (all or nothing). It assigns your human.
3. Push `<type>/<N>-<slug>` and open a draft PR with `Closes #N` (the board's built-in workflow moves
   the issue to In Progress). New work you discover becomes a Backlog issue; do not start it.
4. Finish: mark the PR ready and merge it (see [Merging](#merging)). The merge releases the claim
   (`claims.yml`); a PR closed without a merge needs `hack/claim.sh release-pr <PR>`. Never delete a
   claim branch.
5. Stop early: `hack/claim.sh release <N> --note "<state, branch, next step>"` (keep the branch and
   draft PR), or `hack/claim.sh block <N> <M>` when another issue blocks it. Exit 3 means the claim
   is not yours (another lane: `--lane L`, or `--force` for your own other lane; another human: ask
   them).

Other rules:

- A branch is merged with `origin/main` only when GitHub reports a conflict (`hack/pr-wait.sh` exit 4,
  or the merge queue drops the entry, exit 8). There is no updater bot and no `needs-update` label.
  For an `--admin` merge, merge `origin/main` and wait for green first (it skips the queue, which
  would have tested on main's tip).
- Releases: either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay
  procoduck's. Tags are immutable by ruleset: fix a bad tag with the next rc
  (`docs/dev/releasing.md`).
- Copilot coding agent pull requests are opened or adopted by a human.

## Several agents in one session (lanes)

A session that runs agents in parallel gives each one a lane: the parent takes the claim with
`hack/claim.sh take N --lane L`, creates the lane's worktree with `hack/wt.sh new`, and hands the
agent the issue, the worktree and `docs/agents/subagent-brief.md`. One issue per lane. Pick the model
from the brief's "Which model" section. The parent ships (`hack/ship.sh`) and releases; agents never
call `claim.sh`. `hack/claim.sh mine` lists your claims with their lanes. Two sessions in one lane
are one holder; the lock does not separate them. Each session that runs at the same time as another
one uses its own `--lane`. Each lane tests its own worktree: `hack/e2e-remote.sh` tests the
checkout it is run from, so run it from the lane's worktree.

## Working alongside another human

One human works on the repository today (ADR 0104); these rules hold when a second one joins. The
rules that hold for every branch (merge `main`, never rebase; dependency bumps; proto; golden files;
ADR and migration numbers; conflict classes; when to stop and ask) are in
[AGENTS.md, Branches and pull requests](../../AGENTS.md#branches-and-pull-requests). Before you
start, after `take`:

- **Overlap check.** `gh pr list --state open --json number,author,headRefName,files` and the claim
  refs (`gh api repos/{owner}/{repo}/git/matching-refs/heads/claim/`; one claim: `hack/claim.sh show
  <N>`). If an open PR or another human's claim touches the same files with the same behaviour, run
  `hack/claim.sh block <N> <M>` or ask your human. If it only appends to a shared list or adds a
  sibling file (a new ADR, test, migration, chart assertion), proceed and say so in the PR.
- **Conflict classes** (mechanical, append-site, semantic) are in AGENTS.md. Semantic (the same
  function or behaviour) with the other human's open or last-7-days work: stop, comment on both PRs,
  ask your human.
- **ADR numbers:** reserve first with `hack/claim.sh adr` (prints `NNNN`), then write the file.
- **Stop and ask your human** when the change touches a sensitive path the other human owns
  (`.github/CODEOWNERS`; either human approves the other's PR there), or you would change a public
  API or data model another open PR also changes (the latter holds for every branch: AGENTS.md).
- **Keep branches under a day.** Split the issue and release the rest instead of holding a claim for
  days.

## Merging

The only merge method is a merge commit through the merge queue, and `hack/pr-merge.sh <PR>` does
it: it refuses a draft, runs `hack/pr-wait.sh --checks`, reads every required context on the head,
then enqueues and comments on the PR. It does not call `claim.sh`: the push to main releases the
claim (`claims.yml`, `release-merged`). Do not hand-write the wait or the merge. `--jump` enqueues at
the front. `--admin` is for a PR that changes at least one `.github/**` path (owner decision, ADR
0105); an e2e-relevant one also needs a green `quickstart` on its head. `--break-glass "<reason>"
--approved-at <time>` is for an emergency the owner approved; it opens a `break-glass` issue.
Details: `docs/dev/ci.md#merging`.

The issue tracker and the triage labels the claims work from: `docs/agents/issue-tracker.md` and
`docs/agents/triage-labels.md` (linked from AGENTS.md, Agent skills).
