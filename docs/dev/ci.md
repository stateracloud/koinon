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

`hack/go-test.sh` also exports one `TESTCONTAINERS_SESSION_ID` for the whole run, before `hack/pgserver`
starts, so pgserver and every test process share one testcontainers session and one Ryuk reaper.
Why: Ryuk exits 10 s after its last client disconnects. On a fresh runner a cold image pull leaves
gaps that long between packages; testcontainers-go v0.44.0 then finds the dying reaper without
checking that it runs and waits a fixed 60 s for it, failing a BeforeSuite with `wait for reaper ...:
context deadline exceeded` ([testcontainers-go#3867](https://github.com/testcontainers/testcontainers-go/issues/3867)).
pgserver keeps a container, hence a reaper connection, for the whole run, so Ryuk never idles out
mid-run and still cleans up after a crashed one. Ryuk stays on everywhere; do not set
`TESTCONTAINERS_RYUK_DISABLED` in CI. Runs without pgserver (a single-package `go test`) are not
covered; the upgrade path is #173. `hack/go-test-test.sh` pins the export and its order.

## Persisted-state compatibility

`internal/compat` (koinon-private#652) checks that what a shipped release wrote still loads at head. Each folder
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
test of the flag parser does not see what the chart really passes (koinon-private#284).

The sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`) is tested by the
escape suite: `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./test/escape/ -ginkgo.label-filter=admission`
(`make test-admission`). Its OpenShell cases run on kind: `make dev-kind-up`, then
`make test-escape-kind`.

## Testing layers

Every PR: unit and package tests, the smoke test and the scenarios (`make test` and CI). The kind
e2e (`test/e2e`, ~15-25 min) runs on a PR whenever `hack/needs-e2e.sh` says the change is e2e-relevant
(the e2e's own files included) or the PR carries the `e2e` label, in the PR's own run, so a fork PR
needs nothing from a maintainer to get one (issue #148). A PR nothing in the rule touches does not run
it. It also runs batched on main (each push that changed something
e2e-relevant, one running and the newest pending) and nightly, always, to catch drift no commit
explains.

For a risky PR, `hack/needs-e2e.sh` says whether your change touches what only a full install
observes (the rule lives only there). If yes, the PR's own run already runs the quickstart on GitHub;
with a lab you can get there sooner: `hack/e2e-remote.sh --argo`, a green run posts the commit status
`e2e-lab` on your pushed head and that is enough to merge once the PR's own quickstart has finished (a flaky red one does not hide a green lab run). When it exits 75 (the lab cannot run it
now), 78 (no lab configured) or 79 (the change touches `flavors/`, which the lab cannot build), wait
for the PR's quickstart, or dispatch it earlier (`gh workflow run e2e-kind.yml --ref <branch>`). The
`e2e` label is for a change the rule thinks irrelevant but you want tested.
`hack/e2e-remote.sh` on your own Docker (`--local`) is a dev and debug tool only: it produces
neither `e2e-lab` nor `quickstart`, so it is not merge evidence. `SUITE_LABEL=smoke` (not with
`--argo`) runs steps 1-6 only.

### The `e2e result` gate

The required check is `e2e result`: green when nothing needs the e2e, but for a PR that needs one
(`hack/needs-e2e.sh`) it is red until the head has a green `quickstart` (this run or a dispatched
one) or `e2e-lab` success (`hack/e2e-gate.sh`, koinon-private#562); a later `e2e-lab` or a passing dispatch
re-runs the `e2e result` job itself (not when the PR run is still in progress: then "Re-run failed
jobs"). `e2e-lab` is not a required context of its own, contributors without a lab cannot produce
it.

Trust limit: `result` runs from the PR's own `e2e-kind.yml`, so a PR can edit the YAML to drop the
gate; the guard is the ruleset's required review on `.github/**` (not CODEOWNERS), `e2e result` is
not pinned to an app and anyone with write access can post `e2e-lab`: a guardrail against
mistakes, not a malicious member (pinned integration_id / lab App: koinon-private#568).

Why the e2e exists: koinon-private#281 (branch naming in githubtools) and koinon-private#284 (a startup flag the chart's
migrate Job lacked) passed every package test and failed only in a full install.

## What `ci` runs when

Actions minutes are a limited, shared resource; `hack/ci-skip-heavy.sh` decides, its header and
`hack/ci-skip-heavy-test.sh` are the rule. The heavy jobs are the ones the `changes` job gates:
`go` (go-tidy, go-lint, go-test, go-race; `go vet` is part of golangci-lint's `standard` set), `web` (web-lint, web-test, web-e2e),
`generated code`, `chart`, `chart smoke`, `docs`, `escape (admission)` and `flavors`. `changes` and
`quick checks` (proto, migrations, adr-lint, claim-tests, the Conventional Commits subject check and the dependency age steps in one job,
koinon-private#594: every job bills a whole minute) and the other workflows (`dco`, `gitleaks`, `claim`,
`e2e result`) always run; `lint-shell` and `scripts` run on every pull request (drafts too) and
are skipped only on a push to main whose heavy jobs are skipped. A skipped heavy job reports
success, so every required check still reports.

### go-test on the lab gate first (koinon-private#610)

`hack/ship.sh --ready --lab-go-test` runs `hack/e2e-remote.sh --argo --gate go-test` on the pushed
draft head (a free gate slot) and the lab posts the commit status `go-test-lab`; `changes` reads
it for the exact PR head SHA and the ready run skips `go-test`, which `go` accepts (the required
name stays `go`; no change to the ruleset). Exit 75/78/76/2 or no status: `go-test` runs on
the hosted runner, as it does without a lab. Nothing waits for a late status, so a push to a ready PR
runs `go-test` on the hosted runner (go back to draft first to keep the lab). Push to main
(`ci-skip-heavy.sh decide`) and `hack/release-gate.sh` count a skipped `go-test` only with that
status or a change with no Go. Trust limit as `e2e-lab` (koinon-private#568). See also
`docs/dev/maintainer-homelab.md`.

### Draft pull request (koinon-private#593)

Only `changes`, `quick checks`, `lint-shell` and `scripts` run, and `go` and `web` are RED on
purpose ("draft: not tested, mark ready for review"): a green draft `go` would otherwise stay the
newest run on the head while the ready run is still going. A draft cannot merge, so get WIP
feedback from `hack/preflight.sh`, not CI. Marking it ready fires `ready_for_review`, which runs
everything on the same head, and so does every push to a ready PR. The draft flag comes from the
event payload (koinon-private#676), not from the live state: the run is named `ci (draft)` (ci.yml `run-name`)
when `github.event.pull_request.draft` is true, else `ci`, and `hack/ci-skip-heavy.sh` decides from
that same flag (`PR_DRAFT`), so a run's name and its behaviour always agree. A gate that must
ignore draft runs drops the runs whose `display_title` (`displayTitle` in `gh run list --json`) is `ci (draft)`; the run `name` stays `ci` for every run, so never filter on it;
`pr-wait.sh` (and `pr-merge.sh`, which calls it) still uses its time window until #133, which will
filter on the `ci (draft)` display_title instead. A re-run replays the
original event: re-running a `ci (draft)` run after the PR is ready is a draft run again (`go` and
`web` red, "not tested"). That is safe (it can never leave a green `go` on an untested head) but
`pr-wait.sh` ignores such a re-run (it was created before the ready event), so the ready run's
result stands; to re-run, use the `ci` run of `ready_for_review` (or push)
instead. Mark the PR ready (`hack/ship.sh --ready`,
`gh pr ready`) before you wait on it with `hack/pr-wait.sh`: a draft's green is not merge
evidence.

### One run per head (koinon-private#675)

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

### Push to main (koinon-private#595)

Everything runs, except when the push is a merge commit whose tree equals the tree of its second
parent (the PR head) and every required check `ci.yml` produces is green (or skipped) on that
head, newest run per check, made by GitHub Actions. Then the heavy jobs are skipped: the merge
brought nothing the head was not tested with. The head must also contain the first parent, and
`flavors` (advisory, but `flavor-publish` ships what it tested) must have passed on the head
unless nothing it builds from changed. On such a push `go`, `web`, `scripts` and `lint-shell`
report skipped (koinon-private#594). Any doubt (a merge of an out-of-date branch, an admin or direct push, a
squash, a missing or red check, an API error) runs everything. `flavor-publish` and the kind e2e
(batched on main, nightly) are unaffected. So what no single commit explains is caught by the
nightly and batched e2e, not by `ci` on main. `hack/release-gate.sh` reads a check that main
skipped from the merged PR's head.

## Cache ownership (#147)

A pull request can read its own caches and the default branch's, never another pull request's, so a
cache only helps a pull request if main saved it. Main owns them all: the `warm-caches` jobs (`warm-caches`, `warm-caches-web`, `warm-caches-docs`) run on
every push to main and is never skipped (`hack/ci-skip-heavy.sh` switches the heavy jobs off on a
push whose tree already passed as a pull request head, which used to leave main without fresh
entries). For each cache it does a `lookup-only` restore (no download) and, on a miss, the cheapest
work that fills it, then saves:

| Cache | Key | Filled by |
| --- | --- | --- |
| Go modules and build cache | `go-ci-<os>-<arch>-<go version>-<go.sum hash>` | `go build ./...`, `go test -run '^$' ./...`, one lint run |
| envtest API server binaries | `envtest-<ENVTEST_K8S_VERSION>` | `make envtest-assets` |
| Playwright browser | `playwright-<os>-<arch>-<@playwright/test version>` | `playwright install chromium` |
| pnpm stores (`web`, `docs/site`) | setup-node's, by lockfile | `pnpm install` in `warm-caches-web` and `warm-caches-docs`, each with the consumers' `cache-dependency-path` (setup-node saves after a miss) |
| golangci-lint cache | the action's | a lint run on every push to main, not only on a go-ci miss: the action's key has a 7-day interval (`go-lint` has `skip-save-cache`) |

Every other job restores and never saves: one Go cache scheme (`go-ci`, `setup-go` with
`cache: false` plus an explicit `actions/cache/restore`), and `e2e-go` is one entry per `go.sum`
(no run id; saved by main only when the exact key was missing). The flavor layer cache stays with
`flavors` and `flavor-publish` (see below). The `warm-caches*` jobs are advisory in `hack/ci-required.txt`: it
reports on pushes only. A new key goes into the warm job and the restoring jobs together;
`hack/ci-cache-test.sh` checks the rule.

## Image builds: authenticated downloads and layer cache (#145)

Anonymous GitHub release downloads from shared runner IPs sometimes return a fixed error body,
which fails a pinned checksum. Every Dockerfile download of a release asset goes through
`flavors/common/koinon-fetch` (or the same logic in `hack/e2e-toolchain/Dockerfile`): the job passes
`GITHUB_TOKEN` as the BuildKit secret `github_token`. The github.com web download URL ignores a
token, so the script resolves the asset id from the release (`api.github.com/.../releases/tags/<tag>`)
and downloads `.../releases/assets/<id>` with `Accept: application/octet-stream`; the build log
shows the response's `x-ratelimit-limit`, which proves the request was authenticated. The pinned
sha256 is still checked. No token means an anonymous download from github.com, so local and fork
builds work. The token is never an ARG, ENV, log line or layer (`hack/koinon-fetch-test.sh` pins
this; a new `ADD` of a release asset fails it, also with the URL on a continuation line).

Every Dockerfile `apt-get update` and `apt-get install` carries `-o Acquire::Retries=5 -o
Acquire::http::Timeout=30`, because snapshot.ubuntu.com answers 5xx now and then (#149); a step that
runs apt through another tool (`playwright install --with-deps`) writes an `apt.conf.d` snippet with
both settings. `hack/apt-retries-test.sh` (the `scripts` job) fails on any apt call without them. The
chart smoke suite keeps the envtest kube-apiserver and etcd output and prints it when envtest does
not start.

The flavor layer cache (`flavors-<hash of flavors/**>`, prefix `flavors-`) is restored by `flavors`,
`quickstart` (e2e-kind) and `flavor-publish`. Only pushes to main save it: `flavors` and
`flavor-publish` (which exports with `--cache-to` and saves, so a main push where `flavors` was
skipped by #595 still writes the entry). A pull request never writes it.

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

Third-party analysers are advisory: their checks never enter the list or the ruleset. DeepSource
(GitHub App) reads `.deepsource.toml` at the root (#180): Go, JS/TS, shell and Dockerfile
analysers, generated code and fixtures excluded, no code formatters (a bot commit would start a new
CI run on a new head) and Autofix only when a person clicks it. Read its PR comments as review
input; make it required only by a decision recorded in an issue.

CodeRabbit (GitHub App) is advisory too and reads `.coderabbit.yaml` (#184). Every built-in linter is
off (CI runs them), there are no status checks or comments per push, and the summary stays in its own
walkthrough comment. Reviews are manual by design (`auto_review.enabled: false`): `hack/ship.sh --ready`
posts `@coderabbitai review` once per pushed head, and you can post it again after a push. Its only
finishing touch is docstrings, delivered as a follow-up pull request when asked; nothing commits to
the reviewed branch.

## Merging

The only merge method is a merge commit: `gh pr merge --auto --merge`. `e2e result` is required
and fails a needs-e2e PR without a passing e2e on its head (koinon-private#562), so a plain auto-merge cannot
skip it; still, a pull request that changes startup flags, chart Jobs or hooks, or anything the
e2e expects does not get auto-merge until `hack/pr-wait.sh <PR> --checks --require e2e` passes
(`--checks` also waits for every required context of the ruleset to exist on the head, so a
queued `ci` run is not "green"): a green `e2e-lab` status on the head (from
`hack/e2e-remote.sh --argo`, which needs the branch pushed) or a green `quickstart` run on the
head (it runs on its own for the paths in `.github/workflows/e2e-kind.yml`; otherwise dispatch it
with `gh workflow run e2e-kind.yml --ref <branch>`). Then enable auto-merge and
`hack/claim.sh review <N>` (step 8 of "Picking up work" in `AGENTS.md`).

The maintainer lab (`hack/e2e-remote.sh --argo`, `e2e-lab`, lanes): `docs/dev/maintainer-homelab.md`.

`hack/pr-merge.sh <PR> [--admin] [--lane L] [--approved-at <time>]` does all of this in one script (koinon-private#647): it refuses a
draft, runs `pr-wait.sh --checks` (with `--require e2e` when `needs-e2e.sh` says so; it reads the PR's whole
file list with `gh api pulls/<PR>/files --paginate` and requires e2e when that read fails or is empty),
reads every required context of `hack/ci-required.txt` on the head SHA (newest run per name, success or
skipped), re-reads the PR (not OPEN: exit 3; head not origin's branch tip: exit 6 `STALE-HEAD`), runs
`gh pr merge --merge` (`--admin`, else `--auto`, which prints `auto-merge enabled`), and only after that
succeeded `claim.sh review` in the claim's lane (from `claim.sh mine`, or `--lane`) and a PR comment with the
head SHA and the number of contexts it read. `pr-wait.sh --checks` exits 5 on a draft. After
`ready_for_review` it ignores only the draft's `ci` workflow runs: those created more than about 30 s before
the last ready event (a by-hand re-run of a draft-era run is ignored too). dco, gitleaks,
claim and commit statuses (`e2e-lab`) keep their result; when gh cannot list the runs, each `ci` check run's
start time decides instead. It exits 6 `STALE-HEAD` when the PR's head stays different from
`git ls-remote origin refs/heads/<branch>` for `PR_WAIT_STALE_GRACE` seconds (default 120); re-sync with
`gh api -X PATCH repos/{owner}/{repo}/pulls/<PR> -f base=<base>`.

## OpenSSF Scorecard

`.github/workflows/scorecard.yml` runs `ossf/scorecard-action` weekly and on every push to main,
publishes the result (`publish_results`) and uploads the SARIF to code scanning. It is advisory: not
in `hack/ci-required.txt` (that file lists `ci.yml` jobs and the external required checks only).
