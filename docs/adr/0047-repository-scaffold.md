# 0047. Repository scaffold, data model foundations and CI gates

- Status: Accepted
- Date: 2026-10-21

## Context

Bootstrap deliverable 2 sets up the repository before any product code. The grilling on
2026-10-21 settled the architectural parts with procoduck. The rest are small, reversible choices
Claude made under the "decide small things yourself, record them in an ADR" rule; procoduck can
overrule any of them by amending this ADR.

Facts checked on 2026-10-21: the home lab runs Kubernetes 1.37 on Talos 1.14 with RuntimeClasses
`kata` and `kata-qemu`, a Cilium Gateway serving `*.apps.lab.example.invalid` with a Let's Encrypt
wildcard certificate, the `proxmox-zfs` storage class, and no image registry. GitHub user name
`koinon` is taken (an account from 2018), so an org move needs another name. Later the same day,
procoduck's Argo CD-managed platform added Zot, OpenBao with External Secrets, CloudNativePG 1.30,
Garage S3 and Gitea; items 6 and 12 reflect that.

## Decision

### Decided with procoduck

1. **One Go module** at the root, `github.com/procoduck/koinon`, defined in `go.mod`,
   `internal/project.ModulePath` and the Makefile `MODULE` variable only. `web/` is a separate pnpm
   package. The org move is decided before the v0.1 plan.
2. **Merge gates on `main`** (a repository ruleset requiring a PR and these checks; admin bypass;
   no force-push or deletion): Go lint, vet and tests; web lint, typecheck, tests and build;
   `buf lint` and `buf breaking` against the base branch; generated code up to date; migration
   lint; DCO sign-off checked by a GitHub Actions job (no third-party app).
3. **Keys and Postgres**: UUIDv7 primary keys from Postgres `uuidv7()`; **Postgres 18 minimum**.
   Tables owned by an org are keyed by `(org_id, id)` and reference each other with composite
   foreign keys, so a row can never point into another org. Row-level security may come later as a
   backstop.
4. **Initial migration** `00001_initial.sql`:
   - `orgs`: slug (DNS-safe, unique, immutable; the namespace name derives from it), display name,
     kind (`personal` | `team`), identity mode (`platform` | `idp` | `hybrid`), timestamps, soft
     delete.
   - `users`: OIDC issuer + subject as the identity (email is not a key), email, display name,
     exactly one personal org (enforced with a composite foreign key on `(id, kind)`), last login.
   - `platform_admins`: explicit grants (who, by whom, when) instead of a boolean.
   - `org_group_mappings`: IdP group → org role per org.
   - `org_memberships`: one row per (org, user, source), source `manual` or an IdP group mapping;
     the effective role is the highest across rows. Roles, lowest to highest:
     `viewer` < `member` < `admin` < `owner`. Approver rights stay in per-grant approver policy.
   - A deferred constraint trigger keeps at least one **manual** owner per live org.
5. **Personal orgs**: the org row is created at a user's first login; the namespace and OpenShell
   workspace are created lazily with the first agent.
6. **Dev registry**: Zot v2.1.22 in namespace `zot` at `zot.apps.lab.example.invalid`, installed
   by procoduck as part of the home-lab platform (`docs/dev/registry.md`). It is also the cluster's
   pull-through cache; anonymous reads cover only the mirrored upstreams, so `koinon/*` needs
   credentials for push and pull.

### Decided by Claude (reversible)

7. **Protobuf**: files under `api/proto/koinon/<area>/v1alpha1/`, packages
   `koinon.<area>.v1alpha1`, starting with an empty `koinon.platform.v1alpha1.PlatformService`.
   buf.yaml v2 with `STANDARD` lint and `FILE` breaking rules. Generation uses local plugins only
   (no Buf Schema Registry): protoc-gen-go and protoc-gen-connect-go (connect-go v2) into
   `internal/gen`, protoc-gen-es into `web/src/gen`. Generated code is committed.
8. **Go tooling**: generators (protoc-gen-go, protoc-gen-connect-go, sqlc, goose) pinned with
   `tool` directives in `go.mod` and run via `go tool`. sqlc generates `internal/db/store` with
   pgx/v5 from `internal/db/queries`. golangci-lint v2 with the standard set plus gosec, errorlint,
   revive, bodyclose, noctx and sqlclosecheck; gofumpt and goimports formatting.
9. **Web**: Vite 8 + React 19 + TypeScript from the official template, which now uses **oxlint**
   instead of eslint; kept as is. Vitest with jsdom and Testing Library. pnpm pinned through
   `packageManager`. shadcn/ui and TanStack arrive with the first screens.
10. **Migrations**: zero-padded `NNNNN_snake_case.sql`, contiguous numbers, embedded with
    `embed.FS`; `hack/migrate-lint.sh` checks names and, in pull requests, that merged migrations
    are unchanged. Migration tests run against `postgres:18-alpine` with testcontainers-go and
    Ginkgo.
11. **Makefile targets**: `build`, `test`, `lint`, `generate`, `breaking`, `migrate-lint`, `dev`,
    `web-deps`, `clean`, `help`.
12. **Inner loop**: the Tiltfile allows only the `homelab` context, builds images with ko
    (`custom_build`, pushing straight to the dev registry) and deploys `hack/dev/` with kustomize
    into namespace `koinon`: a dev-only single-instance CloudNativePG `Cluster` (Postgres 18.6,
    using the platform's operator; nothing cluster-wide installed by Koinon), `koinon-api` and
    `koinon-reconciler`. The Vite dev server runs locally.
    _Amended (2026-10-23):_ the Tiltfile now deploys `charts/koinon` with `hack/dev/values*.yaml`,
    on the home lab or a kind profile; see [docs/dev/loop.md](../dev/loop.md).
13. **CI**: `.github/workflows/ci.yml` (jobs `go`, `web`, `proto`, `generated code`, `migrations`)
    and `dco.yml`, on free `ubuntu-latest` runners, every action pinned to a commit SHA.
    While the repository is private, runs use the account's free Actions minutes.
14. **Repository hygiene**: SPDX headers in source files; Contributor Covenant 2.1, with reports
    to the project lead on GitHub until a project address exists; `charts/koinon` is a skeleton.

## Alternatives considered

- Multi-module repository with `go.work`: lets outside users import generated clients alone, at
  the cost of per-module releases. Rejected for now (ADR 0027: one shared version).
- Single-column keys with tenant scoping only in queries: simpler, but one missed `WHERE org_id`
  leaks data. Row-level security from day one: stronger, but complicates every migration and test
  before there is anything to protect. _(reconstructed)_
- One membership row per user: simpler, but group sync would overwrite manual grants and lose
  where a role came from.
- GHCR or ttl.sh for dev images: no install, but quota or public images.
- eslint instead of oxlint: the Vite template switched to oxlint; following the template keeps
  upgrades cheap. _(inferred)_
- The DCO GitHub App: works, but adds a third-party app with repository access.

## Consequences

- Positive: cross-org references are impossible at the schema level; manual owners survive IdP
  changes; API changes are deliberate from the first proto; generated code can't drift.
- Negative: composite keys make every foreign key two columns wide; Postgres 18 excludes older
  managed offerings _(inferred)_; the dev loop depends on the home-lab registry and CloudNativePG operator.
- Follow-ups: the branch ruleset is applied once the scaffold PR merges (so the checks exist);
  `docs/adr/README.md` needs this ADR in its index after PR #12 merges; the Starlight site and the
  Helm chart content come later.

## Open questions

- GitHub org name for the move (`koinon` is taken): decided before the v0.1 plan.
- A project contact address for the code of conduct and security reports (needs `koinon.dev`).
- Row-level security as a backstop (follow-up issue from PR #12).

## Upstream verification

Checked 2026-10-21: Go 1.27.3, golangci-lint 2.15.0, buf 1.74.0, connect-go v2.0.1,
protobuf-go 1.36.12, ginkgo 2.33.1, gomega 1.44.0, testcontainers-go 0.44.0, Tilt 0.37.8, Vite 8,
React 19, Zot chart 0.1.128 / Zot v2.1.22, Helm 4.3.1. The migration suite passes against
`postgres:18-alpine` (10 specs). `tilt ci` passes on the `homelab` context: ko pushes both images
(with SBOMs) to Zot, the pods pull them through the `dev-registry` secret, and `koinon-pg` reaches
a healthy state.

## Links

- ADR 0004, 0022, 0026, 0027, 0034, 0037, 0038, 0039
- [docs/dev/registry.md](../dev/registry.md)
- Issue [#2](https://github.com/procoduck/koinon/issues/2)
