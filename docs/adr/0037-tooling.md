# 0037. Tooling: goose, Starlight, ACP Go SDK, Tilt

- Status: Accepted; the docs-site decision (Starlight in `docs/site/`) superseded in part by [0108](0108-docs-site-in-its-own-repository.md)
- Date: 2026-10-21
- Decision: [design.md#37](../design.md)

## Context

The stack is Go (controller-runtime, connect-rpc, sqlc with pgx) with a React SPA. Four tooling
choices were left for the bootstrap:

- schema migrations
- the docs site
- the ACP client library for `agentd`
- the inner development loop against a maintainer's cluster

Each should be open source, simple and fit the stack. Migrations must be plain SQL that sqlc can
read, and forward-only with an upgrade test ([0027](0027-repo-and-versioning.md)).

## Decision

- **Migrations: goose** (pressly/goose, MIT).
  - Migrations are plain SQL files embedded in the API binary with `embed.FS` and
    `goose.SetBaseFS`, and run on startup or by a migrate subcommand.
  - Each file has `-- +goose Up` only (forward-only); a `Down` section is not written.
  - File names are **zero-padded sequence numbers** (`00001_orgs.sql`). sqlc reads migration files
    in lexicographic order, and goose versions must sort correctly.
  - sqlc reads the same directory as its schema and ignores goose's down sections.
- **Docs site: Astro Starlight**, in `docs/site/`. _(Superseded in part by [0108](0108-docs-site-in-its-own-repository.md):
  Starlight stays, the site moves to its own repository.)_
- **ACP client:** `agentd` uses the community Go SDK **`github.com/coder/acp-go-sdk`**
  (Apache-2.0). It is pre-1.0 (v0.13.5, tags only). ACP has no official Go SDK, so the design's
  "the Go SDK" means this one. Koinon wraps it behind a small internal `acp` package so that a
  switch is local _(inferred)_.
- **Inner loop: Tilt** against a maintainer's Talos+Kata cluster.
  - The Tiltfile allows the maintainer's context explicitly with `allow_k8s_contexts`.
  - It pushes images to a registry reachable from the cluster with `default_registry`.
  - Images are built with ko.

## Alternatives considered

- **Atlas** for migrations: declarative and strong, but the Community Edition (Apache-2.0) only has
  core versioned `migrate diff/apply/status`. Linting, drift detection, views, triggers, functions,
  RLS, the Kubernetes operator, GitHub Actions and the Go SDK need Pro.
- **golang-migrate** _(reconstructed)_: popular, but needs separate up/down files, and sqlc support
  is similar. goose's single annotated file and embed support are simpler.
- **Docusaurus or MkDocs** for docs _(reconstructed)_: both fine. Starlight is lighter, and Astro
  fits the TypeScript toolchain already in `web/`.
- **Writing an ACP client from scratch** _(reconstructed)_: JSON-RPC over stdio is small, but the
  SDK already covers `session/load`, `resume`, `list` and `close` with generated types.
- **Skaffold or DevSpace** instead of Tilt _(reconstructed)_: comparable. Tilt's UI and live update
  suit a multi-service loop.

## Consequences

- Positive: simple, all open source (MIT or Apache-2.0), and one migration directory serves both
  goose and sqlc.
- Negative:
  - The ACP SDK is a pre-1.0 community dependency with possible breaking changes; pin it and watch
    upstream.
  - Starlight is still 0.x (0.42.6, needs Astro 7), so expect upgrades.
  - Tilt requires a registry reachable from the home lab _(inferred)_.
- Follow-ups:
  - Scaffold: `internal/db/migrations/00001_orgs.sql` with `org_id`, a `sqlc.yaml` using
    `sql_package: pgx/v5`, a migration-upgrade test with testcontainers-go.
  - Tiltfile: the maintainer's context name and registry as variables, with no secrets in the file.
  - A CI check that migration file names are zero-padded and only appended _(inferred)_.

## Open questions

- Dev registry: an in-cluster registry, or GHCR with a pull secret?
- ~~Run migrations at API start, or as a Helm pre-upgrade Job?~~ Closed by [0060](0060-migrations-as-a-job.md): as a Job.

## Upstream verification

Checked 2026-10-21:

- **goose** v3.29.0, MIT:
  - `-- +goose Up` is required, Down is optional.
  - `SetBaseFS` supports embedded files.
  - https://github.com/pressly/goose
- **sqlc** v1.31.1 supports goose and ignores down migrations; it reads files in lexicographic
  order. https://docs.sqlc.dev/en/latest/howto/ddl.html
- **Atlas Community Edition limits:** https://www.atlasgo.io/community-edition
- **ACP:** no official Go SDK; `coder/acp-go-sdk` is community-maintained, Apache-2.0, v0.13.5.
  https://agentclientprotocol.com/libraries/community
- **Starlight:** 0.42.6, MIT. **Tilt:** v0.37.8, Apache-2.0; `allow_k8s_contexts` and
  `default_registry` documented at https://docs.tilt.dev/choosing_clusters.html and
  https://docs.tilt.dev/api.html

## Links

- [0016 Execution backend and agentd](0016-harness-and-deployment.md), [0027 Repo and versioning](0027-repo-and-versioning.md)
- [0039 Development setup](0039-development-setup.md)

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
