# 0104. Work claims reduced to the lock: no lease, no heartbeat, holder is login and lane

- Status: Accepted (procoduck, 2026-11-09: "reduce claims to the lock")
- Date: 2026-11-09
- Amends: [0082](0082-work-claims-and-ready-queue.md) decisions 1, 2, 3, 5, 6, 7 and 10 (and its lanes);
  [0094](0094-work-claims-on-the-public-repository.md) decisions 1, 2, 3, 5, 6 and 7
- Relates to: [0046](0046-adr-conventions.md), [0082](0082-work-claims-and-ready-queue.md), [0094](0094-work-claims-on-the-public-repository.md)
- Exit condition: leases and takeover come back (a new ADR) when a second human merges work every week

## Context

ADR 0082 built a lock for several humans, each with several agents: a claim ref as the compare-and-set,
plus a 2 h lease, a heartbeat every 30 minutes, a sweeper, takeover, adopt, a session identity in the
holder, a board mirror and an issue-comment mirror. ADR 0094 added claims by comment, outsider classes,
the `claim:stale` label and more lease rules. `hack/claim.sh` grew to 1787 lines.

Today one human works on the repository. The parts that exist for a second human or for a dead agent
cost more than they give:

- A session restart changes the holder (`<login>/<host>/<session>`), so a restarted session loses its
  own claims until it runs `adopt`. Every agent must export a session id.
- A claim expires unless a background `beat` loop runs. A lost loop releases live work; the sweeper
  then labels and releases it.
- The board and comment mirrors write on every take, beat and review, and fail on tokens that cannot
  write an organization project.

What the lock must still do: two lanes must never take the same issue, and an ADR number must never be
used twice. The compare-and-set on the ref does that alone.

## Decision

1. **The ref stays the only truth.** `claim/<N>` (and `claim/adr-NNNN`), created by `POST git/refs`
   and moved by a non-forced `PATCH` (fast-forward), on an empty-tree commit whose message records
   `holder`, `human`, `pr`, `note`, `state` and, on a release, `released_by`. Refs are never deleted.
   There is no `lease_until`.
2. **Holder = `<login>#<lane>`.** No host and no session. `--lane L` (`^[a-z0-9][a-z0-9-]{0,15}$`) on
   every command; the default lane is `main`, the lane of a session that runs no parallel agents. Same
   login and lane is the same holder, so a restarted session keeps its claims with no environment
   variable. Two sessions in one lane are one holder; the lock does not separate them. Each session
   that runs at the same time as another one uses its own `--lane`. `take` of an issue the same
   holder already holds exits 0 and warns on stderr with the claim's commit date, so a second
   session sees it.
3. **No lease, no heartbeat, no sweeper, no takeover, no adopt.** A held claim stays held until a pull
   request that closes the issue merges (the `release` job of `claims.yml` runs `release-merged` on
   the push to main, ADR 0082 amendment #463) or its human releases it. Since `pull_request_target`
   is gone, a close without a merge runs nothing: its human runs `release-pr <PR>` (it unassigns) or
   `release <N>`. That is also the backstop when `release-merged` misses a merge; ADR 0082's sweeper
   and grace rule do not apply. `release-merged` also releases the claims of a stacked pull request
   that was merged into another pull request's branch (`commits/{sha}/pulls` returns it as merged);
   that is harmless, since its commits reach main only with the outer merge. A stuck claim is released by its human: `release <N> --force`
   releases a claim of another lane of the same human. Another human's claim is released only by
   that human (ask them).
4. **Limits, best effort:** one claim per lane (one `take` of several issues for one pull request
   counts once) and eight held claims per human. They are read, then written, so two concurrent takes
   can pass a limit; the lock on each issue still holds.
5. **The assignee is the only mirror.** `take` assigns the human; `release`, and `release-pr` for a
   pull request closed without a merge, unassign. No board Status writes, no issue comments, no `claim:stale` label. Board Status
   comes from the project's built-in workflows (below).
6. **Commands:** `next`, `take`, `release`, `block`, `show`, `mine`, `adr`, `sensitive-paths` (output
   unchanged), `check-pr`, `release-pr`, `release-merged`, `help`. Removed: `beat`, `adopt`, `review`, `sweep`,
   `comment`, `wait-release`, `limits`, `--human`, `--takeover` and the outsider classes.
   `update-oldest` (the branch updater, not a claim) moves to `hack/pr-updater.sh`.
7. **The `claim` check** (`check-pr`, required) fails only when another human holds a claim on an
   issue the pull request closes, or reserved an ADR number the pull request adds. A pull request
   without `Closes #N` passes. The merge-queue fence of ADR 0094 decision 4 stays. Known limit: the
   check reads the pull request's body and its closing references when it runs. A `Closes #N` in a
   commit message, or a link made in the sidebar after the last event that ran the check, closes
   the issue at the merge although the check did not see it.
8. **ADR reservations are permanent.** `adr` writes `state: reserved` and never expires; a reservation
   from before this ADR (`state: held` with a lease) counts as reserved and its lease is ignored.
9. **`hack/wt.sh rm`** refuses a worktree whose branch issue has a held claim, whatever lease line an
   old claim carries.

### Board Status (checked 2026-11-09)

Project 1 ("Koinon") has these built-in workflows on: Auto-add sub-issues, Auto-add to project, Item
added to project, Item closed, Pull request linked to issue, Pull request merged (Auto-close issue is
off). GitHub's "Pull request linked to issue" workflow sets the linked issue to **In Progress** when a
pull request with `Closes #N` is opened (changelog 2025-11-06). **Done** comes from "Item closed" and
"Pull request merged". **In Review has no built-in trigger:** no workflow fires on "ready for review"
or on a review request, so nothing sets it after this change. `next` already treats In Progress and
In Review with no holder as Ready, so the queue does not depend on it. If In Review is needed on the
board, it is a separate, small workflow (not part of the lock); procoduck may veto its removal.

### Transition

At the merge, about 6 claims are held in the old format (`procoduck/MacBookPro/<session>#l<N>`, with a
lease), about 140 are released and 12 are `claim/adr-*` reservations.

- The new script reads every old ref without error. An old holder reads as its login in its lane (no
  lane: `main`), and its login may release it from any lane. Old held claims stay held, with no lease,
  until their pull request merges or procoduck releases them. `mine` and `show` list them
  as held, `release N` works for their login without `--force`, and `check-pr` passes a pull
  request of that login. They count toward the cap of eight held claims per human, so procoduck
  releases the ones with no pull request (`hack/claim.sh release N`).
- The sweeper schedule ends at the merge (schedules run from the default branch), so no old sweeper
  releases a claim after it.
- For some hours old scripts still run in open sessions and from old pull request bases (the
  `claim` job checks out the base commit):
  - an old `take` reads a new claim's missing lease as "cannot judge liveness" and refuses (safe);
  - an old `check-pr` reads a new claim with no lease as free, which lets only another human's pull
    request through (there is none);
  - an old `release-pr` releases a new claim as before (it reads `state` and `pr` only);
  - an old `beat --mine` loop keeps renewing old-format claims; a `release-pr` release (note
    "pull request #P closed") is dropped from its list, and a release by the new `release`
    (`released_by` without `/`) makes it report "lost" and exit 3. Stop those loops after the merge;
  - an old `mine` hides new claims, so an old `pr-merge.sh` warns on its `review` call and still merges;
  - an old `wt.sh rm` calls a new claim's state unknown and refuses without `--force` (safe).
- The `claim` job of `claims.yml` (on `pull_request` since #463) checks out the base branch's tip at
  run time (`pull_request.base.ref`), not `pull_request.base.sha` (the base of the PR's last push,
  which can be many commits behind), so every run after this merge uses the new `check-pr`. A
  `merge_group` run checks out `merge_group.base_sha`, the queue entry's base. The `release` job
  runs on the push to main with main's own script, so it always runs the new `release-merged`.
- New reservations (`state: reserved`) pass an old `check-pr`, which treats any state but `held` as
  released; only another human could collide, and there is none.

## Alternatives considered

- **Keep leases, drop only the mirrors.** Keeps the restart problem (holder has a session id) and the
  background loop. Rejected: liveness is the expensive part, and one human can release by hand.
- **Expire claims on a crash (a TTL on the lock).** The common advice for agent swarms (Authora,
  below). It protects a team from an abandoned claim; with one human the abandoned claim is that
  human's, and that human releases it.
- **Use the assignee as the lock** (Prow `/assign`, rustbot `@rustbot claim`). Last-write-wins, so two
  agents can both "win". Rejected: the ref's compare-and-set is the reason the claim exists.
- **Drop claims and rely on worktrees.** Worktrees isolate files, not intent: two lanes can still start
  the same issue.

## Prior art

About 15 minutes, 2026-11-09:

- Prow's `/assign` and rust-lang's rustbot `@rustbot claim` make the assignee the claim, with no lock:
  enough for humans who see each other, not for parallel agents.
- File-based agent coordination (agentpatterns.ai) and Anthropic's parallel-Claudes compiler use a git
  push rejection as the lock: the same compare-and-set as our ref.
- Coord returns HTTP 409 on a second claim (an atomic server-side claim); Claude Code Agent Teams keep a
  shared task list. Both separate "who holds it" from "is it alive".
- dev.to posts (2026) say a claim is advisory, that worktrees do not coordinate intent, and that a
  claim should expire on a crash. We keep the atomic claim and drop expiry, because one human
  releases. A blog simulation (not verified) reports duplicate work falling from 78% to 0% with a lock
  plus shared state.

## Consequences

- Positive: `hack/claim.sh` goes from 1787 to about 650 lines and its test from 1078 to about 330, all
  offline. No background loop, no session id, no board token. A restart keeps the claims.
- Positive: `hack/ship.sh` and `hack/pr-merge.sh` no longer call `claim.sh` (`--review` and `--lane`
  are gone); the merge releases the claim.
- Negative: a claim whose human forgets it stays held, also after its pull request closes without a
  merge. `mine` shows it; `release` or `release-pr` ends it.
- Negative: In Review is no longer set on the board (see Board Status).
- Negative: the limits are best effort.
- Follow-ups: release the old held claims or merge their pull requests; stop running `beat` loops.

## Amendment (2026-11-11)

Owner decision (procoduck, 2026-11-11): "Remove claims limit for now. It does not make sense having it
while no one else contributes." The decision above stays as written; this amendment changes one
limit.

- The cap of eight held claims per human is off by default. `KOINON_CLAIM_HUMAN_CAP=N` turns it on
  (empty or 0 means no cap), so it can come back when a second human joins.
- One claim per lane is unchanged.
- Exit condition: set the cap again (a default in `hack/claim.sh`) when a second human works on the
  repository.

## Links

- GitHub changelog, 2025-11-06, improved onboarding flow for GitHub Projects (the "Pull request linked
  to issue" workflow): https://github.blog/changelog/2025-11-06-improved-onboarding-flow-for-github-projects/
- File-based agent coordination: https://www.agentpatterns.ai/multi-agent/file-based-agent-coordination/
- Preventing agents from editing the same files: https://dev.to/_3ffb5be7950c3a975b5d/how-to-prevent-multiple-ai-coding-agents-from-editing-the-same-files-4aib
- Coord: https://aiindigo.com/tutorials/getting-started-with-coord-eliminate-redundant-ai-agent-work
- File-level locking for agents (Authora): https://dev.to/authora/stop-your-ai-coding-agents-from-fighting-file-level-locking-for-shared-codebases-3pej
- Duplicate-work simulation: https://codex.danielvaughan.com/2026/06/20/before-the-pull-request-multi-agent-coordination-codex-cli-grite-duplicate-work-prevention/
- Claude Code Agent Teams: https://www.mindstudio.ai/blog/claude-code-agent-teams-shared-task-list
