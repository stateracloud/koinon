# AGENTS.md

Instructions for coding agents working on Koinon. Humans: [CONTRIBUTING.md](CONTRIBUTING.md).

**Maintainers only:** claims, lanes, shipping, the merge queue, the lab and releases are in
[docs/maintainers/agent-workflow.md](docs/maintainers/agent-workflow.md). A contribution needs none
of it.

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
- ADRs: `hack/adr-lint.sh`, `hack/adr-index.sh`.
- Before you hand over or push: `hack/lane-gates.sh` (blocks; the gates your change touches, one
  `gate=… verdict=pass|fail|notrun` line each, `docs/dev/ci.md#the-local-gate-command`) or
  `hack/preflight.sh` (the diff-selected gates without the heavy jobs).
  Maintainers ship, wait and merge with their own scripts:
  [maintainer commands](docs/maintainers/agent-workflow.md#commands).
- Migrations: `hack/migrate-lint.sh origin/main` (merged migrations unchanged, numbers contiguous;
  also the patch-line rule under Conventions, from the `hack/migrate-patch-line` marker);
  after merging main, `hack/migrate-renumber.sh` renumbers your branch's migrations.
- CI and e2e (details: `docs/dev/ci.md`). A draft PR runs nothing in CI (its `go` and `web` report
  "draft: not run") and its green is not merge evidence; mark it ready (`gh pr ready`) for CI, and get WIP feedback
  from `hack/preflight.sh`.
  [Testing layers](docs/dev/ci.md#testing-layers) · [what `ci` runs when](docs/dev/ci.md#what-ci-runs-when)
- Kind e2e: `hack/needs-e2e.sh` says whether your change needs it. The merge queue's `quickstart` on
  main plus the PR is the only e2e proof (ADR 0105). A pull request runs it only with the `e2e` label
  (advisory; #572). `flavors` is required but runs in the queue and on main, not on pull requests.
  Remote and lab runs, and what happens when main's e2e goes red:
  [maintainer commands](docs/maintainers/agent-workflow.md#commands).
- A new `ci.yml` job goes into `hack/ci-required.txt` (or is listed advisory) in the same PR; a
  required job must report on every PR, push and merge_group build (no workflow-level `paths:`).
  ([required checks](docs/dev/ci.md#required-status-checks))
- Adding or changing a CI check: read `docs/dev/ci-rules.md` first (rules R1-R8, limits L1-L6 of ADR 0111). Its first
  section says when it does not apply. `hack/ci-rules-check.sh` checks the rules a script can decide.
- `go mod tidy` must leave `go.mod` and `go.sum` unchanged; CI checks it.
- Before pushing: `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .`. CI scans
  every commit of a pull request, so a leak fixed in a later commit still fails.

## Picking up work

- Work on an issue your human names. Only humans move issues to Ready; issues open to agents carry
  `agent-ok`.
- Outside contributors do not claim: say on the issue that you are working on it. Maintainers' agents
  claim first: [claims and lanes](docs/maintainers/agent-workflow.md#picking-up-work).
- A pull request names the issue it closes with `Closes #N`. New work you discover becomes a new
  issue; do not start it.

## Branches and pull requests

- **Merge `main`, never rebase** a pushed branch, and merge `origin/main` only when GitHub reports a
  conflict (the merge queue tests the pull request on main's tip). Merging:
  [maintainers](docs/maintainers/agent-workflow.md#merging).
- **Conflicts.** Mechanical (generated code, the ADR index, golden files, a migration number):
  regenerate or renumber. Append-site (both sides add lines to one list): keep both and run the
  check for that area. Semantic (the same function or behaviour): stop and ask your human.
- **No unrelated dependency bumps** in a feature PR. **Proto:** only add fields, never reuse or
  renumber; `make breaking` must pass. **Golden files:** regenerate, do not hand-edit.
- **ADR numbers:** run `hack/adr-index.sh`, never edit the index table; renumber yours if
  `hack/adr-lint.sh` reports a collision (maintainers reserve first:
  [ADR numbers](docs/maintainers/agent-workflow.md#working-alongside-another-human)).
  **Migration numbers:** pick them late; after merging main run `hack/migrate-renumber.sh` if
  `hack/migrate-lint.sh` complains.
- **Chart assertions** go in the owning file of `hack/chart-check.d/`; a new feature adds a new file.
- **Stop and ask your human** when you would need to rebase or force-push, a test only you changed
  starts failing elsewhere, you would change a public API or data model another open PR also
  changes, or the issue turns out bigger than it looked. Overlap with another
  human's work: [working alongside another human](docs/maintainers/agent-workflow.md#working-alongside-another-human).

## Conventions

- Decisions live in `docs/adr/` (ADRs are canonical; `docs/design.md` is the overview). Read the
  relevant ADR before changing architecture; write a new one for new decisions.
- Commits: Conventional Commits, always signed off (`git commit -s`).
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

GitHub issues in `stateracloud/koinon` on the org board. Maintainers' agents take work by claim
(`hack/claim.sh`). See `docs/agents/issue-tracker.md`.

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
