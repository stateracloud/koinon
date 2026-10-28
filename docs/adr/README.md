# Architecture decision records

ADRs are the canonical record of Koinon's architecture decisions. [`../design.md`](../design.md)
is the one-page overview; each of its decisions links to the ADR with the same number. Conventions
are in [0046](0046-adr-conventions.md).

## Adding an ADR

1. Copy [`0000-template.md`](0000-template.md) to `NNNN-short-title.md` with the next free number.
2. Fill in every section; mark reconstructed alternatives and inferred consequences.
3. If it changes an earlier ADR, add "Amends" here and "Amended by" there, or "Superseded by".
4. Open a pull request; update the index below and `docs/design.md` in the same PR.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-full-multi-tenancy.md) | Tenancy | Accepted |
| [0002](0002-namespace-per-org-isolation.md) | Isolation unit | Accepted |
| [0003](0003-postgres-source-of-truth.md) | Source of truth | Accepted, amended 2026-10-26 |
| [0004](0004-human-identity.md) | Human identity | Accepted, amended 2026-10-23, 2026-10-26 |
| [0005](0005-agent-identity.md) | Agent identity | Accepted, amended by 0067 |
| [0006](0006-identity-layers.md) | Identity layers | Accepted, amended by 0043, 0044, 0063, 0065 |
| [0007](0007-connector-model.md) | Connector model | Accepted, amended by 0049, 0056, 0062, 0063, 0064 |
| [0008](0008-approvals-and-risk-tiers.md) | Approvals | Accepted, amended by 0042 |
| [0009](0009-agent-runtime.md) | Agent runtime | Accepted |
| [0010](0010-eventing.md) | Eventing | Superseded by 0065 and 0066 |
| [0011](0011-agent-to-agent.md) | Agent-to-agent | Accepted, amended by 0055, 0065 |
| [0012](0012-cross-org-sharing.md) | Cross-org sharing | Accepted |
| [0013](0013-model-access.md) | Model access | Accepted, amended by 0040, 0041, 0044 |
| [0014](0014-mcp-and-tools.md) | MCP and tools | Accepted, amended by 0040, 0057 |
| [0015](0015-agent-environment-and-browser.md) | Agent environment and browser | Accepted, amended by 0044 |
| [0016](0016-harness-and-deployment.md) | Harness and deployment | Accepted, amended by 0040, 0049 |
| [0017](0017-board-and-chat-interaction-model.md) | Interaction model | Superseded by 0055 and 0065 |
| [0018](0018-v1-screens.md) | v1 screens | Accepted, amended by 0055, 0065, 0066 |
| [0019](0019-personal-agents-and-teams.md) | Personal agents and teams | Accepted, amended by 0065 |
| [0020](0020-agent-builder-and-templates.md) | Creating agents | Accepted |
| [0021](0021-connector-setup-guides.md) | Connector setup | Accepted, amended by 0056 |
| [0022](0022-frontend-architecture.md) | Frontend | Accepted |
| [0023](0023-observability-and-audit.md) | Observability and audit | Accepted, amended 2026-10-26 |
| [0024](0024-roadmap.md) | Roadmap | Accepted, amended 2026-10-26 (v0.1 to v0.3 from #146) |
| [0025](0025-testing-strategy.md) | Testing | Accepted |
| [0026](0026-home-lab-and-license.md) | Home lab and license | Accepted |
| [0027](0027-repo-and-versioning.md) | Repo and versioning | Accepted, amended 2026-10-25 |
| [0028](0028-name-koinon.md) | Name | Accepted |
| [0029](0029-run-level-resume.md) | Resume | Accepted, amended by 0051 |
| [0030](0030-run-state-machine.md) | Run state | Accepted, amended by 0065, 0066 |
| [0031](0031-artifacts-in-object-storage.md) | Artifacts | Accepted |
| [0032](0032-agent-home-checkpoints.md) | Agent home | Accepted, amended by 0065 (checkpoints in v0.1, archive tier) and HC1 (kopia, exec-stream runner, hourly points) |
| [0033](0033-file-based-memory.md) | Memory | Accepted |
| [0034](0034-project-identity.md) | Identity (repo, module, domain) | Accepted, amended 2026-10-26 |
| [0035](0035-v01-flavors.md) | v0.1 flavors | Accepted, amended by 0059 |
| [0036](0036-v01-demo.md) | v0.1 demo | Accepted, amended by 0049, 0062, 0065 |
| [0037](0037-tooling.md) | Tooling | Accepted |
| [0038](0038-governance.md) | Governance | Accepted |
| [0039](0039-development-setup.md) | Development setup | Accepted |
| [0040](0040-agentgateway-model-and-mcp-gateway.md) | agentgateway as the model gateway and MCP gateway | Accepted, amended by 0048 and 2026-10-26 |
| [0041](0041-koinon-spend-ledger-and-budget-stops.md) | Koinon spend ledger and budget stops | Accepted, amended by 0050, 0056 |
| [0042](0042-ask-tier-park-and-execute.md) | Ask tier: park the run, Koinon executes the approved call | Accepted, amended by 0048, 0065 |
| [0043](0043-workload-identity-adapters-and-koinon-issuer.md) | Workload identity adapters and a Koinon OIDC issuer | Accepted, amended by 0063 |
| [0044](0044-in-sandbox-credential-exception.md) | In-sandbox credentials as a gated exception | Accepted |
| [0045](0045-openshell-hardening-defaults.md) | OpenShell hardening defaults | Accepted, amended by 0048 |
| [0046](0046-adr-conventions.md) | ADR conventions | Accepted |
| [0047](0047-repository-scaffold.md) | Repository scaffold, data model foundations and CI gates | Accepted, amended 2026-10-26 |
| [0048](0048-openshell-spike-results.md) | OpenShell spike results | Accepted (2026-10-26, after implementation), amended by 0052 and 2026-10-26 |
| [0049](0049-v01-scope-decisions.md) | v0.1 scope: template, approvals, gateway modes, agentd placement | Accepted, amended by 0051, 0056, 0058, 0062, 0063, 0065, 0066 and (Kubernetes mode on native CRDs, ledger budgets) by its own G2 amendment |
| [0050](0050-spend-ledger-ingestion-and-net-limits.md) | Spend ledger ingestion and net limits | Accepted (2026-10-26, after implementation), amended by 0056 and 2026-10-23 |
| [0051](0051-session-home-generations.md) | Session home generations | Accepted, amended by 0054, 0065 and 2026-10-23, 2026-10-24, 2026-10-26 |
| [0052](0052-opencode-v2-flavor.md) | OpenCode flavor on OpenCode v2 | Accepted |
| [0053](0053-hermes-context-file-patch.md) | Hermes context-file build patch: redo on every upgrade, with a guard test | Superseded by 0059 |
| [0054](0054-unkillable-sandbox-process.md) | A sandbox with an unkillable process is recreated | Accepted, amended 2026-10-24 |
| [0055](0055-chat-as-its-own-object.md) | Chat is its own object | Accepted, amended by 0065 |
| [0056](0056-model-providers-pricing-and-budgets-per-org.md) | Model providers, models, pricing and budgets per org | Accepted, amended 2026-10-23, 2026-10-24 |
| [0057](0057-mcp-servers.md) | MCP servers: remote and container, catalog, allowlists and discovery | Accepted, amended 2026-10-24 |
| [0058](0058-gateway-modes.md) | Gateway modes: standalone, kubernetes and shared | Accepted, amended 2026-10-25, 2026-10-26 |
| [0059](0059-v01-opencode-only.md) | v0.1 ships one harness: OpenCode v2 | Accepted |
| [0060](0060-migrations-as-a-job.md) | Migrations run as a Job, not on start; an Argo CD Sync hook under Argo | Accepted |
| [0061](0061-image-pull-secrets-and-registry-mirrors.md) | Image pull secrets and registry mirrors: Koinon rewrites its images; OpenShell pulls for sandboxes | Accepted |
| [0062](0062-code-host-connectors.md) | Code-host connectors: one interface per provider, capabilities as data | Accepted |
| [0063](0063-forge-credential-classes.md) | Code-host credential classes: allow weaker credentials, with compensation | Accepted |
| [0064](0064-work-tracker-connectors.md) | Work-tracker connectors: tools and triggers, no sync | Accepted (delivery v0.3) |
| [0065](0065-work-model-tasks-inbox.md) | Work model: A2A-shaped tasks, the inbox, and asking people and agents | Accepted |
| [0066](0066-triggers-and-scheduling.md) | Triggers and scheduling: DBOS on Koinon's Postgres, triggers on the agent version | Accepted |
| [0067](0067-concierge.md) | The concierge: a setup and management assistant in koinon-api | Accepted (delivery v0.3) |
| [0068](0068-prompt-caching-always-on.md) | Prompt caching is always on for models that support it | Accepted |
| [0069](0069-native-model-formats.md) | Models speak their family's native API format | Accepted |
| [0070](0070-model-metadata.md) | Model metadata: a reviewed snapshot from models.dev and OpenRouter | Accepted (delivery after v0.1.0, UI milestone) |
| [0071](0071-skill-library.md) | The skill library: pinned, scanned skills delivered by agentd | Accepted (delivery after v0.1.0, UI milestone) |
| [0072](0072-agent-model-run-now-echo-check.md) | The agent's model setting, "run now" for schedules, and the provider echo check | Accepted |
| [0073](0073-agent-passport.md) | Agent passport: a verifiable identity for every agent call | Accepted (delivery after v0.1.0) |
