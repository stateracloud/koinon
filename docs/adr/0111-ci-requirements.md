# 0111. CI requirements

- Status: Accepted
- Date: 2026-11-11
- Area: engineering-process
- Tags: ci, testing, release
- Relates to: [0025](0025-testing-strategy.md) (testing layers), [0047](0047-repository-scaffold.md) (GitHub-hosted runners only),
  [0079](0079-dependency-cooldown.md) (3-day age), [0085](0085-upgrade-compatibility-and-recovery-policy.md) and
  [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md) (no upgrade promise before v0.3),
  [0090](0090-scripts-require-bash-5.md) (scripts), [0094](0094-work-claims-on-the-public-repository.md) and
  [0105](0105-merge-queue-route.md) (the merge queue is the only merge route), [0104](0104-work-claims-reduced-to-the-lock.md)
  (one maintainer), [0109](0109-automated-restore-drill.md) (restore drill)
- Issues: refs #579 (part 1: this ADR, the guide, the rule script), #572 (CI speed), #580 (the proofs that come into force at v0.3)

## Context

The CI grew by single fixes. No document says what the CI must prove, at which stage, or inside which
limits. An agent that adds or changes a check has no rule to follow. It also cannot tell when a rule
no longer applies. Three things made this costly:

- The CI review of 2026-11-11 (72 runs, 20 merged pull requests) measured 31 minutes from the last
  push to merge, and 147 runner minutes for one merged pull request (see the amendment of ADR 0105).
  Checks had been added where they were easy, not where they were cheapest.
- Many agent pull requests are open at the same time. A check that is correct for one pull request can
  starve or block the rest.
- The product is pre-alpha. Some proofs a mature product needs (an upgrade from the last release, a
  scan of the published images) do not exist yet. Pretending they do is worse than saying so.

Prior art, read 2026-11-11 (the full list with links is in the guide, section "Sources"):

- Google's "Software Engineering at Google" (ch. 23): presubmit runs only fast and reliable tests; slow
  tests run after submit.
- Kubernetes (`sig-testing`): presubmit, postsubmit and periodic jobs; a blocking presubmit must also be
  release-blocking and fast (under one hour, preferably under 30 minutes); test jobs must not retry
  automatically.
- Rust's merge bot (`bors`, "not rocket science"): the full suite runs once on the merge candidate.
- GitHub documents what a skipped job and a path-filtered workflow report to the merge queue.
- Firecracker splits its tests into three lanes: required, optional and periodic.

None of them is a list of CI requirements with an in-force date, so the shape of this ADR is ours.
Where a rule has a source, the guide names it. Where it has none (the flaky-test expiry date, the
post-release install check), the guide says "Koinon decision".

## Decision

CI proves eight things (R1 to R8), inside six limits (L1 to L6). A change to CI follows these. The rules
that carry them out are in [`docs/dev/ci-rules.md`](../dev/ci-rules.md). This ADR gives the reasons.
The guide gives the rules and the checks. If the two disagree, this ADR wins.

### What CI must prove

Each proof has a mark. "In force now" means a check exists today and the rule is required. "In force from
v0.3" means the proof is planned and tracked in #580, not built; the rule is not required yet.

| ID | CI proves that | Today's proof | In force |
|---|---|---|---|
| R1 | Code is correct | Unit tests, Postgres integration tests (testcontainers), lint, vet, the race detector on the concurrent packages, web unit tests | now |
| R2 | The product works end to end, from the guide's own commands | The kind install with the fake model and the fake code host (`quickstart`), in the merge queue | now |
| R3 | The sandbox holds | Admission policy tests on a real API server (envtest), the escape suite, the leak guard cases | now (VM tier: see "Limits of CI") |
| R4 | Tenants and the audit chain stay separate and intact | Go tests of org scoping and of the audit chain (`internal/audit`, ADR 0100) | now |
| R5 | Data survives | Migration lint, stored-state fixtures (`internal/compat`), the restore drill (advisory, scheduled) | migration lint and fixtures now; drill and upgrade test from v0.3 |
| R6 | Nothing public breaks | `buf breaking`, generated-code diff, chart checks, chart smoke (every binary starts with the chart's real inputs) | now for proto, generated code and chart; the admin API and chart values contract from v0.3 |
| R7 | The supply chain is clean | 3-day age (ADR 0079), pinned actions and images, gitleaks on every commit, nightly `govulncheck`, workflow lint | now; the SPDX header check, the dependency licence check and the image scan from v0.3 |
| R8 | A release comes only from a proven commit, and what is published can be installed | `hack/release-gate.sh` (the quickstart succeeded for the commit) | the gate now; "pull, verify and install what was published" from v0.3 |

The gaps inside R5 to R8 (marked "from v0.3") are listed in #580. They are planned, not built.

### Limits

| ID | Limit | Reason |
|---|---|---|
| L1 | 20 minutes from push to merged (median), on free GitHub-hosted runners only | Agents wait on CI. The runner minutes of a public repository are free, but the concurrency is not unlimited. The 20 minutes is the maintainer's goal; Fowler's "ten minute build" is the classic bound. The measure and the work to reach it are #572 |
| L2 | No paid model call in CI. The fake model server is the model | A paid call makes a test cost money, makes the result depend on a vendor, and needs a key in CI. Whether a key may ever be in CI is an open decision, see #580 |
| L3 | Each promise is proven one time, at the cheapest stage that can prove it | GitHub has no "already tested, skip" feature, so the rule is carried out by trigger design (`hack/paths.rules`, `hack/ci-skip-heavy.sh`) |
| L4 | Many agent pull requests are open at one time. No check may assume one | A shared fixed port, a shared cache key or a global lock turns many pull requests into one queue |
| L5 | A required check reports on every pull request, push and merge-group build. No work means success, never absent | A required check that does not report leaves a pull request pending, and makes the merge queue wait until its timeout and drop the entry (GitHub docs) |
| L6 | The same command gives the same verdict on a laptop and in CI | An agent must be able to check its work before it pushes (`hack/preflight.sh`, `hack/lane-gates.sh`). What cannot be equal is named in the guide: the runner OS, the CPU count, secrets, the clock |

### Stages

A check runs at one stage. The stages are: pull request, merge queue, push to main, nightly, release, lab.
The pull request stage is fast and gives feedback. The merge queue build is the gate: it runs main plus the
pull request, and it is the only e2e proof (ADR 0105). Push to main runs only what the queue did not prove,
and the cache warm-up. Nightly and release stages hold slow, destructive and fault-injection proofs (the
restore drill, and later the upgrade test). The lab holds what public CI cannot (the VM tier). The table
"a new check: which stage" in the guide says where a new check goes.

### Limits of CI

- **The VM tier** (the Kata runtime class, ADR 0075) is proven only on the lab. GitHub states that nested
  virtualization on hosted runners is not officially supported, so a merge gate must not depend on it.
  The test says so next to the code, and names the place that proves it.
- **Real-model behaviour** is outside CI (L2). It is checked by hand on the maintainer's own cluster and
  budget (`docs/dev/real-model-tests.md`).

### Which requirement is in force

The product is usable from v0.3. A requirement whose proof does not exist today is "in force from v0.3".
Workflow hygiene and security rules are "in force now". The change from "planned" to "in force" is a
change to the guide, made when the proof is built, in the pull request that builds it.

This is a Koinon pattern, not a published one. It is modelled on `hack/migrate-patch-line`: a marker
moves a rule from a report to a failing check. Until a proof exists, a rule marked "from v0.3" has no
check and is not enforced; it is a promise with a tracking issue.

### Where the rules are checked

A rule that a text scan can decide is checked by `hack/ci-rules-check.sh` (job `scripts`, local gate
`scripts`). Its self-test is `hack/ci-rules-check-test.sh`. A rule that another script already decides is
checked by that script, and the guide names it. A rule that no script can decide is a SHOULD, or its
check is "review". The script does not repeat an existing lint.

Two checks arrive with part 2 of #579 (maintainer decision 2026-11-11): the workflow security lint
`zizmor`, and `govulncheck` on pull requests that change Go dependencies. The Playwright retry is removed
in part 2 too.

## Alternatives considered

- **No document; keep the rules in `docs/dev/ci.md`.** `docs/dev/ci.md` describes how each job works. It
  changes with every CI change. A rule kept there has no reason, no source and no way to say that it no
  longer applies. _(reconstructed)_
- **Put the rules in `AGENTS.md`.** `AGENTS.md` is loaded by every agent on every task. The CI rules are
  needed only when a check changes. A pointer is enough; the rules would cost every agent tokens.
  _(reconstructed)_
- **Enforce every requirement now, with report-only jobs for the missing proofs.** A red or noisy job
  for a feature that does not exist teaches people to ignore CI. The "in force from v0.3" mark is
  cheaper and says the same thing. _(reconstructed)_
- **A policy-as-code tool (conftest and Rego) for the workflow rules.** It is a new tool and a new
  language for about six rules a shell script decides. The maintainer may choose it later; the rule IDs
  stay. _(reconstructed)_

## Consequences

- Positive: an agent that changes a check has one place to read (the guide's first section says if it
  applies). A reviewer can ask "which rule" and get an ID, a reason and a check.
- Positive: the gaps are listed with a number (#580) and not hidden. "In force from v0.3" is a promise
  with a tracker.
- Positive: the rule that a flaky test is never retried (T1) is a script check from the start.
- Negative: the guide and `docs/dev/ci.md` can disagree. The guide's first section says which wins; a
  change to a job updates both.
- Negative: the rule script reads text, not YAML. A workflow that hides a `paths:` key in an anchor or a
  flow-style `on:` is not seen. `zizmor` (added in part 2) parses the workflow.
- Negative: T7 (a timeout on every job) was a report at first. 14 of 21 `ci.yml` jobs had none on
  2026-11-11. The CI speed work (#572, pull request 2 of 3, #589) added them, and `hack/runner-lint.sh` now fails a job without one.
- Follow-ups: part 2 of #579 (`zizmor` is in and the Playwright retry is removed, so T1 now fails;
  `govulncheck` on pull requests is left); the proofs in #580; a review of the guide when a premise in its "Valid while" section stops being true
  (paid runners, a second maintainer, a second repository's CI).

## Open questions

- Whether a model key may be in CI for a small real-model smoke test (L2). Tracked in #580.
- Whether the status check bound to its source app (Q5) is worth the ruleset change. It needs the
  maintainer's yes.

## Confirmation

The rule script `hack/ci-rules-check.sh` confirms W1, W2, W5, Q1, Q6, T1, T2 and T7. It runs in the `scripts` job of
`ci.yml` and in `hack/preflight.sh` (gate `scripts`). Other rules name their check in the guide:
`hack/runner-lint.sh`, `hack/ci-required-test.sh`, `hack/ci-merge-group-test.sh`, `hack/migrate-lint.sh`,
`hack/dep-age-check.sh`, `hack/release-gate.sh`, `make breaking`, `hack/generate.sh`, the chart smoke test
and the secret scan. `hack/adr-lint.sh` checks this file.

## Upstream verification

Checked on 2026-11-11 in the primary documents named in the guide's "Sources" section: GitHub Docs (merge
queue, status checks, skipping workflow runs, hosted runners, caching, rulesets), the Kubernetes
`sig-testing` documents, the Rust compiler developer guide (CI), the Firecracker tests README, the
Bazel hermeticity page, buf's breaking-change rules, and Dependabot and pnpm option references. A claim
that rested on a search snippet or a summary only is not cited in the guide.

## Amendments

- 2026-11-12: `govulncheck` on pull requests that change `go.mod` or `go.sum` is in (#579 part 2). It is the job
  `go-vuln`, a part of the required `go` check (no new ruleset context), gated by the path rule `vuln`. The
  nightly run and the job both call `hack/govulncheck.sh`; a failed database fetch fails the run and is not
  retried (T1). Details: `docs/dev/ci.md`.

## Links

- [`docs/dev/ci-rules.md`](../dev/ci-rules.md): the rules, their checks and sources.
- [`docs/dev/ci.md`](../dev/ci.md): how each job works today.
- Tracking: #579, #572, #580.
