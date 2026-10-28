# AGENTS.md

Instructions for coding agents working on Koinon.

## Commands

- Build: `make build` · Test: `make test` · Lint: `make lint` · Codegen: `make generate`
- Go only: `go test ./...` (Postgres tests need Docker; they skip without it)
- Web only: `cd web && pnpm lint && pnpm typecheck && pnpm test`
- Docs site: `make docs` · `make docs-check`. The quickstart's `sh e1=<name>` blocks are what
  `test/e2e/run.sh` runs; after changing one, run `hack/docs-check.sh --update`.
- After changing `.proto` files or `internal/db/queries/*.sql`, run `make generate` and commit the
  output. CI fails if generated code is stale.
- Chart: `hack/chart-check.sh` (lint, every mode rendered, render assertions, kubeconform). Add an
  assertion for every chart behaviour you change.
- Sandbox admission policy (`charts/koinon/templates/sandbox-policy.yaml`): the escape suite,
  `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test ./test/escape/ -ginkgo.label-filter=admission`.
- Migrations: `hack/migrate-lint.sh origin/main` (merged migrations unchanged, numbers contiguous).
- `go mod tidy` must leave `go.mod` and `go.sum` unchanged; CI checks it.
- Before pushing: `gitleaks git --redact --no-banner --log-opts="origin/main..HEAD" .`. CI scans
  every commit of a pull request, so a leak fixed in a later commit still fails.

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
  (`koinon_test_not-a-real-token`).
- Chart renders are deterministic: no Helm `lookup` and no random values in templates. Generated
  secrets come from the ensure-secrets hook Job; objects the reconciler owns are created by the
  reconciler. Every image reference goes through `koinon.imageRef` and every pod spec gets
  `koinon.imagePullSecrets` (ADR 0061). Values files without an OpenShell client TLS Secret must
  set `openshell.insecure: true`.
- Metric labels never carry org, agent, user, ticket or run IDs; use fixed label sets (ADR 0023).
- Every model with a `cache_read` price in the catalog runs with prompt caching on; there is no off
  switch (ADR 0068).

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
