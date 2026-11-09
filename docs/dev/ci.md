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
(docker, git, helm) was missing. A plain `func TestX` that passes in a Ginkgo package counts as a run
test, so the suite reads "partly run" when only that test ran. A plain test that calls `testenv.RequireT`
in a package under `-p` fails the verdict (the ginkgo CLI emits no `go test` events): make it a spec. One package: `go test ./internal/x` (serial) or
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

### The gateway behaviour suite (#272, ADR 0097 step 1)

`internal/gateway/gatewaytest` is what Koinon needs from a gateway, stated as behaviour a client or
the spend ledger sees. `internal/gateway/standalone` runs it against the pinned agentgateway image
(`internal/gateway/kubernetes` against kind); in step 2 Koinon's own gateway runs the same specs by
supplying a `gatewaytest.Env`. Cases: key refusals and the do-not-retry marker, header allowlist
and `kgw_` refusal, usage per API format and dialect (`usagespec.go`), guard coverage per MCP
message kind (`mcpkindspec.go`), the budget stop with the overshoot measured (`budgetspec.go`),
and a returned revision being served (`revisionspec.go`). The rendered config's ban on
`response.body` and `bodyPrefix` stays in the driver render tests.

A case the pinned gateway fails is `gatewaytest.KnownGap(env, "#N", ...)`. A driver lists the gaps
of its gateway in `Env.KnownGaps` (agentgateway v1.7.0: `gatewaytest.AgentgatewayV170Gaps`, with
the reason of each); a driver that does not list `N` runs the case plainly, so Koinon's own gateway
passes the same suite untouched. The case fails when the gap closes, so an upgrade that fixes one
flips it, and an upgrade that breaks a passing case fails as usual. A gateway bump is "suite green";
a changed gap list is reviewed like code. The overshoot and the outcome of each MCP kind are
written to the Ginkgo report (`-v`, or the JSON report). The real-guard case (`guardspec.go`) needs
Postgres (pgtest) and runs in both the standalone and the kind suites.

### The agentgateway test image (#238)

The `internal/gateway/standalone` and `internal/ledger` suites start a real agentgateway container.
Pulling it from its upstream registry at test time made every Go job depend on that registry: on
2026-11-06 `cr.agentgateway.dev` answered 522 and every pull request with Go changes went red. The
image is named once, by digest: the chart's `gateway.standalone.image` in `charts/koinon/values.yaml`
(repository, tag and digest), printed by `hack/agentgateway-pin.sh`. The tests take it from
`internal/testing/agentgatewayimage` (`Digest`, and `Upstream`, the chart's repository at that digest).
CI tests against a copy of that digest in GHCR, `ghcr.io/<owner>/agentgateway`, kept by the manually
dispatched workflow `mirror-agentgateway-test-image.yml`.

`hack/agentgateway-pin.sh --check` (in the `scripts` job, with `hack/agentgateway-pin-test.sh`) fails
when the pin is not a full sha256 digest, when the workflow carries a digest of its own, when
`Digest` or `Upstream`'s repository differs from the chart's, and when any other tracked Go file names
an agentgateway image itself.

When the pin moves (an agentgateway bump changes the chart's tag and digest), do it in one pull
request:

1. Change `gateway.standalone.image` in the chart, and `Digest` in
   `internal/testing/agentgatewayimage/image.go` to the same digest (`--check` fails until they match).
2. Push the branch and dispatch the workflow on it:
   `gh workflow run mirror-agentgateway-test-image.yml --ref <branch>` (input `source`:
   `cr.agentgateway.dev/agentgateway`, or `ghcr.io/agentgateway/agentgateway` while the first is
   down; both serve the same digest). It copies the digest, fails unless the copy has that digest, and
   prints `image: ghcr.io/<owner>/agentgateway@sha256:...` in its log and summary. On a branch it tags
   the copy by its digest (`pin-<hex>`); only on `main` does it move the version tag. On the first
   dispatch for a new package, GHCR can answer DENIED to a manifest PUT right after the run created the
   package: re-run the job once.
3. Re-run the pull request's go-test, which now finds the copy. After the merge, dispatch the workflow
   on `main` to point the version tag at the new digest.

The go-test job logs in to GHCR, pulls `ghcr.io/<owner>/agentgateway@<digest>` once, and sets
`KOINON_AGENTGATEWAY_MIRROR` to that repository; `agentgatewayimage.Ref()` then returns the copy, and
the chart's repository at the same digest when the variable is unset, so a laptop needs no
GHCR credentials. When the pull fails, go-test fails with a pointer to step 2, except in a fork's own
CI, which cannot read the copy: there it warns and the tests pull the chart's repository.

The GHCR package stays private until v0.3. The go-test job pulls it with its own `GITHUB_TOKEN`
(`packages: read` on the job), which works only while the package's settings ("Manage Actions
access") list this repository with read. The image's `org.opencontainers.image.source` label names
the upstream repository (changing it would change the digest), so GHCR does not link the package to
Koinon by itself: after the first run, check that setting and add the repository if it is missing.
Crane is `CRANE_VERSION` in `hack/versions.env`, installed with `go install` (the Go checksum database
verifies it) and dated by `hack/dep-age-check.sh` (ADR 0079).

## Persisted-state compatibility

`internal/compat` (old issue 652) checks that what a shipped release wrote still loads at head. Each folder
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

`hack/chart-check.sh` runs lint, every mode rendered, render assertions and kubeconform; any `FAIL`
from an assertion (at any shell level) makes it exit non-zero, which `hack/chart-check-test.sh`
(the `scripts` job) proves with planted failures. Anything that changes how a Koinon binary starts
(a flag or env var it requires, a mount, a Secret key, an RBAC rule) is covered by
`make test-chart-smoke` (`test/chartsmoke`: runs the rendered Jobs and Deployments as containers);
extend it when you add a component or a startup input, since a unit test of the flag parser does
not see what the chart really passes (old issue 284).

The sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`) is tested by the
escape suite: `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./test/escape/ -ginkgo.label-filter=admission`
(`make test-admission`). Its OpenShell cases run on kind: `make dev-kind-up`, then
`make test-escape-kind`.

## Leak guard exfiltration cases

Two cases try to send private text out through an MCP tool on the gateway path (ADR 0091, #293).
Both use only the fake MCP server (`koinon-fakemcp`) as the receiver, and canaries that look fake
(`koinon-canary-<n>-not-a-secret`); no model is called.

- **Kind e2e** (`test/e2e`, step 6b, `leakGuardExfilE2E` and the in-task case): through a full install
  with the real agentgateway image and the leak guard, a `write_item` call carrying what `read_item`
  returned is refused outside a task (the agent reads `mcpguard.NoRunApprovalText`) and parked inside
  one; the approval's `McpCall.leak_guard` names both legs, `both_legs` and the `external-high` sink
  class, and nothing reaches the fake server before the approval. It runs wherever the kind e2e runs
  (`e2e-kind`, the lab quickstart).
- **Escape suite** (`test/escape/exfil_test.go`, label `leakguard`, with the OpenShell cases): from a
  real sandbox the agent reads the canary through `read_item`, then sends it out through `write_item`
  as a note body and through `fetch_url`, the fake server's fetch-style tool, whose `url` argument is an
  attacker address. Each call must fail with the guard's text and the receiver must see no `write_item`
  or `fetch_url`. It needs a guarded gateway the suite does not start, named by
  `KOINON_ESCAPE_MCP_HOST` (and `_PORT`, default 4000), `KOINON_ESCAPE_MCP_KEY` (an agent's gateway key
  on it) and `KOINON_ESCAPE_MCP_RECEIVER_URL` (the fake server's base URL, as the test process
  reaches it); the fixture sets the tools up as `test/e2e/mcp_test.go` does (`write_item` and `fetch_url`
  auto and undeclared). Without the variables it skips, or fails when `KOINON_REQUIRE` lists
  `escape-mcp-gateway` (one name; it checks all three variables, so a partial setup fails too).
  **In CI** it runs inside the `e2e-kind` job (#348): `e2e-kind.yml` sets `ESCAPE_MCP=1`, `run.sh`
  gives the Go driver `KOINON_E2E_ESCAPE_HOOK=test/e2e/escape-mcp.sh`, and the driver calls it from
  step 6b after its own in-process checks, while `write_item` and `fetch_url` are auto (it flips them
  back to ask at the end of the step). The hook runs `test/escape/kind.sh` with
  `KOINON_ESCAPE_LABEL=leakguard`, no stand-ins and `KOINON_REQUIRE=escape-mcp-gateway`, against the
  install's gateway (`koinon-gateway.koinon.svc.cluster.local:8080`), the agent's own gateway key and
  the port-forwarded fake server; a skip is a failure there. No extra job and no model call, so
  `hack/ci-required.txt` is unchanged (the `e2e result` status covers it). `make test-escape-kind`
  alone still skips the case (`make dev-kind-up` installs OpenShell only). Locally:
  `ESCAPE_MCP=1 test/e2e/run.sh`.

Both fail when the guard is bypassed: make `write_item` pass the guard (declare its sink class `none`)
and the call reaches the server. The escape case sends the canary in `title`, the one argument the fake
`write_item` accepts, and an attacker URL in `fetch_url`'s `url`, so a bypassed call is recorded and the
request-log assertion goes red.

## Testing layers

Every PR: unit and package tests, the smoke test and the scenarios (`make test` and CI). The kind
e2e (`test/e2e`, ~15-25 min) runs on a PR whenever `hack/needs-e2e.sh` says the change is e2e-relevant
(the e2e's own files included) or the PR carries the `e2e` label, in the PR's own run, so a fork PR
needs nothing from a maintainer to get one (issue #148). A PR nothing in the rule touches does not run
it. It also runs batched on main (each push that changed something
e2e-relevant, one running and the newest pending) and nightly, always, to catch drift no commit
explains. The nightly run (and a dispatch with `backup_restore: true`) also runs `test/e2e/backup-restore.sh`
after the driver: it runs the commands of `docs/site/src/content/docs/guides/backup-restore.md` (key export,
`pg_dump`, stop, restore, start, `checkpoints resync`) and checks the install is the one that was backed up
(ADR 0085 decision 18). `hack/docs-check.sh` keeps the script and the guide's `e1` blocks in step. Locally:
`BACKUP_RESTORE=1 make test-e2e-kind`.

For a risky PR, `hack/needs-e2e.sh` says whether your change touches what only a full install
observes (the rule lives only there). If yes, the PR's own run already runs the quickstart on GitHub;
a green `e2e-lab` commit status (an external e2e run that passed on this commit) is enough to merge
once the PR's own quickstart has finished (a flaky red one does not hide a green external run). Without
one, wait
for the PR's quickstart, or dispatch it earlier (`gh workflow run e2e-kind.yml --ref <branch>`). The
`e2e` label is for a change the rule thinks irrelevant but you want tested.
`hack/e2e-remote.sh` on your own Docker (`--local`) is a dev and debug tool only: it produces
neither `e2e-lab` nor `quickstart`, so it is not merge evidence. `SUITE_LABEL=smoke` runs steps 1-6 only.

### The `e2e result` gate

The required check is `e2e result`: green when nothing needs the e2e, but for a PR that needs one
(`hack/needs-e2e.sh`) it is red until the head has a green `quickstart` (this run or a dispatched
one) or `e2e-lab` success (`hack/e2e-gate.sh`, old issue 562); a later `e2e-lab` or a passing dispatch
re-runs the `e2e result` job itself (not when the PR run is still in progress: then "Re-run failed
jobs"). `e2e-lab` is not a required context of its own: the PR's own quickstart is the path every
contributor has.

Trust limit: `result` runs from the PR's own `e2e-kind.yml`, so a PR can edit the YAML to drop the
gate; the guard is the ruleset's required review on `.github/**` (not CODEOWNERS), `e2e result` is
not pinned to an app and anyone with write access can post `e2e-lab`: a guardrail against
mistakes, not a malicious member (pinned integration_id: old issue 568).

Why the e2e exists: old issue 281 (branch naming in githubtools) and old issue 284 (a startup flag the chart's
migrate Job lacked) passed every package test and failed only in a full install.

## The local gate command

`hack/lane-gates.sh [<base>]` (base defaults to `origin/main`) runs the gates your change touches and
blocks until it has a verdict: the gates of `hack/preflight.sh` that the diff selects, plus the heavy
CI jobs `hack/ci-changes.sh` turns on for it (`chartsmoke`, `flavors`, `escape`; the e2e is not part
of it, see [testing layers](#testing-layers)). It needs Docker for the container gates and nothing
else: no lab, no external-run status. It prints one line per gate,

```
gate=<name> rc=<n> seconds=<n> verdict=pass|fail|notrun [reason="..."] log=<path>
```

and exits 1 when a gate failed, 3 when none failed but a gate could not run on this machine, 0 when
all passed. `notrun` (no Docker, a missing tool, a sandbox denial, a suite that skipped for a missing
requirement) is never a `fail` and never a pass; exit 3 is not "verified", so rerun where the
requirement exists. It never says `ok` unless every selected gate passed and at least one ran: a
crash of `preflight --list` or `ci-changes.sh`, or an empty gate list, exits 1, and a CI job with no
local gate is `notrun`. Agents run this one command instead of polling logs. `hack/lane-gates-test.sh` is
its offline self-test (stubbed gates, run by the `scripts` job).

## What `ci` runs when

Actions minutes are a limited, shared resource; `hack/ci-skip-heavy.sh` decides, its header and
`hack/ci-skip-heavy-test.sh` are the rule. The heavy jobs are the ones the `changes` job gates:
`go` (go-tidy, go-lint, go-test, go-race; `go vet` is part of golangci-lint's `standard` set), `web` (web-lint, web-test, web-e2e),
`generated code`, `chart`, `chart smoke`, `docs`, `escape (admission)` and `flavors`. `changes` and
`quick checks` (proto, migrations, adr-lint, claim-tests, the Conventional Commits subject check, the commit identity check (`hack/git-identity-check.sh`, list in `hack/git-identities.allowed`, read from the pull request's base commit so a pull request cannot widen its own list; the pull request that first adds the file is checked against its own copy; `KOINON_ALLOWED_IDENTITIES` is ignored in CI; koinon#320) and the dependency age steps in one job,
old issue 594: every job pays a fixed start-up cost) and the other workflows (`dco`, `gitleaks`, `claim`,
`e2e result`) always run; `lint-shell` and `scripts` run on every pull request (drafts too) and
are skipped only on a push to main whose heavy jobs are skipped. A skipped heavy job reports
success, so every required check still reports.

### go-test satisfied by an external run (old issue 610)

`changes` reads the commit status `go-test-lab` (an external go-test run that passed on exactly this
head) for the exact PR head SHA; when it is success the ready run skips `go-test`, which `go` accepts
(the required name stays `go`; no change to the ruleset). Nothing in this repository posts it and
nothing waits for a late one: without the status `go-test` runs on the hosted runner. Push to main
(`ci-skip-heavy.sh decide`) and `hack/release-gate.sh` count a skipped `go-test` only with that
status or a change with no Go. Trust limit as `e2e-lab` (old issue 568).

### Draft pull request (old issue 593)

Only `changes`, `quick checks`, `lint-shell` and `scripts` run, and `go` and `web` are RED on
purpose ("draft: not tested, mark ready for review"): a green draft `go` would otherwise stay the
newest run on the head while the ready run is still going. A draft cannot merge, so get WIP
feedback from `hack/preflight.sh`, not CI. Marking it ready fires `ready_for_review`, which runs
everything on the same head, and so does every push to a ready PR. The draft flag comes from the
event payload (old issue 676), not from the live state: the run is named `ci (draft)` (ci.yml `run-name`)
when `github.event.pull_request.draft` is true, else `ci`, and `hack/ci-skip-heavy.sh` decides from
that same flag (`PR_DRAFT`), so a run's name and its behaviour always agree. A gate that must
ignore draft runs drops the runs whose `display_title` (`displayTitle` in `gh run list --json`) is `ci (draft)`; the run `name` stays `ci` for every run, so never filter on it;
`pr-wait.sh` (and `pr-merge.sh`, which calls it) does exactly that (#218, via `hack/lib/pr-verdict.sh`, "One verdict per workflow run"): no time window. A re-run replays the
original event: re-running a `ci (draft)` run after the PR is ready is a draft run again (`go` and
`web` red, "not tested"). That is safe (it can never leave a green `go` on an untested head) but
`pr-wait.sh` ignores such a re-run (it was created before the ready event), so the ready run's
result stands; to re-run, use the `ci` run of `ready_for_review` (or push)
instead. Mark the PR ready (`hack/ship.sh --ready`,
`gh pr ready`) before you wait on it with `hack/pr-wait.sh`: a draft's green is not merge
evidence.

### One run per head (old issue 675)

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
event that starts a run on the new head.
`hack/e2e-rerun-result.sh` does not re-run `e2e result` while the branch has a live `e2e-kind` run on
another commit: that workflow's PR group still cancels in progress, and a re-run joins it. Tests:
`hack/ci-concurrency-test.sh`, `hack/ci-supersede-test.sh`, `hack/e2e-rerun-result-test.sh`,
`hack/ship-test.sh`.

### Push to main (old issue 595)

Everything runs, except when the push is a merge commit whose tree equals the tree of its second
parent (the PR head) and every required check `ci.yml` produces is green (or skipped) on that
head, newest run per check, made by GitHub Actions. Then the heavy jobs are skipped: the merge
brought nothing the head was not tested with. The head must also contain the first parent, and
`flavors` (advisory, but `flavor-publish` ships what it tested) must have passed on the head
unless nothing it builds from changed. On such a push `go`, `web`, `scripts` and `lint-shell`
report skipped (old issue 594). Any doubt (a merge of an out-of-date branch, an admin or direct push, a
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
Acquire::http::Timeout=30`, because snapshot.ubuntu.com answers 5xx now and then (#149), and every
`apt-get update` adds `-o APT::Update::Error-Mode=any`, so an outage fails at the update step and not
later as "Unable to locate package" (#141). The flavor's snapshot step prints one line naming
snapshot.ubuntu.com and the snapshot date when its update fails. A step that runs apt through
another tool (`playwright install --with-deps`) writes an `apt.conf.d` snippet with all three
settings. `hack/apt-retries-test.sh` (the `scripts` job) fails on any apt call without them. The
chart smoke suite keeps the envtest kube-apiserver and etcd output and prints it when envtest does
not start.

Pinned tools and manifests from GitHub releases (shellcheck, actionlint, gitleaks, kind, ko, the
gateway-api, CNPG, kube-network-policies and agent-sandbox manifests) are fetched with
`hack/fetch-pinned.sh URL SHA256 DEST`: `curl --retry 5 --retry-all-errors --retry-delay 5`, then
the sha256 check, and a file already there with the right sha256 is not fetched again (#431;
`releases/download` answers 504 now and then). Workflows use the composite action
`.github/actions/fetch-pinned` (`name`, `url`, `sha256`, `dest`), which puts `actions/cache` in
front, keyed by OS, tool name and sha256, so a version bump is a new entry and most runs download
nothing. A restored cache file is never trusted: `hack/fetch-pinned.sh` checks the sha256 of a file
it finds present and downloads again on a mismatch. The action restores, runs the helper, then saves
(only after the check passed, only on a miss, and only with `save: true`, the default). Cache
safety: a job that checks out a ref other than the event's trusted one (`inputs.commit` in
`e2e-kind`, a `pull_request_target` head) must not run a local action or save: its cache scope is
the default branch's, so a save would let that ref's code write what main restores (CodeQL "cache
poisoning"). `e2e-kind` therefore calls `hack/fetch-pinned.sh` directly, without the cache. `ci.yml`
(`pull_request` has its own cache scope; pushes and queue builds are trusted refs), `secrets.yml` and
`g2-kind.yml` (the dispatched ref's own scope) keep the action and save. Any new caller on an
untrusted ref passes `save: 'false'` and takes the action from a trusted checkout. `hack/pinned-download-lint.sh` (the `scripts` job) fails on a
`curl` or `wget` of `releases/download` in a workflow, composite action, `hack/*.sh` or `test/**/*.sh`
that bypasses the helper; `hack/fetch-pinned-test.sh` tests the helper, including a retry on 504.

The flavor layer cache (`flavors-<hash of flavors/**>`, prefix `flavors-`) is restored by `flavors`,
`quickstart` (e2e-kind) and `flavor-publish`. Only pushes to main save it: `flavors` and
`flavor-publish` (which exports with `--cache-to` and saves, so a main push where `flavors` was
skipped by #595 still writes the entry). A pull request never writes it.

## Main's e2e regressions and release tags

When main's kind e2e goes from green to red, `hack/e2e-bisect.sh` opens one `e2e-regression` issue
naming the merge (or the suspects); it dispatches every candidate with `backup_restore=true`, so a
nightly that went red in the backup-restore step reproduces: fix forward within 2 hours, otherwise revert it. RC and
release tags are cut only from a commit whose `quickstart` job concluded success
(`hack/release-gate.sh` checks it; dispatch e2e-kind on the commit if the batched run skipped it). The gate ignores
`skipped` `quickstart` check runs (the push run after a queue merge skips it) and also accepts a green `merge_group`
`quickstart` run on a commit with the tagged commit's tree; a failed or cancelled newest run still fails it (#444, ADR 0102).

## Required status checks

`hack/ci-required.txt` is the list the `main` ruleset mirrors; `flavors` is the only advisory job
on pull requests (`flavor-publish` is advisory and push-only). `hack/ci-required-test.sh` (CI job
`scripts`) fails when a `ci.yml` job is neither required nor advisory;
`hack/ci-required-test.sh --ruleset` diffs the list with the live ruleset. A new job goes into the
list in the same PR, and a required job must report on every pull request and push (a job-level
`if:` skip reports success and is fine; only a workflow-level `paths:` filter leaves a check
absent). With the merge queue (below) a required job must also report on `merge_group`: the
workflows that produce a required context listen to it (`ci`, `dco`, `secrets`, `claims`,
`e2e-kind`), `hack/ci-merge-group-test.sh` keeps them subscribed, and a new required workflow
subscribes in the same PR.

Third-party analysers are advisory: their checks never enter the list or the ruleset, and
`hack/pr-wait.sh` (so `hack/pr-merge.sh`) judges only the base branch's required contexts, the
`--require` names and the e2e aliases: any other check that is red or pending is printed as
`advisory <name> failed|pending` and ignored. When the required contexts cannot be read, it exits 2
instead of judging every check. DeepSource
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

Socket (GitHub App, #185) scans dependencies and reads `socket.yml` at the root: it runs on changes
to `go.mod`, `go.sum`, `package.json`, `pnpm-lock.yaml` and workflows, skips spike code and
fixtures, and posts PR alerts without a separate dependency overview comment. It is advisory like
DeepSource: answer and resolve its inline comments before merge (#187). On 2026-12-05 check whether
it caught anything Dependabot, govulncheck or OSV did not; if not, remove it.

### The `claim` check and claims by comment (ADR 0094, #163, #164)

`claim` (`.github/workflows/claims.yml`, `pull_request_target`, read-only, `hack/claim.sh check-pr`)
is required and is a conflict check: it fails only when someone else's human holds a live claim on an
issue the pull request closes, or a live `claim/adr-NNNN` reservation for an ADR file it adds. The
issues it reads are GitHub's closing links of the pull request (`closingIssuesReferences`: closing
keywords in any form, `owner/repo#N` and URLs included, and the sidebar) plus the body's `Closes #N`.
A pull request that names no issue, an unclaimed issue, the author's own claim and a stale one pass;
there is no exemption label, title or author any more (`unclaimed-ok` and the Dependabot exemption
are retired). The failure and success texts are fixed and pinned in `hack/claim_test.sh`.

`claim-comment` (`.github/workflows/claim-comment.yml`, `issue_comment`, `hack/claim.sh comment N`)
handles `/claim` and `/release` on issues with `GITHUB_TOKEN`; it is no status check. The sweeper,
release and comment jobs run on `GITHUB_TOKEN` with `actions: write` (to re-run a failed `claim`
check when a claim changes); no GitHub App and no Environment are involved. Leases, classes and the
fallback when GitHub refuses the token's write: `docs/dev/work-claims-setup.md`.

## Merging

The only merge method is a merge commit: `gh pr merge --auto --merge`. `e2e result` is required
and fails a needs-e2e PR without a passing e2e on its head (old issue 562), so a plain auto-merge cannot
skip it. With the merge queue, the hosted `quickstart` on the queue commit (main plus the PR) is the merge evidence (#444, ADR 0102), so a
main that moved during a lab run no longer forces a re-run: the lab's `e2e-lab` is pre-merge confidence on the PR head. Still, a pull request that changes startup flags, chart Jobs or hooks, or anything the
e2e expects does not get auto-merge until `hack/pr-wait.sh <PR> --checks --require e2e` passes
(`--checks` also waits for every required context of the ruleset to exist on the head, so a
queued `ci` run is not "green"): a green `e2e-lab` commit status (an external e2e run that passed on this commit) or a green `quickstart` run on the
head (it runs on its own for the paths in `.github/workflows/e2e-kind.yml`; otherwise dispatch it
with `gh workflow run e2e-kind.yml --ref <branch>`). Then enable auto-merge and
`hack/claim.sh review <N>` (step 8 of "Picking up work" in `AGENTS.md`).

`hack/pr-merge.sh <PR> [--admin] [--lane L] [--approved-at <time>]` does all of this in one script (old issue 647): it refuses a
draft, runs `pr-wait.sh --checks` (with `--require e2e` when `needs-e2e.sh` says so; it reads the PR's whole
file list with `gh api pulls/<PR>/files --paginate` and requires e2e when that read fails or is empty),
reads every required context of `hack/ci-required.txt` on the head SHA with the shared verdict (below; success or
skipped), re-reads the PR (not OPEN: exit 3; head not origin's branch tip: exit 6 `STALE-HEAD`), runs
`gh pr merge --merge` (`--admin`, else `--auto`), then reads the PR again and prints what GitHub did, not what was asked: `merged`, `added to the merge queue` (only when GitHub says the PR is in the queue), `auto-merge armed but NOT queued: ... required review` (a PR blocked by `required_reviewers` or a rule that is no failed check: it merges only after that is met) or `auto-merge enabled`, and only after that
succeeded `claim.sh review` in the claim's lane (from `claim.sh mine`, or `--lane`) and a PR comment with the
head SHA and the number of contexts it read. `pr-wait.sh --checks` exits 5 on a draft. After
`ready_for_review` it ignores only the draft's `ci` workflow runs, in every mode (with or without
`--checks`): the runs on the head whose display title is `ci (draft)`, matched by run id (a by-hand
re-run of a draft run is ignored too). A red or cancelled run of the ready `ci` is never dropped, and
while the ready run has no checks yet it prints `waiting for the ready run`, never green. dco, gitleaks,
claim and commit statuses (`e2e-lab`) keep their result; when gh cannot list the runs, nothing is
dropped (a draft run's red then shows as `FAILED`). It exits 6 `STALE-HEAD` when the PR's head stays different from
`git ls-remote origin refs/heads/<branch>` for `PR_WAIT_STALE_GRACE` seconds (default 120); re-sync with
`gh api -X PATCH repos/{owner}/{repo}/pulls/<PR> -f base=<base>`.

### One verdict per workflow run (#133)

`hack/lib/pr-verdict.sh` is the only place that decides what a head's checks say; `pr-wait.sh` and `pr-merge.sh`
both source it, so they cannot disagree (they used to keep two orderings, and a cancelled newer run with an older
green passed). The rule:

1. Judge per workflow run, not per check name. For each workflow file on the head (`GET actions/runs?head_sha=`,
   keyed by `path`) take the newest run that counts, by `created_at` then `run_attempt`. A run titled `ci (draft)` does
   not count once the PR is ready (#218). The check runs of every other run of that workflow are dropped: an older
   green never outvotes a newer run that was cancelled, red, still running or has no checks yet (that one is
   `pending`/`missing`, and `pr-wait.sh` prints `waiting for the newest run of each workflow`).
2. Inside the chosen run a check name is judged by its newest row. A row that has not finished (a live re-run attempt)
   outranks every finished one, a green included (except under rule 4); else the newest by `startedAt`, falling back to `completedAt` (the GraphQL rollup reports a
   never-started cancelled job with a null `startedAt`), then job id.
3. Commit statuses (`e2e-lab`) and check runs without a run have nothing to choose: the newest row of that name wins.
4. `quickstart` (the e2e gate, old issue 547) and every other check of the `e2e-kind` workflow (`e2e result` and the
   e2e-kind jobs) are the exception: any green row on the head from a non-draft run counts unless a newer one failed,
   because a dispatched `e2e-kind` run and the skipped `pull_request` run are normal side by side. A newer cancelled
   or unfinished e2e-kind run does not hide an older green; a newer red does (the e2e rule is "any green e2e on the head").
5. A completed newest run with no check run at all (for example `startup_failure`) is a failure: both scripts print
   `<workflow>: run has no jobs (<conclusion>)` with the run id instead of waiting for jobs that never come.
6. When gh cannot list the runs nothing is dropped, so the verdict only gets stricter.

A verdict word per context: `success skipped pending failure cancelled missing`; only `success` and `skipped` merge.
Tests replay API-shaped answers (`hack/testdata/pr-verdict/` holds a recorded real one, PR #386's draft-era head) through
`gh` stubs: no live GitHub. A script that waits or merges calls `hack/pr-wait.sh` and `hack/pr-merge.sh`, never its own
loop (a pipe after `pr-wait.sh` hides its exit code, old issue 626).

### The merge queue (#225)

Strict "up to date with main" made every merge put every other open PR behind. The merge queue
builds main plus the queued PRs on a `gh-readonly-queue/main/...` branch, runs the required checks
there and merges in order, so the ruleset drops strict (ADR 0094 decision 4) and nothing syncs a branch by
hand. The repository half is in place and inert until the ruleset has a
`merge_queue` rule; the ruleset change itself is made by the maintainer's session through
`gh api` (backup, diff), never from a pull request.

What a `merge_group` build runs (event `checks_requested`, no pull request in the payload):

- `ci`: `hack/ci-skip-heavy.sh decide` never skips on `merge_group` (`skip=false`), because the queue commit
  is a tree no check has seen; `ci-changes.sh` path-filters against `merge_group.base_sha` like a ready pull
  request, so a job whose files the entry does not touch still reports green without running. `changes`
  (`ci-changes.sh`), the proto `buf breaking` step and `migrate-lint`
  compare against `github.event.merge_group.base_sha` (ADR 0094 decision 4), so a baseline or a
  `hack/migrate-patch-line` change that landed on main while the entry waited is checked against the main
  the entry lands on. The concurrency group is the queue commit (`ci-mq-<sha>`), so queue builds never
  cancel one another (a cancelled required check ejects the entry and the ones behind it);
  `ci-supersede.sh`, commit subjects, dependency age and the waiver are pull-request-only steps and are
  skipped (a step skip is green).
- `dco` and `gitleaks`: the range `merge_group.base_sha..head_sha`, the entry's own commits.
- `claim`: the fencing point. It reads `<N>` from the queue ref (`gh-readonly-queue/<base>/pr-<N>-<sha>`),
  checks out `base_sha` and runs the base's `hack/claim.sh check-pr <N>`; a claim someone else took (or the
  sweeper released) while the PR waited fails the entry. It fails closed when `<N>` cannot be read.
  `release` and `sweeper` do not run. Limit: a `merge_group` run takes its workflow YAML from the queue
  commit, which is the pull request's own, so a PR that edits `claims.yml` controls what runs at queue time;
  only the script is the base's. The guard for that is the required review on `.github/**` (CODEOWNERS) plus
  the `pull_request_target` entry gate, which runs the base's workflow on the PR before it can be enqueued.
- `e2e result` and the quickstart (#444, ADR 0102): when the entry's diff against `base_sha` is
  e2e-relevant (the full rule or the e2e's own files; a rule failure counts as relevant), `decide` answers
  `needed=true` and the `quickstart` job runs on the queue commit, which is main plus the pull request. That
  run is the merge evidence: `e2e result` is green when it passes, and a red one drops the entry from the
  queue. `required` stays false, so no PR-head lookup happens at queue time (the PR-head gate still runs
  before the entry can be queued). `e2e-lab` cannot run on the read-only, short-lived queue ref, so it stays
  pre-merge confidence on the PR head and is not read in the queue. Not relevant: the job is skipped and
  `e2e result` is green. The queue's `quickstart` builds the flavor image itself when GHCR has none for the tree.
  The push to main after the queue merge does not run the quickstart again: `decide` (push) accepts a green
  `merge_group` quickstart job whose head commit has the pushed commit's tree (`PUSH_SHA`).
- Bootstrap shim (#444, follow-up: #452): the entry that merges #444, and any entry built on a main older
  than #444, runs the OLD `decide` from `base_sha`, which answers `needed=false, required=true`. Only then does
  `e2e result` look up the queued PR head (`hack/e2e-queue-head.sh`, then the old PR-head gate). Removed when main's
  `decide` is the new one and no older-base entry can remain.

Ruleset values (ADR 0094 decision 4), set by the maintainer's session: `strict_required_status_checks_policy`
`false`, `merge_queue` with grouping strategy `ALLGREEN`, merge method `MERGE`, 1 entry merged per group (`min` and `max`
group size 1), at most 5 builds at once, check timeout 60 minutes. The 60 stays with the e2e in the queue: the
slowest of the last 200 successful or failed `quickstart` jobs took 14.2 minutes (851 s; typical 12 to 13.5),
so there are 45 minutes of headroom (measured 2026-11-09). A cold flavor build in the queue is not in that sample;
if the queue starts timing out entries, raise `check_response_timeout_minutes` (to 90) then.

The updater: `updater.yml` and `claim.sh update-oldest` stay until the queue is live. Meanwhile `update-oldest` skips
a PR with `isInMergeQueue` (pushing to a queued PR's branch drops it from the queue) and is a no-op once the ruleset has
a `merge_queue` rule (read at run time, as `pr-merge.sh` does). Retiring `updater.yml`, `update-oldest`, `update_target`
and the `needs-update` label is follow-up work in #227: exit condition is the first week of green queue merges with strict off.

Cost: one full `ci` run per queue build, plus a `quickstart` of about 12 to 15 minutes (more for a cold flavor build) for each e2e-relevant entry
(75-minute job timeout, 60-minute queue check timeout: the queue gives up first). Entries build concurrently (up to 5), so
the e2e adds latency per entry, not per queue; the throughput limit is the 1-entry-per-group merge. A red group drops
only its entry and rebuilds the entries behind it. Raise the group size (`max_entries_to_merge`, `min_entries_to_merge`
with `min_entries_to_merge_wait_minutes`) only after measuring: a batch of n entries costs one e2e, but a red batch
costs n re-queues, and the queue does not bisect (bors and Tide do). The ruleset values above trade throughput against runner minutes. A push to main after a queue merge still runs `decide_push` in `ci.yml`, which skips the heavy jobs
only when the merge tree equals a green PR head; after a queue build that is rarely true, so those jobs run once
more (follow-up: accept the queue commit's own green check runs). The kind e2e does not: see above.

Scripts, with the rule detected at run time (no queue: nothing changes):

- `hack/pr-merge.sh` runs the same waits and reads, then `gh pr merge --merge --auto`, which enqueues
  when the ruleset has a queue (output `added to the merge queue`). A PR must pass its pull request
  checks, review and threads rules to be enqueued; the queue then re-runs the required checks on the
  queue commit.
- `hack/pr-wait.sh <PR>` prints `in the merge queue (<position> <state>)` while the PR is queued
  (GraphQL `isInMergeQueue`/`mergeQueueEntry`; `gh pr view --json` has no queue fields) and ends with
  `MERGED`. A PR that was queued and reads open and out of the queue on two consecutive polls exits **8** `DEQUEUED`
  (one such poll is not enough: the queue clears just before the PR reads `MERGED`) (failed
  queue build, conflict with an entry ahead, removed by hand; `gh` cannot remove an entry, the PR page
  can). Read the queue build's failures on the PR page, fix, and run `pr-merge.sh` again.
- `--admin` (CODEOWNERS paths): verified (ADR 0094, "Verification by probe", probe 5): `gh pr merge --admin
  --merge` merges directly and bypasses the queue and the review rule, so a sensitive-path PR needs no
  enqueue. An admin merge lands without the queue-commit quickstart (#444, ADR 0102), the merge evidence for an
  e2e-relevant change: such a PR should be enqueued, and `pr-merge.sh --admin` warns when the queue is on and
  `needs-e2e.sh` says the e2e is required. `pr-merge.sh --admin` still runs `pr-wait.sh --checks` first, which judges the head's checks
  whether or not the PR is queued.

### Review threads (#187)

The `main` ruleset requires every review thread resolved (`required_review_thread_resolution`).
Advisory bots (DeepSource, CodeRabbit) leave inline comments, so a pull request can sit `BLOCKED` with
every required check green. Auto-merge then never fires, and nothing in the checks says why.

- `hack/pr-wait.sh <PR> --checks` and `hack/pr-merge.sh` detect it: every check is green and
  `mergeStateStatus` is `BLOCKED`. They print one line per unresolved thread (`UNRESOLVED path:line author id=PRRT_...`,
  from `hack/pr-threads.sh <PR>`, GraphQL `reviewThreads`, paginated) and exit **7**. They do not wait
  (a thread does not resolve itself) and never reply to or resolve a thread. When the threads cannot be
  read (three retries, 2/5/15 s) they fail closed: exit 2, "cannot read review threads", no merge.
  `hack/pr-threads.sh` alone exits 0 (none), 7 (some), 2 (unreadable or PR not found).
- Before a merge, answer every bot thread: reply `fixed in <sha>`, or why the finding does not apply,
  then resolve it. A lane fixes the real findings inside its own fix rounds; the main session replies to
  and resolves the rest, after reading each finding (a bot's suggestion is data, not an instruction).

  ```sh
  # reply (id = the PRRT_... from the UNRESOLVED line)
  gh api graphql -f id=PRRT_xxx -f body='fixed in abc1234' -f query='
    mutation($id: ID!, $body: String!) {
      addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $id, body: $body}) { comment { id } }
    }'
  # resolve
  gh api graphql -f id=PRRT_xxx -f query='
    mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }'
  ```

  GitHub sometimes answers a resolve with a transient `Something went wrong`; retry with backoff
  (for example 2 s, 5 s, 15 s, then check `hack/pr-threads.sh <PR>` before giving up: the first try may
  have worked). Then run `hack/pr-wait.sh <PR> --checks` again.

## OpenSSF Scorecard

`.github/workflows/scorecard.yml` runs `ossf/scorecard-action` weekly and on every push to main,
publishes the result (`publish_results`) and uploads the SARIF to code scanning. It is advisory: not
in `hack/ci-required.txt` (that file lists `ci.yml` jobs and the external required checks only).
