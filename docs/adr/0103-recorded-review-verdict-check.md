# 0103. A recorded review verdict, checked by `review`, replaces the second-owner approval

- Status: Accepted (direction by procoduck, 2026-11-09; details by the implementing lane)
- Date: 2026-11-09
- Amends: [0082](0082-work-claims-and-ready-queue.md) decision 9 (approval on sensitive paths), once the ruleset step below is done
- Relates to: [0094](0094-work-claims-on-the-public-repository.md) (the merge queue), [0102](0102-merge-queue-runs-the-kind-e2e.md) (the queue runs the e2e), [0046](0046-adr-conventions.md)

## Context

ADR 0082 decision 9 requires one approval from the other human on the sensitive paths (the `main`
ruleset's `required_reviewers` rule, 15 patterns from the `# Sensitive paths` section of
`.github/CODEOWNERS`). An audit on 2026-11-09 found the rule cannot be met in practice: the repository
has one active human (99 of the last 100 pull requests have one author; the second owner has 0
reviews), 74% of merged pull requests touch those paths, and so 83% of merges use the org-admin bypass.
That bypass also skips the merge queue and so the queue-commit e2e of ADR 0102.

The review that does happen is an independent review by a separate agent (Opus) of the pull request's
head. Its verdict lives only in a session log; GitHub cannot see it or require it.

## Decision

1. **Evidence: the commit status `review-verdict` on the reviewed commit.** `hack/review-verdict.sh
   <PR> <ready|not-ready> <reviewer> <reviewed sha> [note]` posts `success` (ready) or `failure`
   (not ready); the description names the verdict, the reviewer and the note (140 characters). It
   refuses when the pull request is not open (exit 3) or its head is no longer the reviewed SHA (exit 4,
   `STALE`). A commit status, not a check run: any account with write access can post one with `gh`,
   no App is needed, and the pattern is the one `e2e-lab` already uses (`hack/e2e-status.sh`).
2. **Gate: the check `review`** (`.github/workflows/review.yml`, logic in `hack/review-gate.sh`, tests
   in `hack/review-gate-test.sh`, CI job `scripts`).
   - A pull request that touches no sensitive path passes. The patterns come from `hack/claim.sh
     sensitive-paths`, which reads the CODEOWNERS section: one source for the gate and, until the
     ruleset step, for `required_reviewers`. Files are the API's file list of the pull request; a
     rename counts by both names; an empty or truncated (3000 files) list is treated as sensitive.
   - Otherwise the newest `review-verdict` status on the head **from a listed poster** must be
     `success`. A failure or none fails with the command to run. Every read error fails the gate.
   - **Listed posters.** Only a status whose `creator.login` is one of the user logins that own the
     sensitive lines of the base branch's `.github/CODEOWNERS` counts (today `procoduck` and
     `ovirandunu`). A team owner (`@org/team`) is not a poster, a `*[bot]` login never is, and an
     empty list fails closed. The source is CODEOWNERS and not a separate file because those lines
     already name who answers for the sensitive paths, they are user logins (no team lookup, which
     the job's token could not do), and CODEOWNERS is itself under `.github/**`, so a change to the
     list is a sensitive change that needs a verdict from the old list. Any account with write access
     (and any workflow with `statuses: write`) can post a commit status, so without this list a
     pull request's own workflow, or a contributor, could post `success`.
   - **Statuses from other logins count in neither direction.** A newer status from a login outside
     the list does not pass, and it does not block either: otherwise any writer (or a bot) could
     veto a reviewed pull request by posting `failure` after the owner's `success`. The gate names
     how many it ignored. The statuses are read with pagination, so many statuses from other logins
     cannot push the listed one off the first page.
   - **Exact SHA only.** A push makes the verdict stale, as `dismiss_stale_reviews_on_push` does for
     approvals today.
   - **Drafts:** the job is skipped, which counts as passed for a required check; `ready_for_review`
     starts a new run. A draft cannot be queued or merged, so the skip opens nothing. Push to main:
     passes.
   - **`merge_group`:** the queued pull request's number comes from the queue ref
     (`gh-readonly-queue/<base>/pr-<N>-<sha>`, as the `claim` check does); its head and file list are
     read from the API and judged as above, and that head must be a parent of the queue commit
     (`merge_group.head_sha`, read through the commits API), so the verdict is for the commit the queue
     merges. It fails closed when the number, the head or the parents cannot be read.
   - **Matching:** patterns are ruleset patterns (`dir/**` or an exact path); an exact path also
     matches `<path>/*`, so a path that becomes a directory stays covered.
   - **`hack/pr-merge.sh --admin`** runs the same gate on the pull request head before it merges and
     refuses (exit 9, naming `hack/review-verdict.sh`) when a sensitive-path pull request has no
     `success` verdict from a listed poster: `--admin` bypasses the ruleset, so the check would not
     hold there otherwise.
   - **Late evidence:** a status event starts no workflow, so `review-verdict.sh` re-runs the `review`
     job of the pull request's newest finished run on that SHA when its result disagrees with the
     verdict (the `hack/e2e-rerun-result.sh` pattern). Not while that run is still in progress: the
     script says so, and "Re-run all jobs" is the manual path.
3. **Who controls the gate.** Pull request runs use `pull_request_target`: the YAML, the gate,
   `claim.sh` and CODEOWNERS are the base branch's and the pull request's code is never checked out,
   so a pull request's own edits to them take effect only after it merges (the `claim` check's model,
   ADR 0094). That alone does not stop a pull request from weakening the gate for the next one, so the
   gate's files are sensitive paths themselves: `.github/workflows/review.yml` and `.github/CODEOWNERS`
   are under `.github/**`, and `/hack/review-gate.sh`, `/hack/review-verdict.sh` and `/hack/claim.sh`
   have their own CODEOWNERS lines. A change to any of them needs a verdict from a listed poster on its
   head (`hack/review-gate-test.sh` checks that `sensitive-paths` covers them). What remains is the
   precondition below: a second workflow that reports a check named `review`. This deviates
   from `e2e result`, which runs the pull request's own YAML: that YAML was guarded by the approval rule
   this ADR removes. On `merge_group` the YAML is the queue commit's (GitHub's rule); the gate, `claim.sh`
   and CODEOWNERS are taken from `merge_group.base_sha`. A pull request that edits `review.yml` touches
   `.github/**`, so the base's gate requires a verdict on it before it can be queued.
4. **Rollout.** The check lands advisory: it is listed in `hack/ci-required.txt` as an advisory check of
   another workflow (a comment line, so `hack/ci-merge-group-test.sh` still checks its `merge_group`
   trigger) and the ruleset is unchanged. Then sensitive-path pull requests can use `hack/pr-merge.sh`
   without `--admin`, and so the merge queue, once the owner's step is done.

   **Precondition (before the owner's step), one of:**
   - (a) a ruleset "Require workflows to pass before merging" rule pinned to
     `.github/workflows/review.yml` on `main`, so only that file's run counts. The docs place this rule
     at the organization or enterprise level, and the 2023 changelog puts it on GitHub Enterprise plans;
     the organization is on the Team plan, so (a) is probably not available (not verified).
   - (b) the experiment, not run yet. It runs in a scratch public repository in the organization, not
     in this one: a test ruleset on `main` here blocks every pull request while the test runs, and
     `mergeStateStatus` says nothing about `review` while the check is not required.
     - Set-up: the scratch repository holds copies of `.github/workflows/review.yml`,
       `hack/review-gate.sh`, `hack/review-verdict.sh`, `hack/claim.sh` and `.github/CODEOWNERS`, and
       a ruleset on its `main` that requires the context `review`, with an empty bypass list.
     - The pull request: it touches a sensitive path, has no `review-verdict`, and adds a second
       workflow whose job `review` always passes. The real `review` fails on that head.
     - The stand-in: a second workflow file (trigger `pull_request`) with its own `name:` (not
       `review`), and in it a job with `name: review`, which matches the check name exactly.
     - Three orders, 1 and 2 each on a new head: order 1, the stand-in job finishes before the real
       one; order 2, it finishes after it (a `sleep 120` in the stand-in); order 3, after both runs of
       a head have finished (real red, stand-in green), "Re-run job" on the stand-in only, so the
       newest check run named `review` on that head is green. Order 3 is how a "newest run wins" rule would show.
     - Viewer: the person who reads the result has no bypass right on the scratch ruleset (or first
       confirms that `mergeStateStatus` does not depend on the viewer's bypass right). An admin's own
       view can hide the block.
     - Result: the precondition is met only if `gh pr view <N> --json mergeStateStatus` gives
       `BLOCKED` in all three orders, after the runs end.
     - Record: the scratch repository's name, the pull request number and the three
       `mergeStateStatus` results go under "Upstream verification" in this ADR before the owner's
       ruleset step.
     - After the test, the owner deletes the scratch repository.

   **If the precondition is not met**, the step does not go ahead with a context matched by name only.
   Options for the owner to decide (this ADR does not choose):
   - a required-workflow rule pinned to `review.yml`, if the plan has it (see (a));
   - keep the `required_reviewers` approval for `.github/**` only and drop it for the other patterns,
     so that a pull request cannot add a stand-in workflow without a human approval. Sensitive-path
     pull requests outside `.github/**` then use the check; changes to workflows still need the second
     owner or `--admin`;
   - an App pin (`integration_id`) when an App posts the check; the GitHub Actions App does not help,
     because both runs are Actions.

   **The owner's step** (after the precondition; by the owner's session through `gh api`, backup and
   diff, never from a pull request):
   - Before it: every open pull request needs a `review` run on its head (a push, or "Re-run" of the
     review workflow), or the merge box shows `review` as "Expected" and blocks it.
   - One `PUT repos/stateracloud/koinon/rulesets/26610739` that, in the same body, adds the context
     `review` (the name stays `review`) to `required_status_checks` and sets the `pull_request` rule's
     `required_reviewers` to `[]`. One PUT, so there is no window with both rules or with neither.
   - Move the `review` line of `hack/ci-required.txt` to `external review`.
   - `require_extra_approval_for_unattributed_changes` stays as it is (see Open questions; not verified).
   - Drift to know about, and the list for a rollback that restores `required_reviewers`: the live
     entry has 15 patterns; `hack/claim.sh sensitive-paths` now prints 18. Missing from the live rule:
     `hack/migrate-patch-line` (already in CODEOWNERS before this ADR), `hack/review-gate.sh` and
     `hack/review-verdict.sh` (added here). A rollback uses the full `sensitive-paths` output.

## Prior art

Quick pass (about 15 minutes, 2026-11-09): Prow and Tide docs, bors-ng reference and forum, Mergify
conditions docs, GitHub's ruleset docs.

- **Prow Tide:** merge criteria are labels (`lgtm`, `approved`) set by bot commands from people listed
  in OWNERS files; Tide merges when the labels are present and the jobs pass. The record of a review is
  a machine-readable marker that the merge bot requires, not a GitHub approval. We took that idea.
- **bors-ng:** its own `required_approvals` replaces GitHub's review rule (the docs say to switch the
  GitHub rule off and require the `bors` status instead), and `up_to_date_approvals` makes an approval
  count only for the current commit. We took both: the check replaces the native rule, and a verdict is
  valid for one SHA.
- **Mergify:** queue conditions combine `#approved-reviews-by>=1` with `check-success=` entries; a review
  is one more condition next to the checks. We did not take a third-party service.
- **GitHub "expected source":** a required check can be pinned to an App (`integration_id`), so a status
  from anyone else does not count. Not taken now: there is no App for this; the gate's own poster list
  does the same job for the status. It is the path to separation of duties and to closing the
  second-workflow gap of the precondition (old issue 568 tracks the same for `e2e result`).

## Consequences

- Positive: sensitive-path merges can go through the queue and its e2e; the admin bypass becomes the
  exception it was meant to be. The review becomes visible on the pull request and auditable per SHA.
- Positive _(inferred)_: the CODEOWNERS patterns the live ruleset lacks (`hack/migrate-patch-line`, and
  the two added here) are covered by the gate, because the gate reads the list at run time.
- Negative: **this is a record of a review, not a separation of duties.** A listed owner posts the
  status, and that owner may be the author of the pull request; nothing technical stops a listed owner
  from posting `ready` without a review. The review's independence rests on the process (a separate
  agent reviews the head) and on the description naming the reviewer. The poster list only stops
  everyone else (contributors, bots, workflows) from posting or vetoing.
- Negative: **an admin who merges in the web UI** ("merge without waiting for requirements") skips
  the check, as today. `hack/pr-merge.sh --admin` runs the gate itself; the web UI and a hand-written
  `gh pr merge --admin` do not. The bypass list of the ruleset is the limit.
- Negative: a merge of `main` into the branch makes the verdict stale and needs a new review, even
  when the merge is clean.
- Negative _(inferred)_: one more workflow and one more required context.
- **Exit condition:** the human approval comes back (`required_reviewers`, or the check requires an
  approving review next to the verdict) when a second owner reviews every week.

## Open questions

- **A head that differs from the reviewed SHA only by a clean merge of `main`.** Candidate rule: the
  head is a merge commit whose first parent is the reviewed SHA and whose other parent is on `main`, and
  the pull request's diff against its merge base is the same as at the reviewed SHA (an empty
  `git range-diff`, or equal `git diff base...sha` blobs). Not built: a "clean" merge can still change
  behaviour through code on `main`, and the rule needs history in the gate. Revisit when stale verdicts
  after main merges are measured as a real cost.
- **`require_extra_approval_for_unattributed_changes: true`** stays in the `pull_request` rule. Whether
  it asks for an approval once `required_reviewers` is gone (for example on a commit GitHub cannot
  attribute) is not verified; check it on the first queued sensitive-path pull request after the step.
- Pin `review` to an App (`integration_id`) together with old issue 568.

## Upstream verification

- 2026-11-09, `gh api repos/stateracloud/koinon/rulesets/26610739`: the `pull_request` rule has
  `required_approving_review_count: 0`, one `required_reviewers` entry (15 patterns, a team reviewer,
  1 approval), `dismiss_stale_reviews_on_push: true`; `merge_queue` is live (ALLGREEN, group size 1,
  60 minutes); `required_status_checks` has 14 contexts, none pinned.
- 2026-11-09, `gh run list -w claims.yml --event pull_request_target`: a `pull_request_target` run's
  `headSha` is the pull request head, so `review-verdict.sh` finds the run with `--commit <sha>`.
- Not verified: a `pull_request_target` check run satisfying a required context on the pull request
  head is assumed from the live `claim` check, which is required and runs the same way.
- 2026-11-09, GitHub docs, two check runs with the same name on one SHA: the troubleshooting page says
  "If a check and a commit status have the same name, both must pass when that name is required." and
  "Required checks must pass on the latest commit SHA. Checks from earlier commits don't satisfy the
  requirement." It says nothing about two check runs with the same name: **not verified**, hence the
  experiment (b) of the precondition. A commit status named `review` posted by a writer does not pass the
  check alone, by the first sentence.
- 2026-11-09, GitHub docs, "Require workflows to pass before merging": the rulesets page says
  "Ruleset workflows can be configured at the organization or enterprise level" and "The workflow file
  needs to be in a repository that matches the visibility of the repositories you want to run it in."
  The GitHub Enterprise Server troubleshooting page for rules lists the `pull_request`,
  `pull_request_target` and `merge_group` events for such workflows and says a workflow needs a
  `merge_group` trigger to run in the queue. The changelog of 2023-08-02 says required workflows "will
  only be available on GitHub Enterprise plans via Repository Rules". The page does not name the plans
  for the rule itself, and merge-queue support is stated only on the Enterprise Server page: **not
  verified** for github.com on the Team plan.
- 2026-11-09, the expected-source text: "When you add a required status check rule, you can select an
  app as the expected source of status updates." and "If the status is set by any other person or
  integration, merging won't be allowed."

## Links

- `hack/review-verdict.sh`, `hack/review-gate.sh`, `hack/review-gate-test.sh`, `.github/workflows/review.yml`
- `docs/dev/ci.md` "The `review` check"
- Prow Tide: https://docs.prow.k8s.io/docs/components/core/tide/config
- bors-ng reference: https://github.com/bors-ng/bors-ng.github.io/blob/e87e702fff1a5fd338dfd449d3f64845db4252b3/reference.md
- Mergify conditions: https://docs.mergify.com/configuration/conditions.md
- GitHub, troubleshooting required status checks: https://docs.github.com/en/enterprise-cloud@latest/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks
- GitHub, available rules for rulesets: https://docs.github.com/en/enterprise-cloud@latest/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets
- GitHub Enterprise Server, troubleshooting rules: https://docs.github.com/en/enterprise-server@3.19/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/troubleshooting-rules
- GitHub changelog, required workflows move to rulesets: https://github.blog/changelog/2023-08-02-github-actions-required-workflows-will-move-to-repository-rules/
