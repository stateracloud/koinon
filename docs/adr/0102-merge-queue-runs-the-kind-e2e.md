# 0102. The merge queue runs the kind e2e on the queue commit

- Status: Accepted (procoduck, 2026-11-09)
- Date: 2026-11-09
- Amends: [0094](0094-work-claims-on-the-public-repository.md) decision 4 (no kind e2e in the queue)
- Relates to: [0046](0046-adr-conventions.md)
- Issue: #444 (draft PR #451)

## Context

ADR 0094 decision 4 kept the kind e2e out of the merge queue: `e2e result` read the e2e evidence on the
pull request head, and the revisit condition was "run the kind e2e in the queue when a PR that passed
on its head failed on main". That happened. On 2026-11-09 three pull requests passed the lab e2e and
could not merge because main moved during the run; each needed a full re-run on the synced head.

## Decision

1. **For an e2e-relevant queue entry, `decide` starts the hosted `quickstart` on the queue commit**
   (main plus the pull request), and `e2e result` is green when it passes. A red run drops the entry from
   the queue. "E2e-relevant" is the entry's diff against `merge_group.base_sha`: the full rule, the e2e's
   own files, a rule failure, or a missing base sha. Otherwise the job is skipped and `e2e result` is
   green.
2. **Evidence split.** The hosted quickstart on the queue commit is the merge evidence. `e2e-lab`
   cannot run on the read-only, short-lived queue ref, so it stays pre-merge confidence on the PR head.
   The PR-head gate (`quickstart` or `e2e-lab`) still decides whether a PR can be enqueued; the queue
   does not read it again.
3. **Flow.** An e2e-relevant PR is enqueued, not merged with `--admin`: an admin merge bypasses the
   queue and so the queue-commit run. `hack/pr-merge.sh --admin` warns in that case.
4. **Cost.** Each e2e-relevant entry adds about 12 to 15 minutes of queue latency (more for a cold flavor build). Entries build concurrently
   (up to 5), so this is latency per entry, not per queue. The queue check timeout (60 min) is below the
   job timeout (75 min), so the queue gives up first. Measured on 2026-11-09: the slowest `quickstart` job
   of the last 200 finished runs took 14.2 minutes (typical 12 to 13.5), so the 60-minute check timeout keeps
   45 minutes of headroom and stays. A cold flavor build in the queue is not in the sample; raise
   `check_response_timeout_minutes` to 90 if entries start timing out. Group size stays 1: a red e2e costs one
   entry, not a batch. Batching is a later, measured change. No ruleset change is needed: `e2e result` is
   already required and already reports on `merge_group`.
5. **The push to main does not repeat the e2e.** `decide` on a push also accepts a green `merge_group`
   `quickstart` job whose head commit has the pushed commit's tree (compared through the git commits API).
   `hack/release-gate.sh` follows the same logic: a `skipped` `quickstart` check run (the push run's) is ignored,
   the newest run that is not skipped decides, and with none a green `merge_group` `quickstart` on the tagged
   commit's tree counts. A failure or cancellation, or an API error, fails the gate.
6. **Bootstrap shim.** The queue entry that merges this change runs the old `decide` (from `base_sha`), which
   answers `needed=false, required=true`. `e2e result` therefore keeps the old queued-PR-head lookup, guarded
   by `merge_group` with `required=true` (only an old base's `decide` produces that), in
   `hack/e2e-queue-head.sh`. Exit condition: remove it when main's `decide` is the new one and no entry built
   on an older base can remain (follow-up: #452).

## Prior art

Quick check from the sources ADR 0094 pins plus general knowledge; the sources were not re-read.

- **Kubernetes Prow Tide:** required jobs run on a batch of PR heads merged with the base; a red batch
  falls back to single PRs. Slow e2e suites are why Tide batches. We borrow "test the merged result
  before it lands".
- **rust-lang bors:** base plus PR is built on a bot branch and main is fast-forwarded only when green
  (`src/bors/merge_queue.rs` at `rust-lang/bors@1fbe59b4`). It runs the full suite on every merge, one at
  a time, and accepts the latency. Same idea as ours.
- **GitHub merge queue users:** gate slow suites on `merge_group`; group size and
  `min_entries_to_merge_wait_minutes` are the cost knobs. A failed group drops its entry and rebuilds
  those behind it; GitHub does not bisect a failed batch.
- **We differ:** group size 1, so a failure never penalises other PRs.

## Consequences

- Main stops needing a second e2e run when it moves during the first.
- The queue is slower for e2e-relevant entries; non-relevant entries are unchanged.
- The lab result no longer counts at merge time; it is confidence only.
