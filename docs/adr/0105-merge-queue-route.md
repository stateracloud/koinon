# 0105. One route to main: the merge queue, and drafts run nothing

- Status: Accepted (procoduck, 2026-11-09; built in PR A #479 and PR B #487; amended 2026-11-11, below)
- Date: 2026-11-09
- Area: engineering-process
- Tags: ci, e2e, github, claims
- Supersedes in part: [0102](0102-merge-queue-runs-the-kind-e2e.md) decisions 2, 3 and 6 (evidence split, the admin
  flow, the bootstrap shim); 0102 decisions 1, 4 and 5 stay
- Amends: [0094](0094-work-claims-on-the-public-repository.md) decision 4 ("The way in is unchanged", "Admin merges
  still merge directly", the updater retirement condition); [0025](0025-testing-strategy.md) (the lab as e2e evidence; "Amended by 0105");
  [0026](0026-home-lab-and-license.md) and [0090](0090-scripts-require-bash-5.md) (the lab statuses as merge evidence);
  [0047](0047-repository-scaffold.md) (the pull request e2e rule); [0082](0082-work-claims-and-ready-queue.md) (decisions 8 and 9: the updater, the sensitive-path review)
- Relates to: [0103](0103-recorded-review-verdict-check.md) (the `review` check), [0104](0104-work-claims-reduced-to-the-lock.md)
  (claims as a plain lock), [0046](0046-adr-conventions.md)
- Lands in two pull requests: PR A (decisions 7 to 10, no change of route; merged as #479) and PR B (decisions 1
  to 6, this ADR and the code). PR B merges only together with or after the owner's ruleset step below
- Issues: closes #452 (the bootstrap shims); refs #399 (the `e2e-label.yml` item of #107 is moot) 

## Context

The owner decided on 2026-11-09 to implement the recommendations of the evaluation "Koinon claims and CI:
evaluation", and on the same day: pick what makes development faster; procoduck is the only contributor.
The data of the evaluation:

- **83% of merges used the org-admin bypass.** The main cause is the `required_reviewers` rule on sensitive
  paths: one human cannot approve his own pull request. ADR 0103 (accepted, with the `review` check live and advisory)
  replaces that rule with a required `review` check, except for `.github/**` (decision 6), once the owner's
  ruleset step below is done.
- **The merge queue (ADR 0094, live 2026-11-09) carried 4 of 30 merges.** Every bypass merge skips the
  queue and so the queue-commit e2e that ADR 0102 made the merge evidence.
- **55 of 62 red pull request runs were draft runs, red by design** (`go` and `web` report "draft: not
  tested", old issue 593). A red that means nothing trains people and agents to ignore red.
- **Three e2e proofs exist on a pull request head** (the PR's own `quickstart`, a dispatched `quickstart`,
  the lab's `e2e-lab` status), plus the queue's run. ADR 0102 already calls the queue's run the merge
  evidence, but `e2e result` on `pull_request`, `pr-merge.sh --require e2e`, `ship.sh --auto-merge`,
  `e2e-gate.sh`, `e2e-rerun-result.sh`, `e2e-label.yml` and rule 4 of `pr-verdict.sh` still gate on the
  others. Each proof has its own code, tests and trust limit.
- **The updater and the sync points remain.** `updater.yml` (with `hack/pr-updater.sh` after ADR 0104) and
  the AGENTS.md sync points exist for strict "up to date" checks, which the queue replaced
  (`strict_required_status_checks_policy: false` on the live ruleset, read 2026-11-09).

Hosted runner cost, the open check of the evaluation: "The following usage remains free independently and
does not reduce the included compute usage: Standard GitHub-hosted runners in public repositories.
Self-hosted runners." and "Larger runners are billed for public repositories."
(docs.github.com/en/billing/concepts/product-billing/github-actions, read 2026-11-09). What stays limited is
concurrency: "Standard GitHub-hosted runner | Team | 60" total concurrent jobs, and "Each job in a workflow
can run for up to 6 hours of execution time" (docs.github.com/en/actions/reference/limits). The organization
is on the Team plan (`gh api orgs/stateracloud --jq .plan.name` answers `team`); the 60 jobs are shared by
every repository of the organization. Koinon uses only standard runners, so the queue e2e costs no money,
only queue latency and runner slots.

## Decision

### PR B: the route

1. **The merge queue is the route to main.** Every pull request merges through the queue
   (`hack/pr-merge.sh <PR>`, which runs `gh pr merge --merge --auto`), except the `.github/**` case of
   decision 6. The queue builds main's tip plus the pull request, runs every required check on that commit
   (ADR 0094) and the kind e2e when the entry is e2e-relevant (ADR 0102 decision 1), and merges only that
   tested commit.

2. **One e2e proof: the queue's `quickstart`.** On `merge_group`, `e2e result` is green when `quickstart`
   passed, or when `decide` answered `needed=false` and `quickstart` was skipped; else red. Removed, with
   their code:
   - the PR head lookup (`hack/e2e-gate.sh`), the dispatched-run path and its re-run helper
     (`hack/e2e-rerun-result.sh`), and `decide`'s `required` output;
   - `e2e-lab` as evidence. The lab stays for the dev loop and real-model runs; `hack/e2e-remote.sh` may still
     post `e2e-lab` as information. `e2e-lab` is never a name in `hack/ci-required.txt` or in the ruleset, and
     `hack/ci-required-test.sh` (also with `--ruleset`) fails if it appears;
   - `--require e2e` in `pr-merge.sh` and `pr-wait.sh`, the e2e refusal in `ship.sh --auto-merge`, and rule 4
     ("any green e2e row") of `hack/lib/pr-verdict.sh`;
   - `e2e-label.yml` and `hack/e2e-label.sh`: the label is read at queue time (decision 3).

   On `pull_request` the `quickstart` stays an **advisory signal**: it runs when the change is e2e-relevant
   or carries the `e2e` label, so the author and the reviewer see a red before they spend a review and a
   queue slot. For a fork pull request it runs only with the `e2e` label (a maintainer adds it): the flavor
   images on GHCR stay private until v0.3, so a fork's run cannot pull them and would build them cold.
   `e2e result` on `pull_request` passes without reading it (the required context must exist for the entry to
   be enqueued); `pr-wait.sh` prints a red PR `quickstart` as `advisory quickstart failed`. Revisit: if a
   queue `quickstart` waits more than 5 minutes for a runner in a week, the PR run becomes label-only.

3. **The `e2e` label forces the queue e2e.** On `merge_group`, `decide` reads the pull request number from the
   queue ref (`gh-readonly-queue/main/pr-<N>-<sha>`, as `claim` does) and answers `needed=true` when pull
   request `<N>` carries `e2e`. `decide` keeps `pull-requests: read` for this read; a failed read, or a ref it
   cannot parse, counts as "needed" (fail closed). The label is read when the queue builds the entry: a label
   added after the entry is built does not rebuild it. To get the e2e then, remove the pull request from the
   queue (PR page) and enqueue it again.

4. **Removed with the queue as the route:** the `--require e2e` paths above, and the AGENTS.md text that names
   the three proofs. (The updater and sync points go in PR A, decision 10.)

5. **Urgent and emergency merges.**
   - **Urgent (normal route):** `hack/pr-merge.sh <PR> --jump` enqueues at the front (GraphQL
     `enqueuePullRequest(jump: true)`; schema: "Add the pull request to the front of the queue"). The change is
     still tested on main's tip. GitHub warns that heavy use slows the queue: for a red main or a security fix.
   - **Emergency (break glass):** `hack/pr-merge.sh <PR> --break-glass "<reason>" --approved-at <time>`, for a
     queue that cannot merge anything (GitHub's queue or hosted runners down, or every entry fails for a cause
     outside the pull request and the fix cannot pass the queue). Never for "slow" or "flaky". Only the owner
     approves it; an agent asks. It runs the ADR 0103 review gate on the head, merges with `gh pr merge --admin
     --merge`, and opens a `break-glass` issue (sha, reason, approver, skipped checks). `pr-wait.sh --checks` and
     the required-context read still run, but with the hosted runners down (a main reason for break glass) they
     cannot pass, so a failure there is a **warning recorded** in the PR comment and the issue, not a stop; a
     closed or draft PR still stops it.
   - **`--admin` stays for `.github/**` only** (decision 6): a pull request that changes at least one `.github/**` path.
     `pr-merge.sh --admin` refuses (exit 9) one with none, and names the queue and `--break-glass`. It skips the
     queue's quickstart, so it stays fail-closed for the e2e: when the base's `hack/needs-e2e.sh` says the change
     needs the e2e (or cannot say), the head needs a green `quickstart` of its own, else exit 10.
   - **The alarm.** The real alarm is the overnight local report: it reads the rule-suites API (`GET
     /repos/{owner}/{repo}/rulesets/rule-suites?ref=refs/heads/main&rule_suite_result=bypass`), which records
     every bypass of the `main` ruleset whatever tool made it, and lists each one: a `.github/**` admin merge as
     expected, anything else as a break-glass event to explain. A fast in-repo signal backs it up: a
     `route-guard` workflow on push to main checks whether a successful `merge_group` `ci` run exists for the
     pushed tree; when none does, it opens (or comments on) an issue labelled `break-glass` or, for a
     `.github/**` pull request. A queue merge is recognised by the REQUIRED jobs (`hack/ci-required.txt`) passing on
     a `merge_group` run of that tree, not by the run's conclusion (the queue merges on the required contexts
     while an advisory job may be red or the run still running (when this was written `flavors` was advisory; since #572 it is required, see the amendment below)); an in-progress run is polled for up
     to about 10 minutes. The expected `--admin` merge of a `.github/**` pull request whose required checks and
     `review` were green on its head opens no issue; a direct push, or anything else, opens a `break-glass`
     issue. The job stays green: it never turns main red, because a red job
     on main is noise for every later reader and cannot undo the merge.
   - **The ruleset keeps one bypass actor** (OrganizationAdmin, `bypass_mode: pull_request`, no direct push):
     the `.github/**` route needs it, and without it a broken queue could not merge its own fix. Target: break
     glass at most once a month; more is a retro item and an issue about the cause.

6. **`.github/**` pull requests merge with `--admin` (interim).** ADR 0103 keeps the CODEOWNERS approval for
   `.github/**`, because a pull request's own workflow files run on `pull_request` and `merge_group` and could
   fake `review`. With one human that approval needs the bypass (7 of the last 40 merged pull requests touched
   `.github/`, read 2026-11-09). The owner chose speed: no machine account; these pull requests keep
   `pr-merge.sh --admin`, which still runs `pr-wait.sh --checks` and the review gate on the head, and skips the
   queue (an e2e-relevant one needs a green `quickstart` on its head, exit 10 otherwise). The batched push e2e on
   main and the nightly are the backstop.

   **Exit condition: a pinned required workflow.** Organization rulesets are on the Team plan since
   2025-06-16 (github.blog/changelog/2025-06-16-organization-rulesets-now-available-for-github-team-plans/).
   An organization ruleset rule "require workflows to pass" runs `review.yml` from main (pinned to a ref or
   sha), not from the pull request, so a pull request cannot fake `review`. A later step, with its own
   scratch-repository probe, checks: the rule runs main's file on `pull_request` and on `merge_group`; a pull
   request that edits `review.yml` or adds a second job named `review` cannot pass; the queue waits for it.
   When the probe passes, the owner adds the rule, drops the `.github/**` `required_reviewers` entry, and
   `--admin` is removed from `pr-merge.sh` (it then exits 64 and names `--break-glass`).

### PR A: no change of route (landed as #479; recorded here so the route has one ADR)

7. **Drafts run nothing (Rec 3).** On a draft (`github.event.pull_request.draft`), every heavy `ci` job is
   skipped and `go` and `web` report skipped (green), not red. A skipped job reports "Success" and "will not
   prevent a pull request from merging, even if it is a required check" (GitHub docs, "Using conditions to
   control job execution"). That is safe: a draft cannot be enqueued or merged (`pr-merge.sh` and
   `pr-wait.sh --checks` refuse it, exit 5); `ready_for_review` starts the full run, which is the newest run
   of the workflow and carries the verdict (`pr-verdict.sh` rule 1); the queue re-runs every required check.
   Decisions 8 and 9 remove the two places that trusted a PR head's green. So the `ci (draft)` filter of
   `pr-wait.sh` and `pr-verdict.sh` (`display_title`, #218) goes, and `run-name` loses its draft form. `dco`,
   `gitleaks` and `claim` still run on drafts.

   Why not drop the "open a draft" rule instead: the draft sets the board to "In Progress" (the built-in
   "Pull request linked to issue" workflow, ADR 0104) and shows a lane's files to the overlap check; ADR 0104
   already removed the 30-minute timing; and a rule for agents does not stop a human or a fork from opening a
   draft, so the red-draft code would have to stay.

8. **The push to main trusts the queue build.** `hack/ci-skip-heavy.sh decide_push` skips the heavy jobs only
   when a successful `merge_group` `ci` run exists for a commit with the pushed commit's tree. The PR-head
   comparison goes (a draft-era green head must never count as tested). An admin or break-glass merge has no
   such run, so its push runs everything.

9. **The release gate reads the queue build.** `hack/release-gate.sh` reads each required context, in order,
   from the tagged commit itself, then from a successful `merge_group` run on the same tree. Only the
   pull-request-only contexts (`dco`, `claim`, `review`, and the dependency-age steps of `quick checks`) may
   then come from the merged pull request's head; `go`, `web` and every other `ci` job never do.
   - An rc on an admin or break-glass commit (no `merge_group` run): the `ci` contexts come from that commit's
     own push run, which ran everything (decision 8); `dco`, `claim` and `review` from the merged pull
     request's head; `quickstart` from the batched push run when the change was e2e-relevant, else the gate
     fails and says to dispatch e2e-kind on the commit (as today). A missing or red context fails the gate.
   - The `quickstart` rule of ADR 0102 decision 5 is unchanged.

10. **The updater and the sync points go.** `updater.yml`, `hack/pr-updater.sh` (from ADR 0104), the
    `needs-update` label, the `claims-bot` Environment's App secrets (nothing else reads them), and the
    AGENTS.md sync points. A branch is merged with `origin/main` only when GitHub reports a conflict
    (`pr-wait.sh` exit 4) or the queue drops the entry for one (exit 8). "Merge `main`, never rebase" stays.
    ADR 0094's exit condition ("the first week of green queue merges with strict off") is replaced: the
    updater has been a no-op since the queue went live (it returns at once when the ruleset has a
    `merge_queue` rule), so there is nothing to wait for. This holds whatever the route: an admin merge needs
    a mergeable branch, not a synced one.

## Owner step: the ruleset

Not code, and never from a pull request. The owner's session changes ruleset 26610739 with `gh api`, after a
backup (`gh api repos/stateracloud/koinon/rulesets/26610739 > ~/.config/koinon/ruleset-main-$(date +%F).json`)
and a diff. **PR B merges only together with or after this step.** Without it, `pr-merge.sh --admin` (which
now refuses a pull request with no `.github/` path) leaves a CODEOWNERS pull request stuck `BLOCKED` in the
queue: the `required_reviewers` rule still asks for an approval nobody can give.

It is the step of ADR 0103, done as one `PUT` so no window has both rules or neither. Live state read
2026-11-10; the diff is against the `rules` array (everything else stays):

```diff
 pull_request.parameters.required_reviewers[0].file_patterns
-  [".github/**", "AGENTS.md", "docs/design.md", "docs/dev/releasing.md", "docs/adr/**", "charts/**",
-   "internal/db/migrations/**", "internal/gateway/**", "internal/agentd/**", "internal/reconciler/**",
-   "internal/ledger/**", "internal/auth/**", "internal/secretstore/**", "internal/tracing/scrub/**",
-   "hack/claim.sh"]
+  [".github/**"]
 required_status_checks.parameters.required_status_checks
   ... {"context": "quick checks"}
+  {"context": "review", "integration_id": 15368}
```

- The second human's approval requirement stays only for `.github/**` (same Team reviewer, 1 approval); every
  other sensitive path moves to the `review` check (ADR 0103) and the queue.
- `integration_id: 15368` (the GitHub Actions app) is the pin of ADR 0103: a commit status `review`=success
  posted by a user does not count.
- Required checks apply on `merge_group` with no further ruleset field; `hack/ci-merge-group-test.sh` proves
  that every required name has a job that reports there.
- `merge_queue` (MERGE, ALLGREEN, group size 1, 60 minutes), `strict_required_status_checks_policy: false` and the
  one bypass actor (OrganizationAdmin, `bypass_mode: pull_request`) stay. Never `always`.
- The full `rules` array after the change (the live ruleset 26610739 read with `gh api
  repos/stateracloud/koinon/rulesets/26610739`, read only; the whole PUT body, with `name`, `target`,
  `enforcement`, `conditions` and `bypass_actors` unchanged, is in `/tmp/claude-502/kw/ruleset-26610739-after.json`,
  the live one in `ruleset-26610739-before.json`, for the owner's `gh api -X PUT`):

```json
[
  {
    "type": "deletion"
  },
  {
    "type": "non_fast_forward"
  },
  {
    "type": "pull_request",
    "parameters": {
      "required_approving_review_count": 0,
      "dismiss_stale_reviews_on_push": true,
      "required_reviewers": [
        {
          "minimum_approvals": 1,
          "file_patterns": [
            ".github/**"
          ],
          "reviewer": {
            "id": 20443899,
            "type": "Team"
          }
        }
      ],
      "require_code_owner_review": false,
      "dismissal_restriction": {
        "enabled": false,
        "allowed_actors": []
      },
      "require_last_push_approval": false,
      "required_review_thread_resolution": true,
      "require_extra_approval_for_unattributed_changes": true,
      "allowed_merge_methods": [
        "merge"
      ]
    }
  },
  {
    "type": "required_status_checks",
    "parameters": {
      "strict_required_status_checks_policy": false,
      "do_not_enforce_on_create": false,
      "required_status_checks": [
        {
          "context": "dco"
        },
        {
          "context": "go"
        },
        {
          "context": "web"
        },
        {
          "context": "generated code"
        },
        {
          "context": "gitleaks"
        },
        {
          "context": "chart"
        },
        {
          "context": "escape (admission)"
        },
        {
          "context": "claim"
        },
        {
          "context": "e2e result"
        },
        {
          "context": "scripts"
        },
        {
          "context": "lint-shell"
        },
        {
          "context": "chart smoke"
        },
        {
          "context": "docs"
        },
        {
          "context": "quick checks"
        },
        {
          "context": "review",
          "integration_id": 15368
        }
      ]
    }
  },
  {
    "type": "merge_queue",
    "parameters": {
      "merge_method": "MERGE",
      "max_entries_to_build": 5,
      "min_entries_to_merge": 1,
      "max_entries_to_merge": 1,
      "min_entries_to_merge_wait_minutes": 0,
      "grouping_strategy": "ALLGREEN",
      "check_response_timeout_minutes": 60
    }
  }
]
```

- Before the PUT every open pull request needs a `review` run on its head (ADR 0103). After it, move the
  `review` line of `hack/ci-required.txt` per ADR 0103, and watch one real queue entry report `review` on
  `merge_group`.
- Label `break-glass` (owner step; `route-guard` creates an issue without a label if one is
  missing and says so).

## Open: probe P3

The scratch-repository probe of two facts is **not done**: (a) `enqueuePullRequest(jump: true)` works for a user
with write access; (b) the sha that lands on main after a group-size-1 queue merge equals
`merge_group.head_sha`. Until (b) is known, `ci-skip-heavy.sh decide_push`, `release-gate.sh` and
`route-guard.sh` keep comparing **trees**, not shas (a queue merge with a merge commit may produce a different
commit with the same tree). If (b) holds, a later change may compare shas. `--jump` is documented with the
GraphQL schema text only; if (a) fails, `--jump` is removed in a follow-up.

## Exit conditions of what this removes or keeps

- PR-head e2e evidence, lab evidence, updater, sync points, red drafts: come back only with a new ADR that
  removes the queue.
- The ADR 0102 bootstrap shim (`hack/e2e-queue-head.sh`, #452): its condition is met (main's `decide` is the
  post-#444 one; no entry on a pre-#444 base remains). Close #452 with PR B.
- `--admin` for `.github/**`: ends with the pinned required workflow (decision 6).
- The advisory PR `quickstart`: label-only when the runner-slot condition of decision 2 holds.

## Prior art

Quick pass (about 15 minutes), sources read 2026-11-09 unless noted.

- **GitHub merge queue docs** (managing-a-merge-queue): workflows "must use the `merge_group` event" or the
  required check is never reported in the queue; a failing check removes the entry and the queue regroups the
  rest; a pull request must pass the required checks before it is added; jumping to the top exists and "can
  slow down the velocity of merges". Nothing about drafts or bypass.
- **Kubernetes Prow/Tide** (from ADR 0102's pass, not re-read): Tide is the only merge route; it tests the
  merged result and batches slow e2e suites. The `wip` plugin labels a draft `do-not-merge/work-in-progress`,
  so "draft" means "not mergeable", not "red". Admin merges by hand are rare.
- **rust-lang bors** (from ADR 0102's pass): every merge goes through bors on main's tip; urgency is a priority
  (`p=`), and `treeclosed` stops the queue. Same split: jump for urgent, break glass only for a broken queue.
- **We differ:** group size 1 (a red e2e costs one entry), and the alarm reads GitHub's own bypass record
  (rule suites) instead of trusting the tools that merge.

## Consequences

- One merge route for everything outside `.github/**`, and one e2e proof. About 11 scripts and workflows go.
- Each e2e-relevant merge waits for a queue `quickstart` (12 to 15 minutes). A flaky queue e2e removes the
  entry; the author enqueues it again. Flakes now cost a merge each, so the flake rate is a tracked number.
- `.github/**` merges skip the queue until the required-workflow probe passes; the overnight report counts them.
- Agents never sync a branch by hand except for a conflict.
- Draft runs stop being red; a red run on a ready pull request means something again.
- The lab is no longer on the merge path, so a lab outage blocks nothing.

## Amendment 2026-11-11: the review verdict is advisory

ADR 0103 now treats the recorded review verdict as advice (see its 2026-11-11 amendment), and `review` is off the
ruleset. This changes two decisions above. Decision 5 (break glass): `pr-merge.sh --break-glass` no longer runs
the ADR 0103 review gate on the head. Decision 6 (`--admin` for `.github/**`): `pr-merge.sh --admin` still runs
`pr-wait.sh --checks` but no longer runs the review gate. The route guard no longer reads `review`: the expected
`--admin` merge needs only the other required checks green on the head. The rest of both decisions stands.

## Amendment 2026-11-11 (#546): the `review` check is gone

ADR 0103 is deprecated: `review.yml`, `hack/review-gate.sh` and `hack/review-verdict.sh` are removed. Read every
`review` context and review-gate step above as historical; the `main` ruleset never requires it.

## Amendment 2026-11-11 (#572): the pull request e2e is label-only, and `flavors` is a queue check

The CI review of 2026-11-11 (72 runs, 20 merged pull requests) measured 31 minutes from the last push to merge,
and 147 runner minutes for one merged pull request. Two decisions above repeated work the queue does anyway.

1. **The PR `quickstart` runs only with the `e2e` label** (decision 2, and the "Revisit" clause there, which this
   amendment takes up). On `pull_request`, `decide` answers `needed=false` unless the pull request carries the
   `e2e` label, a fork pull request included. No path rule starts it: the queue applies the full rule
   (`hack/needs-e2e.sh`) to every entry and is the only e2e proof. The label stays for a change where a red
   before review is worth the runner time. `e2e result` on `pull_request` still passes without reading the
   quickstart, so the required context exists on every pull request; `merge_group`, `push`, the nightly run, rc
   tags and dispatch decide as before.
2. **`flavors` is required and does not run on pull requests.** Its job is skipped on `pull_request` (a job-level
   skip reports success, so the name exists on every pull request, push and merge group build), and runs on the
   queue commit and on pushes to main when the flavor inputs changed. A red `flavors` in the queue now ejects the
   entry; before, it was advisory and `flavor-publish` shipped an image the queue had not proven unless
   `hack/ci-skip-heavy.sh` noticed. `hack/ci-required.txt` lists `flavors` as required (it closes #128). The owner
   adds `flavors` to the required checks of ruleset 26610739 at merge time of the pull request that changes
   `ci.yml`; until then the name is advisory in the ruleset and the file is ahead of it
   (`hack/ci-required-test.sh --ruleset` shows the difference).

Also in the same change (no decision of this ADR, recorded for the route): `lint-shell` and `scripts` are
path-gated and off on drafts, `go-tidy` and `go-lint` run only when Go files, `go.mod`, `go.sum` or the lint
config change, `gitleaks` is not run again on a push to main that the queue made (`hack/secrets-skip.sh`: the
queue run of the same workflow scanned that exact commit), and the OpenSSF Scorecard runs weekly, not on every push.

Gap and its closure: `flavors` reports a green skip on every pull request head, and `--admin` skips the queue, so a
`.github/**` pull request that also changes flavor inputs could merge with no `flavors` run. `ci.yml` has no
`workflow_dispatch` trigger that runs `flavors` for a branch, and none is added here. `hack/pr-merge.sh --admin`
therefore asks the base's `hack/ci-changes.sh` with `CI_CHANGES_IGNORE_PLUMBING=1`, which drops the `everything ci`
rule of `hack/paths.rules` (`ci.yml`, `ci-changes.sh`, `ci-skip-heavy.sh`, `paths.rules`, `paths-lib.sh`) and decides
on the real flavor inputs only: the `job flavors` rule, the suite's build graph, `Makefile`, `go.mod` and `go.sum`.
(A first version of the guard used the full answer and so refused every `ci.yml` change, which can only go by
`--admin`: a deadlock.) On the `--admin` route a CI-plumbing-only change is not held for `flavors`: the push run on
main runs it after the merge, because `ci-skip-heavy.sh` skips a push only when a successful `merge_group` run tested
the same tree, and an `--admin` merge has none, so the `changes` job answers every job `true` and the `flavors` job
(`if: github.event_name != 'pull_request' && needs.changes.outputs.flavors == 'true'`) runs. A break there is fixed
forward. A pull request that mixes real flavor inputs with `.github` paths is refused with exit 10 and must be split:
the flavor change goes through the merge queue, the `.github` change through `--admin`. The guard reads the rule from
the base, as the e2e guard does, so a pull request cannot loosen its own check.
Until this change is on main, the base copy does not know the flag, so the guard of this branch refuses each pull request that changes `ci.yml`;
the pull request that lands this change is merged with `hack/pr-merge.sh` from a main checkout.
