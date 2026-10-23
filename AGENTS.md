# AGENTS.md

Instructions for coding agents working on Koinon.

## Commands

- Build: `make build` · Test: `make test` · Lint: `make lint` · Codegen: `make generate`
- Go only: `go test ./...` (Postgres tests need Docker; they skip without it)
- Web only: `cd web && pnpm lint && pnpm typecheck && pnpm test`
- After changing `.proto` files or `internal/db/queries/*.sql`, run `make generate` and commit the
  output. CI fails if generated code is stale.

## Conventions

- Decisions live in `docs/adr/` (ADRs are canonical; `docs/design.md` is the overview). Read the
  relevant ADR before changing architecture; write a new one for new decisions.
- Commits: Conventional Commits, always signed off (`git commit -s`).
- The module path (`github.com/procoduck/koinon`) and API domain (`koinon.dev`) are defined once in
  `internal/project`; never hard-code them elsewhere in Go.
- Migrations are forward-only goose files `internal/db/migrations/NNNNN_name.sql`. Never edit a
  merged migration. Tables owned by an org are keyed by `(org_id, id)` with composite foreign keys.
- Every source file starts with `SPDX-License-Identifier: Apache-2.0`.
- Tests: Ginkgo/Gomega and testcontainers-go in Go, Vitest in `web/`. CI never calls paid models:
  use the fake model server.

## Hard rules

- **Never Kyverno.** Admission policy is native ValidatingAdmissionPolicy only.
- **No secrets in sandboxes.** Credentials stay in OpenShell provider records, agentgateway or the
  secret store; never put them in sandbox env, images, home volumes or logs. The only exception is
  ADR 0044 and it is gated.
- No CNI-specific features (no Cilium dependency).
- Don't weaken OpenShell defaults from ADR 0045 (`enforcement: enforce`, Kata runtime class).
