# 0082. Work claims and the ready queue: a claim branch is the lock, the board is the queue

- Status: Accepted
- Date: 2026-10-30
- Relates to: [0038](0038-governance.md), [0046](0046-adr-conventions.md), [0079](0079-dependency-cooldown.md)
- Verification: ref compare-and-set semantics spiked on 2026-10-30 (see Upstream verification)

## Context

Two humans (`@procoduck`, Andre; `@ovirandunu`, a contributor with admin and full trust) each run
several coding agents against the same backlog. Nothing stops two agents picking up one issue, two
pull requests colliding on a migration or ADR number, or an agent holding an issue it has abandoned.

What GitHub gives us: assignees, labels, Project Status and comments are all last-write-wins, so
none can be a lock. The one transactional primitive is the git ref: creating a ref fails if it
exists, and a non-forced update must fast-forward. In every system that coordinates agents well
(Anthropic's parallel-Claudes compiler used a push rejection as the lock; Gas Town and beads have a
ready queue, an atomic claim and a liveness watcher) the pattern is the same: one ready queue, an
atomic claim, a liveness watcher, a serialised merge.

The repository is private on the Team plan: merge queue is not available. Merge commits only,
auto-merge on, and a repository rule that no branch is ever deleted.

## Decision

1. **A claim is a branch `claim/<issue>`, created through the GitHub API.** Ref creation is the
   compare-and-set. The ref points at a commit on the empty tree whose message records `holder`
   (`<github-login>/<host>/<session-id>`), `human` (login), `lease_until` (RFC 3339 UTC, computed
   from the server's `Date` header, never the local clock), `pr`, `note` and `state` (`held` or
   `released`). Heartbeat, release and takeover append a commit and fast-forward the ref
   (`PATCH`, `force=false`). **Claim refs are never deleted**: release writes `state: released`, and
   taking a released or expired claim is also a fast-forward, so exactly one taker wins.
2. **Lease 2 h, heartbeat every 30 min, takeover only after lease + 15 min grace.** A takeover needs
   `take --takeover`; the previous holder's next `beat` fails with exit code 3 ("lost claim").
3. **Mirrors for humans, best effort; the ref is the truth.** Taking assigns the human, sets Project
   Status to In Progress and writes one issue comment carrying `<!-- koinon-claim -->`, edited (never
   appended) on every later change. Release unassigns and sets Status to Ready.
4. **Ordering.** Board Status: Backlog, Ready, In Progress, In Review, Done. A Priority field P0 to
   P3. Label `agent-ok`. `claim.sh next` lists only issues that are Ready, `agent-ok`, in the
   current milestone, have no open native "blocked by" (`issues/{n}/dependencies/blocked_by`) and
   no claim, sorted by Priority then age. Only humans move items to Ready.
5. **WIP caps.** One claim per session; four per human (held, unexpired claim refs whose `human`
   matches). **Lanes:** `.github/lanes.yaml` lists the Components each human's agents prefer;
   `next --lane` filters on the board's Component field. A lane is a preference, not a lock: `take`
   works on any issue and warns when another active claim shares the issue's `area:` label.
6. **Liveness and the sweeper** (`claims.yml`, every 15 minutes). Liveness is the later of the
   lease and the last push or comment on the linked pull request plus one lease. Past liveness +
   grace the sweeper comments, mentioning the human, and labels `claim:stale`. Two hours after the
   lease ran out (about 2 x TTL since the last renewal) it releases the claim, sets Status Ready and
   keeps the draft pull request. When a pull request closes its claims are released. A pull request
   that is ready for review (not draft) is never swept: it is waiting on a person, not abandoned.
7. **PR check `claim`** (required). Every pull request references `Closes #N` unless its title is
   `chore`, `ci` or `deps`, it is from Dependabot, or it has the label `unclaimed-ok`. Each linked
   issue must have a held claim whose `human` equals the PR author.
8. **Merge ordering.** Strict required status checks plus a serialising updater (`claims.yml`): on
   every push to main, the oldest auto-merge pull request that is behind main gets
   `PUT pulls/{n}/update-branch`, unless another auto-merge pull request is already up to date and
   running checks. The updater runs as a GitHub App, because pushes by `GITHUB_TOKEN` do not start
   CI. `hack/adr-lint.sh` (check `adr-lint`) fails when two pull requests claim the same ADR number.
   Use the native merge queue once the repository is public.
9. **Review.** The agent's human is the PR author; GitHub forbids self-approval, so the other human
   reviews. Approval is required on **sensitive paths only**: `.github/`, `AGENTS.md`, `docs/adr/`,
   the sandbox admission policy, migrations, `internal/gateway/`, `internal/auth/`,
   `internal/secretstore/`, `internal/tracing/scrub/` and `hack/claim.sh`. `CODEOWNERS` lists both
   humans on `*` and on these paths. See Open questions for how this is enforced.
10. **Project writes and the updater use a GitHub App** (`KOINON_BOT_APP_ID`,
    `KOINON_BOT_PRIVATE_KEY`, read through `actions/create-github-app-token`, pinned by SHA and
    older than the ADR 0079 cooldown). Jobs skip with a notice while the secrets are absent.

Exit codes of `hack/claim.sh`: 0 success; 1 error; 2 refused (held by someone else, not takeable yet,
a cap, lost the race): pick another issue; 3 lost claim: stop and tell your human.

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
- Follow-ups: the App and the settings in [`docs/dev/work-claims-setup.md`](../dev/work-claims-setup.md)
  are applied by hand; the sweeper's liveness uses the PR's last commit date, not push time.

## Open questions

- Enforcing review on sensitive paths only. "Require review from Code Owners" would not do it:
  `CODEOWNERS` lists both humans on `*` (so GitHub requests the other human's review on every pull
  request), and the rule would then require an approval everywhere. Two ways, Andre picks after
  looking at the ruleset UI: (a) if the `main` ruleset offers **Require reviewers** with file
  patterns (the `required_reviewers` rule), add one rule for the sensitive paths above, one approval
  from a team of the two humans, and leave the general approval count at 0; (b) if the plan does not
  offer it, leave approvals at 0 and treat sensitive-path review as a convention: `CODEOWNERS`
  requests the other human's review and the author waits for it. Nothing in the repository changes
  between (a) and (b). The checklist in `docs/dev/work-claims-setup.md` has both.
- Whether `ovirandunu` should drop from admin to Write (the research recommends it; Andre
  keeps full trust for now).

## Upstream verification

2026-10-30 on `stateracloud/koinon`, refs `claim/spike-*` (left in place):

- `POST git/refs` for an existing ref returns 422 "Reference already exists".
- `PATCH git/refs/heads/<ref>` with `force=false` to a commit that is not a descendant returns 422
  "Update is not a fast forward"; a descendant is accepted.
- Three runs of two concurrent `PATCH` calls from the same parent: exactly one succeeded each time;
  the loser got 422 "Reference cannot be updated". Callers must treat any 422 as "lost".
- A commit on the empty tree (`4b825dc...`) with no parent can be created through
  `POST git/commits`, and the server `Date` header is available on any response.

## Links

- [Issue dependencies (GA 2025-08-21)](https://github.blog/changelog/2025-08-21-dependencies-on-issues/)
- [Git refs REST API](https://docs.github.com/en/rest/git/refs)
- [Building a C compiler with parallel Claudes](https://www.anthropic.com/engineering/building-c-compiler)
- [ADR 0079](0079-dependency-cooldown.md), [setup checklist](../dev/work-claims-setup.md)
