# 0094. Work claims on the public repository: one lock, claims by comment, a conflict check, the native merge queue

- Status: Accepted (procoduck, 2026-11-05, after the grilling session and the probes, which changed no decision)
- Reviewed with procoduck 2026-11-05 (grilling). Confirmed as drafted:
  - outsiders claim only on `good first issue` or `help wanted` issues;
  - leases of 3 days for people and 14 days for outsiders, with a day-10 reminder and a cap of 2;
  - the conflict-only check;
  - no kind e2e in the queue (PR-head evidence plus the main backstop); amended by [0102](0102-merge-queue-runs-the-kind-e2e.md);
  - `/release` by the holder or any person;
  - `next` and `take` skip issues assigned to another person;
  - the App retired, with no fallback unless procoduck asks later;
  - decisions 6 and 7 ship first, as one PR.

  Probe 3 is skipped; decision 1 has a built-in fallback instead.
- Date: 2026-11-05
- Area: engineering-process
- Tags: claims, github, ci
- Amended by: [0102](0102-merge-queue-runs-the-kind-e2e.md) (decision 4: the queue runs the kind e2e);
  [0104](0104-work-claims-reduced-to-the-lock.md) (2026-11-09: decisions 1, 2, 3, 5, 6 and 7; no comment
  claims, outsider classes, leases or sweeper);
  amendment of 2026-11-10 below (the updater is retired; decision 4's exit condition is replaced);
  [0105](0105-merge-queue-route.md) (decision 4: the way in, admin merges and the e2e evidence; the queue is the only route)
- Amends: ADR 0082 (work claims and the ready queue): decisions 2, 3, 4, 6, 7, 8 and 10, and its Context
- Relates to: [0082](0082-work-claims-and-ready-queue.md), [0046](0046-adr-conventions.md), [0079](0079-dependency-cooldown.md)
- Issue: #159 (draft PR #160)
- Verification: read-only API reads and write probes on a throwaway repository, 2026-11-05 (below)

## Context

ADR 0082 was written for a private repository on the Team plan with two trusted humans. Since the
move (2026-11-05) `stateracloud/koinon` is public and outside contributors can open pull requests.
Three parts of ADR 0082 no longer fit:

- **The `claim` check is a permission gate.** It passes only when the pull request's author holds
  the claim. A contributor without write access cannot run `hack/claim.sh take`, so no outside pull
  request can ever pass (public-CI review P0-1).
- **The GitHub App is a second system to run.** The sweeper, release-on-close, the updater and the
  board writes all need `KOINON_BOT_APP_ID` and `KOINON_BOT_PRIVATE_KEY`. Those secrets were not set
  on the new repository, so all four jobs have been no-ops since the move and ghost claims fill the
  human cap of eight (P1-1).
- **`updater.yml` copies GitHub's merge queue.** ADR 0082 decision 8 and its Alternatives said to use
  the native queue once the repository is public. The 2026-11-02 refusal (HTTP 422 on a
  `merge_queue` rule, `docs/research/2026-11-merge-queue.md`) was made on the private repository.

ADR number reservations broke at the same move: `hack/claim.sh adr` handed out 0093 although the
open PR #136 already adds `docs/adr/0093-koinon-core-opencode-plugin.md`. The `claim/adr-*` refs
did not move with the repository, and `adr` reads only the default branch's files and those refs.

Two research passes informed this ADR (the maintainer's research store,
`2026-11-05-work-coordination.md` from docs and `2026-11-05-work-coordination-code.md` from source at
pinned SHAs):

- **No project with outside contributors has a hard claim, and none ties merging to one.**
  - Kubernetes Prow `/assign` (`pkg/plugins/assign/assign.go`) adds the commenter to GitHub's
    assignees. It checks nothing: not who comments, not whether someone already holds the issue, not
    who may unassign whom.
  - Prow lets GitHub decide who is assignable. It diffs the assignees the API returns and comments
    "GitHub didn't allow me to assign ..." for anyone missing.
  - Kubernetes ages issues out with a label ladder (`lifecycle/stale` at 90 days, `rotten` 30 days
    later, closed 30 days after that). The ladder ignores assignees.
- **Rust triagebot `@rustbot claim` replaces the current assignee silently** (`set_assignee` adds the
  claimer, then removes everyone else).
  - When GitHub refuses the assignment, triagebot assigns the bot and records the claimer in a hidden
    marker in the issue body.
  - Only team members may release someone else's claim.
  - There is no expiry. PR #2532, which would have added one, was closed unmerged; its reviewer asked
    for automatic stale release and a per-user cap rather than warnings.
- **Merging is where both projects invest.** Tide tests and merges batches selected by a label
  query. bors builds base plus PR on a bot branch and fast-forwards `main` only when that commit is
  green (`merge_queue.rs`). Neither uses GitHub's native queue. Both run long-lived services under a
  GitHub App or a PAT, with Postgres for triagebot and bors.
- **Agent systems** (Anthropic's parallel-Claudes compiler, Claude Code agent teams, beads) use one
  atomic claim inside one trust domain. None supports outsiders, and none documents lease-based
  recovery. In Kleppmann's terms ours is an efficiency lock: a duplicate costs model budget, not
  correctness.

The conclusion of both passes, which procoduck took on 2026-11-05: the claim ref is stronger than
anything these projects have, and agents need it (`claim.sh next` hands every lane the same top
issue, so without a lock duplicates are certain). Keep it. Borrow the surfaces: comment to claim,
let GitHub refuse an assignment and explain, and a check message that says exactly what is wrong.
Do not copy the machinery, and stop building our own copy of the merge queue.

## Decision

The claim ref of ADR 0082 decision 1 stays the one lock: `claim/<issue>`, created by compare-and-set,
fast-forwarded, never deleted. The agent lease, the heartbeat, `beat --mine`, takeover, lanes and the
session cap are unchanged. What changes is who can create a claim, what the check enforces, which
token the bots use, and how pull requests merge.

### 1. Claims by comment: `/claim` and `/release`

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): no claim by comment; `claim-comment.yml` and the `comment` command are removed. A claim is taken with `hack/claim.sh take`, and the holder is `<login>#<lane>`.)_

A new workflow `.github/workflows/claim-comment.yml` runs on `issue_comment` (`created`) for issues,
not pull requests (`github.event.issue.pull_request` is empty). It ignores comments by bots. It runs
the default branch's `hack/claim.sh` with the built-in `GITHUB_TOKEN` (job permissions
`contents: write`, `issues: write`, `pull-requests: read`, `actions: write` for the check re-run). It
never checks out or runs anything from the commenter. A comment is a command only when its first line
is exactly `/claim` or `/release` (regex `^/(claim|release)[[:space:]]*$`). The body and the login
reach the script through environment variables, never through `${{ }}` in a `run:` line.
Concurrency is one group per issue.

**Who is who.** The script reads `GET repos/{o}/{r}/collaborators/{login}/permission`.

- A **person** has `write`, `maintain` or `admin`: the maintainers.
- An **outsider** is anyone else.

Agents never claim by comment; they keep `hack/claim.sh take`.

**Holder and lease.** A comment claim writes the same claim commit as `take`, with:

| Class | `holder` | Lease | Cap |
|---|---|---|---|
| Agent (`take`, unchanged) | `<login>/<host>/<session>[#lane]` | 2 h, beat every 30 min | 1 per session and lane, 8 per human |
| Person (`/claim`, or `take --human`) | `<login>/github/human` (`take --human` keeps `<login>/<host>/human`) | 3 days | 8 per human, shared with that human's agents |
| Outsider (`/claim`) | `<login>/github/outsider` | 14 days | 2 live claims per login |

Liveness stays ADR 0082's rule: the later of the lease and the last non-bot push or comment on the
linked pull request plus one lease. The holder renews a comment claim by commenting `/claim` again;
`take --human` claims keep `beat --human`.

**Where outsiders may claim.** Only on open issues labelled `good first issue` or `help wanted`
(both labels exist). Elsewhere the bot answers that maintainers have not opened the issue to
outside claims and that a pull request is welcome anyway (decision 2 does not block it). People may
`/claim` any open issue.

**Refusals are answered, never silent.** In every case the bot replies in one comment and the ref is
unchanged:

- the issue is held by someone else (it names the holder's human and the lease, never the session);
- the issue is closed;
- the outsider label is missing;
- a cap is reached (it lists the commenter's live claims);
- the ref race was lost (any 422; the comment says to retry).

`/release` works for the claim's human and for any person (triagebot's rule; Prow's "anyone may
unassign anyone" is the bug to avoid). It writes `state: released`, as `claim.sh release` does.

**Mirrors.** The bot edits the existing `<!-- koinon-claim -->` comment, the same comment `take`
writes (a comment, not the issue body, so it never fights human edits). Then it tries to assign the
commenter through `POST issues/{n}/assignees`, compares the returned assignees with the request, and
does not depend on the outcome. When GitHub left the login out, the marker comment says: "GitHub did
not allow assigning @login (only collaborators, org members with read access and people who
commented here can be assigned); the claim is recorded in the ref `claim/N`." After a claim or a
release it re-runs the failed `claim` check of any open pull request that closes the issue, as `take`
does today.

**Fallback when GitHub refuses the write.** Nobody has proven that a non-collaborator's
`issue_comment` run gets a write token (probe 3 is skipped). So the workflow handles a refusal
itself. When the claim ref write, or any other write, answers 401, 403 or a permission error, the
bot does three things:

1. It comments "comment claims are not available for outside contributors yet; open a pull request
   that says Closes #N instead". An open pull request already makes `next` and `take` skip the issue
   (decision 5).
2. It opens one Backlog issue for the maintainers, deduplicated by its fixed title: it looks for an
   open issue with that title first.
3. It exits 0, so the run is not red.

### 2. The `claim` check is a conflict check

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): the check fails only when another human holds a claim on a closed issue or an ADR number; a live-claim or lease test does not exist, and the outsider classes are removed.)_

The required check `claim` (`claims.yml`, `pull_request_target`, read-only token, the base branch's
script) **fails only when someone else holds a live claim** on what the pull request takes:

- a claim on an issue the pull request closes (`Closes #N` and the other closing keywords), or
- an ADR number reservation `claim/adr-NNNN` for a `docs/adr/NNNN-*.md` file the pull request adds.

"Someone else" means the claim's `human` is not the pull request's author (case-insensitive).
"Live" means `state: held` and the sweeper would not release it now (liveness plus the release
delay of the holder's class).

Everything else passes:

- no linked issue (a `::notice::` asks for `Closes #N`);
- a free, released or dead claim;
- a claim of the author's own human.

So these ADR 0082 decision 7 rules are retired: the "no claim" and "not your claim" failures, the
`chore`/`ci`/`build(deps)` title exemption with its source-file test, the `unclaimed-ok` label with
its labeled-by-a-maintainer test, and the Dependabot exemption (Dependabot never collides with a
claim). Opening a pull request creates no claim: the open pull request is itself what `next` and
`take` respect (decision 5), so the check stays read-only.

The failure text is fixed. `hack/claim_test.sh` pins it, so outsiders and agents can match on it:

```text
::error::#<N> is claimed by @<human> until <lease-or-liveness RFC 3339> (<pull request #P | no pull request yet>); this pull request is by @<author>. Ask @<human> on #<N> to comment /release, or ask a maintainer. If the claim goes stale the sweeper frees it at <release time>, and this check runs again when it does.
::error::<path> uses ADR number <NNNN>, which @<human> reserved (claim/adr-<NNNN>) until <lease RFC 3339>. Take a free number with hack/claim.sh adr (a maintainer can do it for you) and rename the file.
```

The success lines name what was checked, for example `claim check: #159 is free`, `claim check: #159
is claimed by @procoduck (the author)`, `claim check: no Closes #N; nothing to check`.

### 3. The sweeper and release-on-close use `GITHUB_TOKEN`; the App is retired

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): there is no sweeper; only the `release` job (`release-merged`) remains, and a close without a merge is released by its human with `release-pr`.)_

The `sweeper` and `release` jobs of `claims.yml` drop `actions/create-github-app-token` and the
`claims-bot` Environment. They run with `GITHUB_TOKEN`, job permissions `contents: write` (fast-forward
the claim refs), `issues: write` (labels, the marker comment, unassign) and `pull-requests: read`. The
repository default stays `read` (verified below); each job asks for exactly this much.

The sweeper adds the day-scale classes:

- **Person claims** follow `take --human` today: stale at liveness plus 15 minutes, released 2 h later.
- **Outsider claims** get a warning comment that mentions the holder 4 days before liveness runs out
  (day 10 of 14 without activity), get `claim:stale` when it runs out, and are released 2 h after.
  This follows Kubeflow's 2-3 weeks and the stale release that triagebot #2532 asked for; Prow's
  90 days is too slow even for people.
- A pull request that is ready for review is still never swept.

**Board writes and the App.** The App was needed for two things: pushes that start CI (the updater,
gone under decision 4) and writes to the organization Project (`GITHUB_TOKEN` is scoped to the
repository and cannot write an organization Project v2). Without the App, the bots make no board
writes at all:

- **Done** comes from the board's built-in workflows (item closed, pull request merged), which
  `docs/dev/work-claims-setup.md` step 1 already sets.
- **In Progress** is still written by `take`, and **Ready** on `release` and `block`, with the
  human's own token, as today.
- **The two writes the bots used to make are dropped:** Ready after the sweeper releases a claim, and
  Ready after a pull request closed without merging. Claims by comment never write Status either. The
  board is best effort by ADR 0082 decision 3, and the ref is the truth.
- **Drift is repaired by the next `claim.sh` call that sees it.** `next` counts an item whose Status
  is In Progress or In Review, with no live claim and no open pull request that closes it, as Ready
  (it was Ready before a `take` moved it). `take` writes In Progress again.

So the App, the `claims-bot` Environment and their two secrets are retired, with no fallback (procoduck,
2026-11-05). Only if procoduck asks later would the App come back, and then for exactly those two writes
(sweeper Ready, close-without-merge Ready).

### 4. GitHub's native merge queue replaces `updater.yml`

- **`main` ruleset.** Add a `merge_queue` rule: merge method `MERGE`, grouping `ALLGREEN`, at most
  1 entry merged per group and up to 5 built concurrently (each entry is tested on main plus the
  entries ahead of it), status-check timeout 60 minutes. Start at group size 1 and raise it only
  with measurements (`docs/research/2026-11-merge-queue.md` section 4 has the cost arithmetic).
  `strict_required_status_checks_policy` goes to `false`: the queue tests the merged result, which
  is what the strict rule approximated.
- **Retired:** `.github/workflows/updater.yml`, `claim.sh update-oldest` with `update_target` and
  `open_prs`, the `needs-update` label (the queue drops a conflicting pull request and says so), and
  the merge-commit-attribution caveat of `work-claims-setup.md` step 5.
- **The way in is unchanged.** `gh pr merge --auto --merge` and `hack/pr-merge.sh` put the pull
  request in the queue once its own checks pass. Admin merges (`--admin`, the org-admin bypass of the
  `main` ruleset) still merge directly.
  _(Changed by [0105](0105-merge-queue-route.md): the queue is the only route; `--admin` is only for a pull request that changes a `.github/**` path.)_
- **`merge_group` on every required workflow.**

  | Workflow | Check | On `merge_group` |
  |---|---|---|
  | `ci.yml` | `go`, `web`, `quick checks`, `generated code`, `chart`, `chart smoke`, `docs`, `escape (admission)`, `lint-shell`, `scripts` | Runs as on a ready pull request. `changes`, the proto breaking check and `migrate-lint` take their base from `github.event.merge_group.base_sha` |
  | `dco.yml` | `dco` | Checks `base_sha..head_sha` without the queue's merge commits |
  | `secrets.yml` | `gitleaks` | Scans `base_sha..head_sha` |
  | `claims.yml` | `claim` | A `merge_group` job reads `<N>` from the queue ref (`gh-readonly-queue/main/pr-<N>-<base sha>`, verified by probe 1) and runs the base's `check-pr <N>`. This is the fencing point: a claim someone else took while the PR waited fails here |
  | `e2e-kind.yml` | `e2e result` | See below |

  `hack/ci-required-test.sh` learns to fail a required check whose workflow has no `merge_group`
  trigger, the same way it fails an unlisted job. This keeps the AGENTS.md rule that a required job
  must report everywhere.
- **`e2e result` on the queue commit.** (Amended by [0102](0102-merge-queue-runs-the-kind-e2e.md): the queue
  now runs the kind e2e. The text below is the original decision.) On `merge_group` the `decide` job never starts the kind e2e,
  and `e2e result` runs the base's `hack/e2e-gate.sh` against **the head SHA of pull request `<N>`**,
  not the queue commit.
  - A needs-e2e pull request still needs a green `quickstart` or `e2e-lab` on its own head before it
    can be queued, and the queue re-checks that this evidence is there.
    _(Changed by [0102](0102-merge-queue-runs-the-kind-e2e.md) and [0105](0105-merge-queue-route.md): the `e2e-lab` status no longer counts toward a merge, and the queue's own `quickstart` is the only e2e proof.)_
  - The queue commit itself gets no kind e2e. Fifteen to 25 minutes per entry would hold the queue,
    and the lab cannot test the read-only, short-lived queue ref.
  - The backstop stays as it is: the batched e2e-kind on pushes to main, the nightly run, and
    `hack/e2e-bisect.sh` with the 2-hour fix-forward-or-revert rule.
  - What this gives up: the strict rule made the PR head's tree the merged tree, so an e2e on the head
    tested exactly what landed. The queue does not. Revisit (run the kind e2e in the queue for
    needs-e2e entries) once main's e2e goes red on a merge whose PR-head e2e was green.

### 5. `next` and `take` respect people, outsiders and open pull requests

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): outsider and person claim classes are removed; `next` skips held claims and issues an open pull request closes, and `take` assigns only the human.)_

- **`next` skips** issues with any held claim (as today, which now includes person and outsider
  claims), issues that an open pull request closes (`closingIssuesReferences`, any author), and issues
  assigned to someone other than the caller's human (an assignee is a person's soft claim, Prow
  style).
- **`take` refuses with exit 2** in the same three cases. For a claim of another class or human it
  names the holder's human and the lease; for an open pull request it names the pull request and its
  author.
- **`take --takeover`** of a person or outsider claim follows the dead rule of ADR 0082 decision 2
  with that class's lease. It is still refused while the linked pull request is ready for review.
- **An open pull request by the caller's own human** (a draft left by an earlier session) is not a
  refusal; `take` links it as `pr`.

### 6. ADR numbers: the open pull requests count

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): an ADR reservation is permanent (`state: reserved`, no lease); the highest-of-three-sets rule stays.)_

`hack/claim.sh adr` takes the highest of three sets, plus one:

- the `docs/adr/NNNN-*.md` files on the default branch;
- the `claim/adr-NNNN` refs;
- the `docs/adr/NNNN-*.md` files that open pull requests add (GraphQL
  `pullRequests(states: OPEN) { files { path changeType } }`, `ADDED` only, paginated).

The open pull requests are the in-flight truth, and they move with the repository; the reservation
refs did not. The conflict half is decision 2: a pull request that adds an ADR under a number someone
else reserved fails `claim` with the fixed message. Duplicates between two open pull requests that
both skipped `adr` remain `adr-lint`'s mechanical conflict after the first merges (rename the second),
as in ADR 0082 decision 8.

### 7. `beat --mine` drops a claim its own session released

_(Changed by [0104](0104-work-claims-reduced-to-the-lock.md): `beat` is removed with the lease, so this rule no longer applies.)_

A claim that its own holder released is not lost. Today `beat --mine` stops with exit 3 ("lost #140
#143 #145: stop work") when the same session released those claims itself with `claim.sh release`
between two rounds. The loop only knows that a claim it tracked is no longer held, and it excuses
only a release on pull request close.

- **Every release commit records who released it**, in a new field `released_by`:
  - the releasing holder (`<login>/<host>/<session>[#lane]`) for `release`, `block` and `/release`
    by the holder;
  - `sweeper` for the sweeper;
  - `pr-close` for release-on-close;
  - `comment:<login>` for a `/release` by someone else.

  The release commit keeps `holder` as it is today.
- **`beat --mine` drops a tracked claim silently** when it is released and `released_by` names this
  session (any of its lanes, since the parent releases on behalf of every lane), or `pr-close`.
- **Exit 3 is for claims someone else ended:** a takeover (held under another holder), or a release
  by the sweeper, another session or another person. The message names who ended each claim.
- A release commit without `released_by` (written before this change) keeps today's rule.

## Verified on 2026-11-05 (read-only)

`gh api` against `api.github.com`; nothing was changed.

- `GET repos/stateracloud/koinon/actions/permissions/workflow`:
  `default_workflow_permissions: read`, `can_approve_pull_request_reviews: false`. Jobs that need
  write must ask for it in `permissions:` (decisions 1 and 3 do).
  `GET repos/.../actions/permissions`: `allowed_actions: all`, `sha_pinning_required: true`. The
  organization-level setting answered 403 (it needs `admin:org`), so whether the org caps what a job
  may ask for is not known.
- Repository: `visibility: public`; merge commit only (squash and rebase off); auto-merge on;
  `delete_branch_on_merge: false`. Organization plan `team`.
- Ruleset `main` (id 26610739, active, `~DEFAULT_BRANCH`):
  - rules: `deletion`, `non_fast_forward`, `pull_request` (merge method `merge`, approvals 0, one
    `required_reviewers` path rule, `require_extra_approval_for_unattributed_changes: true`) and
    `required_status_checks` (`strict: true`, 14 contexts: `dco`, `go`, `web`, `generated code`,
    `gitleaks`, `chart`, `escape (admission)`, `claim`, `e2e result`, `scripts`, `lint-shell`,
    `chart smoke`, `docs`, `quick checks`, none pinned to an integration);
  - bypass: OrganizationAdmin in `pull_request` mode.
  - Ruleset `claims` (id 26610736, active, `refs/heads/claim/**`): `non_fast_forward`, `deletion`, no
    bypass actors.
- Merge queue: GraphQL `repository.mergeQueue(branch: "main")` is `null` (none configured). The
  schema's `RepositoryRuleType` offers `MERGE_QUEUE`. The effective rules on `main` contain no
  `merge_queue`. GitHub's 2023 GA note says public organization repositories qualify; probe 1
  (below) confirmed that the API accepts the rule.
- ADR reservations: `claim/adr-0093` and `claim/adr-0094` exist, both created on 2026-11-05 by the
  same session. Open PR #136 adds `docs/adr/0093-koinon-core-opencode-plugin.md`, so 0093 was
  handed out twice; decision 6's scan would have skipped it.
- Labels `good first issue`, `help wanted`, `claim:stale`, `needs-update`, `unclaimed-ok`,
  `start-here` and `agent-ok` exist.

## Verification by probe

The main session ran the probes on 2026-11-05.

- Probes 1, 2, 4 and 5 ran on a throwaway public repository in the org,
  `stateracloud/koinon-queue-probe`, which got a copy of koinon's `claims` ruleset (on
  `refs/heads/claim/**`: block non-fast-forward updates and deletion, no bypass actors). The
  repository is archived now, not deleted.
- Only the disabled merge-queue ruleset check of probe 1 ran on koinon itself.

No result changes a decision.

1. **Merge queue rule: decision 4 stands.**
   - On `stateracloud/koinon`: `POST rulesets` with `enforcement: "disabled"` and one `merge_queue`
     rule answered 201 (ruleset 26632458). `DELETE` answered 204, and koinon's rulesets are unchanged.
     The 2026-11-02 refusal was the private repository.
   - On the probe repository, with an active queue (`MERGE`, 5 built, 1 merged, 60 minutes):
     - `gh pr merge --auto --merge` enqueues the pull request.
     - A `merge_group` run of the required check started and passed.
     - The queue ref is `gh-readonly-queue/main/pr-<N>-<base sha>`, so `<N>` parses from the ref,
       and the `commits/{sha}/pulls` fallback is not needed.
     - The landed commit is a real merge commit, made by `github-merge-queue[bot]`: its parents are
       the old main and the PR head, and its message is "Merge pull request #N".
     - An `on: push` workflow ran on main after a queue merge (actor `github-merge-queue[bot]`), so
       the batched e2e backstop on main still fires.
2. **`GITHUB_TOKEN` writes `claim/*` refs: decisions 1 and 3 stand.** With `contents: write`:
   - `POST git/refs` for `claim/probe-<run>` answered 201;
   - `PATCH` with `force=false` to a descendant answered 200;
   - a non-fast-forward `PATCH` answered 422;
   - a non-fast-forward `PATCH` with `force=true` also answered 422, so the `claims` ruleset binds
     the Actions bot as well.
3. **`issue_comment` from a non-collaborator: skipped** (procoduck, 2026-11-05). It would need a second
   GitHub account. Decision 1's fallback covers the failure instead: a refused write makes the bot
   point the outsider to a pull request with `Closes #N`, open one deduplicated Backlog issue for the
   maintainers and exit 0. The first real outsider `/claim` is the test. Whether GitHub silently drops
   an assignee it refuses also stays open; it changes only the mirror text.
4. **Re-running the `claim` check with `GITHUB_TOKEN`: decision 1 stands.** `POST
   actions/runs/{id}/rerun-failed-jobs` with `actions: write` answered 201, and attempt 2 ran as
   `github-actions[bot]`.
5. **Admin bypass with a queue: `hack/pr-merge.sh --admin` keeps working.** With the queue active,
   `gh pr merge --admin --merge` merged directly, without enqueueing; the result is a merge commit
   with two parents.

## Alternatives considered

- **Keep the permission gate and exempt outsiders by `authorAssociation`, and set the App secrets**
  (the cheaper option of the first research pass). It can be done today, but the App and the updater
  stay a second system. Outsiders also stay invisible to agents: an agent can `take` an issue an
  outsider is halfway through.
- **Assignee as the lock, Prow or triagebot style.** Last-write-wins (Prow adds, triagebot silently
  replaces). Outsiders cannot always be assigned, and `next` would hand every lane the same issue.
- **No coordination.** It works for humans (the open-source norm) and fails for agents: duplicates are
  certain when many lanes start together.
- **Auto-claim when a pull request opens.** Another writer with a write token on
  `pull_request_target`, for no gain: `next` and `take` already skip issues with an open pull request.
- **Run the kind e2e in the merge queue.** It is exact, but costs 15-25 minutes per needs-e2e entry
  and holds the queue. Deferred here; adopted later by [0102](0102-merge-queue-runs-the-kind-e2e.md).
- **Keep the App only for board Status.** It is a second identity for a mirror that ADR 0082 already
  calls best effort. Rejected; procoduck may ask for it later (decision 3).
- **Tide or bors as the queue.** They are services with their own state; the native queue needs no
  code of ours.

## Consequences

- Positive:
  - Outsiders can claim and merge.
  - Agents see what people and outsiders are doing.
  - No second identity remains to run, and the ghost claims since the move are swept again.
  - The merge order is GitHub's, so `updater.yml` and its 30-minute schedule go away.
- Negative:
  - A maintainer pull request without a claim now passes. Agent discipline rests on `take` and
    AGENTS.md, not on the merge check; the check still fences conflicts.
  - Every queued pull request runs CI once more on its queue commit, even when it was not behind main.
  - The board's Status can lag after a sweep until the next `claim.sh` call.
- Follow-ups, in this order, each its own issue, all with `claim_test.sh` cases:
  1. **Decisions 6 and 7, as one PR.** The `adr` scan of open PRs, plus `released_by` and the
     `beat --mine` fix. It needs no probe, so it goes first.
  2. **The conflict-only check and the skips:** `check-pr` (decision 2), and the `next` and `take`
     skips (decision 5).
  3. **Comment claims and the token move:** `claim-comment.yml` with its fallback, and the sweeper
     and release-on-close on `GITHUB_TOKEN` (decisions 1 and 3). After that, retire the App, the
     Environment and the `unclaimed-ok` label, and update `docs/dev/work-claims-setup.md` and the
     `claims.yml` header.
  4. **The merge queue:** `merge_group` on the five workflows and the `ci-required-test.sh` rule.
     Then the ruleset change, and removing `updater.yml`, `update-oldest` and the `needs-update`
     label, with `docs/dev/ci.md` (Merging) and AGENTS.md ("Picking up work" step 11) updated.

## Links

- [ADR 0082](0082-work-claims-and-ready-queue.md), [merge queue research](../research/2026-11-merge-queue.md), [setup checklist](../dev/work-claims-setup.md)
- [Managing a merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
- [Merge queue GA (2023-07-12)](https://github.blog/changelog/2023-07-12-pull-request-merge-queue-is-now-generally-available/)
- [Assigning issues](https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/assigning-issues-and-pull-requests-to-other-github-users)
- Prow `pkg/plugins/assign/assign.go` at `kubernetes-sigs/prow@a23969f4`; triagebot `src/handlers/assign.rs` at `rust-lang/triagebot@d34363e9`; bors `src/bors/merge_queue.rs` at `rust-lang/bors@1fbe59b4`; [triagebot #2532](https://github.com/rust-lang/triagebot/pull/2532)

## Amendment: trigger (2026-11-09, procoduck, #463)

The `claim` check no longer runs from `pull_request_target`; see the amendment in ADR 0082. Decision 8 (the merge queue) is unchanged.

## Amendment: the updater is retired (2026-11-10, procoduck)

Decision 4 kept `updater.yml` until "the first week of green queue merges with strict off". That
condition is replaced: the updater has been a no-op since the queue went live (`hack/pr-updater.sh`
returns at once when the ruleset has a `merge_queue` rule, and `strict_required_status_checks_policy` is
`false`), so there is nothing to wait for. Retired in the same change: `.github/workflows/updater.yml`,
`hack/pr-updater.sh` and its test, the `needs-update` label (an owner deletes it), the `claims-bot`
Environment with its App secrets (an owner deletes them, `docs/dev/work-claims-setup.md` step 7) and the
AGENTS.md sync points. A branch is merged with `origin/main` only when GitHub reports a conflict
(`pr-wait.sh` exit 4, or the queue drops the entry, exit 8); "merge `main`, never rebase" stays.

Also in this change: a draft pull request runs nothing heavy (its `go` and `web` report success with
"draft: not run"), and the push to main skips its heavy jobs only when a successful `merge_group` `ci` run
tested the pushed tree. The route itself (what admin merges may still do) is unchanged here; the follow-up
pull request that makes the queue the single route (decisions on `--admin`, the single e2e proof and the
break-glass path) carries its own ADR.
