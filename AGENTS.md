# AGENTS.md

Instructions for coding agents working on Koinon.

## Commands

- Build: `make build` · Test: `make test` · Lint: `make lint` · Codegen: `make generate`
- Go only: `hack/go-test.sh` (what `make test` runs; Postgres tests need Docker and skip without it;
  exits 3 when an expected requirement was missing). One package: `go test ./internal/x` or
  `GOFLAGS=-tags=testing go tool ginkgo -p ./internal/x` (the tag gives kopia an instant test key
  derivation; without it kopia specs are several times slower under `-race`; `hack/go-test.sh` sets it).
  A new suite joins the parallel list in that script once it shares
  no database, fixed port or fixed path between processes. `test/harnesses`: `make test-harnesses`.
  Details: `docs/dev/ci.md#go-test-suites`.
- Web only: `cd web && pnpm lint && pnpm typecheck && pnpm test`
- Docs site: not in this repository (ADR 0108); it is `stateracloud/koinon-docs`, live at
  <https://koinon.stateracloud.com>. The quickstart and backup commands live in `test/e2e/commands/*.e1`;
  `test/e2e` runs them and `make e2e-commands-check` checks them. Edit the file; the docs repository's CI
  compares its pages with it at the release it names.
- After changing `.proto` files or `internal/db/queries/*.sql`, run `make generate` and commit the
  output. CI fails if generated code is stale.
- Chart: `hack/chart-check.sh`. Add an assertion for every chart behaviour you change, in the matching
  `hack/chart-check.d/NN-<feature>.sh`. Anything that changes how a Koinon binary starts (a flag or
  env var it requires, a mount, a Secret key, an RBAC rule) is covered by `make test-chart-smoke`;
  extend it when you add a component or a startup input (old issue 284). Details: `docs/dev/ci.md#chart-and-admission-tests`.
- Sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`): `make test-admission`; its
  OpenShell cases run on kind: `make dev-kind-up`, then `make test-escape-kind`.
- Local cluster: kind is the dev loop (`make dev-kind-up`, `make dev`, `make test-e2e-kind`). Nothing
  else (no registry, no secret store) is needed; see CONTRIBUTING.md and `docs/dev/loop.md`.
  Real-model runs on your own cluster and budget: `docs/dev/real-model-tests.md`.
- Claims: `hack/claim.sh help`; `hack/claim_test.sh` (offline). ADRs: `hack/adr-lint.sh`, `hack/adr-index.sh`.
- Before you hand over or push: `hack/lane-gates.sh` (blocks; the gates your change touches, one
  `gate=… verdict=pass|fail|notrun` line each, `docs/dev/ci.md#the-local-gate-command`) or
  `hack/preflight.sh` (the diff-selected gates without the heavy jobs).
  Ship a branch with `hack/ship.sh` (sign-off, blob and gitleaks checks, merge of `origin/main`,
  push, PR, board, optional auto-merge). Wait on a PR with `hack/pr-wait.sh`
  (in the background) and merge it with `hack/pr-merge.sh <PR>` (refuses a draft, reads every required context on the head); read why CI failed with `hack/ci-fail.sh <PR>`. Worktrees: `hack/wt.sh`.
- Migrations: `hack/migrate-lint.sh origin/main` (merged migrations unchanged, numbers contiguous;
  also the patch-line rule under Conventions, from the `hack/migrate-patch-line` marker);
  after merging main, `hack/migrate-renumber.sh` renumbers your branch's migrations.
- CI and e2e (details: `docs/dev/ci.md`). Mark the PR ready (`hack/ship.sh --ready`, `gh pr ready`)
  before you wait on it with `hack/pr-wait.sh`: a draft runs nothing in CI (its `go` and `web` report
  "draft: not run") and its green is not merge evidence; get WIP feedback from `hack/preflight.sh`.
  [Testing layers](docs/dev/ci.md#testing-layers) · [what `ci` runs when](docs/dev/ci.md#what-ci-runs-when)
- Kind e2e: `hack/needs-e2e.sh` says whether your change needs it. The merge queue's `quickstart` on
  main plus the PR is the only e2e proof (ADR 0105). A pull request runs it only with the `e2e` label
  (advisory; #572). `flavors` is required but runs in the queue and on main, not on pull requests. `hack/e2e-remote.sh` (lab or `--local`) is dev feedback, not merge evidence: [testing layers](docs/dev/ci.md#testing-layers).
- When main's kind e2e goes red, fix forward within 2 hours or revert the merge (`hack/e2e-bisect.sh`
  opens an `e2e-regression` issue). RC and release tags are cut only from a commit whose `quickstart`
  job succeeded (`hack/release-gate.sh` checks it).
- A new `ci.yml` job goes into `hack/ci-required.txt` (or is listed advisory) in the same PR; a
  required job must report on every PR, push and merge_group build (no workflow-level `paths:`).
  ([required checks](docs/dev/ci.md#required-status-checks))
- Adding or changing a CI check: read `docs/dev/ci-rules.md` first (rules R1-R8, limits L1-L6 of ADR 0111). Its first
  section says when it does not apply. `hack/ci-rules-check.sh` checks the rules a script can decide.
- `go mod tidy` must leave `go.mod` and `go.sum` unchanged; CI checks it.
- Before pushing: `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .`. CI scans
  every commit of a pull request, so a leak fixed in a later commit still fails.

## Picking up work

Claim an issue before you work on it. The lock is the ref `claim/<N>` (ADR 0082, reduced by ADR
0104): creating it is a compare-and-set, so two lanes cannot hold one issue. A claim has no lease and
no heartbeat; it is held until a pull request that closes the issue merges, or you release it. Use `hack/claim.sh` (gh and jq only). The holder is `<login>#<lane>`, so a restarted session keeps
its claims; the default lane is `main`.

1. `hack/claim.sh next` (or `next --label start-here`): Ready, `agent-ok`, current-milestone,
   unblocked issues that nobody holds, by Priority then age. Only humans move issues to Ready; never
   start an issue that is not listed unless your human names it. Then the overlap check (below).
2. `hack/claim.sh take <N>` (exit 2: pick another issue). One claim per lane; the per-human cap is off while one human works on the repository (ADR 0104 amendment); a
   PR that closes several issues takes them together, `take N M` (all or nothing). It assigns your
   human.
3. Push `<type>/<N>-<slug>` and open a draft PR with `Closes #N` (the board's built-in workflow moves
   the issue to In Progress). New work you discover becomes a Backlog issue; do not start it.
4. Finish: mark the PR ready and merge it (see "Merging"). The merge releases the claim
   (`claims.yml`); a PR closed without a merge needs `hack/claim.sh release-pr <PR>`. Never delete a
   claim branch.
5. Stop early: `hack/claim.sh release <N> --note "<state, branch, next step>"` (keep the branch and
   draft PR), or `hack/claim.sh block <N> <M>` when another issue blocks it. Exit 3 means the claim
   is not yours (another lane: `--lane L`, or `--force` for your own other lane; another human: ask
   them).

Other rules:

- A branch is merged with `origin/main` only when GitHub reports a conflict (`hack/pr-wait.sh` exit 4,
  or the merge queue drops the entry, exit 8). There is no updater bot and no `needs-update` label. For an `--admin` merge, merge
  `origin/main` and wait for green first (it skips the queue, which would have tested on main's tip).
- Releases: either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay
  procoduck's. Tags are immutable by ruleset: fix a bad tag with the next rc
  (`docs/dev/releasing.md`).
- Copilot coding agent pull requests are opened or adopted by a human.

### Several agents in one session (lanes)

A session that runs agents in parallel gives each one a lane: the parent takes the claim with
`hack/claim.sh take N --lane L`, creates the lane's worktree with `hack/wt.sh new`, and hands the
agent the issue, the worktree and `docs/agents/subagent-brief.md`. One issue per lane. Pick the model
from the brief's "Which model" section. The parent ships (`hack/ship.sh`) and releases; agents never
call `claim.sh`. `hack/claim.sh mine` lists your claims with their lanes. Two sessions in one lane
are one holder; the lock does not separate them. Each session that runs at the same time as another
one uses its own `--lane`. Each lane tests its own worktree: `hack/e2e-remote.sh` tests the
checkout it is run from, so run it from the lane's worktree.

## Working alongside another human

One human works on the repository today (ADR 0104); these rules hold when a second one joins. Before
you start, after `take`:

- **Overlap check.** `gh pr list --state open --json number,author,headRefName,files` and the claim
  refs (`gh api repos/{owner}/{repo}/git/matching-refs/heads/claim/`; one claim: `hack/claim.sh show
  <N>`). If an open PR or another human's claim touches the same files with the same behaviour, run
  `hack/claim.sh block <N> <M>` or ask your human. If it only appends to a shared list or adds a
  sibling file (a new ADR, test, migration, chart assertion), proceed and say so in the PR.
- **Merge `main`, never rebase** a pushed branch. Merge `origin/main` only on a conflict
  (`pr-wait.sh` exit 4, merge queue exit 8); for an `--admin` merge, merge
  `origin/main` and wait for green first.
- **Conflict classes.** Mechanical (generated code, the ADR index, golden files, a migration number):
  regenerate or renumber. Append-site (both sides add lines to one list): keep both and run the
  check for that area. Semantic (the same function or behaviour) with the other human's open or
  last-7-days work: stop, comment on both PRs, ask your human.
- **ADR numbers:** reserve first with `hack/claim.sh adr` (prints `NNNN`), then write the file; run
  `hack/adr-index.sh`, never edit the index table. **Migration numbers:** pick them late; after
  merging main run `hack/migrate-renumber.sh` if `hack/migrate-lint.sh` complains.
- **Chart assertions** go in the owning file of `hack/chart-check.d/`; a new feature adds a new file.
- **No unrelated dependency bumps** in a feature PR. **Proto:** only add fields, never reuse or
  renumber; `make breaking` must pass. **Golden files:** regenerate, do not hand-edit.
- **Stop and ask your human** when: the change touches a sensitive path the other human owns
  (`.github/CODEOWNERS`; either human approves the other's PR there), you would change a public API
  or data model another open PR also changes, you need to rebase or force-push, a test only you
  changed starts failing elsewhere, or the issue turns out bigger than its claim.
- **Keep branches under a day.** Split the issue and release the rest instead of holding a claim for days.

## Conventions

- Decisions live in `docs/adr/` (ADRs are canonical; `docs/design.md` is the overview). Read the
  relevant ADR before changing architecture; write a new one for new decisions.
- Commits: Conventional Commits, always signed off (`git commit -s`) by a human. AI use is disclosed in
  the PR template's "AI use" field; an `Assisted-by:` trailer is optional (CONTRIBUTING.md).
- The module path (`github.com/stateracloud/koinon`) and API domain (`koinon.dev`) are defined once in
  `internal/project`; never hard-code them elsewhere in Go.
- Migrations are forward-only goose files `internal/db/migrations/NNNNN_name.sql`. Never edit a
  merged migration. Tables owned by an org are keyed by `(org_id, id)` with composite foreign keys.
- **Patch-line migrations (ADR 0085, from v0.3: ADR 0099).** There is no schema or upgrade promise
  before v0.3: until the marker names a line (it is `none` until v0.3.0 ships), migrations may
  rename and drop, on `main` and on `release/*` branches. Within a shipped minor line
  (`hack/migrate-patch-line` names it, e.g. `v0.3`) the old and the new binaries share the schema,
  so migrations only expand: no `RENAME`, `DROP COLUMN`, `DROP TABLE`, `DROP TYPE` and no column
  type change. Add the new column or table, keep the old one working (see `00014` and `00036` for
  the pattern), and contract in the next minor. `main` may contract only while the marker is `none`,
  i.e. after the change that opens the next minor. `hack/migrate-lint.sh` enforces this from the
  marker at the merge base or `MIGRATE_LINT_PATCH_LINE=1`; a `release/*` base alone does not turn it
  on. When to flip the marker: `docs/dev/releasing.md`.
- Every source file starts with `SPDX-License-Identifier: Apache-2.0`.
- Tests: Ginkgo/Gomega and testcontainers-go in Go, Vitest in `web/`. CI never calls paid models:
  use the fake model server. Fake secrets in tests are obviously fake and low-entropy
  (`koinon_test_not-a-real-token`) from the first commit: gitleaks scans every commit of a pull
  request, so a fixture that looks real and is fixed later still fails. Declare what a test needs from the machine with `testenv.Require` (in BeforeSuite or a spec, never before RunSpecs); CI sets `KOINON_REQUIRE` to turn those skips into failures; `KOINON_TESTENV_UNAVAILABLE=docker` simulates a machine without Docker.
- Dependencies are at least 3 days old before they enter the tree, security fixes excepted with
  checks (ADR 0079; `hack/dep-age-check.sh origin/main`).
- Chart renders are deterministic: no Helm `lookup` and no random values in templates. Generated
  secrets come from the ensure-secrets hook Job, with one exception: koinon-api creates the KEK Secret
  on its first start (ADR 0056, amendment 2026-11-11), because only it can see the database and
  refuse a new key when encrypted values exist; a native ValidatingAdmissionPolicy guards that
  Secret. Objects the reconciler owns are created by the reconciler. Every image reference goes through `koinon.imageRef` and every pod spec gets
  `koinon.imagePullSecrets` (ADR 0061). Values files without an OpenShell client TLS Secret must
  set `openshell.insecure: true`.
- Metric labels never carry org, agent, user, ticket or run IDs; use fixed label sets (ADR 0023).
- Every model with a `cache_read` price in the catalog runs with prompt caching on; there is no off
  switch (ADR 0068).

## Working rules

Lessons that cost time more than once. They apply to humans and agents alike.

- **Explicit cluster, always.** Pass `KUBECONFIG=<file>` or `--context` to every kubectl, helm,
  tilt, kind and test command you run, and to every agent or subagent you start. Never rely on the
  default context: it may point at a shared or real cluster. Tests and subagents that must not touch a
  cluster run with `KUBECONFIG=/dev/null`.
- **agentgateway log and tracer fields never read `response.body` or `bodyPrefix`** (CEL in the
  gateway config). Any expression that reads a response body makes agentgateway buffer and record every
  response (up to 32 MiB on an LLM route). Use the status, the headers and the parsed LLM fields.
- **OpenShell exec stdin is capped.** More than about 4 MiB of stdin that the command does not read
  makes `exec` exit 74. Code that restores or uploads data through exec must chunk it and have the
  command consume each chunk.
- **Merging.** The only merge method is a merge commit through the merge queue, and
  `hack/pr-merge.sh <PR>` does it: it refuses a draft, runs `hack/pr-wait.sh --checks`, reads every
  required context on the head, then enqueues and comments on the PR. It does not call `claim.sh`: the push to
  main releases the claim (`claims.yml`, `release-merged`). Do not hand-write the wait or the merge.
  `--jump` enqueues at the front. `--admin` is for a PR that changes at least one `.github/**` path (owner
  decision, ADR 0105); an e2e-relevant one also needs a green `quickstart` on its head. `--break-glass "<reason>" --approved-at <time>` is for an emergency the owner approved; it
  opens a `break-glass` issue. Details: `docs/dev/ci.md#merging`.
- **Root causes, not workarounds.** When you hit the same workaround twice, find out why and fix the
  cause. An interim fix is allowed only with an issue that names the exit condition (when it can be
  removed), linked from the code or doc that carries it.
- **OpenShell upstream (NVIDIA/OpenShell).** Issues need a first-hand human user story, a repro that
  uses only an OpenShell deployment, and OpenShell-only framing. No AI attribution (no
  `Co-Authored-By`, no "generated with" footer) in anything posted upstream. A pull request needs a
  maintainer's vouch first. A human posts upstream, in their own words; an agent drafts.
- **Non-interactive shell.** No pagers and no prompts: `git --no-pager`, `GH_PAGER=cat`,
  `PAGER=cat`, `gh ... --json` or `--jq` instead of tables. Use the real tool, not a shell
  alias (`/bin/ls`, not an `ls` that is really `eza`), and do not assume your human's aliases or
  interactive shell setup exist.

## Agent skills

### Issue tracker

GitHub issues in `stateracloud/koinon` on the org board; work is taken by claim (`hack/claim.sh`).
See `docs/agents/issue-tracker.md`.

### Triage labels

The five triage roles map onto Koinon's labels (`ready-for-agent` is `agent-ok`, `ready-for-human`
is `human-only`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` at the root and `docs/adr/`. See `docs/agents/domain.md`.

## Principles

- Favor the long-term, sustainable design: build foundations generic enough to extend, and take no
  shortcuts or stopgaps presented as the default. Koinon isn't public until it's polished, so there's
  time to do it properly.
- When you offer options, the recommended one is the sustainable one; name what a cheaper option
  costs later.

## Hard rules

- **Never Kyverno.** Admission policy is native ValidatingAdmissionPolicy only.
- **No secrets in sandboxes.** Credentials stay in OpenShell provider records, agentgateway or the
  secret store; never put them in sandbox env, images, home volumes or logs. The only exception is
  ADR 0044 and it is gated.
- No CNI-specific features (no Cilium dependency).
- Don't weaken OpenShell defaults from ADR 0045 (`enforcement: enforce` in every tier) or the
  isolation tiers of ADR 0075 (the Kata runtime class is required for the `vm` tier).
