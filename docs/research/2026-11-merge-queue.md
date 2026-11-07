# Merge queue instead of the strict up-to-date rule

Research date: 2026-11-02, for #597. Research only: no workflow or ruleset changes. Sources:

- the `main` ruleset, read through the API on 2026-11-02 (`gh api repos/stateracloud/koinon/rulesets/25718673`);
- `hack/ci-required.txt` and `.github/workflows/*.yml` at `692990d`;
- ADR 0082 and AGENTS.md ("Merging", "Required status checks");
- the CI cost figures in #593, #595 and #597 (measured 2026-10-26 to 11-02).

**Caveat.** The quotes of GitHub's documentation were read on 2026-11-02 by a second session; this
one cannot reach docs.github.com. A statement about GitHub's behaviour that those pages do not make
is marked **[unverified]**, with the page or test that would settle it.

## Short answer

- **Not available today: refused by the API on 2026-11-02 (Team plan, private repo).** The facts:
  - The org is on the Team plan with 2 seats (`gh api orgs/stateracloud`: plan `team`).
  - The repository is private (`"private": true` from the repos API).
  - No queue is configured (GraphQL `repository.mergeQueue(branch: "main")` is null), which does
    not prove a queue is unavailable.
  - ADR 0082 (Context) states "The repository is private on the Team plan: merge queue is not
    available", and `claims.yml` says the same in its header. ADR 0082 decision 8 already says
    "Use the native merge queue once the repository is public".

  - Creating a disabled ruleset with a `merge_queue` rule fails with HTTP 422 "Invalid rule
    'merge_queue': ". The same payload with a `deletion` rule is accepted (section 1).

  GitHub's error text is empty, and its pages on the queue, its plans and its pricing do not state
  which plans include it. So the plan is the most likely cause, but GitHub does not name it.
- **Even where it is available, a queue saves little once #598 (#593 + #595) lands.**
  - Without #598, a size-1 queue roughly breaks even.
  - With #598, it costs more than today unless groups batch about four PRs or the heavy jobs move
    from the pull request into the queue. The cost section has the arithmetic.
- **Recommendation.**
  - Keep the strict rule and the updater (ADR 0082's home-built queue of size 1), and land #598.
  - Make the updater cheaper: sync only ready PRs whose own checks are green.
  - Prepare the migration below for the day the repository is public (or on Enterprise Cloud),
    and adopt the native queue then.
  - Turning the strict rule off without a queue is cheaper now and costs later (see the end).

## 1. The plan requirement

| Fact | Source |
|---|---|
| Repository is private, org-owned, merge commits only, auto-merge on | repos API (`private: true`, `allow_merge_commit: true`, squash/rebase off, `allow_auto_merge: true`) |
| Plan is Team, 2 seats | `gh api orgs/stateracloud` (read 2026-11-02) |
| No queue configured on `main` | GraphQL `repository.mergeQueue(branch: "main")` is null |
| Merge queue refused for this private repository on Team | The rulesets API, 2026-11-02 (below); ADR 0082 Context; `claims.yml` header. Neither [managing-a-merge-queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue), the [plans page](https://docs.github.com/en/get-started/learning-about-github/githubs-plans) nor [pricing](https://github.com/pricing) states plan availability. |

The API test was run on 2026-11-02 by a second session with the gh CLI:

- `POST repos/stateracloud/koinon/rulesets` with `enforcement: "disabled"` and a `merge_queue`
  rule (all seven parameters, grouping strategy `ALLGREEN`, merge method `MERGE`). It was aimed at
  `~DEFAULT_BRANCH`, and in a second try at a branch that does not exist. Both answered HTTP 422
  "Validation Failed", with errors `["Invalid rule 'merge_queue': "]`.
- Control: the same payload with a `deletion` rule in place of `merge_queue` was accepted (ruleset
  26401043) and deleted at once.
- Afterwards the ruleset list was unchanged: `claims`, `main` and the three release-tag rulesets.

So the API refuses `merge_queue` for this repository while it accepts the other rules. That
matches ADR 0082. The error names no reason; the Team plan on a private repository is the likely
one.

Paths to a queue:

1. Make the repository public. AGENTS.md: "Koinon isn't public until it's polished".
2. Move the org to Enterprise Cloud, a per-seat price change (https://github.com/pricing), if
   that is what unlocks it **[unverified]**.

Neither is a CI decision. Until one happens, the queue is a plan for later, and this note is its
design.

## 2. The required checks on `merge_group`

The queue builds a temporary branch holding main plus the queued PR(s); for third-party CI the docs
say to "run when a branch that begins with the special prefix `gh-readonly-queue/{base_branch}` is
pushed to" ([managing-a-merge-queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)). Actions sees it as the `merge_group` event, activity type
`checks_requested`, with `GITHUB_SHA` the "SHA of the merge group" and `GITHUB_REF` the "Ref of the
merge group" ([events](https://docs.github.com/en/actions/writing-workflows/choosing-when-your-workflow-runs/events-that-trigger-workflows#merge_group)). The same managing page:

> You **must** use the `merge_group` event to trigger your GitHub Actions workflow when a pull
> request is added to a merge queue.

> If your repository uses GitHub Actions to perform required checks on pull requests in your
> repository, you need to update the workflows to include the `merge_group` event as an additional
> trigger. Otherwise, status checks will not be triggered when you add a pull request to a merge
> queue. The merge will fail as the required status check will not be reported.

The queue "will wait for required checks to be reported", up to its "Status check timeout"; a PR
removed that way reads "Timed out awaiting a successful CI result based off the configured timeout
setting". So every required check below needs a `merge_group` trigger, or another job that reports
the same check name on the group's commit.

Required today (`hack/ci-required.txt`, the same 18 contexts as the live ruleset, none pinned to an
app, `strict_required_status_checks_policy: true`):

| Check | Workflow, today's triggers | On `merge_group` |
|---|---|---|
| `go`, `web`, `quick checks`, `generated code`, `chart`, `chart smoke`, `docs`, `escape (admission)`, `lint-shell`, `scripts` | `ci.yml`: `pull_request`, `push` to main | Add `merge_group:`. `changes` passes `BASE=''` for any event except `pull_request`, so the group runs **every** job. To keep path filtering, pass the group's base (`github.event.merge_group.base_sha`) to `hack/ci-changes.sh`. the proto breaking check (in `quick checks`) and `migrate-lint`'s base are also `pull_request`-only; both would read `merge_group.base_sha`. The concurrency group `ci-${{ github.ref }}` is unique per queue ref, so groups do not cancel each other. |
| dependency age (steps of `quick checks`, `if: pull_request`) | `ci.yml` | A skipped step does not fail the job, so it passes as is. The PR run already checked it. |
| `dco` | `dco.yml`: `pull_request` only | Add `merge_group:` and check `merge_group.base_sha..head_sha`. The commits are the PRs' own. The group's merge commits are exempt (`--no-merges`), so it passes when the PR passed. |
| `gitleaks` | `secrets.yml`: `pull_request`, `push` | Add `merge_group:` and scan `base_sha..head_sha`. Redundant with the PR scan, but cheap, and it has to report. |
| `claim` | `claims.yml`: `pull_request_target` only | `pull_request_target` is a pull request event, not a group one, so it does not run for the group **[unverified]**: the events page lists `merge_group` separately and says nothing either way. Two options: (a) a `merge_group` job that takes `<N>` from the queue ref and runs the base's `hack/claim.sh check-pr <N>`; (b) one that reports success, since the claim was checked on the PR before it could be queued. (a) keeps the check meaningful for a claim lost while queued. A group of several PRs needs every PR number, and whether the ref names all of them is **[unverified]**. |
| `e2e result` | `e2e-kind.yml`: `pull_request`, `push`, schedule, dispatch | The hard one; see below. |

**`e2e result` and the externally-run e2e.** `hack/e2e-gate.sh` looks for evidence on the PR's head SHA:

1. this run's quickstart;
2. another e2e-kind run on that head;
3. the `e2e-lab` commit status that an external e2e run posts (see the note at the end).

A group's commit is a new SHA, so none of the three is there. Options:

- **Map the group to its PRs** and accept evidence on each PR head. This is cheap, but the
  meaning weakens. Today the strict rule makes the PR head's tree the merged tree, so an
  external run tested exactly what lands. In a queue, it tested the PR without the other PRs in its group
  and without what main gained since. That gap is what the strict rule closes today, and #281 and
  #284 are the failures it exists for.
- **Run the e2e in the queue** for groups that contain a needs-e2e PR. This is exact, but each
  run takes 15-25 minutes on a GitHub-hosted runner. It also holds the whole queue, unless
  build concurrency ("The maximum number of `merge_group` webhooks to dispatch (between `1` and
  `100`)") runs groups in parallel.
  An external run cannot serve it: it takes a pushed branch, and the queue ref is
  read-only and short-lived.
- **Recommended if a queue comes:** map to PR heads for the gate, and keep the batched e2e on main
  (`push`, as now) as the backstop. `hack/e2e-bisect.sh` already names the merge to revert within
  2 hours. Whatever the queue needs must come from the base branch's copy of `e2e-gate.sh`, as
  today. The trust limit in AGENTS.md (no pinned `integration_id`, #568) is unchanged.

**DCO and claims on the temporary branch.** Neither runs the PR's code from the group. `claim`
keeps reading the base's `hack/claim.sh`. The `claims` ruleset covers only `claim/*`, so the queue's
`gh-readonly-queue/*` refs are untouched. Claim refs never point at the queue.

## 3. The push-to-main run

A queue merge still moves main. Whether that fires `push` on main is **[unverified]**: none of the
pages read says so. Test it on the first day, because these depend on it:
`ci.yml`, `secrets.yml`, the batched `e2e-kind`, and `claims.yml` (its `updater`, which becomes
pointless). `flavor-publish` must keep running on push, since it publishes.

If main then points at the very commit the group tested **[unverified]**, a green `ci` run on that
SHA with event `merge_group` is stronger evidence than #595's "tree equals a green PR head", and
the push run can skip its heavy jobs on it.

#595 as implemented in #598 reads the merge commit's second parent and requires exactly two
parents. A queue's commit may not have that shape: a group of several PRs stacks merge commits, and
only the top one is pushed **[unverified]**. So the queue needs its own check by SHA (above), not
#598's parent check; otherwise every queued merge runs the full push run. The skip must still run everything when:

- the SHA has no green `merge_group` run (an admin bypass or a direct push);
- the job publishes something.

So yes: a group's green run can replace the push-to-main run, with the same job-level `if:`
pattern #595 introduces (a skipped required job reports success).

## 4. Cost compared with today

Figures (one week, 2026-10-26 to 11-02, Blacksmith billed minutes; from #593, #595 and #597):

- Merge-main-into-branch runs: 109, costing 2,322 min (about 21 min each). 1,393 min of that is on
  ready PRs and about 929 on drafts.
- Draft PRs, all runs: 3,119 min (#593).
- The push-to-main `ci` run: about 200 runs, about 36 min each in the new layout; 36% of all CI
  minutes. #595 estimates "up to ~4,000 min a week" saved by skipping it.
- Merges: about 200 a week (the push runs). Recent days had 44 and 32 merges on main (git log
  2026-11-01 and 11-02).

Model: every queued PR gets one `merge_group` run of about 21 min (path-filtered, as a PR run) to
36 min (everything, as `changes` does today without a base). At group size *k*, about 200/*k*
group runs a week.

| Scenario | Merge-sync | Push to main | Queue runs | Change vs today |
|---|---|---|---|---|
| **Today** | 2,322 | ≈4,000 removable | none | baseline |
| **#598 only** (drafts fast-only; main skips a green tree) | ≈1,393 (draft syncs become cheap) | ≈0 heavy | none | about −4,900 |
| **Queue, *k* = 1, without #598** | 0 | ≈0 (exact-SHA skip) | 4,200–7,200 | −2,322 −4,000 +4,200..7,200 = **about −2,100 to +900** |
| **Queue, *k* = 1, with #593** | 0 | ≈0 | 4,200–7,200 | vs #598 only: **+2,800 to +5,800** |
| **Queue, *k* ≈ 4, with #593** | 0 | ≈0 | 1,050–1,800 | vs #598 only: **about −350 to +400** |
| **Queue as the only full gate** (ready PRs run fast checks; the heavy jobs run in the queue), *k* ≈ 4 | 0 | ≈0 | 1,050–1,800 | also drops the ready PRs' own heavy runs; the largest saving, and the largest change in feedback |

Reading it:

- Today's strict rule is a queue of size 1 that runs only for PRs that are behind. A native queue
  runs for **every** PR, behind or not.
- #595 already removes the push-to-main re-test, which is the queue's main structural saving.
- After #598, a native queue is a win only with batching. Batching needs merges to overlap: at
  30-45 merges a day they often do, but a group of 4 that fails costs a re-run of the remainder
  (how the queue splits a failed group is **[unverified]**). A queue also wins if the full suite moves out of
  the PR, which is a different review-feedback model and its own decision.
- Not counted: agent and human time. Today's updater syncs one PR at a time, so a busy day queues
  PRs behind one another (wall-clock latency, not minutes). A queue with build concurrency
  greater than 1 cuts that wait, and that, more than minutes, is its real gain.

## 5. Risks

- **Queue stalls.** A required check that never reports on `merge_group` holds the group until
  its status check timeout and then removes it (section 2). Every new required job then needs the `merge_group` trigger.
  `hack/ci-required-test.sh` should learn to fail a required `ci.yml` job whose workflow lacks
  `merge_group`, as it already fails an unlisted job.
- **Flaky checks.** Today a flake fails one PR's run and is re-run there. In a queue, a flake
  fails the group and ejects its PRs; with *k* > 1 one flake delays several PRs, and a re-queue
  re-runs the rest. The flake work (#533, #567: wait on signals, not sleeps) matters more under a
  queue.
- **Admin merges.** The ruleset's bypass is `OrganizationAdmin`, mode `pull_request`.
  [Merging with a queue](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/incorporating-changes-from-a-pull-request/merging-a-pull-request-with-a-merge-queue): "if you are an administrator, you can: Directly merge the pull
  request by checking **Merge without waiting for requirements to be met (bypass branch
  protections)**, if allowed by branch protection settings". Either way, a bypassed
  merge has no green `merge_group` run on its SHA, so the push run must run in full. The
  exact-SHA skip of section 3 gives that for free.
- **Sensitive-path review.** `required_reviewers` (one approval on the CODEOWNERS sensitive
  paths, ADR 0082 decision 9) is part of the `pull_request` rule and is checked before a PR can
  enter the queue. Nothing in the pages read says the queue re-evaluates it **[unverified]**, so nothing should
  change there; see the [ruleset rules](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets).
  `dismiss_stale_reviews_on_push` is unaffected: the queue does not push to the PR branch.
- **The e2e meaning** (section 2): a lab run on the PR head no longer proves the merged tree.
- **Tooling drift.** These all assume the updater and `needs-update`:
  - `hack/ship.sh`, `hack/pr-wait.sh`;
  - `claim.sh update-oldest` (`claims.yml` `updater`);
  - AGENTS.md "Merging" and step 11 of "Picking up work";
  - ADR 0082 decision 8.

  They change together with the switch.

## 6. Migration steps and rollback

For the day the plan allows it:

1. **Triggers first; no ruleset change yet** (one PR):
   - add `merge_group:` to `ci.yml`, `dco.yml` and `secrets.yml`, and a `merge_group` job named
     `claim` (option (a)) and one named `e2e result` (map to PR heads);
   - make `changes` and the proto and migrations steps of `quick checks` read `merge_group.base_sha`;
   - teach `hack/ci-required-test.sh` the trigger rule.

   Harmless while no queue exists: the event never fires.
2. **Push run skip.** Extend #595's skip to accept a green `merge_group` run on the same SHA,
   alongside its two-parent check.
3. **Ruleset.** In the `main` ruleset:
   - add the merge queue rule: method merge, group size 1, build concurrency 2, status check
     timeout 60 min (above the e2e's 25 min if the e2e runs in the queue), "Only merge non-failing
     pull requests" on. The docs give the ranges (concurrency and group size 1-100; merge, rebase or
     squash) but no defaults, so set each one explicitly;
   - turn `strict_required_status_checks_policy` off;
   - keep `required_reviewers` and the 18 contexts.

   Record the old ruleset JSON in the PR first (`gh api .../rulesets/25718673 > before.json`).
4. **Retire the updater.** Disable the `updater` job and `update-oldest`; drop `needs-update` from
   the docs. Update `hack/ship.sh`, `hack/pr-wait.sh`, AGENTS.md "Merging" and ADR 0082 decision 8,
   as an amendment of 0082.
5. **Watch a week.** Measure queue minutes, ejections, flakes and time to merge. Raise the group
   size to 3 or 4 only if ejections stay rare.

Rollback, from any step:

- Remove the merge queue rule and set `strict_required_status_checks_policy: true` (restore
  `before.json`).
- Re-enable the `updater` job. PRs that were in the queue fall back to auto-merge and the updater
  syncs them.
- The `merge_group` triggers can stay: they never fire without a queue.

Steps 1 and 2 need no rollback.

## Recommendation

1. **Sustainable, now:**
   - Keep the strict rule and the updater. Land #598.
   - Narrow the updater to ready PRs whose own required checks are green, so a red PR is not
     re-run against new main for nothing. That targets the 1,393 ready-PR sync minutes directly.
     Overlap: #596 (PR #600) trims the `claims` workflow's updater in parallel, so this change
     belongs in that PR or follows it, never as a separate edit of the same job.
   - Do step 1 of the migration whenever convenient (inert until a queue exists), so the switch is
     a ruleset edit when the repository goes public, as ADR 0082 decision 8 already plans.
2. **Adopting the queue waits for the repository going public, or a plan change. Then:**
   - Adopt the native queue with group size 1 and build concurrency 2 for latency.
   - Consider "the queue is the full gate" only as its own decision, since it changes PR feedback.
3. **The cheaper option and what it costs later:** turn the strict rule off with no queue. That
   saves most of the 1,393 ready-PR sync minutes now. Main then takes merges no one tested
   together:
   - Two green PRs with a semantic conflict make main red. #281 and #284 show that what only a full
     install sees is exactly what slips through.
   - Each red main costs a bisect issue, a fix-forward or revert within 2 hours, and blocked
     merges meanwhile.
   - #595's tree-equality skip stops applying (the tree no longer equals a green head), so the
     push run goes back to full cost and eats the saving.

## Note (2026-11-07, issue #300)

When this was written, the third kind of evidence came from a maintainer's remote-run client
(`hack/e2e-remote.sh --argo`). That client has left the repository; the repository's e2e path is kind, and
`e2e-lab` is any external run that posts the commit status. The analysis above is unchanged.
