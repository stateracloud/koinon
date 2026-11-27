# AGENTS.md

Instructions for coding agents working on Koinon. Humans: [CONTRIBUTING.md](CONTRIBUTING.md).

**Maintainers only:** claims, lanes, shipping, the merge queue, the lab and releases:
[docs/maintainers/agent-workflow.md](docs/maintainers/agent-workflow.md). A contribution needs none of it.

## Hard rules

- **Never Kyverno.** Admission policy is native ValidatingAdmissionPolicy only.
- **No secrets in sandboxes.** Credentials stay in Koinon's gateway, the secret store or, until ADR 0119 phase 5
  (ADR 0120 section 7), OpenShell provider records; the execution layer injects only a short-lived actor identity
  token, outside the guest. Never put a credential in sandbox env, images, home volumes, snapshots or logs. The only
  exception is ADR 0044 and it is gated.
- No CNI-specific features (no Cilium dependency).
- Don't weaken the execution hardening defaults of ADR 0120 (default-deny egress and enforced L7 rules on every
  inspected or credentialed route, in every tier; Landlock and seccomp in the guest where the tier supports them) or
  the isolation tiers of ADR 0075 (the `vm` tier requires a microVM per sandbox: a separate guest kernel under hardware
  virtualisation with guest seccomp on; Kata is the default runtime, not a requirement). Until #1094 removes the
  OpenShell driver, its profile in ADR 0120 section 7 also holds (`enforcement: enforce` in every tier).

## Rules for a folder

The file of the folder you edit adds to this one. How they are made: [docs/agents/agents-md.md](docs/agents/agents-md.md).

- Editing under `internal/db/`: read `internal/db/AGENTS.md` (migrations, keys, patch-line rule).
- Editing under `charts/`: read `charts/AGENTS.md` (render rules, chart checks, admission tests).
- Editing under `.github/`: read `.github/AGENTS.md` (CI and e2e rules, required checks).
- Editing under `docs/adr/`: read `docs/adr/AGENTS.md` (ADR lint and index).
- Editing under `api/proto/`: read `api/proto/AGENTS.md` (add-only fields, `make breaking`).

## Commands

- Build: `make build` · Test: `make test` · Lint: `make lint` · Codegen: `make generate`
- Go only: `hack/go-test.sh` (what `make test` runs). Postgres tests need Docker and skip without it. One package, the
  `testing` tag and the parallel list: `docs/dev/ci.md#go-test-suites`.
- Web only: `pnpm --dir web lint`, `pnpm --dir web typecheck`, `pnpm --dir web test` (one command per call, no `cd web
  &&` and no pipe, so sandbox allow rules match)
- Docs site: not in this repository (ADR 0108), it is `stateracloud/koinon-docs`. The quickstart and backup commands
  live in `test/e2e/commands/*.e1`; edit the file and run `make e2e-commands-check`.
- After changing `.proto` files or `internal/db/queries/*.sql`, run `make generate` and commit the output. CI fails if
  generated code is stale.
- Local cluster: kind is the dev loop (`make dev-kind-up`, `make dev`, `make test-e2e-kind`); see CONTRIBUTING.md,
  `docs/dev/loop.md` and `docs/dev/real-model-tests.md`.
- Before you hand over or push: `hack/lane-gates.sh` (blocks; one `gate=… verdict=pass|fail|notrun` line per gate your
  change touches, `docs/dev/ci.md#the-local-gate-command`; a long run: `--start`, then `--wait <file> --max 540` again
  after each exit 75) or `hack/preflight.sh` (the diff-selected gates without the heavy jobs). Maintainers ship and merge with [their own scripts](docs/maintainers/agent-workflow.md#commands).
- `go mod tidy` must leave `go.mod` and `go.sum` unchanged; CI checks it.
- A draft PR runs nothing in CI (its `go` and `web` report "draft: not run") and its green is not merge evidence; mark
  it ready (`gh pr ready`) for CI, and get WIP feedback from `hack/preflight.sh`.
- Kind e2e: `hack/needs-e2e.sh` says whether your change needs it. The rules for CI and e2e are in `.github/AGENTS.md`.
- Before pushing: `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .`. CI scans every commit of a pull
  request, so a leak fixed in a later commit still fails.

## Picking up work

- Work on an issue your human names, or a Ready issue labelled `agent-ok`; never start any other. Only humans move
  issues to Ready.
- Outside contributors do not claim: say on the issue that you are working on it. Maintainers' agents claim first:
  [claims and lanes](docs/maintainers/agent-workflow.md#picking-up-work); a lane agent follows [the lane
  procedure](docs/maintainers/lane.md).
- Maintainers' agents: `hack/claim.sh next` shows the current milestone only, and the overlap check also applies to a PR
  you fix or merge ([picking up work](docs/maintainers/agent-workflow.md#picking-up-work)).
- A sandboxed `gh` or `git` call that reaches GitHub needs `allowed_domains` `api.github.com` and `github.com`. A checkout
  can be far behind: `git fetch` and count commits behind `origin/main` before a reader or reviewer uses it.
- A pull request names the issue it closes with `Closes #N`. New work you discover becomes a new issue; do not start it.

## Branches and pull requests

- **Merge `main`, never rebase** a pushed branch, and merge `origin/main` only when GitHub reports a conflict (the merge
  queue tests the pull request on main's tip). Merging: [maintainers](docs/maintainers/agent-workflow.md#merging).
- **Conflicts.** Mechanical (generated code, the ADR index, golden files, a migration number): regenerate or renumber.
  Append-site (both sides add lines to one list): keep both and run the check for that area. Semantic (the same function
  or behaviour as someone else's work): stop and ask your human.
- **Stop and ask your human** when you would need to rebase or force-push, a test only you changed starts failing
  elsewhere, you would change a public API or data model another open PR also changes, or the issue turns out bigger
  than it looked ([details](docs/maintainers/agent-workflow.md#working-alongside-another-human)).
- **No unrelated dependency bumps** in a feature PR. **Golden files:** regenerate, do not hand-edit.

## Conventions

- Decisions live in `docs/adr/` (ADRs are canonical; `docs/design.md` is the overview). Read the relevant ADR before
  changing architecture; write a new one for new decisions.
- Public text (PR and issue text, review comments, docs) follows Simplified Technical English (ASD-STE100): short
  sentences, active voice, no idioms. Code and commit subjects are exempt: [ADR 0117](docs/adr/0117-simplified-technical-english.md).
- Commits: Conventional Commits, always signed off (`git commit -s`) by a human. AI use is disclosed in the PR
  template's "AI use" field (a `--pr-body` file carries it too); an `Assisted-by:` trailer is optional
  (CONTRIBUTING.md).
- The module path (`github.com/stateracloud/koinon`) and API domain (`koinon.dev`) are defined once in
  `internal/project`; never hard-code them elsewhere in Go.
- Every source file starts with `SPDX-License-Identifier: Apache-2.0`.
- Tests: Ginkgo/Gomega and testcontainers-go in Go, Vitest in `web/`. CI never calls paid models: use the fake model
  server. Fake secrets are obviously fake and low-entropy (`koinon_test_not-a-real-token`) from the first commit:
  gitleaks scans every commit of a pull request. Declare what a test needs from the machine with `testenv.Require` (in
  BeforeSuite or a spec, never before RunSpecs); CI sets `KOINON_REQUIRE` to turn those skips into failures, and
  `KOINON_TESTENV_UNAVAILABLE=docker` simulates a machine without Docker.
- Dependencies are at least 3 days old before they enter the tree, security fixes excepted with checks (ADR 0079;
  `hack/dep-age-check.sh origin/main`).
- Metric labels never carry org, agent, user, ticket or run IDs; use fixed label sets (ADR 0023).
- Every model with a `cache_read` price in the catalog runs with prompt caching on; there is no off switch (ADR 0068).

## Working rules

- **Explicit cluster, always.** Pass `KUBECONFIG=<file>` or `--context` to every kubectl, helm, tilt, kind and test
  command you run, and to every agent or subagent you start. Never rely on the default context: it may point at a shared
  or real cluster. Tests and subagents that must not touch a cluster run with `KUBECONFIG=/dev/null`.
- **agentgateway log and tracer fields never read `response.body` or `bodyPrefix`** (CEL): it makes the gateway buffer
  every response (up to 32 MiB on an LLM route). Use the status, the headers and the parsed LLM fields.
- **OpenShell exec stdin is capped.** About 4 MiB of stdin that the command does not read makes `exec` exit 74. Chunk
  uploads and restores, and have the command consume each chunk.
- **Root causes, not workarounds.** When you hit the same workaround twice, find out why and fix the cause. An interim
  fix is allowed only with an issue that names the exit condition (when it can be removed), linked from the code or doc
  that carries it.
- **OpenShell upstream (NVIDIA/OpenShell).** A human posts upstream, in their own words; an agent drafts. The rules for
  the text (no AI attribution, a human user story, a vouch before a pull request) are in
  [agent-workflow.md](docs/maintainers/agent-workflow.md#openshell-upstream).
- **Non-interactive shell.** No pagers and no prompts: `git --no-pager`, `GH_PAGER=cat`, `PAGER=cat`, `gh ... --json` or
  `--jq` instead of tables. Use the real tool, not a shell alias (`/bin/ls`, not an `ls` that is really `eza`), and do
  not assume your human's aliases or interactive shell setup exist.
- **No token on a screen.** Never screenshot or snapshot a page that shows a token or a key; read the value from the
  secret store.

## Principles

- Favor the long-term, sustainable design: foundations generic enough to extend, no shortcuts or stopgaps as the
  default. Koinon isn't public until it's polished, so there's time to do it properly.
- When you offer options, the recommended one is the sustainable one; name what a cheaper option costs later.

## Agent skills

Issue tracker, triage labels and domain docs: [issue-tracker.md](docs/agents/issue-tracker.md),
[triage-labels.md](docs/agents/triage-labels.md), [domain.md](docs/agents/domain.md).
