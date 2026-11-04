<!-- SPDX-License-Identifier: Apache-2.0 -->

# CI, e2e and merge reference

The detail behind the short CI bullets in `AGENTS.md`. The rules an agent must not break stay in
`AGENTS.md`; this page says how the gates work and why.

## Go test suites

`hack/go-test.sh` is what `make test` runs: the agentd, api, durable, checkpoint and compat suites under
Ginkgo `-p` (one shared Postgres server per run, a template-copied database per process, see
`internal/testing/pgtest`), the rest under `go test` (Postgres tests need Docker; they skip without
it). A new suite joins the parallel list in that script once it shares no database, fixed port or
fixed path between processes. `hack/testverdict` reports a skipped suite as SKIPPED (not PASS) and
fails one that did not run (NOT RUN); `hack/go-test.sh` exits 3 when an expected requirement
(docker, git, helm) was missing. One package: `go test ./internal/x` (serial) or
`go tool ginkgo -p ./internal/x`. `test/flavors` needs the flavor images: `make test-flavors`.

## Persisted-state compatibility

`internal/compat` (#652) checks that what a shipped release wrote still loads at head. Each folder
`testdata/compat/<release>/` is recorded by `hack/record-compat-fixtures.sh <release>` from that
release's code and holds three formats:

- `agent_configs/`: the stored `agent_versions.config` of every shipped template, plus `full` and
  `full-no-template` (`compat.FullConfig`: every `agentconfig.Config` field set, bindings included;
  a field added to `Config` must be added there, the self-test fails otherwise), each with the decode
  `agentconfig.Parse` made of it then; head must decode it to the same value.
- `home_checkpoints.sql` and `manifest.json`: `home_checkpoints` rows written for the schema version
  in the manifest, and the backup agent's `home_repositories` row with its `password_envelope`
  sealed under the fake KEK `koinon_test_not-a-real-kek`; the check migrates an empty database to that
  version, loads them, migrates to head, compares the recorded columns raw and through the generated
  store queries, and opens the envelope with `checkpoint.Passwords.Get` (Postgres, needs Docker).
- `home_backup/`: a kopia repository restored with `checkpoint.Writer.Restore`, using the prefix and
  password that came out of the envelope above (fake password `koinon_test_not-a-real-password`),
  and compared file by file.

The suite is in the parallel list of `hack/go-test.sh`: it takes its databases from `pgtest.Run`
(one per spec, unique per process), binds no port and writes only to temp directories. It sits in
its own package, not in `internal/db/migrations`, because it spans three subsystems.

The first folder is recorded at the v0.1.0 cut (`docs/dev/releasing.md`); until then the suite reports
"NO RECORDED RELEASE" and checks nothing. Its "harness self-test" records a folder at head into a
temp dir with the script and loads it: that proves the recorder and the loader agree and is not the
compatibility check.

A folder is never refreshed to make a failure go away. If a spec fails, head broke the reader of an
old format: fix the reader or add a migration (a patch line only expands, ADR 0085). Only a decided,
documented break of a format (an ADR, and a release that says so in its upgrade notes) may change a
folder, by deleting it and recording it again from the tag of its release with the script, in a
commit that names the break.

## Chart and admission tests

`hack/chart-check.sh` runs lint, every mode rendered, render assertions and kubeconform. Anything
that changes how a Koinon binary starts (a flag or env var it requires, a mount, a Secret key, an
RBAC rule) is covered by `make test-chart-smoke` (`test/chartsmoke`: runs the rendered Jobs and
Deployments as containers); extend it when you add a component or a startup input, since a unit
test of the flag parser does not see what the chart really passes (#284).

The sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`) is tested by the
escape suite: `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./test/escape/ -ginkgo.label-filter=admission`
(`make test-admission`). Its OpenShell cases run on kind: `make dev-kind-up`, then
`make test-escape-kind`.

## Testing layers

Every PR: unit and package tests, the smoke test and the scenarios (`make test` and CI). The kind
e2e (`test/e2e`, ~15-25 min) is not per PR: it runs on a PR only when the PR changes the e2e's own
files or carries the `e2e` label, and otherwise batched on main (each push that changed something
e2e-relevant, one running and the newest pending) and nightly, always, to catch drift no commit
explains.

For a risky PR, `hack/needs-e2e.sh` says whether your change touches what only a full install
observes (the rule lives only there). If yes, with a lab run `hack/e2e-remote.sh --argo`: a green
run posts the commit status `e2e-lab` on your pushed head and that is enough to merge. Without a
lab, or when it exits 75 (the lab cannot run it now), 78 (no lab configured) or 79 (the change
touches `flavors/`, which the lab cannot build; nothing triggers GitHub for it, dispatch it by
hand), run it on GitHub (`gh workflow run e2e-kind.yml --ref <branch>`, or the `e2e` label).
`hack/e2e-remote.sh` on your own Docker (`--local`) is a dev and debug tool only: it produces
neither `e2e-lab` nor `quickstart`, so it is not merge evidence. `SUITE_LABEL=smoke` (not with
`--argo`) runs steps 1-6 only.

### The `e2e result` gate

The required check is `e2e result`: green when nothing needs the e2e, but for a PR that needs one
(`hack/needs-e2e.sh`) it is red until the head has a green `quickstart` (this run or a dispatched
one) or `e2e-lab` success (`hack/e2e-gate.sh`, #562); a later `e2e-lab` or a passing dispatch
re-runs the `e2e result` job itself (not when the PR run is still in progress: then "Re-run failed
jobs"). `e2e-lab` is not a required context of its own, contributors without a lab cannot produce
it.

Trust limit: `result` runs from the PR's own `e2e-kind.yml`, so a PR can edit the YAML to drop the
gate; the guard is the ruleset's required review on `.github/**` (not CODEOWNERS), `e2e result` is
not pinned to an app and anyone with write access can post `e2e-lab`: a guardrail against
mistakes, not a malicious member (pinned integration_id / lab App: #568).

Why the e2e exists: #281 (branch naming in githubtools) and #284 (a startup flag the chart's
migrate Job lacked) passed every package test and failed only in a full install.

## What `ci` runs when

Actions minutes cost money; `hack/ci-skip-heavy.sh` decides, its header and
`hack/ci-skip-heavy-test.sh` are the rule. The heavy jobs are the ones the `changes` job gates:
`go` (go-tidy, go-lint, go-vet, go-test, go-race), `web` (web-lint, web-test, web-e2e),
`generated code`, `chart`, `chart smoke`, `docs`, `escape (admission)` and `flavors`. `changes` and
`quick checks` (proto, migrations, adr-lint, claim-tests and the dependency age steps in one job,
#594: every job bills a whole minute) and the other workflows (`dco`, `gitleaks`, `claim`,
`e2e result`) always run; `lint-shell` and `scripts` run on every pull request (drafts too) and
are skipped only on a push to main whose heavy jobs are skipped. A skipped heavy job reports
success, so every required check still reports.

### go-test on the lab gate first (#610)

`hack/ship.sh --ready --lab-go-test` runs `hack/e2e-remote.sh --argo --gate go-test` on the pushed
draft head (a free gate slot) and the lab posts the commit status `go-test-lab`; `changes` reads
it for the exact PR head SHA and the ready run skips `go-test`, which `go` accepts (the required
name stays `go`; no change to the ruleset). Exit 75/78/76/2 or no status: `go-test` runs on
Blacksmith, as it does without a lab. Nothing waits for a late status, so a push to a ready PR
runs `go-test` on Blacksmith (go back to draft first to keep the lab). Push to main
(`ci-skip-heavy.sh decide`) and `hack/release-gate.sh` count a skipped `go-test` only with that
status or a change with no Go. Trust limit as `e2e-lab` (#568). See also
`docs/dev/maintainer-homelab.md`.

### Draft pull request (#593)

Only `changes`, `quick checks`, `lint-shell` and `scripts` run, and `go` and `web` are RED on
purpose ("draft: not tested, mark ready for review"): a green draft `go` would otherwise stay the
newest run on the head while the ready run is still going. A draft cannot merge, so get WIP
feedback from `hack/preflight.sh`, not CI. Marking it ready fires `ready_for_review`, which runs
everything on the same head, and so does every push to a ready PR. The draft state is read live (a
re-run of an old draft run does not skip). Mark the PR ready (`hack/ship.sh --ready`,
`gh pr ready`) before you wait on it with `hack/pr-wait.sh`: a draft's green is not merge
evidence.

### One run per head (#675)

`ci.yml` groups `pull_request` runs by the pull request's head SHA (`ci-pr-<N>-<sha>`) and a push to
main by its own SHA (`ci-push-<sha>`), with `cancel-in-progress: false`. So runs on one head (the push's
run and the `ready_for_review` run, a re-run, `gh run rerun` on a cancelled run) never cancel each
other, and a push to main never cancels or is cancelled by another one. A cancelled run on the head
you wait for therefore means a person or a timeout cancelled it, not the workflow.

GitHub allows one running and one pending run per group; a third run on the same head replaces the
pending one. That is accepted: the replaced run and the new one are the same head and the same suite
(the draft state is read live), so the newest one carries the verdict. Only the older of two
waiting runs shows as cancelled.

Runs of an older head of the same pull request are cancelled on purpose: the first step of
`changes` runs `hack/ci-supersede.sh`, which cancels the queued or running ci runs of the PR with
another head SHA, and cancels its own run when the PR head has already moved on (a late start).
It needs `actions: write` on `changes` and is best effort (a fork's read-only token cannot cancel).

`hack/ship.sh --ready` on an existing draft PR marks it ready BEFORE the push, so the push is the only
event that starts a run on the new head. With `--lab-go-test` the order stays push, lab gate, ready
(the lab status must exist on the pushed head); the ready run is then the second run on that head and
queues behind the first.
`hack/e2e-rerun-result.sh` does not re-run `e2e result` while the branch has a live `e2e-kind` run on
another commit: that workflow's PR group still cancels in progress, and a re-run joins it. Tests:
`hack/ci-concurrency-test.sh`, `hack/ci-supersede-test.sh`, `hack/e2e-rerun-result-test.sh`,
`hack/ship-test.sh`.

### Push to main (#595)

Everything runs, except when the push is a merge commit whose tree equals the tree of its second
parent (the PR head) and every required check `ci.yml` produces is green (or skipped) on that
head, newest run per check, made by GitHub Actions. Then the heavy jobs are skipped: the merge
brought nothing the head was not tested with. The head must also contain the first parent, and
`flavors` (advisory, but `flavor-publish` ships what it tested) must have passed on the head
unless nothing it builds from changed. On such a push `go`, `web`, `scripts` and `lint-shell`
report skipped (#594). Any doubt (a merge of an out-of-date branch, an admin or direct push, a
squash, a missing or red check, an API error) runs everything. `flavor-publish` and the kind e2e
(batched on main, nightly) are unaffected. So what no single commit explains is caught by the
nightly and batched e2e, not by `ci` on main. `hack/release-gate.sh` reads a check that main
skipped from the merged PR's head.

## Main's e2e regressions and release tags

When main's kind e2e goes from green to red, `hack/e2e-bisect.sh` opens one `e2e-regression` issue
naming the merge (or the suspects): fix forward within 2 hours, otherwise revert it. RC and
release tags are cut only from a commit whose `quickstart` job concluded success
(`hack/release-gate.sh` checks it; dispatch e2e-kind on the commit if the batched run skipped it).

## Required status checks

`hack/ci-required.txt` is the list the `main` ruleset mirrors; `flavors` is the only advisory job
on pull requests (`flavor-publish` is advisory and push-only). `hack/ci-required-test.sh` (CI job
`scripts`) fails when a `ci.yml` job is neither required nor advisory;
`hack/ci-required-test.sh --ruleset` diffs the list with the live ruleset. A new job goes into the
list in the same PR, and a required job must report on every pull request and push (a job-level
`if:` skip reports success and is fine; only a workflow-level `paths:` filter leaves a check
absent).

## Merging

The only merge method is a merge commit: `gh pr merge --auto --merge`. `e2e result` is required
and fails a needs-e2e PR without a passing e2e on its head (#562), so a plain auto-merge cannot
skip it; still, a pull request that changes startup flags, chart Jobs or hooks, or anything the
e2e expects does not get auto-merge until `hack/pr-wait.sh <PR> --checks --require e2e` passes
(`--checks` also waits for every required context of the ruleset to exist on the head, so a
queued `ci` run is not "green"): a green `e2e-lab` status on the head (from
`hack/e2e-remote.sh --argo`, which needs the branch pushed) or a green `quickstart` run on the
head (it runs on its own for the paths in `.github/workflows/e2e-kind.yml`; otherwise dispatch it
with `gh workflow run e2e-kind.yml --ref <branch>`). Then enable auto-merge and
`hack/claim.sh review <N>` (step 8 of "Picking up work" in `AGENTS.md`).

The maintainer lab (`hack/e2e-remote.sh --argo`, `e2e-lab`, lanes): `docs/dev/maintainer-homelab.md`.

`hack/pr-merge.sh <PR> [--admin] [--lane L] [--approved-at <time>]` does all of this in one script (#647): it refuses a
draft, runs `pr-wait.sh --checks` (with `--require e2e` when `needs-e2e.sh` says so; it reads the PR's whole
file list with `gh api pulls/<PR>/files --paginate` and requires e2e when that read fails or is empty),
reads every required context of `hack/ci-required.txt` on the head SHA (newest run per name, success or
skipped), re-reads the PR (not OPEN: exit 3; head not origin's branch tip: exit 6 `STALE-HEAD`), runs
`gh pr merge --merge` (`--admin`, else `--auto`, which prints `auto-merge enabled`), and only after that
succeeded `claim.sh review` in the claim's lane (from `claim.sh mine`, or `--lane`) and a PR comment with the
head SHA and the number of contexts it read. `pr-wait.sh --checks` exits 5 on a draft. After
`ready_for_review` it ignores only the draft's `ci` workflow runs: those created more than about 30 s before
the last ready event whose newest attempt (a by-hand re-run counts) also started before it. dco, gitleaks,
claim and commit statuses (`e2e-lab`) keep their result; when gh cannot list the runs, each `ci` check run's
start time decides instead. It exits 6 `STALE-HEAD` when the PR's head stays different from
`git ls-remote origin refs/heads/<branch>` for `PR_WAIT_STALE_GRACE` seconds (default 120); re-sync with
`gh api -X PATCH repos/{owner}/{repo}/pulls/<PR> -f base=<base>`.
