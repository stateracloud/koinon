# 0082. Work claims and the ready queue: a claim branch is the lock, the board is the queue

- Status: Accepted; amended by [0094](0094-work-claims-on-the-public-repository.md) (2026-11-05) and
  [0104](0104-work-claims-reduced-to-the-lock.md) (2026-11-09: no lease, heartbeat, sweeper, takeover
  or board mirror; holder `<login>#<lane>`)
- Date: 2026-10-30
- Relates to: [0038](0038-governance.md), [0046](0046-adr-conventions.md), [0079](0079-dependency-cooldown.md)
- Verification: ref compare-and-set semantics spiked on 2026-10-30 (see Upstream verification)

## Context

Two humans (`@procoduck`, procoduck; `@ovirandunu`, a contributor with admin and full trust) each run
several coding agents against the same backlog. Nothing stops two agents picking up one issue, two
pull requests colliding on a migration or ADR number, or an agent holding an issue it has abandoned.

What GitHub gives us: assignees, labels, Project Status and comments are all last-write-wins, so
none can be a lock. The one transactional primitive is the git ref: creating a ref fails if it
exists, and a non-forced update must fast-forward. In every system that coordinates agents well
(Anthropic's parallel-Claudes compiler used a push rejection as the lock; Gas Town and beads have a
ready queue, an atomic claim and a liveness watcher) the pattern is the same: one ready queue, an
atomic claim, a liveness watcher, a serialised merge.

The repository is private on the Team plan: merge queue is not available. Merge commits only
(squash and rebase are off), auto-merge on, and a rule that no branch is ever deleted. The deletion
rule for `claim/*` only takes hold once the `claims` ruleset (decision 1) exists; until then a
claim ref could be deleted by hand.

## Decision

1. **A claim is a branch `claim/<issue>`, created through the GitHub API.** Ref creation is the
   compare-and-set. The ref points at a commit on the empty tree whose message records `holder`
   (`<github-login>/<host>/<session-id>`), `human` (login), `lease_until` (RFC 3339 UTC, computed
   from the server's `Date` header, never the local clock), `pr`, `note` and `state` (`held` or
   `released`). Heartbeat, release and takeover append a commit and fast-forward the ref
   (`PATCH`, `force=false`). **Claim refs are never deleted**: release writes `state: released`, and
   taking a released or expired claim is also a fast-forward, so exactly one taker wins.
2. **Leases.** An agent claim lasts 2 h with a heartbeat every 30 min; at lease + 15 min (grace)
   the sweeper warns, and at lease + 2 h the claim is freed. A human claim (`take <N> --human`)
   lasts 3 days with a daily heartbeat (`beat <N> --human`) and does not count against the session
   cap. Takeover needs `take --takeover` and is refused while the lease and the linked pull
   request's human activity (commits and comments, never bots) have both run out for the grace
   period, and always while the linked pull request is open and not a draft (it is waiting for a
   person: ask the holder's human). Pushes alone do not renew a lease: agents must `beat`. The
   previous holder's next `beat` fails with exit code 3 ("lost claim"). A different human taking
   over opens a new pull request, because the `claim` check requires the claim's human to be the PR
   author.
3. **Mirrors for humans, best effort; the ref is the truth.** Taking assigns the human, sets Project
   Status to In Progress and writes one issue comment carrying `<!-- koinon-claim -->`, edited (never
   appended) on every later change. Release unassigns and sets Status to Ready.
4. **Ordering.** Board Status: Backlog, Ready, In Progress, In Review, Done. A Priority field P0 to
   P3. Label `agent-ok`. `claim.sh next` lists only issues that are Ready, `agent-ok`, in the
   current milestone, have no open native "blocked by" (`issues/{n}/dependencies/blocked_by`) and
   no claim, sorted by Priority then age. Only humans move items to Ready.
5. **WIP caps.** One claim per session; eight per human (held, unexpired claim refs whose `human`
   matches; it was four until the 2026-11-02 amendment). Several issues can be taken as one unit (`take N M`) for one pull request that closes
   them all; it is all or nothing. There are **no exclusive lanes**: a Component is not a lock. procoduck puts the
   label `start-here` on Ready issues suited to the contributor (`next --label start-here`); everyone
   else sees the whole queue. `take` warns when another active claim
   shares the issue's `area:` label. The Phase field is retired: milestones are the release gate, and
   `next` uses the milestone only.
6. **Liveness and the sweeper** (`claims.yml`, every 30 minutes). Liveness is the later of the
   lease and the last push or comment on the linked pull request plus one lease. Past liveness +
   grace the sweeper comments, mentioning the human, and labels `claim:stale`. Two hours after the
   lease ran out it releases the claim, sets Status Ready and
   keeps the draft pull request. When a pull request closes its claims are released. A pull request
   that is ready for review (not draft) is never swept: it is waiting on a person, not abandoned.
7. **PR check `claim`** (required from day one). Every pull request references `Closes #N` and each
   linked issue has a held claim whose `human` is the PR author. Exemptions: Dependabot (by actor
   login, never by title); a title starting `chore`, `ci` or `build(deps)`, but only when the pull
   request changes no source files (Go, TypeScript, proto, SQL, scripts, YAML, Dockerfiles, charts,
   go.mod), because a title is spoofable; the label `unclaimed-ok`, accepted only when the label's
   latest `labeled` event was made by one of the humans (the owners on the `*` line of CODEOWNERS).
   An agent running with a human's token is indistinguishable from that human, so the label is
   reviewed like any other change. A Copilot coding agent's pull requests cannot pass (the author
   is the bot): a human opens or adopts them. The check runs from `pull_request_target`, using the
   base branch's workflow and script and never checking out the pull request. After `take`, the
   script re-runs the failed check.
8. **Merge ordering.** Strict required status checks plus a serialising updater (`updater.yml`): on
   a push to main, a finished `ci` or `e2e-kind` run of a pull request, auto-merge being enabled or the
   30-minute schedule (:07 and :37), the oldest auto-merge pull request that is behind main, not a
   draft, and green on its own head (every required check of `hack/ci-required.txt` reported, the
   newest run of each name succeeded; advisory and third-party checks and the updater's own run do
   not count) gets
   `PUT pulls/{n}/update-branch` (with `expected_head_sha`), unless another auto-merge pull request is already up to date and
   running checks. The updater runs as a GitHub App, because pushes by `GITHUB_TOKEN` do not start
   CI. A pull request that conflicts with main gets the label `needs-update` and is skipped; the bot never
   pushes code to a pull request branch. Merge-conflict reducers: ADR numbers are reserved with
   `hack/claim.sh adr` (a `claim/adr-NNNN` ref, created like a claim), numbers must be unique but may
   have gaps, and the README index is generated by `hack/adr-index.sh` and checked by `adr-lint`;
   `hack/migrate-renumber.sh` renumbers a branch's migrations after main moved; the chart assertions
   live in `hack/chart-check.d/NN-<feature>.sh`; `docs/plans/v0.1.md` no longer tracks status (the
   board does).
   Use the native merge queue once the repository is public.
9. **Review.** The agent's human is the PR author; GitHub forbids self-approval, so the other human
   reviews. Approval is required on **sensitive paths only**, enforced by the `main` ruleset's
   native `required_reviewers` path rule: one approval from either human other than the author. The
   sensitive paths are the owner-of-record areas and the guard rails: `.github/`, `AGENTS.md`,
   `docs/design.md`, `docs/dev/releasing.md`, `docs/adr/`, `charts/` (including the sandbox policy),
   `internal/db/migrations/`, `internal/gateway/`, `internal/agentd/`, `internal/reconciler/`,
   `internal/ledger/`, `internal/auth/`, `internal/secretstore/`, `internal/tracing/scrub/` and
   `hack/claim.sh`. **One source of truth:** the `# Sensitive paths` section of `.github/CODEOWNERS`;
   `hack/claim.sh sensitive-paths` prints the patterns to paste into the ruleset. The general
   required-approval count stays 0, and "Require review from Code Owners" stays off (the `*` line would
   make it apply everywhere).
10. **Project writes and the updater use a GitHub App** (`KOINON_BOT_APP_ID`,
    `KOINON_BOT_PRIVATE_KEY`, read through `actions/create-github-app-token`, pinned by SHA and
    older than the ADR 0079 cooldown), held as secrets of an Environment limited to main; each job
    asks for the narrowest token permissions. Jobs skip with a notice while the secrets are absent.
11. **People.** `@ovirandunu` stays admin with full trust in the repository. He gets no access to
    procoduck's secret store, model budget or private test infrastructure; he uses his own setup for those. Both humans'
    agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay procoduck's.

Exit codes of `hack/claim.sh`: 0 success; 1 error; 2 refused (held by someone else, not takeable yet,
a cap, lost the race): pick another issue; 3 lost claim: stop and tell your human.

## Amendment: session lanes (old issue 379)

Decision 5 caps a session at one claim. A session that runs several agents in parallel, one issue
each in its own worktree, could therefore work only one issue at a time. The cap is now counted per
**lane**: at most one claim per (session, lane).

- A lane is a short name chosen by the session's parent agent: `--lane L` on `take`, `beat`, `review`,
  `release`, `limits` and `mine`, or `KOINON_CLAIM_LANE` in the environment (the flag wins). `L`
  matches `^[a-z0-9][a-z0-9-]{0,15}$`.
- With a lane the holder is `<login>/<host>/<session>#<L>`; without one it stays
  `<login>/<host>/<session>`, which is its own lane, so behaviour without `--lane` is unchanged.
  Session ids never contain `#`, so the first `#` of the last holder segment starts the lane.
- A claim is yours only under the lane that took it: `beat`, `review` and `release` from another lane
  (or with no lane) exit 3, like any claim that is not yours. Takeover, leases, the sweeper and the PR
  `claim` check are unchanged; they read the login and the lease, and the lane is only part of the
  holder's identity. `--human` claims have no lane.
- The **human cap counts** every claim of the human, lanes or not (four when this amendment was
  written; eight since the 2026-11-02 amendment below). So one session holds up to that many claims,
  one per lane. When `take` is refused by the session cap the message lists the
  claims the session already holds in that lane (with their pull requests) and suggests `--lane` or
  `release`.
- **Subagents still never call `claim.sh`** and never set a session id or a lane. The parent takes
  one lane per subagent (`take <N> --lane <name>`), passes the issue and branch to the subagent, and
  does all the heartbeats, reviews and releases itself, with the same `--lane`.
- These lanes are about *who holds* a claim. They are not the exclusive component lanes rejected under
  Alternatives considered: a lane locks nothing but the session's own cap.

## Alternatives considered

- **Assignee or label as the lock.** Last-write-wins: two agents both succeed. _(reconstructed)_
- **A comment or a Project field as the lock.** Same problem; neither has compare-and-set.
- **A claim file committed to main.** Needs a push race on a protected branch, and pollutes history.
- **A lock service or a database row.** A second system to run and secure for a pre-alpha team.
  _(reconstructed)_
- **A tracker with atomic claims (beads).** A second tracker beside GitHub Issues.
- **Delete the ref on release.** Cleaner, but the repository forbids branch deletion; a
  fast-forward `released` commit also keeps the history of who held what.
- **Merge queue.** Unavailable for a private Team-plan repository. Revisit when public.
- **Review on every pull request.** Slows two trusted people; sensitive paths only keeps the
  risk-bearing changes (policy, migrations, auth, workflows) reviewed.
- **Exclusive lanes by Component.** A git-history study showed the areas overlap too much; lanes
  would block work without preventing collisions. Review ownership by path plus the `start-here`
  label gives the same guidance without a lock.
- **Milestones instead of a Ready status.** Milestones say "when", Ready says "a human has decided
  this is safe for an agent now".

## Consequences

- Positive: exactly one agent can hold an issue; abandoned work returns to the queue without losing
  its draft PR; humans see the claim in the places they already look.
- Negative: `refs/heads/claim/*` accumulate (one per issue ever claimed; list them with
  `git ls-remote origin 'refs/heads/claim/*'`). Mirrors can lag the ref; trust `claim.sh show`.
- Negative: every pull request must name an issue, and a stale claim blocks a pull request's
  `claim` check until released or taken over.
- Negative: the updater costs one CI run per merge for each queued pull request that falls behind.
  _(inferred)_
- Follow-ups: the App, the Environment and the settings in [`docs/dev/work-claims-setup.md`](../dev/work-claims-setup.md)
  are applied by hand; the sweeper's liveness uses the PR's last commit date, not push time.

## Open questions

- The sweeper's liveness uses the pull request's last human commit date, not push time.

## Upstream verification

2026-10-30 on `stateracloud/koinon`, refs `claim/spike-*` (left in place):

- `POST git/refs` for an existing ref returns 422 "Reference already exists".
- `PATCH git/refs/heads/<ref>` with `force=false` to a commit that is not a descendant returns 422
  "Update is not a fast forward"; a descendant is accepted.
- Three runs of two concurrent `PATCH` calls from the same parent: exactly one succeeded each time;
  the loser got 422 "Reference cannot be updated". Callers must treat any 422 as "lost".
- The live `main` ruleset's `pull_request` rule exposes `required_reviewers`, so the native path rule
  is available on this plan.
- A commit on the empty tree (`4b825dc...`) with no parent can be created through
  `POST git/commits`, and the server `Date` header is available on any response.

## Links

- [Issue dependencies (GA 2025-08-21)](https://github.blog/changelog/2025-08-21-dependencies-on-issues/)
- [Git refs REST API](https://docs.github.com/en/rest/git/refs)
- [Building a C compiler with parallel Claudes](https://www.anthropic.com/engineering/building-c-compiler)
- [ADR 0079](0079-dependency-cooldown.md), [setup checklist](../dev/work-claims-setup.md)

## Amendment: human cap raised to eight (2026-11-02, procoduck)

The human cap of decision 5 is now **8** (it was 4). One human runs several local lanes plus a cloud
lane at once, and four claims was the bottleneck. The default of `KOINON_CLAIM_HUMAN_CAP` in
`hack/claim.sh` changes from 4 to 8; the variable still overrides it. The session cap (one claim per
session and lane) is unchanged, as is everything else in decision 5 and the lanes amendment.

## Amendment: amended by ADR 0094 (2026-11-05, procoduck)

The repository is public since 2026-11-05. [ADR 0094](0094-work-claims-on-the-public-repository.md)
keeps decision 1 (the claim ref is the lock), the agent lease, takeover, lanes and the caps, and
changes the rest as follows. Where the two differ, 0094 wins.

- Decisions 2 and 5: comment claims (`/claim`, `/release`) for people (3 days) and outsiders
  (14 days, at most 2, only on `good first issue` or `help wanted` issues).
- Decision 4: `next` and `take` also skip issues that an open pull request closes or that someone
  else is assigned to.
- Decisions 3 and 10: the App and the `claims-bot` Environment are retired, and the bots make no
  board writes.
- Decision 6: the sweeper runs on `GITHUB_TOKEN`.
- Decision 7: the `claim` check fails only on a live claim held by someone else; its exemptions and
  `unclaimed-ok` are retired.
- Decision 8: the native merge queue replaces `updater.yml` and the strict rule, and `adr` also
  counts the ADR files that open pull requests add.
- Lanes amendment and `beat --mine`: a release records `released_by`, and a claim its own session
  released is dropped silently instead of being reported as lost.
- The Context line "merge queue is not available" and the Alternatives entry "Merge queue" no longer
  hold.

## Amendment: no pull_request_target (2026-11-09, procoduck, #463)

GitHub blocks `pull_request_target` by default for public repositories: a scheduled brownout began
2026-11-09 (further windows on 2026-11-16), and the block is permanent from 2026-12-02. During a
window every `claims` run ended in `startup_failure` with no jobs, and `claim` is a required check, so
nothing could merge. Sources: the
[GitHub changelog, 2026-11-02](https://github.blog/changelog/2026-11-02-pull_request_target-blocked-by-default-december-2)
and [Securely using pull_request_target](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target).
The maintainer decision is to follow GitHub's recommendation and keep the default block: no admin
Actions event policy exception, and no `pull_request_target` workflow in the repository. This replaces
the trigger sentence of decision 7 ("The check runs from `pull_request_target`") and the matching
sentence of ADR 0094.

- **`claim` check** (`claims.yml`): runs on `pull_request` (same activity types as before, `closed`
  dropped) and `merge_group`. The token is read-only (`contents`, `issues`, `pull-requests`: read),
  there is no secret and no Environment. The job still checks out the base commit
  and runs that commit's `hack/claim.sh check-pr`; it never runs the pull request's code. Since ADR
  0104 that commit is the base branch's tip at run time (`pull_request.base.ref`), not
  `pull_request.base.sha`, which is the base of the PR's last push and can be far behind.
- **Release on merge** (`release` job): runs on `push` to `main` from main's own workflow file. It runs
  `hack/claim.sh release-merged <before> <after>`: GitHub links every pushed commit to its pull
  requests, and the claims of each merged one are released as `release-pr` did. Pull requests closed
  without a merge are freed by the sweeper (30 minutes): `claim_liveness` finds the claim's open or
  linked pull request, or else a pull request of the claim's human that closes the issue and closed
  since the claim was last renewed, and `sweep_one` releases it. A merged pull request frees the
  claim at once. A pull request closed without a merge frees an agent claim only when it closed at or
  after the lease start (a beat moves the lease start) and the server time is at least
  `CLOSED_GRACE` (45 minutes: the 30-minute agent beat period plus 15 minutes of slack) after the
  close, so a holder that closes PR X and opens PR Y keeps the claim; the `claim` check and the
  merge queue use the same rule. A person claim (`--human`) ignores a closed-unmerged pull request and
  waits for its lease. The closed-pull-request search reads the newest 50; a miss only delays the
  release. A released closed-unmerged pull request
  unassigns the human and returns the issue to Ready, as `release-pr` did. If `release-merged` finds
  no merged pull request (the API lags behind the push), it prints one line and the sweeper or the
  lease frees the claim later. ADR 0104 (the same day) removes the sweeper and the lease, and with
  them the grace rule above: a merged pull request still frees its claims through `release-merged`,
  and a pull request closed without a merge, or a release that `release-merged` missed, is released
  by its human with `hack/claim.sh release-pr <PR>` (which unassigns on a close without a merge) or
  `hack/claim.sh release <N>`.
- **Fork pull requests:** on `pull_request` a first-time contributor's fork pull request gets no
  `claim` run until a maintainer approves its workflows (`pull_request_target` ran without approval).
- **Updater** (`updater.yml`): the `auto_merge_enabled` trigger is dropped, because it needs
  `pull_request_target` and an Environment secret. The `push`, `workflow_run` and `schedule` triggers
  remain, so a new auto-merge pull request waits up to one schedule period (30 minutes) at most. The
  merge queue (ADR 0094 decision 8) replaces the updater anyway.
- **Lint**: `hack/runner-lint.sh` (CI `scripts` job and preflight's `runnerlint` gate) fails
  on `pull_request_target` in any workflow. Bringing it back needs an ADR.

Trade-off, stated: with `pull_request` the workflow file comes from the pull request's head, so a pull
request can edit `claims.yml` and make its own `claim` check pass (before, the base's file ran). The
exposure is small: the check is a coordination check (two agents on one issue), not a security
boundary; `.github/**` is a CODEOWNERS path and needs a review; the token is read-only and holds no
secret, so a changed workflow gains nothing beyond its own status. A ruleset "required workflow"
(Actions > "Require workflows to pass before merging", which runs a workflow from a fixed ref of a
chosen repository, not from the pull request) could close the gap: the base's `claims.yml` would run
whatever the head contains. It is a repository or organization setting, not configured here; revisit it
if the check ever guards something that matters.
