<!-- SPDX-License-Identifier: Apache-2.0 -->

# CI rules

- Status: Accepted, with [ADR 0111](../adr/0111-ci-requirements.md) (the reasons; the ADR wins if the two disagree)
- Owner: the maintainer
- Last reviewed: 2026-11-11

## Use this guide when / do not use it when

Use it when you add, change, move or remove a CI check: a job in `.github/workflows/`, a script that a
job runs, a test stage, a path rule, or the list of required checks.

Do not use it when:

- You work in another repository. The docs site (`stateracloud/koinon-docs`) has its own CI.
- Another CI system runs the code. These rules name GitHub Actions, the GitHub merge queue and rulesets.
- You work on the lab and its scripts. The lab is outside the repository and outside public CI.
- You cut a release. That is `docs/dev/releasing.md`. Rules R8 and V1 here only say what a release needs.
- You want to know how a job works today. That is `docs/dev/ci.md`. This guide holds the rules, that page
  holds the description. If they disagree on a rule, ADR 0111 wins, then this guide. Fix the other page.
- A premise below is false. Stop and ask (see "Stop and ask").

### Valid while

| Premise | Revisit when |
|---|---|
| The repository is public and CI runs on GitHub-hosted free runners only (ADR 0047) | Paid or larger runners are added, or the repository goes private |
| The GitHub merge queue is the only merge route (ADR 0105) | Another merge route is added |
| One human maintainer (ADR 0104) | A second maintainer joins |
| Pre-v0.3: no schema or upgrade promise (ADR 0085, 0099) | v0.3 ships: turn on every "from v0.3" rule |
| The fake model server is the only model in CI (L2) | The key-in-CI question in #580 is decided |

## How to read a rule

MUST, MUST NOT, SHOULD and MAY in capitals are requirement words (RFC 2119, RFC 8174). Lower case has no
special force. A MUST names the check that confirms it. A rule that nobody can check is a SHOULD.

Each rule has these fields:

- **Check**: a script, a CI job, or "review" (a person or agent reads the diff).
- **Source**: where the pattern comes from, or "Koinon decision" and the ADR.
- **Scope**: "general pattern" (other projects can use it) or "Koinon only".
- **In force**: "now", "from v0.3" (planned in #580, no check yet) or "with #579 part 2" (the check arrives
  with the next part of that issue). A MUST rule marked "from v0.3" has no check until #580 delivers it and does
  not bind before v0.3.
- **Not when**: one line, only where it helps.

Run `hack/ci-rules-check.sh` to check the rules that a text scan can decide. It prints one line per rule:
`rule=<ID> verdict=pass|fail|report`. A `report` never fails.

## Rules

### Stages (S)

**S1. MUST** run a check at the cheapest stage that can prove its promise, and one time per change (L3).
Why: the same proof twice costs minutes and queue time. GitHub has no "already tested, skip" feature, so the
rule is carried out by trigger design.
Check: review of the stage table below; `hack/paths-test.sh` and `hack/ci-skip-heavy-test.sh` test the triggers.
Source: [Google, Software Engineering at Google, ch. 23](https://abseil.io/resources/swe-book/html/ch23.html) (presubmit
runs fast, reliable tests; slow ones run after submit); [Rust compiler guide, CI](https://rustc-dev-guide.rust-lang.org/tests/ci.html)
(the full suite runs on the merge candidate before main moves). Scope: general pattern. In force: now.

**S2. MUST** treat the merge queue build as the gate for the cluster e2e. A pull request runs the kind e2e
only when it has the `e2e` label (ADR 0105 amendment, #572).
Why: the queue tests main plus the pull request. The pull request's own run is not merge evidence (ADR 0105).
Check: `hack/e2e-decide-test.sh`, `hack/ci-skip-heavy-test.sh`.
Source: Koinon decision, ADR 0105 and its 2026-11-11 amendment. Scope: Koinon only. In force: now.

**S3. SHOULD** make a check merge-blocking only after it ran stable at a later stage (nightly or advisory)
and fits the time budget (L1).
Why: a flaky or slow blocking check stops every pull request.
Check: review.
Source: [Kubernetes sig-testing, testing strategy](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/testing-strategy.md)
(a blocking presubmit is also release-blocking, under one hour, preferably under 30 minutes). Koinon's
budget is L1. Scope: general pattern. In force: now.

**S4. MUST** run slow, destructive and fault-injection proofs (restore drill, upgrade, chaos) on a schedule
or at release, not on each pull request. A red scheduled run MUST open or update one issue.
Why: they are too slow for L1, and a red nightly nobody reads proves nothing.
Check: review for the stage; `hack/e2e-bisect-test.sh` (the nightly and main e2e) and `hack/drill-issue-test.sh`
(the drill; the script arrives with #572) for the issue. The nightly `govulncheck` run notifies the maintainer through the failed
run only; it opens no issue yet.
Source: [Firecracker tests README](https://github.com/firecracker-microvm/firecracker/blob/main/tests/README.md)
(three lanes: required, optional, periodic). The upgrade test is "from v0.3". Scope: general pattern. In force: now.

**S5. MUST** write what public CI cannot prove next to the test, with the place that proves it. The VM tier
(Kata) is proven on the lab only.
Why: GitHub says nested virtualization on hosted runners is not officially supported, so a gate must not
depend on it ([GitHub-hosted runners](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners)).
Check: review. Source: Firecracker README (as S4). Scope: general pattern. In force: now.

### Stage table: a new check, which stage

Put a new check in the earliest row that fits. "yes" runs and blocks, "adv" runs and never blocks, "-" does not run.
A cell with "(with #572)" shows the decided state of the parts of the CI speed work that are not yet merged (part 3).

| Kind of check | Example here | Pull request | Merge queue | Push to main | Nightly | Release | Lab | Time limit | Blocks merge |
|---|---|---|---|---|---|---|---|---|---|
| Static and lint | `quick checks`, `lint-shell`, `scripts`, `go-lint`, `web-lint` | yes | yes | yes, unless the queue tested the tree | - | - | - | minutes | yes |
| Unit | `go-test` (no container) | yes | yes | as above | - | - | - | minutes | yes (`go`) |
| Postgres integration | `go-test`, testcontainers | yes | yes | as above | - | - | - | minutes | yes (`go`) |
| Envtest admission | `escape (admission)` | yes | yes | as above | - | - | - | minutes | yes |
| Chart render and smoke | `chart`, `chart smoke` | yes | yes | as above | - | - | - | up to 35 min job limit | yes |
| Web unit | `web-test` | yes | yes | as above | - | - | - | minutes | yes (`web`) |
| Web e2e (mocked) | `web-e2e` | yes | yes | as above | - | - | - | minutes | yes (`web`) |
| Kind e2e quickstart | `quickstart`, gate `e2e result` | label `e2e` only | yes, when relevant | batched | yes | tag needs a green one | optional | about 15 min | yes, in the queue |
| Flavors image build | `flavors` | - | yes | publish | - | - | - | up to 60 min job limit | yes in the queue |
| Secret scan | `gitleaks` | yes | yes | yes | - | - | - | minutes | yes |
| CodeQL | `codeql` workflow (with #572) | only if Go or web code changed | - | weekly | - | - | - | minutes | adv |
| govulncheck | `govulncheck.yml` | with #579 part 2, if `go.mod` or `go.sum` changed | - | - | yes | - | - | minutes | decided in part 2 |
| Restore drill | `drill` job (ADR 0109) | - | - | - | yes | rc tag | final tag | long | adv |
| Upgrade from last release | planned, #83, #519 | - | - | - | from v0.3 | from v0.3 | - | long | from v0.3 |
| VM tier (Kata) | lab only | - | - | - | - | - | yes | n/a | no |
| Real model | `docs/dev/real-model-tests.md` | outside CI | outside CI | outside CI | outside CI | outside CI | by hand | n/a | no |

### Required checks (Q)

**Q1. MUST** have a required check report on every pull request, push and merge-group build. Do not put
`paths:` or `paths-ignore:` on a workflow that produces a required check. Gate at job level with `if:`.
Why: a job skipped by `if:` reports success. A workflow skipped by a path filter leaves its checks pending,
and the merge queue waits for a required check until its timeout and then removes the entry.
Check: `hack/ci-rules-check.sh` (rule Q1).
Source: GitHub Docs, [skipping workflow runs](https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-workflow-runs/skipping-workflow-runs)
and [managing a merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue).
Scope: general pattern. In force: now. Not when: a workflow that is advisory only (CodeQL, with #572).

**Q2. MUST** list the `merge_group` trigger (`types: [checks_requested]`) in every workflow that produces
a required check.
Why: without it the queue never gets the check. Check: `hack/ci-required-test.sh`, `hack/ci-merge-group-test.sh`.
Source: GitHub Docs, [events that trigger workflows](https://docs.github.com/en/actions/writing-workflows/choosing-when-your-workflow-runs/events-that-trigger-workflows).
Scope: general pattern. In force: now.

**Q3. MUST** list a new `ci.yml` job in `hack/ci-required.txt`, as required or advisory, in the same change.
The list equals the live ruleset.
Why: an unlisted job is a check nobody decided about. Check: `hack/ci-required-test.sh` (and `--ruleset` for the live diff).
Source: Koinon decision. Scope: Koinon only. In force: now.

**Q4. MUST** run an aggregator job with `if: always()` and have it fail on every needed job that is not
`success` or a named, allowed skip.
Why: a skipped needed job reads as success unless the aggregator looks.
Check: `hack/ci-go-gate-test.sh`, `hack/ci-web-gate-test.sh` (the `go` and `web` gates).
Source: [re-actors/alls-green](https://github.com/re-actors/alls-green) and
[Jake Wharton on fan-in checks](https://jakewharton.com/fan-in-to-a-single-required-github-action/).
Scope: general pattern. In force: now.

**Q5. SHOULD** bind a required check to its source app in the ruleset, where a forged status is a risk.
Why: GitHub accepts a status from any source unless the rule names one.
Check: review. A ruleset change needs the maintainer's yes (see "Stop and ask").
Source: GitHub Docs, [available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets).
Scope: general pattern. In force: now.

**Q6. MUST NOT** cancel a run in progress on a `merge_group` or a push to main. A literal
`cancel-in-progress: true` in a workflow with a required check is a failure.
Why: a cancelled required check reads as failed and drops the queue entry and the entries behind it.
Check: `hack/ci-rules-check.sh` (rule Q6).
Source: Koinon decision (no official text found). Scope: Koinon only. In force: now.

### Tests (T)

**T1. MUST NOT** retry a failed test automatically: no ginkgo flake attempts, no retry action, no Playwright
`retries` above 0.
Why: a retry turns a real intermittent defect into a green run, and the retry rate becomes the only number.
Check: `hack/ci-rules-check.sh` (rule T1). The Playwright retry (`web/playwright.config.ts`) is reported now and removed with #579 part 2,
when the report becomes a failure.
Source: [Kubernetes, flaky tests](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/flaky-tests.md)
(test jobs must not retry automatically); [DORA, test automation](https://dora.dev/capabilities/test-automation/)
(do not tolerate flaky tests). Maintainer decision 2026-11-11. Scope: general pattern. In force: now (Playwright: with #579 part 2).

**T2. MUST** give a flaky test one issue, take it out of the merge checks, and give it an expiry date. A waiver
past its date fails.
Why: a skipped test with no owner and no date stays skipped.
Check: `hack/ci-rules-check.sh` (rule T2). A waiver is the comment `flaky-waiver: #<issue> until <YYYY-MM-DD>`
next to the skip, in a `.go`, `.ts`, `.sh` or `.yml` file.
Source: Koinon decision (the expiry date has no primary source). Kubernetes quarantines flaky tests with an issue.
Scope: Koinon only. In force: now.

**T3. MUST** keep tests hermetic: no fixed port, path or database shared between processes; random ports; one
container per suite. A new suite joins the parallel list of `hack/go-test.sh` only when it shares none of these.
Why: many agents run tests at the same time (L4).
Check: `hack/go-test-test.sh` (the parallel list); review for the rest.
Source: [Bazel, hermeticity](https://bazel.build/basics/hermeticity); Testcontainers maps random ports
([Docker blog](https://www.docker.com/blog/testcontainers-best-practices/)). Scope: general pattern. In force: now.

**T4. MUST** declare what a test needs from the machine with `testenv.Require` (`internal/testing/testenv`).
A missing need skips on a laptop and fails in CI, where `KOINON_REQUIRE` is set.
Why: a silent skip in CI is a proof that did not run.
Check: `internal/testing/testenv` tests; `KOINON_REQUIRE` in the `go-test` job of `ci.yml`.
Source: Koinon decision (old issue 507). Scope: Koinon only. In force: now.

**T5. MUST** give every CI gate one local command (`hack/lane-gates.sh`, `hack/preflight.sh`). A new job adds its
gate there (L6). What cannot be equal: runner OS, CPU count, secrets, the clock.
Why: an agent must be able to check before it pushes.
Check: `hack/lane-gates-test.sh`, `hack/preflight-test.sh`, `hack/paths-test.sh`.
Source: Koinon decision, [docs/dev/ci.md](ci.md#the-local-gate-command). Scope: general pattern. In force: now.

**T6. MUST NOT** call a paid model in CI. Tests use the fake model server (L2).
Why: cost, vendor dependence, and a key in CI.
Check: `hack/ci-rules-check.sh` (rule W5: no secret besides `GITHUB_TOKEN`, so no key); review for the rest.
Source: Koinon decision. Scope: Koinon only. In force: now.

**T7. MUST** set `timeout-minutes` on every job.
Why: the default is 360 minutes (#572 review); a hung job holds a runner and a queue slot for six hours.
Check: `hack/runner-lint.sh` fails a `ci.yml` or `release.yml` job without one. `hack/ci-rules-check.sh` (rule T7)
defers to it and gives `pass` while that script has the rule.
Source: Koinon decision (#572). Scope: general pattern. In force: now.

**T8. MUST** pin the node image of a cluster job, and export and upload the logs with `if: always()`.
Why: an unpinned image changes the test; lost logs make a red run unreadable.
Check: review; `test/e2e/kind/up.sh` pins the image by digest.
Source: Koinon decision (`test/e2e/kind/up.sh` pins the node image by digest). Scope: Koinon only. In force: now.

**T9. SHOULD** give a security policy test a deny case and an allowed "canary" case per rule, and run the same
assertions inside and outside the sandbox. Use envtest. Never Kyverno, also not as a test tool.
Why: a policy test with only deny cases passes when the policy denies everything.
Check: review. Source: [Gatekeeper gator](https://open-policy-agent.github.io/gatekeeper/website/docs/v3.15.x/gator)
(offline policy test suites, the pattern for allowed and denied cases). Scope: general pattern. In force: now.

### Workflow security (W)

**W1. MUST** set a top-level `permissions:` in every workflow, read-only by default. Write scopes go on the job.
Check: `hack/ci-rules-check.sh` (rule W1).
Source: [GitHub, security hardening](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions);
OpenSSF Scorecard check Token-Permissions ([checks](https://github.com/ossf/scorecard/blob/main/docs/checks.md)).
Scope: general pattern. In force: now.

**W2. MUST** pin every third-party action by its full commit SHA, with the release tag in a comment.
Why: a tag can move to other code; a commit SHA cannot.
Check: `hack/ci-rules-check.sh` (rule W2); the age of the pinned release is `hack/dep-age-check.sh`.
Source: GitHub security hardening (as W1); Scorecard check Pinned-Dependencies. Scope: general pattern. In force: now.

**W3. MUST NOT** use `pull_request_target`, and no self-hosted runner in this public repository. `workflow_run`
MUST NOT run pull request code.
Check: `hack/runner-lint.sh` (the first two); review (`workflow_run`; no workflow uses it today).
Source: GitHub security hardening; Scorecard check Dangerous-Workflow. Scope: general pattern. In force: now.

**W4. MUST** pass untrusted values (titles, branch names, bodies) to `run:` only through an `env:` variable.
Check: `zizmor`, with #579 part 2. Until then review. Source: GitHub security hardening (script injection).
Scope: general pattern. In force: with #579 part 2.

**W5. MUST** keep every secret out of a job that runs pull request code from a fork. Today no workflow reads
a secret besides `GITHUB_TOKEN`.
Check: `hack/ci-rules-check.sh` (rule W5). Source: GitHub security hardening. Scope: general pattern. In force: now.

**W6. MUST** make the decision to skip a required check in the workflow file, not in a script that the change
under test can edit.
Why: workflow files are code-owner protected; scripts under `hack/` are not. A pull request that edits the deciding
script could make its own required check green with no run.
Koinon example: in `.github/workflows/secrets.yml` the `skip` step has `if: github.event_name == 'push'`, so
`pull_request` and `merge_group` always scan. Only the scan of a push to main can be skipped.
Check: `hack/secrets-skip-test.sh` for this instance; no general machine check.
Source: Koinon decision (#572). Scope: general pattern. In force: now.

### Data and compatibility (D)

**D1. MUST** never change a merged migration; keep the numbers contiguous; within a shipped minor line only expand.
Why: old and new binaries share the schema inside a line (ADR 0085, 0099).
Check: `hack/migrate-lint.sh origin/main`. Source: [Fowler, ParallelChange](https://martinfowler.com/bliki/ParallelChange.html).
Scope: general pattern. In force: now (the patch-line part turns on with `hack/migrate-patch-line`; it is `none` today).

**D2. MUST** regenerate generated code and diff it in CI. Regenerate golden files; do not edit them by hand.
Check: the `generated code` job; `hack/generate-test.sh`. Source: Koinon decision. Scope: general pattern. In force: now.

**D3. MUST** pass `make breaking` for every `.proto` change. It enforces the buf category `FILE` (`buf.yaml`),
except `FILE_SAME_GO_PACKAGE`.
Check: the `proto (buf breaking)` step of `quick checks`. Source: [buf, breaking rules](https://buf.build/docs/breaking/rules).
Scope: general pattern. In force: now.

**D4. MUST** extend the chart smoke test when a change alters how a binary starts (a flag, an env var, a mount,
a Secret key, an RBAC rule).
Check: `make test-chart-smoke` (job `chart smoke`). Source: Koinon decision (old issue 284). Scope: Koinon only. In force: now.

**D5. MUST** test an upgrade from the last release on a schedule. Check: planned, #83, #519. Source: Cilium runs its
upgrade e2e on a schedule, not on pull requests ([tests-e2e-upgrade.yaml](https://github.com/cilium/cilium/blob/main/.github/workflows/tests-e2e-upgrade.yaml)).
Scope: general pattern. In force: from v0.3.

### Supply chain (X)

**X1. MUST** use dependencies that are at least 3 days old (ADR 0079). pnpm's `minimumReleaseAge` is 4320
(set explicitly: the tool default is 1440); Dependabot `cooldown` is 3.
Why: a bad release is usually caught in days. The number 3 is a project choice; no study gives it.
Check: `hack/dep-age-check.sh` (job `quick checks`, pull requests only).
Source: Koinon decision, ADR 0079; [pnpm settings](https://pnpm.io/settings/dependency-resolution);
[Dependabot options](https://docs.github.com/en/code-security/reference/supply-chain-security/dependabot-options-reference).
Scope: general pattern. In force: now.

**X2. MUST** scan every commit of a pull request with gitleaks. Fake secrets look fake from the first commit.
Check: the `gitleaks` job (`secrets.yml`). Source: Koinon decision. Scope: general pattern. In force: now.

**X3. MUST** run `govulncheck` nightly now, and on pull requests that change `go.mod` or `go.sum`.
Check: `govulncheck.yml` (nightly); the pull request job arrives with #579 part 2. Source: Koinon decision, ADR 0079
(maintainer decision 2026-11-11). Scope: general pattern. In force: now (nightly); with #579 part 2 (pull requests).

**X4. MUST** check the SPDX header on every source file, check dependency licences, and scan the published
images for vulnerabilities. Check: planned, #580. Source: [REUSE specification](https://reuse.software/spec-3.3/).
Scope: general pattern. In force: from v0.3.

### Release (V)

**V1. MUST** cut an rc or release tag only from a commit whose `quickstart` job succeeded.
Check: `hack/release-gate.sh` (`hack/release-gate-test.sh`). Source: Koinon decision, ADR 0105. Scope: Koinon only. In force: now.

**V2. MUST** pull, verify (cosign keyless, `gh attestation verify`) and install what a release published.
Check: planned, #580. Source: Koinon design; no published example was found. Scope: Koinon only. In force: from v0.3.

### Measure (M)

**M1. SHOULD** track push-to-merged median, runner minutes per merged pull request and cache size in #572. A change
that adds more than about 1 minute to the pull request or queue path says so in its description.
Why: L1 is a number; without a measure it is a wish. Check: review.
Source: Fowler's [ten minute build](https://martinfowler.com/articles/continuousIntegration.html) is the classic bound;
the 20 minutes is the maintainer's goal. Scope: Koinon only. In force: now.

**M2. MUST** save caches from main (or a schedule) and restore them elsewhere. The cache is 10 GB with
least-recently-used eviction, and a run restores only its own branch's or the default branch's entries.
Why: a pull request cannot read a sibling's cache, and a cache saved per pull request evicts main's.
Check: `hack/ci-cache-test.sh`. Make no promise of a time gain from the Go test cache.
Source: GitHub Docs, [caching](https://docs.github.com/en/actions/using-workflows/caching-dependencies-to-speed-up-workflows).
Scope: general pattern. In force: now.

## Flaky tests

1. A test that fails and passes on the same commit is flaky. Do not re-run it until it is green (T1).
2. File one issue (title "[Flaky test] <name>", as Kubernetes does). Search first.
3. Take the test out of the merge checks: skip it, and write `flaky-waiver: #<issue> until <YYYY-MM-DD>` next to the
   skip. Choose a date at most 30 days ahead.
4. `hack/ci-rules-check.sh` fails the build when the date passes. Fix the test, or the maintainer extends the date
   in the issue and in the code.
5. Never raise a timeout to hide a flake without a cause. Use `racescale.D` for windows under `-race`
   (`docs/dev/ci.md`, "The race detector").

Worked example: issue 586, pull request 587. A script test failed about 1 run in 4. A re-run would have hidden the
cause: the test asserted a promise that the code does not make. The fix removed the dependence on time. It uses one
owner process per slot and a fixed clock. It adds no longer sleep and no retry.

## Waivers and exceptions

A rule can have an exception only in a written place: an ADR amendment, or a waiver comment that has an issue and a
date (T2). The rule scripts have no per-line ignore. If a MUST fails for a reason outside your change, see below.

## Stop and ask

Stop and ask the maintainer when:

- You change the required-check list (`hack/ci-required.txt`) or the ruleset. A ruleset change needs the maintainer's yes on the diff.
- You add a secret to CI.
- You add a paid service or a new runner type.
- A check needs more time than the budget (L1).
- A rule's check fails for a reason outside your change.
- A premise in "Valid while" is false.

## Change a rule

A rule changes by a new ADR or by an amendment to ADR 0111, in the same pull request as the guide change. Mark the old
rule "superseded by <ID>" and keep its ID. Do not reuse an ID. Move a rule from "from v0.3" to "now" in the pull
request that builds its proof.

## Sources

Primary documents read on 2026-11-11 (a claim that rested on a search snippet or a summary only is not cited):
GitHub Docs (merge queue, skipping workflow runs, events, status checks, hosted runners, caching, rulesets,
security hardening); Kubernetes `sig-testing` (testing strategy, flaky tests); Google, Software Engineering at Google
(ch. 23); Rust compiler guide (CI); Firecracker tests README; Bazel hermeticity; Gatekeeper gator; kind quick start;
buf breaking rules; Dependabot and pnpm option pages; OpenSSF Scorecard checks; Fowler (ParallelChange, Continuous
Integration); DORA test automation; re-actors/alls-green; REUSE 3.3; RFC 8174.
