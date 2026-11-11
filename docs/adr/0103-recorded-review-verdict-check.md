# 0103. A recorded review verdict, checked by `review`, replaces the second-owner approval outside `.github/`

- Status: Accepted (direction by procoduck, 2026-11-09; details by the implementing lane); amended 2026-11-11: the review verdict is advisory, not a gate
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
3. **Who controls the gate.** Pull request runs use `pull_request` with a read-only token and no
   secret; there is no `pull_request_target` (GitHub blocks it by default in public repositories from
   2026-12-02, and `hack/runner-lint.sh` refuses it, ADR 0082 amendment for #463). The job checks out
   **main's tip at run time** (`pull_request.base.ref`, the branch name; `branches: [main]` keeps it
   main; `merge_group.base_sha` in the queue, the entry's base) and runs that commit's gate, `claim.sh`
   and CODEOWNERS. Not `pull_request.base.sha`: it is the base recorded when the pull request was opened
   or last pushed, so an old pull request would run an old gate and poster list, or none. When the
   checkout has no gate, the job passes only if main's tip has none either (the pull request that adds
   it) and fails otherwise. The paths and the listed posters are main's, and the pull request's code
   is never checked out or run. The gate's files are sensitive paths
   themselves: `.github/workflows/review.yml` and `.github/CODEOWNERS` are under `.github/**`, and
   `/hack/review-gate.sh`, `/hack/review-verdict.sh` and `/hack/claim.sh` have their own CODEOWNERS
   lines (`hack/review-gate-test.sh` checks that `sensitive-paths` covers them), so a change to any of
   them needs a verdict from a listed poster under the old rules.

   **The trade-off.** On `pull_request` (and on `merge_group`) the workflow file itself comes from the
   pull request's head, so a pull request can edit `review.yml` (or add a second workflow with a job
   named `review`) to make `review` pass. The guard is the CODEOWNERS approval rule, **kept for
   `.github/**` only**. `.github/**` and not `.github/workflows/**`: every file a workflow can be made
   to run from the head (local actions under `.github/actions/`, workflow files, and configuration
   that workflows read) is under `.github/`, it is the pattern the ruleset already has, and the
   `claim` check relies on the same pattern since #463. The cost: a pull request that changes
   anything under `.github/` still needs the second human's approval (or an admin merge); every other
   sensitive path moves to the `review` check and the merge queue. The ruleset "Require workflows to
   pass before merging" rule (which runs main's `review.yml`) is the way to remove that last pattern
   when the plan has it (see the precondition). `hack/pr-merge.sh --admin` runs the base's gate
   itself, so the admin route is covered either way.
4. **Rollout.** The check lands advisory: it is listed in `hack/ci-required.txt` as an advisory check of
   another workflow (a comment line, so `hack/ci-merge-group-test.sh` still checks its `merge_group`
   trigger) and the ruleset is unchanged. Then sensitive-path pull requests can use `hack/pr-merge.sh`
   without `--admin`, and so the merge queue, once the owner's step is done.

   **Precondition (before the owner's step): the experiment**, not run yet. It runs in a scratch
   public repository in the organization, not in this one: a test ruleset on `main` here blocks every
   pull request while the test runs, and `mergeStateStatus` says nothing about `review` while the
   check is not required.
   - Set-up: the scratch repository holds copies of `.github/workflows/review.yml`,
     `hack/review-gate.sh`, `hack/review-verdict.sh`, `hack/claim.sh` and `.github/CODEOWNERS`, and a
     ruleset on its `main` with an empty bypass list that requires the context `review` and, as the
     owner's step will, a `required_reviewers` entry for `.github/**` only (1 approval).
   - Viewer: the person who reads the result has no bypass right on the scratch ruleset (or first
     confirms that `mergeStateStatus` does not depend on the viewer's bypass right). An admin's own
     view can hide the block.
   - Case A, a stand-in `review`: a pull request touches a sensitive path outside `.github/`, has no
     `review-verdict`, and adds a second workflow whose job `review` always passes. The stand-in is a
     second workflow file (trigger `pull_request`) with its own `name:` (not `review`), and in it a job
     with `name: review`, which matches the check name exactly. The real `review` fails on that head.
     Three orders, 1 and 2 each on a new head: order 1, the stand-in job finishes before the real one;
     order 2, it finishes after it (a `sleep 120` in the stand-in); order 3, after both runs of a head
     have finished (real red, stand-in green), "Re-run job" on the stand-in only, so the newest check
     run named `review` on that head is green. Order 3 is how a "newest run wins" rule would show. Run
     A once with the approval rule and once without it.
   - Case B, an edited `review.yml`: a pull request touches a sensitive path, has no verdict and no
     approval, and edits `review.yml` so that its job always passes.
   - Case C, a hand-posted status. GitHub requires a check run and a commit status of the same name
     to both pass ("If a check and a commit status have the same name, both must pass when that name
     is required", [Troubleshooting required status checks](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks)).
     So on a head where the real `review` check fails, `BLOCKED` proves nothing about the pin. Case C
     therefore uses a test head with no `review` check run at all and no other blocker: a pull request
     in the scratch repository whose `review.yml` does not run on it (for example a path filter that
     excludes the test file, in the scratch copy only), with the approval rule met or not applying, and
     every other required context green. Two steps, in this order:
     - Positive control, ruleset without the pin: the context `review` is required with no
       `integration_id`. Any account with write access posts `review`=success on the head
       (`gh api repos/<scratch>/statuses/<sha> -f state=success -f context=review`). The head must
       become mergeable (`mergeStateStatus` `CLEAN`). If it stays `BLOCKED`, the head has another
       blocker and the experiment says nothing: fix the head and run the control again.
     - Pinned: the same head and the same status, with the context pinned to the GitHub Actions app
       (`integration_id: 15368`, as in the owner's step). The head must be `BLOCKED`, and the merge
       box must say the check was not set by the expected app. The poster list guards only
       `review-verdict`, so the pin is what must block this status.
     Case C is accepted only if both results hold.
   - Result: the precondition is met only if `gh pr view <N> --json mergeStateStatus` gives `BLOCKED`
     for case B, for case C (positive control `CLEAN`, then pinned `BLOCKED`), and for all three orders of case A with the approval rule, after the runs end. Case A
     without the approval rule is recorded, not required: it shows whether a name-only context can be
     met by a stand-in, which decides whether `.github/**` could ever be dropped without the
     required-workflows rule (it cannot be dropped for case B in any event).
   - Record: the scratch repository's name, the pull request numbers and each `mergeStateStatus`
     result go under "Upstream verification" in this ADR before the owner's ruleset step.
   - After the test, the owner deletes the scratch repository.

   **If the precondition is not met**, the step does not go ahead. Options for the owner to decide
   (this ADR does not choose):
   - the ruleset "Require workflows to pass before merging" rule pinned to `.github/workflows/review.yml`
     on `main`, if the plan has it. The docs place this rule at the organization or enterprise level,
     and the 2023 changelog puts it on GitHub Enterprise plans; the organization is on the Team plan,
     so it is probably not available (not verified);
   - keep the full `required_reviewers` rule as it is today and use `review` as a record only;
   - an App pin (`integration_id`) when an App posts the check; the GitHub Actions App does not help,
     because both runs are Actions.

   **The owner's step** (after the precondition; by the owner's session through `gh api`, backup and
   diff, never from a pull request):
   - Before it: every open pull request needs a `review` run on its head (a push, or "Re-run" of the
     review workflow), or the merge box shows `review` as "Expected" and blocks it.
   - One `PUT repos/stateracloud/koinon/rulesets/26610739` that, in the same body, adds the context
     `review` (the name stays `review`), pinned to the GitHub Actions app (`integration_id: 15368`), to
     `required_status_checks` and changes the `pull_request`
     rule's `required_reviewers` entry to the one pattern `.github/**` (same reviewer team, 1 approval).
     One PUT, so there is no window with both rules in full or with neither.
   - The pin matters: without it, any account with write access could post a plain commit status
     `review`=success and pass the check without the gate. With `integration_id` set, GitHub accepts
     only the Actions app for that context, so statuses posted by users are blocked (case C).
   - Move the `review` line of `hack/ci-required.txt` to `external review`.
   - `require_extra_approval_for_unattributed_changes` stays as it is (see Open questions; not verified).
   - Drift to know about, and the list for a rollback that restores the full `required_reviewers`: the live
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
  head-workflow gap of decision 3 (old issue 568 tracks the same for `e2e result`).

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
- Negative: **a pull request that changes anything under `.github/` still needs the second human's
  approval** (or an admin merge), because on `pull_request` its own workflow files run (decision 3).
  Every other sensitive path moves to the `review` check and the merge queue. The required-workflows
  rule removes this last pattern when the plan has it.
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
  it asks for an approval outside `.github/**` once `required_reviewers` is cut to that pattern (for example on a commit GitHub cannot
  attribute) is not verified; check it on the first queued sensitive-path pull request after the step.
- A dedicated App for `review` (instead of the Actions pin) together with old issue 568.

## Upstream verification

- 2026-11-09, `gh api repos/stateracloud/koinon/rulesets/26610739`: the `pull_request` rule has
  `required_approving_review_count: 0`, one `required_reviewers` entry (15 patterns, a team reviewer,
  1 approval), `dismiss_stale_reviews_on_push: true`; `merge_queue` is live (ALLGREEN, group size 1,
  60 minutes); `required_status_checks` has 14 contexts, none pinned.
- 2026-11-09, `gh run list -w claims.yml --event pull_request` (after #465 moved `claim` off
  `pull_request_target`): run 42695412077's `headSha` is PR 465's head (7b8e1d714), not a test merge
  commit, so `review-verdict.sh` finds the run with `--commit <sha> --event pull_request`. The
  required `claim` check runs the same way (`pull_request`, base checkout) and satisfies its context.
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

## 2026-11-11: the review verdict is advisory

procoduck decided on 2026-11-11 that the independent review verdict no longer gates merges of
sensitive-path pull requests: it "just causes blockers". A verdict is valid for one SHA only, so every
push (a merge of `main` included) needed a fresh review and a re-run before the queue could take the
pull request, and the gate sat in front of fixes that the review itself had asked for.

What stays:

- Independent Opus reviews of a pull request's head keep running, and their findings are still fixed.
- `review.yml` still posts the `review` check, `hack/review-gate.sh` still computes it, and
  `hack/review-verdict.sh` still records a verdict. The check is information on the pull request.
- ADR 0105 is unchanged: `--admin` stays for a pull request with a `.github/` path (exit 9 otherwise), and
  the CODEOWNERS approval on `.github/**` stays.

What goes:

- `review` is removed from the required status checks of the `main` ruleset (done by the main session on
  2026-11-11). It is `advisory` in `hack/ci-required.txt` and still reports on `merge_group`.
- The `hack/pr-merge.sh` gate: `--admin` and `--break-glass` no longer run `hack/review-gate.sh` and no longer
  exit 9 for a missing verdict. `hack/review-gate.sh` now has no caller but `review.yml`.
- Decision 4's ruleset step (add `review` as a required context) is cancelled.

Cost: nothing in the repository now stops a sensitive-path pull request that nobody reviewed. The queue's
required checks and the owner's habit of running the review are the guard.

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
