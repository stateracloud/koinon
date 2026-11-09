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
   - Otherwise the newest `review-verdict` status on the head must be `success`. A failure or none fails
     with the command to run. Every read error fails the gate.
   - **Exact SHA only.** A push makes the verdict stale, as `dismiss_stale_reviews_on_push` does for
     approvals today.
   - **Drafts:** the job is skipped (not red); `ready_for_review` runs it. Push to main: passes.
   - **`merge_group`:** the queued pull request's number comes from the queue ref
     (`gh-readonly-queue/<base>/pr-<N>-<sha>`, as the `claim` check does); its head and file list are
     read from the API and judged as above. It fails closed when the number or the head cannot be read.
   - **Late evidence:** a status event starts no workflow, so `review-verdict.sh` re-runs the `review`
     job of the pull request's newest finished run on that SHA when its result disagrees with the
     verdict (the `hack/e2e-rerun-result.sh` pattern). Not while that run is still in progress: the
     script says so, and "Re-run all jobs" is the manual path.
3. **Who controls the gate.** Pull request runs use `pull_request_target`: the YAML, the gate,
   `claim.sh` and CODEOWNERS are the base branch's and the pull request's code is never checked out,
   so a pull request cannot weaken its own check (the `claim` check's model, ADR 0094). This deviates
   from `e2e result`, which runs the pull request's own YAML: that YAML was guarded by the approval rule
   this ADR removes. On `merge_group` the YAML is the queue commit's (GitHub's rule); the gate, `claim.sh`
   and CODEOWNERS are taken from `merge_group.base_sha`. A pull request that edits `review.yml` touches
   `.github/**`, so the base's gate requires a verdict on it before it can be queued.
4. **Rollout.** The check lands advisory: it is listed in `hack/ci-required.txt` as an advisory check of
   another workflow (a comment line, so `hack/ci-merge-group-test.sh` still checks its `merge_group`
   trigger) and the ruleset is unchanged. The owner's step: move it to `external review`, add the
   context `review` to the ruleset's `required_status_checks`, and remove the `required_reviewers`
   entry of the `pull_request` rule. Then sensitive-path pull requests can use `hack/pr-merge.sh`
   without `--admin`, and so the merge queue.

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
  from anyone else does not count. Not taken now: there is no App for this, and the poster is the
  maintainer anyway (see Consequences). It is the path to separation of duties (old issue 568 tracks the
  same for `e2e result`).

## Consequences

- Positive: sensitive-path merges can go through the queue and its e2e; the admin bypass becomes the
  exception it was meant to be. The review becomes visible on the pull request and auditable per SHA.
- Positive _(inferred)_: the 16th CODEOWNERS pattern, `hack/migrate-patch-line`, which the live ruleset
  lacks (it has 15), is covered by the gate, because the gate reads the list at run time.
- Negative: **this is a record of a review, not a separation of duties.** The same account that writes
  the code posts the status; nothing technical stops it from posting `ready` without a review. The
  review's independence rests on the process (a separate agent reviews the head) and on the description
  naming the reviewer.
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

## Links

- `hack/review-verdict.sh`, `hack/review-gate.sh`, `hack/review-gate-test.sh`, `.github/workflows/review.yml`
- `docs/dev/ci.md` "The `review` check"
- Prow Tide: https://docs.prow.k8s.io/docs/components/core/tide/config
- bors-ng reference: https://github.com/bors-ng/bors-ng.github.io/blob/e87e702fff1a5fd338dfd449d3f64845db4252b3/reference.md
- Mergify conditions: https://docs.mergify.com/configuration/conditions.md
- GitHub rulesets, required status checks and expected source: https://docs.github.com/en/enterprise-cloud@latest/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks
