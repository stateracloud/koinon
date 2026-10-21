# Koinon design decisions

Status: accepted design, 21 October 2026. Owner: procoduck (@procoduck).

Koinon (pronounced KOY-non, after the leagues of independent Greek city-states) is an open source
(Apache-2.0) org-and-work layer for AI agents on Kubernetes. Orgs get their own agents, connectors
and secrets; agents pick up work from a ticket board and talk over A2A. Execution, isolation,
credential brokering and inference routing are reused from NVIDIA OpenShell on Kubernetes Agent
Sandbox instead of being built.

## Architecture at a glance

```
Web UI ─────────────┐        ┌──── Slack, Teams, webhooks, crons
                    ▼        ▼
┌──────────────── Koinon control plane (we build) ────────────────┐
│ API + Postgres          Reconciler                A2A gateway   │
│ orgs, grants,           renders workspaces,       agent-to-agent│
│ tickets, audit          policies, providers,      tasks, links, │
│                         sandbox templates         depth policy  │
└───────────────────────────────┬─────────────────────────────────┘
                                │ renders
┌──────────── NVIDIA OpenShell (reused) ───────┐  ┌─ Shared services (reused) ─┐
│ OpenShell gateway: lifecycle, providers,     │◄─│ Secret store (Vault/OpenBao)│
│ policies, inference.local                    │  │                             │
│ Sandbox per agent (Agent Sandbox, Kata):     │─►│ LiteLLM: models, budgets    │
│  agentd drives the harness over ACP          │  │                             │
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
| Model routing, budgets, virtual keys | LiteLLM behind OpenShell's `inference.local` | budget hierarchy and cost per ticket |
| Shared MCP servers | ToolHive operator and registry (to evaluate) | grants and the progressive per-agent endpoint |
| Secret storage | Vault or OpenBao | connection cards and secure inputs |
| Orgs, personal orgs, roles, cross-org links | — | all of it, aligned with OpenShell workspaces (RFC 0011) |
| Tickets, A2A, parking, approvals, routines, webhooks | — | all of it |
| Connector types, setup guides, agent builder, templates | — | all of it |
| Board, ticket, agents and admin UI | — | all of it |

## Decisions

### Platform architecture (1–16)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| 1 | Tenancy | Full multi-tenancy: a team or user gets an org and sets up its own agents, connector cards and secrets; cross-org agent use is opt-in | Retrofitting tenancy later is the expensive mistake |
| 2 | Isolation unit | One namespace per org, mapped to one OpenShell workspace (`workspaceMode: managed`); optional dedicated NodePools per org; placement kept abstract | The OpenShell supervisor and Kata VM are the real boundary; the namespace scopes quotas, RBAC and secret refs |
| 3 | Source of truth | Postgres via a Go API (encryption at rest, TLS, least-privilege roles, PITR); a reconciler renders Kubernetes and OpenShell objects; GitOps only for the platform layer | Self-service authz Kubernetes RBAC can't express; tickets and audit don't belong in etcd |
| 4 | Human identity | Generic OIDC; memberships and roles in Postgres; per-org mode `platform`, `idp` or `hybrid` (highest role wins); at least one manual owner; group sync at login, SCIM later; `platform-admin` above all orgs | Teams differ in how much they use their IdP |
| 5 | Agent identity | Agents act as themselves; every run records `requested_by` and `approved_by`, added to PRs, commits and audit; on-behalf-of only as a per-connector opt-in | Crons have no user, delegated tokens expire, permissions stay designed per agent |
| 6 | Identity layers | Per-agent ServiceAccount; the sandbox gets no usable token; federated credentials (AWS, Entra, ADO, Vault) are minted outside the sandbox and held as OpenShell provider records injected only for allowed endpoints; SPIFFE-style names now, SPIRE later; connector auth types `federated`, `app-credential`, `delegated-user`; A2A uses OpenShell-issued sandbox tokens or ServiceAccount JWTs plus link policy | Cloud providers trust the cluster OIDC issuer; Slack and GitHub need app credentials |
| 7 | Connector model | Type (platform-curated: auth, hosts, scopes, setup guide) → connection (per-org card) → grant (per agent, subset of scopes); built-ins Slack, Teams, GitHub, Azure DevOps, AWS, Azure, generic HTTP API | Platform isn't a bottleneck, scopes stay enforceable |
| 8 | Approvals | Platform risk tiers per scope (read, reversible write, external or irreversible, forbidden); presets per grant (Read only, Standard, Trusted); optional intent classifier for the ask tier; learned rule suggestions (aligned with OpenShell RFC 0002); each grant compiles to OpenShell network policy (binary, host, method, path) plus MCP gateway tool rules; approval bound to the exact request; run parks while waiting; per-grant approver policy, optional two-person rule | Boundary does the work, humans only see truly risky actions |
| 9 | Agent runtime | Persistent agent, ephemeral compute: durable home volume; one OpenShell sandbox per agent, suspended between runs (`operatingMode: Suspended`); optional `alwaysOn`; one run per agent at a time; flavor only selects the harness image | Remembers and resumes at near-zero idle cost |
| 10 | Eventing | v1: the ticket board is the queue; webhooks, crons and internal events match org subscriptions and create a ticket or append to an open one (dedupe = debounce); CloudEvents envelope; budgets per subscription; event bus only if v1 shows the need | No dual-write problem, nothing extra to operate |
| 11 | Agent-to-agent | A2A is the protocol; every A2A task is persisted as a ticket; short wait then park; `input-required` for clarification; agent cards for discovery; max depth, cycle detection, cost inherited by parent, results as quoted data | Dynamic back-and-forth with a durable, visible record |
| 12 | Cross-org sharing | Private (default), shared on request, or public (platform-admin only); links carry an exposure profile (reduced grants), rate limit and budget; consumer org pays; calls run in the provider org, approvals go to provider approvers | Prevents the confused deputy |
| 13 | Model access | Sandboxes call OpenShell `inference.local`, which forwards to LiteLLM (API keys, OpenAI-compatible self-hosted models, provider presets); virtual keys carry budgets platform → org → agent → ticket, soft 80% and hard 100% (run parks); v1 = popular providers, API keys, GitHub Copilot via OpenCode; subscriptions personal-only and later; review vendor terms of service first | One path for budgets and observability |
| 14 | MCP and tools | Shared MCP servers deployed from the UI, outside sandboxes, behind one progressive (search + call) endpoint per agent with grants; local MCP servers declared per agent and run inside the sandbox; evaluate ToolHive | Low context overhead, credentials out of the sandbox |
| 15 | Agent environment and browser | Flavor images based on OpenShell sandbox images; agents self-install into `$HOME` (mise/aqua, uv, npm) from allowlisted sources via a pull-through cache; installs recorded in `environment.lock`; org admins promote common tools; browser in the sandbox with per-agent profile, credentials only through human takeover, cookies treated as secrets | Agents find tool gaps at runtime; cookies would bypass the secret model |
| 16 | Harness and deployment | Control plane talks to the OpenShell gateway (gRPC) through an `ExecutionBackend` interface (OpenSandbox as alternative); a thin `agentd` in the sandbox drives the harness over ACP; OpenShell supervisor does all enforcement; dedicated agents cluster is the reference, co-location supported with a small cluster-scoped footprint and an install-time isolation-level check | Reuses a maintained execution layer; one contract per flavor |

### UI, operations and project (17–27)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| 17 | Interaction model | Board and chat are two views of one thing: board is home, every conversation is a ticket, `waiting on you` is the key column, Slack and Teams mirror tickets, configuration is secondary | Board stays the queue; agents feel like colleagues |
| 18 | v1 screens | Board, Ticket, Agents, Agent editor, Connections, MCP servers, Org settings, Platform admin, Audit; every user gets an automatic personal org | Personal agents need no special code path |
| 19 | Personal agents and teams | Membership-implied access: personal agents may call team agents that accept "org members' personal agents", capped by exposure profile and the user's role; personal org pays; default on for read-tier work only | Personal agents act like employees asking the experts |
| 20 | Creating agents | Agent builder (a platform agent) is the main path, templates are building blocks; result is always a draft; only an org admin or owner activates and grants | Builder proposes access, never grants it |
| 21 | Connector setup | Connector types ship machine-readable setup guides (fields, steps, validation check) shared by UI cards and builder; builder generates setup-specific config; secrets via inline secure inputs straight to Vault; each step validated | Help at the moment of failure, secrets never in chat |
| 22 | Frontend | React SPA (Vite) served by the Go API; connect-rpc with generated TypeScript client; streaming updates from the outbox; backend-for-frontend OIDC with httpOnly cookie; noVNC live view via the API; shadcn/ui and TanStack; virtualized lists, optimistic updates | Fast, reactive, no second backend |
| 23 | Observability and audit | All optional: `/metrics` and optional ServiceMonitors; run traces only to an OTLP endpoint an org or platform admin supplies; audit on by default, hash-chained in Postgres, opt-out per platform policy; business records always kept | Operators choose their stack |
| 24 | Roadmap | v0.1 run an agent → v0.2 connectors, tiers, routines, webhooks → v0.3 A2A, parking, builder → v0.4 multi-tenancy → v0.5 MCP, browser, self-installed tools → v1.0 cross-org, security review, stable APIs; `org_id` in the data model from v0.1 (see 36 and 37 for v0.1 detail) | Each release installable and demo-able |
| 25 | Testing | Ginkgo/Gomega, envtest, testcontainers-go; Vitest and Playwright; fake OpenAI/Anthropic model server so CI never calls paid models; flavor contract tests; e2e on kind; sandbox escape suite against OpenShell sandboxes on every PR; capped nightly evals; free GitHub runners | Deterministic, free CI; core security promise tested continuously |
| 26 | Home lab and license | Home lab Talos cluster with the Kata runtime for development and local testing only, never public CI; Apache-2.0 with DCO sign-off, no CLA | Real isolation testing at no cloud cost |
| 27 | Repo and versioning | Monorepo (`api/ cmd/ internal/ web/ flavors/ connectors/ templates/ charts/ test/ docs/`); ko for Go images; connectors and templates as data; SemVer, one shared version; CRDs `v1alpha1` until 1.0; `buf breaking` in CI; forward-only migrations with an upgrade test; GoReleaser, cosign, SBOMs, OCI Helm chart | Components move together; supply-chain hygiene |

### State, resume, artifacts and memory (28–33)

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| 28 | Name | Koinon (KOY-non) | Independent orgs that cooperate through links |
| 29 | Resume | Run-level resume: the harness continues from its last saved session turn (`--resume`, ACP `session/load`); repeated turns are safe because the MCP gateway and OpenShell supervisor record external writes per run with idempotency keys; step-level workflows later | Harness loops can't be checkpointed from outside |
| 30 | Run state | No workflow engine in v0.x: Postgres state machine per run with an ownership epoch per agent (stale writers rejected) and an alarms table, behind an `AgentStateStore` interface; Restate re-evaluated with workflows; celld on the watch list | Short, fixed process; Durable Objects pattern without its runtime |
| 31 | Artifacts | S3-compatible storage (AWS S3, MinIO, SeaweedFS) under an org prefix, metadata in Postgres; agents publish via a `publish_artifact` tool; referenced by ID from tickets and A2A results; signed URLs after a permission check; retention per org | Standard pattern; fits A2A artifacts |
| 32 | Agent home | PVC checkpointed to S3 at park, run end and periodically; incremental (restic or kopia) with history; includes memory, sessions, `environment.lock`, encrypted browser profile; excludes checkouts and caches; carries the ownership epoch; restorable in any zone | Zone-bound volumes; rollback and export |
| 33 | Memory | File-based memory stores mounted into the sandbox: agent store read-write, org knowledge read-only with admin-approved proposals, user store for personal agents; each a git repo with history and audit; harnesses pointed at them via `CLAUDE.md` / `AGENTS.md`; optional Hindsight (MIT) or Mem0 (Apache-2.0) via MCP | Memory is solved; Koinon adds scoping and audit |

### Project bootstrap (34–39), 21 October 2026

| # | Area | Decision | Why |
| --- | --- | --- | --- |
| 34 | Identity | Repo `github.com/procoduck/koinon`, Go module `github.com/procoduck/koinon`; a `koinon` GitHub org is wanted later, so a move (and module path change) must happen before v0.1 if at all; domain `koinon.dev`, CRD API groups under it (e.g. `agents.koinon.dev`) | Start fast under a personal account; avoid a path change after users exist |
| 35 | v0.1 flavors | OpenCode, Hermes and pi, all over ACP (pi via the `pi-acp` adapter); Claude Code and Codex later | Open source harnesses, any model, matching the open source project |
| 36 | v0.1 demo | 15-minute quickstart: Helm install → create an agent from a template → message it → watch the run live → approve one GitHub action; the GitHub connector moves into v0.1 (the full connector framework stays v0.2) | Shows the core promise: useful agent, safe by default |
| 37 | Tooling | goose for migrations (plain SQL, embedded, sqlc-compatible); Starlight docs site; ACP via the Go SDK; Tilt for the inner loop against the home lab | Simple, open, fits the stack |
| 38 | Governance | BDFL now (procoduck) with `MAINTAINERS.md` and a written path to maintainership; GitHub private vulnerability reporting for security issues | Light process while the project is small |
| 39 | Development setup | Claude Code works on procoduck's Mac in a new local repo; tests deploy to the home-lab Talos cluster (Kata installed); the agent has full cluster and full GitHub access but asks before big or hard-to-reverse actions | Fast iteration with real isolation |

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
| kagent, Ark, Kagenti | agents as CRDs, A2A, SPIFFE (Kagenti) | no sandbox credential model, tickets or tenancy |
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
- pi ACP adapter: https://zed.dev/acp/agent/pi
- Atlas Community Edition limits: https://www.atlasgo.io/community-edition
- Anthropic Managed Agents memory: https://sdtimes.com/anthropic/anthropic-adds-memory-to-claude-managed-agents/
- Letta MemFS: https://docs.letta.com/concepts/memfs
- Hindsight MCP memory: https://hindsight.vectorize.io/blog/2026/03/04/mcp-agent-memory
- Restate Virtual Objects: https://docs.restate.dev/concepts/services
- celld: https://github.com/denoland/celld
- grill-me skill: https://skills.sh/mattpocock/skills/grilling
