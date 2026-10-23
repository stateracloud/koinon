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
| [0003](0003-postgres-source-of-truth.md) | Source of truth | Accepted |
| [0004](0004-human-identity.md) | Human identity | Accepted |
| [0005](0005-agent-identity.md) | Agent identity | Accepted |
| [0006](0006-identity-layers.md) | Identity layers | Accepted, amended by 0043, 0044 |
| [0007](0007-connector-model.md) | Connector model | Accepted, amended by 0049 |
| [0008](0008-approvals-and-risk-tiers.md) | Approvals | Accepted, amended by 0042 |
| [0009](0009-agent-runtime.md) | Agent runtime | Accepted |
| [0010](0010-eventing.md) | Eventing | Accepted |
| [0011](0011-agent-to-agent.md) | Agent-to-agent | Accepted |
| [0012](0012-cross-org-sharing.md) | Cross-org sharing | Accepted |
| [0013](0013-model-access.md) | Model access | Accepted, amended by 0040, 0041, 0044 |
| [0014](0014-mcp-and-tools.md) | MCP and tools | Accepted, amended by 0040 |
| [0015](0015-agent-environment-and-browser.md) | Agent environment and browser | Accepted, amended by 0044 |
| [0016](0016-harness-and-deployment.md) | Harness and deployment | Accepted, amended by 0049 |
| [0017](0017-board-and-chat-interaction-model.md) | Interaction model | Accepted |
| [0018](0018-v1-screens.md) | v1 screens | Accepted |
| [0019](0019-personal-agents-and-teams.md) | Personal agents and teams | Accepted |
| [0020](0020-agent-builder-and-templates.md) | Creating agents | Accepted |
| [0021](0021-connector-setup-guides.md) | Connector setup | Accepted |
| [0022](0022-frontend-architecture.md) | Frontend | Accepted |
| [0023](0023-observability-and-audit.md) | Observability and audit | Accepted |
| [0024](0024-roadmap.md) | Roadmap | Accepted |
| [0025](0025-testing-strategy.md) | Testing | Accepted |
| [0026](0026-home-lab-and-license.md) | Home lab and license | Accepted |
| [0027](0027-repo-and-versioning.md) | Repo and versioning | Accepted |
| [0028](0028-name-koinon.md) | Name | Accepted |
| [0029](0029-run-level-resume.md) | Resume | Accepted |
| [0030](0030-run-state-machine.md) | Run state | Accepted |
| [0031](0031-artifacts-in-object-storage.md) | Artifacts | Accepted |
| [0032](0032-agent-home-checkpoints.md) | Agent home | Accepted |
| [0033](0033-file-based-memory.md) | Memory | Accepted |
| [0034](0034-project-identity.md) | Identity (repo, module, domain) | Accepted |
| [0035](0035-v01-flavors.md) | v0.1 flavors | Accepted |
| [0036](0036-v01-demo.md) | v0.1 demo | Accepted, amended by 0049 |
| [0037](0037-tooling.md) | Tooling | Accepted |
| [0038](0038-governance.md) | Governance | Accepted |
| [0039](0039-development-setup.md) | Development setup | Accepted |
| [0040](0040-agentgateway-model-and-mcp-gateway.md) | agentgateway as the model gateway and MCP gateway | Accepted |
| [0041](0041-koinon-spend-ledger-and-budget-stops.md) | Koinon spend ledger and budget stops | Accepted |
| [0042](0042-ask-tier-park-and-execute.md) | Ask tier: park the run, Koinon executes the approved call | Accepted |
| [0043](0043-workload-identity-adapters-and-koinon-issuer.md) | Workload identity adapters and a Koinon OIDC issuer | Accepted |
| [0044](0044-in-sandbox-credential-exception.md) | In-sandbox credentials as a gated exception | Accepted |
| [0045](0045-openshell-hardening-defaults.md) | OpenShell hardening defaults | Accepted |
| [0046](0046-adr-conventions.md) | ADR conventions | Accepted |
| [0047](0047-repository-scaffold.md) | Repository scaffold, data model foundations and CI gates | Accepted |
| [0048](0048-openshell-spike-results.md) | OpenShell spike results | Proposed |
| [0049](0049-v01-scope-decisions.md) | v0.1 scope: template, approvals, gateway modes, agentd placement | Accepted |
| [0050](0050-spend-ledger-ingestion-and-net-limits.md) | Spend ledger ingestion and net limits | Proposed |
| [0051](0051-session-home-generations.md) | Session home generations | Accepted |
| [0052](0052-opencode-v2-flavor.md) | OpenCode flavor on OpenCode v2 | Accepted |
