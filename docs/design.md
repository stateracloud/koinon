# Koinon design decisions

Status: accepted design, 21 October 2026. Owner: procoduck (@procoduck).

Koinon (pronounced KOY-non, after the leagues of independent Greek city-states) is an open source
(Apache-2.0) org-and-work layer for AI agents on Kubernetes. Orgs get their own agents, connectors
and secrets; agents take work as tasks, from people, triggers and other agents, and the inbox shows what needs a
person (ADR 0065, 0066). Execution, isolation,
credential brokering and inference routing are reused from NVIDIA OpenShell on Kubernetes Agent
Sandbox instead of being built.

## Architecture at a glance

```
Web UI ─────────────┐        ┌──── Slack, Teams, webhooks, crons
                    ▼        ▼
┌──────────────── Koinon control plane (we build) ────────────────┐
│ API + Postgres          Reconciler                A2A gateway   │
│ orgs, grants,           renders workspaces,       (external,    │
│ tasks, audit            policies, providers,      later), links,│
│                         sandbox templates         depth policy  │
└───────────────────────────────┬─────────────────────────────────┘
                                │ renders
┌──────────── NVIDIA OpenShell (reused) ───────┐  ┌─ Shared services (reused) ─┐
│ OpenShell gateway: lifecycle, providers,     │◄─│ Secret store (Vault/OpenBao)│
│ policies, inference.local                    │  │                             │
│ Sandbox per agent (Agent Sandbox, Kata):     │─►│ LiteLLM: models, budgets    │
│  harness speaks ACP (agentd: control plane)  │  │                             │
│  supervisor: L7 policy, placeholder swap     │─►│ MCP gateway: shared tools,  │
│  Landlock + seccomp; no real keys inside     │  │ grants, ask tier            │
│  home volume: memory, skills, sessions       │  │                             │
└───────────────────────┬──────────────────────┘  └──────────────┬──────────────┘
                        ▼                                         ▼
        External systems: AWS, Azure, Azure DevOps, Slack, GitHub, model providers
```

Our reconciler turns orgs, grants and agents into OpenShell workspaces, policies and providers;
the supervisor in each sandbox is the only exit, so real credentials, model keys and shared tools
stay outside the agent.

## Build vs reuse

| Layer | Reuse | We build |
| --- | --- | --- |
| Sandbox lifecycle, warm pools, suspend and resume | Kubernetes Agent Sandbox | nothing |
| Isolation, egress policy, credential placeholders, harness images | NVIDIA OpenShell gateway and supervisor (Kubernetes compute driver) | OpenShell policies rendered from our grants and risk tiers |
| Fallback execution backend | OpenSandbox credential vault, behind the same interface | the `ExecutionBackend` interface |
| Model routing, budgets, virtual keys | LiteLLM behind OpenShell's `inference.local` | budget hierarchy and cost per task |
| Shared MCP servers | ToolHive operator and registry (to evaluate) | grants and the progressive per-agent endpoint |
| Secret storage | Postgres: provider keys and connection secrets are envelope-encrypted, the KEK is a Kubernetes Secret (ADR 0056); no hard dependency on Vault or OpenBao | connection cards and secure inputs |
| Orgs, personal orgs, roles, cross-org links | — | all of it, aligned with OpenShell workspaces (RFC 0011) |
| Tasks, A2A, parking, approvals, triggers | DBOS Go as a library for schedules, timers, webhook processing and per-org limits (ADR 0066) | tasks, the inbox, triggers, park and resume |
| Connector types, setup guides, agent builder, templates | — | all of it |
| Inbox, tasks, agents and admin UI | — | all of it (ADR 0065) |

## Decisions

Each decision links to its ADR in [`docs/adr/`](adr/README.md). The ADRs are canonical: changes land
there first, and this page stays the one-page overview.

### Platform architecture (1–16)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| [1](adr/0001-full-multi-tenancy.md) | Tenancy | Full multi-tenancy: a team or user gets an org and sets up its own agents, connector cards and secrets; cross-org agent use is opt-in | Retrofitting tenancy later is the expensive mistake |
| [2](adr/0002-namespace-per-org-isolation.md) | Isolation unit | One namespace per org, mapped to one OpenShell workspace (`workspaceMode: managed`); optional dedicated NodePools per org; placement kept abstract | The OpenShell supervisor and Kata VM are the real boundary; the namespace scopes quotas, RBAC and secret refs |
| [3](adr/0003-postgres-source-of-truth.md) | Source of truth | Postgres via a Go API (encryption at rest, TLS, least-privilege roles, PITR); a reconciler renders Kubernetes and OpenShell objects; GitOps only for the platform layer | Self-service authz Kubernetes RBAC can't express; tickets and audit don't belong in etcd |
| [4](adr/0004-human-identity.md) | Human identity | Generic OIDC; memberships and roles in Postgres; per-org mode `platform`, `idp` or `hybrid` (highest role wins); at least one manual owner; group sync at login, SCIM later; `platform-admin` above all orgs | Teams differ in how much they use their IdP |
| [5](adr/0005-agent-identity.md) | Agent identity (amended by ADR 0067) | Agents act as themselves; every run records `requested_by` and `approved_by`, added to PRs, commits and audit; on-behalf-of only as a per-connector opt-in; one exception: the concierge acts as the logged-in user, capped at their role | Crons have no user, delegated tokens expire, permissions stay designed per agent |
| [6](adr/0006-identity-layers.md) | Identity layers (amended by ADR 0043, 0044, 0063, 0065) | Per-agent ServiceAccount; the sandbox gets no usable token; federated credentials (AWS, Entra, ADO, Vault) are minted outside the sandbox and held as OpenShell provider records injected only for allowed endpoints; SPIFFE-style names now, SPIRE later; connector auth types `federated`, `app-credential`, `delegated-user`, plus static code-host tokens behind placeholders (ADR 0063); no traffic between sandboxes, agents ask each other through `ask_agent` (ADR 0065) | Cloud providers trust the cluster OIDC issuer; Slack and GitHub need app credentials |
| [7](adr/0007-connector-model.md) | Connector model (amended by ADR 0062, 0063, 0064) | Type (platform-curated: auth, scopes, setup guide; for code hosts also CI paths and a capability descriptor) → connection (per-org card, with base URL and CA bundle for code hosts) → grant (per agent, subset of scopes); code hosts GitHub, GHES and Gitea in v0.1; work trackers through tools and triggers, never synced | Platform isn't a bottleneck, scopes stay enforceable |
| [8](adr/0008-approvals-and-risk-tiers.md) | Approvals (amended by ADR 0042) | Platform risk tiers per scope (read, reversible write, external or irreversible, forbidden); presets per grant (Read only, Standard, Trusted); optional intent classifier for the ask tier; learned rule suggestions (aligned with OpenShell RFC 0002); each grant compiles to OpenShell network policy (binary, host, method, path) plus MCP gateway tool rules; approval bound to the exact request; run parks while waiting; per-grant approver policy, optional two-person rule | Boundary does the work, humans only see truly risky actions |
| [9](adr/0009-agent-runtime.md) | Agent runtime | Persistent agent, ephemeral compute: durable home volume; one OpenShell sandbox per agent, stopped when idle and started on demand (none exists before the first run); optional operator-capped `agents.keep_warm` (replaces `alwaysOn`); one run per agent at a time; harness only selects the harness image | Remembers and resumes at near-zero idle cost |
| [10](adr/0010-eventing.md) | Eventing (superseded by ADR 0065, 0066) | Tasks are the queue; triggers on the versioned agent config (schedules, code-host events, chat events, direct messages) create a task or wake the open task with the same correlation key; ingress lands in DBOS Go on Postgres for dedupe and retries; event bus only if the need shows | No dual-write problem, nothing extra to operate |
| [11](adr/0011-agent-to-agent.md) | Agent-to-agent (amended by ADR 0055, 0065) | Inside an install: `ask_agent` and `reply` MCP tools over Postgres, the request is a child task, the caller parks (`a2a`); an external A2A gateway with agent cards later (v1.0); guards: no agent twice in the chain, at most 3 deep, at most 5 open questions per agent, repeat questions return the existing child; each agent's spend on its own budget, results as quoted data | Dynamic back-and-forth with a durable, visible record |
| [12](adr/0012-cross-org-sharing.md) | Cross-org sharing | Private (default), shared on request, or public (platform-admin only); links carry an exposure profile (reduced grants), rate limit and budget; the called agent spends its own budget (ADR 0065); calls run in the provider org, approvals go to provider approvers | Prevents the confused deputy |
| [13](adr/0013-model-access.md) | Model access (amended by ADR 0040, 0041, 0044) | Sandboxes call OpenShell `inference.local`, which forwards to LiteLLM (API keys, OpenAI-compatible self-hosted models, provider presets); virtual keys carry budgets platform → org → agent → task, warning at 80%, stop at 100% (amended 2026-10-30); v1 = popular providers, API keys, GitHub Copilot via OpenCode; subscriptions personal-only and later; review vendor terms of service first | One path for budgets and observability |
| [14](adr/0014-mcp-and-tools.md) | MCP and tools (amended by ADR 0040) | Shared MCP servers deployed from the UI, outside sandboxes, behind one progressive (search + call) endpoint per agent with grants; local MCP servers declared per agent and run inside the sandbox; evaluate ToolHive | Low context overhead, credentials out of the sandbox |
| [15](adr/0015-agent-environment-and-browser.md) | Agent environment and browser (amended by ADR 0044) | Harness images based on OpenShell sandbox images; agents self-install into `$HOME` (mise/aqua, uv, npm) from allowlisted sources via a pull-through cache; installs recorded in `environment.lock`; org admins promote common tools; browser in the sandbox with per-agent profile, credentials only through human takeover, cookies treated as secrets | Agents find tool gaps at runtime; cookies would bypass the secret model |
| [16](adr/0016-harness-and-deployment.md) | Harness and deployment | Control plane talks to the OpenShell gateway (gRPC) through an `ExecutionBackend` interface (OpenSandbox as alternative); `agentd` runs in the control plane and drives the harness over ACP (ADR 0049); OpenShell supervisor does all enforcement; dedicated agents cluster is the reference, co-location supported with a small cluster-scoped footprint and an install-time isolation-tier check | Reuses a maintained execution layer; one contract per harness |

### UI, operations and project (17–27)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| [17](adr/0017-board-and-chat-interaction-model.md) | Interaction model (superseded by [0055](adr/0055-chat-as-its-own-object.md) and [0065](adr/0065-work-model-tasks-inbox.md)) | Board and chat are two views of one thing: board is home, every conversation is a ticket, `waiting on you` is the key column, Slack and Teams mirror tickets, configuration is secondary | Board stays the queue; agents feel like colleagues |
| [18](adr/0018-v1-screens.md) | v1 screens (amended by ADR 0055, 0065, 0066) | Inbox (home) with chat, Board (secondary view), Task, Agents, Agent editor (with triggers), Connections, MCP servers, Org settings, Platform admin, Audit; every user gets an automatic personal org | Personal agents need no special code path |
| [19](adr/0019-personal-agents-and-teams.md) | Personal agents and teams (amended by ADR 0065) | Membership-implied access: personal agents may call team agents that accept "org members' personal agents", capped by exposure profile and the user's role; each agent's spend on its own budget (ADR 0065); default on for read-tier work only | Personal agents act like employees asking the experts |
| [20](adr/0020-agent-builder-and-templates.md) | Creating agents | Agent builder (a platform agent) is the main path, templates are building blocks; result is always a draft; only an org admin or owner activates and grants | Builder proposes access, never grants it |
| [21](adr/0021-connector-setup-guides.md) | Connector setup | Connector types ship machine-readable setup guides (fields, steps, validation check) shared by UI cards and builder; builder generates setup-specific config; secrets via inline secure inputs straight to the encrypted store (envelope-encrypted in Postgres; ADR 0056); each step validated | Help at the moment of failure, secrets never in chat |
| [22](adr/0022-frontend-architecture.md) | Frontend | React SPA (Vite) served by the Go API; connect-rpc with generated TypeScript client; streaming updates from Postgres `LISTEN/NOTIFY` wake-ups that clients follow by run event sequence (no outbox, see ADR 0010); backend-for-frontend OIDC with httpOnly cookie; noVNC live view via the API; shadcn/ui and TanStack; virtualized lists, optimistic updates | Fast, reactive, no second backend |
| [23](adr/0023-observability-and-audit.md) | Observability and audit (amended by ADR 0074) | All optional: `/metrics` and optional ServiceMonitors; run traces only to an OTLP endpoint an org or platform admin supplies, through Koinon's trace router (ADR 0074); audit on by default, hash-chained in Postgres, opt-out per platform policy; business records always kept | Operators choose their stack |
| [24](adr/0024-roadmap.md) | Roadmap (amended 2026-10-26) | v0.1 Foundation (GitHub, GHES and Gitea; tasks, inbox, `ask_user`, DBOS; schedule and direct-message triggers; home checkpoints) → v0.2 events and channels (code-host and Slack/Teams events, Slack and Teams connectors, `ask_agent`) → v0.3 concierge, tracker connectors, personal agent → v0.4 multi-tenancy → v0.5 MCP, browser, self-installed tools → v1.0 cross-org, security review, stable APIs; `org_id` in the data model from v0.1 | Each release installable and demo-able |
| [25](adr/0025-testing-strategy.md) | Testing | Ginkgo/Gomega, envtest, testcontainers-go; Vitest and Playwright; fake OpenAI/Anthropic model server so CI never calls paid models; harness contract tests; e2e on kind; sandbox escape suite (admission cases in PR CI, OpenShell cases on kind); no nightly eval job, real-model evals run locally or on your own cluster; free GitHub runners | Deterministic, free CI; core security promise tested continuously |
| [26](adr/0026-home-lab-and-license.md) | Home lab and license | A maintainer's Talos+Kata cluster for development and local testing only, never public CI; Apache-2.0 with DCO sign-off, no CLA | Real isolation testing at no cloud cost |
| [27](adr/0027-repo-and-versioning.md) | Repo and versioning | Monorepo (`api/ cmd/ internal/ web/ harnesses/ connectors/ templates/ charts/ test/ docs/`); ko for Go images; connectors and templates as data; SemVer, one shared version; CRDs `v1alpha1` until 1.0; `buf breaking` in CI; forward-only migrations with an upgrade test; GoReleaser, cosign, SBOMs, OCI Helm chart | Components move together; supply-chain hygiene |

### State, resume, artifacts and memory (28–33)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| [28](adr/0028-name-koinon.md) | Name | Koinon (KOY-non) | Independent orgs that cooperate through links |
| [29](adr/0029-run-level-resume.md) | Resume | Run-level resume: the harness continues from its last saved session turn (`--resume`, ACP `session/load`); repeated turns are safe because the MCP gateway and OpenShell supervisor record external writes per run with idempotency keys; step-level workflows later | Harness loops can't be checkpointed from outside |
| [30](adr/0030-run-state-machine.md) | Run state (amended by ADR 0065, 0066) | Postgres state machine per run with an ownership epoch per agent (stale writers rejected); timeouts as columns; schedules and timers on DBOS Go, a library on the same Postgres; park reasons `approval`, `input_required`, `a2a` in use; each failed run carries a `failure_class` that drives retries | Short, fixed process; Durable Objects pattern without its runtime |
| [31](adr/0031-artifacts-in-object-storage.md) | Artifacts | S3-compatible storage (AWS S3, MinIO, SeaweedFS) under an org prefix, metadata in Postgres; agents publish via a `publish_artifact` tool; referenced by ID from tickets and A2A results; signed URLs after a permission check; retention per org | Standard pattern; fits A2A artifacts |
| [32](adr/0032-agent-home-checkpoints.md) | Agent home (amended by ADR 0065) | PVC checkpointed to S3, built in v0.1 (saved on sandbox stop and before recreate, restored after); whether the park, run-end and periodic points also ship in v0.1 is open; archived after 7 days stopped once a checkpoint is complete; a restored home keeps its generation; incremental (restic or kopia) with history; includes memory, sessions, `environment.lock`, encrypted browser profile; excludes checkouts and caches; carries the ownership epoch; restorable in any zone | Zone-bound volumes; rollback and export |
| [33](adr/0033-file-based-memory.md) | Memory | File-based memory stores mounted into the sandbox: agent store read-write, org knowledge read-only with admin-approved proposals, user store for personal agents; each a git repo with history and audit; harnesses pointed at them via `CLAUDE.md` / `AGENTS.md`; optional Hindsight (MIT) or Mem0 (Apache-2.0) via MCP | Memory is solved; Koinon adds scoping and audit |

### Project bootstrap (34–39), 21 October 2026

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| [34](adr/0034-project-identity.md) | Identity | Repo and Go module `github.com/stateracloud/koinon` (moved from `procoduck` on 2026-10-23, see the amendment in ADR 0034); domain `koinon.dev`, CRD API groups under it (e.g. `agents.koinon.dev`) | Start fast under a personal account; avoid a path change after users exist |
| [35](adr/0035-v01-harnesses.md) | v0.1 harnesses | OpenCode over ACP only in v0.1 ([0059](adr/0059-v01-opencode-only.md)); Hermes, pi, Claude Code and Codex later | Open source harnesses, any model, matching the open source project |
| [36](adr/0036-v01-demo.md) | v0.1 demo (amended by ADR 0049, 0062, 0065) | 15-minute quickstart: Helm install → create an agent from a template → message it → watch the run live → approve one action; v0.1 is now tested on the home lab with Gitea through three stories (ADR 0024 amendment); code hosts GitHub, GHES and Gitea (ADR 0062) | Shows the core promise: useful agent, safe by default |
| [37](adr/0037-tooling.md) | Tooling | goose for migrations (plain SQL, embedded, sqlc-compatible); Starlight docs site; ACP via the Go SDK; Tilt for the inner loop against a maintainer's cluster | Simple, open, fits the stack |
| [38](adr/0038-governance.md) | Governance | BDFL now (procoduck) with `MAINTAINERS.md` and a written path to maintainership; GitHub private vulnerability reporting for security issues | Light process while the project is small |
| [39](adr/0039-development-setup.md) | Development setup | Claude Code works on procoduck's Mac in a new local repo; tests deploy to a maintainer's Talos+Kata cluster (Kata installed); the agent has full cluster and full GitHub access but asks before big or hard-to-reverse actions | Fast iteration with real isolation |

## Revisions after the bootstrap grilling (21 October 2026)

Upstream research during the ADR work changed some of the decisions above. Details in each ADR.

| ADR | Change |
| --- | --- |
| [0040](adr/0040-agentgateway-model-and-mcp-gateway.md) | agentgateway replaces LiteLLM as model gateway and serves as the MCP gateway; OpenShell removed `inference.local` in 0.1.0, so each agent gets a provider record pointing at agentgateway |
| [0041](adr/0041-koinon-spend-ledger-and-budget-stops.md) | Koinon owns the authoritative spend ledger; agentgateway enforces pushed per-key limits |
| [0042](adr/0042-ask-tier-park-and-execute.md) | Ask tier (amended by ADR 0065: safe defaults, scoped approvals): ask-tier scopes are MCP tools only; the run parks and Koinon executes the exact approved call (OpenShell has no ask tier) |
| [0043](adr/0043-workload-identity-adapters-and-koinon-issuer.md) | Cloud identity via per-cloud adapters and a Koinon-owned OIDC issuer; replaces "Pod Identity" (EKS-only) |
| [0044](adr/0044-in-sandbox-credential-exception.md) | Gated exception for credentials inside the sandbox; since 2026-11-11 only for a named auth type that cannot be held outside, personal orgs by default, no release assigned |
| [0045](adr/0045-openshell-hardening-defaults.md) | OpenShell hardening defaults (always `enforce`, no secrets in sandbox env, Kata default) |
| [0046](adr/0046-adr-conventions.md) | ADR conventions |

## Revisions from the v0.1 planning (22 October 2026)

| ADR | Change |
| --- | --- |
| [0049](adr/0049-v01-scope-decisions.md) | `agentd` runs in the control plane (amends 0016); Koinon reads only Kubernetes Secrets, external stores via ESO (amends 0007; connection secrets later moved into Postgres by 0056); agentgateway standalone or Kubernetes mode; v0.1 template, scopes, UI and approval semantics (settles 0036's open questions) |

## Revisions from the 23 October 2026 decisions

| ADR | Change |
| --- | --- |
| [0053](adr/0053-hermes-context-file-patch.md) | Superseded by [0059](adr/0059-v01-opencode-only.md): Hermes context-file build patch is redone on every upgrade, with a guard test |
| [0054](adr/0054-unkillable-sandbox-process.md) | A sandbox with an unkillable process is saved (uncommitted work, sealed, pushed only on approval), marked unhealthy after 60 s and recreated; three recreations a day make the agent unhealthy |
| [0055](adr/0055-chat-as-its-own-object.md) | Chat is its own object; supersedes 0017 (v0.2) |
| [0056](adr/0056-model-providers-pricing-and-budgets-per-org.md) | Providers, models, pricing and budgets per org, with an admin API |
| [0057](adr/0057-mcp-servers.md) | MCP servers: remote and container, catalog, per-agent allowlists |
| [0058](adr/0058-gateway-modes.md) | Gateway modes: standalone, kubernetes, shared |

## Revisions from the 26 October 2026 decisions

| ADR | Change |
| --- | --- |
| [0062](adr/0062-code-host-connectors.md) | Code-host interface per provider plus data (CI paths, capability descriptor); hosts per connection; GitHub and GHES keep the Git Data API, Gitea writes by git push from koinon-api; GitHub, GHES and Gitea in v0.1 |
| [0063](adr/0063-forge-credential-classes.md) | Weaker credentials allowed with compensation: one Gitea bot per connection, read token in the sandbox via placeholder, write token in koinon-api; warnings, no blocking or rotation; narrow exception to 0043 |
| [0064](adr/0064-work-tracker-connectors.md) | Work trackers through tools and signed-webhook triggers (polling fallback); no sync, link or import; v0.3 |
| [0065](adr/0065-work-model-tasks-inbox.md) | A2A-shaped tasks replace tickets; the inbox is home; `ask_user` and `ask_agent` park and resume, with safe defaults, scoped approvals and spawn guards; home checkpoints with lineage and an archive tier in v0.1; ephemeral agents; failure classes |
| [0066](adr/0066-triggers-and-scheduling.md) | DBOS Go on Koinon's Postgres; four trigger types on the agent version; signed webhooks, Socket Mode, polling; correlation keys; latency defaults tunable per org; only people interrupt a running turn; task-scoped `watch` (not agent-created triggers); warm pools for home-less sandboxes in v0.3 |
| [0067](adr/0067-concierge.md) | Concierge in koinon-api, acting as the user capped at their role (exception to 0005); plan, approve, execute; v0.3 |

## Revisions from the 28 October 2026 decisions

| ADR | Change |
| --- | --- |
| [0068](adr/0068-prompt-caching-always-on.md) | Prompt caching always on for every model with a `cache_read` price, no switch to turn it off; markers set per provider format; uncached runs warn and record `run.uncached`; OpenRouter runs send the run ID as `session_id` |
| [0069](adr/0069-native-model-formats.md) | Each model family speaks its native API: the wire format belongs to the model (`openai` or `anthropic`), defaults in one `internal/modelformat` table, admin override; gateway renders each model in its format; Claude uses Messages, OpenRouter too since the live check of 2026-10-29; thinking is on unless the agent turns it off, decided by one `internal/thinking` table and pinned in the gateway (amendment 2026-10-29) |
| [0070](adr/0070-model-metadata.md) | Reviewed model metadata snapshot from models.dev only (context windows, limits, capabilities); weekly sync PR, the catalog wins and conflicts are flagged; OpenRouter's catalog is never embedded, each install fetches it (amendment 2026-11-11); optional online refresh only proposes; after v0.1.0 |
| [0071](adr/0071-skill-library.md) | Skill library: platform and org libraries of immutable, digest-pinned skill versions referenced by agent versions and templates; one-line import (`npx skills add`, `owner/repo`, marketplaces), upload, paste or in-browser editor, fetched by the control plane; SkillSpector and Cisco skill-scanner always run in a control-plane Job, optional LLM judge on the org's model; review gate `auto` (default), `auto-if-clean` or `human`, org overrides platform, Blocked always possible; agentd writes skills to `/sandbox/.koinon/skills` and checks digests every prompt and turn; after v0.1.0 |
| [0073](adr/0073-agent-passport.md) | Agent passport: a short-lived, Koinon-signed JWT (org, agent, version, run, task, requested-by, approval, scope, RFC 8693 `act` chain), one audience per receiver, 5 min default; minted by a Koinon token service and attached by agentgateway (ext_authz or token exchange, after a spike), never visible to the sandbox or logged; a gateway key per run; on for Koinon's and org-run servers, opt-in for third parties; JWKS at `/.well-known/jwks.json` with an optional bucket mirror; Koinon as OAuth authorization server for MCP with per-user OAuth in v0.2; after v0.1.0 |
| [0074](adr/0074-run-traces-trace-router.md) | Run traces: agentd emits turn, tool and wait spans, and agentgateway sends model-call spans only to a Koinon trace router in koinon-api, which attributes them by run key (`session_id` until per-run keys), sets the ledger's cost, scrubs per org content level (`none` default, `redacted`, `full` for owners only, audited; platform ceiling) and exports through bounded queues to org endpoints (that org only) and a platform endpoint (level `none` unless an org opts in); GenAI conventions pinned to a dated commit; one trace per turn, run and task as the session, no span across parks; credentials in the secret store, endpoints tested before save; agentd-only first (rc.9, before v0.1.0), then gateway spans |
| [0075](adr/0075-isolation-tiers.md) | Isolation tiers per install: `vm` (Kata, AKS Pod Sandboxing, GKE `microvm`; golden path), `hardened` (runc, user namespaces, SELinux, a tainted sandbox pool without supervisor pods, enforced NetworkPolicy; several orgs with a warning, optional per-org pools) and `dev` (kind and CI, red); preflight Landlock and seccomp-notify probes; the tier shown in the UI, agent pages, audit and refusals; Kata escape spec "not applicable" outside `vm`; gVisor a non-goal; amends 0045; pending Bottlerocket qualification |
| [0076](adr/0076-database-modes.md) | `postgres.mode`: `external` (RDS or any Postgres 18+), `cnpg` (an existing operator, never installed by Koinon) or `standalone` (Koinon's single-replica StatefulSet on the official `postgres:18` image pinned by digest, TLS and least-privilege roles, a `pg_dump` CronJob to a PVC or S3 that also backs up the KEK; documented limits and a migration path to RDS or CNPG); no default mode (the install fails and lists the three; the installer's `eval` profile sets `standalone`); Bitnami's chart rejected after its 2025 changes; amends 0049 |
| [0077](adr/0077-pinned-openshell-and-installer.md) | Each release pins and tests exact OpenShell and Agent Sandbox versions in `release/versions.yaml` (read by preflight, docs check, e2e and installer), within OpenShell's latest-two-minor support window; two equal install paths for the same pinned releases, as Argo CD and Istio do: plain `helm install` of each chart in a documented order, and a `koinon install`/`upgrade` CLI (`--render` for GitOps) that runs only those documented steps; neither installs CNPG, a CNI or Kata; the Agent Sandbox chart is vendored; bring-your-own with a version check; umbrella chart with eval and production profiles in v0.2, operator and appliance later |
| [0078](adr/0078-sandbox-observer.md) | The reconciler reads the pod of a sandbox that never became ready (`get` only, by name, in workspace namespaces; the Agent Sandbox object when the pod is missing) and records the kubelet's reason and message in place of OpenShell's opaque `ConfigurationPending`, so image pulls and crash loops get their own wait causes (`image_pull`, new `crash_loop` and `container_create`; the supervisor pod is read too); access is one reconciler-created RoleBinding per namespace to a chart ClusterRole, bounded by a native ValidatingAdmissionPolicy (labelled OpenShell namespaces of `openshell.gatewayId`, fixed name, role and subject); `sandboxObserver.enabled` on by default with an opt-out and a preflight warning; a workaround removed when OpenShell surfaces pod failures itself; amends 0048 and 0066 |
| [0079](adr/0079-dependency-cooldown.md) | No dependency version (Go, npm, Actions, images, Helm, ADR 0077 platform pins; majors included) enters Koinon until it is 3 days old; enforced by Dependabot `cooldown`, strict pnpm `minimumReleaseAge` with `trustPolicy: no-downgrade`, and a CI age check (label-only waiver, procoduck only); security fixes take a fast path (reviewed advisory with the lowest fixing version, govulncheck reachability or high/critical/KEV, verified publication, human diff review, exact-version pnpm exemptions); accepted gap: multi-day CI-tool and image compromises (tj-actions, Trivy, Kong) and long-dwell attacks; amends 0052 |

The Build-vs-reuse rows and execution layer notes that mention LiteLLM, `inference.local` or Pod
Identity are superseded by these ADRs.

## Execution layer details

OpenShell replaces the egress lock, egress proxy and escape-suite work originally planned.

What OpenShell provides:
- Gateway: sandbox lifecycle and auth boundary (OIDC with roles, mTLS); its Kubernetes compute
  driver creates `agents.x-k8s.io` `Sandbox` resources, honours `runtimeClassName`, suspends
  instead of deleting so the workspace PVC survives.
- Supervisor per sandbox: Landlock, seccomp, unprivileged identity, all traffic through its proxy.
- Network policy in YAML (OPA/Rego): deny by default, matched by binary, host, method and path for
  HTTP, GraphQL and MCP; hot-reloaded; every decision logged.
- Credentials in gateway provider records (Vault optional); placeholders swapped only for
  authorised endpoints.
- Inference through `inference.local`; one provider per gateway, so point it at LiteLLM.
- Workspaces (RFC 0011, in progress) with Platform Admin, Workspace Admin and User roles.

What we keep:
- Kata as default `runtimeClassName`.
- A ValidatingAdmissionPolicy on sandbox namespaces (allowed runtime classes, no `hostNetwork`,
  no ephemeral containers, pinned supervisor image). Never Kyverno.
- AWS SigV4 and CLI-only tools go through MCP servers with Pod Identity.
- The escape suite, run against OpenShell sandboxes as an acceptance test.

Verify before relying on it (OpenShell spike):
- [ ] Credential mechanism: launch material says placeholders, README says provider values injected as env vars into agent child processes.
- [ ] Kubernetes path is labelled experimental; AppArmor defaults to `Unconfined`.
- [ ] Policy granularity expresses our risk tiers and exact-request approvals.
- [ ] Gateway API covers what the reconciler needs (sandbox templates, providers, policies per agent).

## Open items

- [ ] OpenShell spike on the home lab (criteria above)
- [ ] Hands-on evaluation of Multica and LiteLLM Agent Platform; contribute vs build
- [ ] Join OpenShell operator discussion #1719; align orgs with workspaces RFC 0011
- [ ] Evaluate ToolHive for shared MCP servers
- [ ] Deep dive on `agentd`: run lifecycle, ACP client, parking and resume
- [ ] Full vendor terms-of-service review before any subscription use
- [ ] Check availability: `koinon` GitHub org, `koinon.dev` domain, trademarks
- [ ] Verify Restate's server license before reconsidering it

## Prior art

| Project | Overlap | Missing |
| --- | --- | --- |
| NVIDIA OpenShell | gateway, supervisor, L7 policy, credential placeholders, inference routing, Agent Sandbox driver, OIDC roles, workspaces RFC | orgs, tickets, A2A, connectors, product UI |
| LiteLLM Agent Platform (MIT) | Agent Sandbox, Postgres, dashboard, credential vault proxy | alpha, OpenCode only, no org model, tickets or A2A |
| Multica | agents on an issue board, Go + sqlc + Postgres, Helm, workspaces and roles | local daemon runtime; sandboxing and secrets undocumented |
| OpenSandbox (Apache-2.0) | Kata/gVisor/Firecracker, namespace tenancy, credential vault sidecar | sandbox only |
| Kortix, Omnigent, Agyn | credential brokering, policies, harness wrapping | no Kubernetes-native org platform |
| Paperclip, OpenClaw operator | company model, heartbeats, budgets | no Kubernetes-native isolation |
| kagent 1.x (v1.0.0-rc2, 2026-11-04) | agents as CRDs, A2A; kagent injects credentials outside the sandbox through Substrate's egress gateway, runs Claude and Codex harnesses | work layer (orgs, triggers, tasks, inbox, code host), approvals executed outside the runtime (`requireApproval` is enforced in its ADK), spend and run caps, org tenancy |
| Ark, Kagenti | agents as CRDs, A2A, SPIFFE (Kagenti) | no sandbox credential model, tickets or tenancy |
| Agent Substrate (v1.0.0-rc.2, 2026-11-04; CNCF Sandbox) | actor runtime with memory, rootfs and volume snapshots, gVisor or Cloud Hypervisor, SPIFFE per actor, egress credential injection | pre-1.0, no OpenShell support (gVisor lacks Landlock and seccomp notify), GKE-first; not adopted for v0.x, see ADR 0101 |
| celld, Restate, Dapr Actors | Durable Objects / virtual actor patterns | building blocks, not adopted in v0.x |
| Hindsight, Mem0, Letta MemFS | agent memory | building blocks |

## Key sources

- NVIDIA OpenShell: https://github.com/NVIDIA/OpenShell
- OpenShell compute drivers: https://docs.nvidia.com/openshell/v0.0.116/reference/sandbox-compute-drivers
- OpenShell Helm chart: https://github.com/NVIDIA/OpenShell/blob/main/deploy/helm/openshell/README.md
- OpenShell inference routing: https://docs.nvidia.com/openshell/sandboxes/inference-routing
- OpenShell operator discussion: https://github.com/NVIDIA/OpenShell/issues/1719
- Kubernetes Agent Sandbox: https://agent-sandbox.sigs.k8s.io/docs/getting_started/overview/
- OpenSandbox review: https://wavect.io/blog/opensandbox-ai-agent-sandbox-review/
- Multica: https://github.com/multica-ai/multica
- Atlas Community Edition limits: https://www.atlasgo.io/community-edition
- Anthropic Managed Agents memory: https://sdtimes.com/anthropic/anthropic-adds-memory-to-claude-managed-agents/
- Letta MemFS: https://docs.letta.com/concepts/memfs
- Hindsight MCP memory: https://hindsight.vectorize.io/blog/2026/03/04/mcp-agent-memory
- Restate Virtual Objects: https://docs.restate.dev/concepts/services
- celld: https://github.com/denoland/celld
- grill-me skill: https://skills.sh/mattpocock/skills/grilling
