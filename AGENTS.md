# AGENTS.md

Instructions for coding agents working on Koinon.

## Commands

- Build: `make build` · Test: `make test` · Lint: `make lint` · Codegen: `make generate`
- Go only: `go test $(go list ./... | grep -v '/test/flavors$')`, which is what `make test` runs
  (Postgres tests need Docker; they skip without it). `test/flavors` needs the flavor images:
  `make test-flavors`.
- Web only: `cd web && pnpm lint && pnpm typecheck && pnpm test`
- Docs site: `make docs` · `make docs-check`. The quickstart's `sh e1=<name>` blocks are what
  `test/e2e/run.sh` runs; after changing one, run `hack/docs-check.sh --update`.
- After changing `.proto` files or `internal/db/queries/*.sql`, run `make generate` and commit the
  output. CI fails if generated code is stale.
- Chart: `hack/chart-check.sh` (lint, every mode rendered, render assertions, kubeconform). Add an
  assertion for every chart behaviour you change, in the matching `hack/chart-check.d/NN-<feature>.sh`.
- Sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`): the escape suite,
  `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./test/escape/ -ginkgo.label-filter=admission`
  (`make test-admission`). Its OpenShell cases run on kind: `make dev-kind-up`, then `make test-escape-kind`.
- Local cluster: kind is the dev loop (`make dev-kind-up`, `make dev`, `make test-e2e-kind`). Nothing
  needs the maintainers' home lab, registry or secret store; see CONTRIBUTING.md and `docs/dev/loop.md`.
  Real-model runs on your own cluster and budget: `docs/dev/real-model-tests.md`.
- Claims: `hack/claim.sh help`; `hack/claim_test.sh` (offline). ADRs: `hack/adr-lint.sh`, `hack/adr-index.sh`.
- Migrations: `hack/migrate-lint.sh origin/main` (merged migrations unchanged, numbers contiguous);
  after merging main, `hack/migrate-renumber.sh` renumbers your branch's migrations.
- `go mod tidy` must leave `go.mod` and `go.sum` unchanged; CI checks it.
- Before pushing: `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .`. CI scans
  every commit of a pull request, so a leak fixed in a later commit still fails.

## Picking up work

Several humans' agents share the backlog. Claim an issue before you work on it; the lock is a claim
branch (ADR 0082), so two agents cannot hold one issue. Use `hack/claim.sh` (gh and jq only; `gh auth
refresh -s project` once, for the board).

Session identity: the claim holder is `<login>/<host>/<session>`. Claude Code sessions use
`CLAUDE_CODE_SESSION_ID` automatically; any other agent must `export KOINON_AGENT_SESSION=<unique id>`
before it calls the script (otherwise one holder is generated per worktree). **Claude subagents share
their parent's id: only the parent takes claims; a subagent never takes its own or sets its own id.**
Copilot coding agent pull requests cannot pass the `claim` check (the author is the bot): a human opens
or adopts them.

1. `hack/claim.sh next` (or `next --label start-here` for the issues procoduck marked for the contributor):
   Ready, `agent-ok`, current-milestone, unblocked, unclaimed issues, by Priority then age. Only
   humans move issues to Ready; never start an issue that is not listed unless your human names it.
2. Overlap check, see "Working alongside another human" below.
3. `hack/claim.sh take <N>`: one claim per session, four per human, 2 h lease. A pull request that
   closes several issues takes them together, `take N M` (all or nothing). Exit 2: pick another
   issue. It assigns your human and sets In Progress. A person's own claim is `take <N> --human`
   (3 days, `beat <N> --human` daily).
4. Within 30 minutes: push `<type>/<N>-<slug>` and open a draft PR with `Closes #N`. The `claim`
   check needs the claim first; after `take` the script re-runs the failed check.
5. `hack/claim.sh beat <N>` every 30 minutes and before long steps. **Pushes alone are not
   liveness: you must `beat`.**
6. Exit code 3 from any claim command means the claim is lost (taken over or released): stop work
   on that issue and tell your human. Exit 1 is an error (for example the network): retry.
7. Blocked by another issue: `hack/claim.sh block <N> <M>` (adds "blocked by", releases). New work
   you discover becomes a Backlog issue; do not start it.
8. Finish: mark the PR ready, enable auto-merge (unless it needs `e2e-kind` first, see "Merging" under
   Working rules), then `hack/claim.sh review <N>`. The claim is
   released when the PR merges or closes. Never delete a claim branch: claims are only released.
9. Hand off: `hack/claim.sh release <N> --note "<state, branch, next step>"`; keep the branch and
   draft PR.
10. Takeover only after the holder is dead (lease and PR activity out, plus 15 minutes) and never
    while its PR is ready for review: `hack/claim.sh take <N> --takeover`, then continue the
    existing branch. If you are a different human's agent, open a new PR from that branch: the
    `claim` check wants the claim's human to be the PR author.
11. A PR that conflicts with main while auto-merge is on gets the label `needs-update`: merge
    `origin/main` yourself (the updater bot never pushes code to your branch).
12. Releases: either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay
    procoduck's. Tags are immutable by ruleset: fix a bad tag with the next rc (`docs/dev/releasing.md`).

## Working alongside another human

Two humans' agents change the same tree. Before you start, after `take`:

- **Overlap check.** `gh pr list --state open --json number,author,headRefName,files` and
  `hack/claim.sh` live claims (`gh api repos/{owner}/{repo}/git/matching-refs/heads/claim/`). If an
  open PR or a claim by the other human touches the same files with the same behaviour, run
  `hack/claim.sh block <N> <M>` or ask your human. If it only appends to a shared list or adds a
  sibling file (a new ADR, test, migration, chart assertion), proceed and say so in the PR.
- **Merge `main`, never rebase** a pushed branch. Sync points: session start, before a push after an
  hour on the branch, before marking ready, and when `needs-update` appears.
- **Conflict classes.** Mechanical (generated code, the ADR index, golden files, a migration number):
  regenerate or renumber. Append-site (both sides add lines to one list): keep both and run the
  check for that area. Semantic (the same function or behaviour) with the other human's open or
  last-7-days work: stop, comment on both PRs, ask your human.
- **ADR numbers:** reserve first with `hack/claim.sh adr` (prints `NNNN`), then write the file; run
  `hack/adr-index.sh`, never edit the index table. **Migration numbers:** pick them late; after
  merging main run `hack/migrate-renumber.sh` if `hack/migrate-lint.sh` complains.
- **Chart assertions** go in the owning file of `hack/chart-check.d/`; a new feature adds a new file.
- **No unrelated dependency bumps** in a feature PR. **Proto:** never reuse or renumber a field
  number, only add; `make breaking` must pass. **Golden files:** regenerate, do not hand-edit.
- **Stop and ask your human** when: the change touches a sensitive path the other human owns
  (`.github/CODEOWNERS` lists them; either human approves the other's PR there), you would change a
  public API or the data model another open PR also changes, you need to rebase or force-push, a
  test only you changed starts failing elsewhere, or the issue turns out bigger than its claim.
- **Keep branches under a day.** Split the issue and release the rest instead of holding a claim
  for days.

## Conventions

- Decisions live in `docs/adr/` (ADRs are canonical; `docs/design.md` is the overview). Read the
  relevant ADR before changing architecture; write a new one for new decisions.
- Commits: Conventional Commits, always signed off (`git commit -s`).
- The module path (`github.com/stateracloud/koinon`) and API domain (`koinon.dev`) are defined once in
  `internal/project`; never hard-code them elsewhere in Go.
- Migrations are forward-only goose files `internal/db/migrations/NNNNN_name.sql`. Never edit a
  merged migration. Tables owned by an org are keyed by `(org_id, id)` with composite foreign keys.
- Every source file starts with `SPDX-License-Identifier: Apache-2.0`.
- Tests: Ginkgo/Gomega and testcontainers-go in Go, Vitest in `web/`. CI never calls paid models:
  use the fake model server. Fake secrets in tests are obviously fake and low-entropy
  (`koinon_test_not-a-real-token`) from the first commit: gitleaks scans every commit of a pull
  request, so a fixture that looks real and is fixed later still fails.
- Dependencies are at least 3 days old before they enter the tree, security fixes excepted with
  checks (ADR 0079; `hack/dep-age-check.sh origin/main`).
- Chart renders are deterministic: no Helm `lookup` and no random values in templates. Generated
  secrets come from the ensure-secrets hook Job; objects the reconciler owns are created by the
  reconciler. Every image reference goes through `koinon.imageRef` and every pod spec gets
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
- **Merging.** The only merge method is a merge commit: `gh pr merge --auto --merge`. Auto-merge
  does not wait for `e2e-kind`, which is not a required check. So a pull request that changes startup
  flags, chart Jobs or hooks, or anything the e2e expects does not get auto-merge until `e2e-kind` is
  green on the PR: it runs on its own for the paths in `.github/workflows/e2e-kind.yml`, otherwise
  dispatch it (`gh workflow run e2e-kind.yml --ref <branch>`) or run `make test-e2e-kind`, then enable
  auto-merge and `hack/claim.sh review <N>` (step 8 above).
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
