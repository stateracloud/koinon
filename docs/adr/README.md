# Architecture decision records

ADRs are the canonical record of Koinon's architecture decisions. [`../design.md`](../design.md)
is the one-page overview; each of its decisions links to the ADR with the same number. Conventions
are in [0046](0046-adr-conventions.md).

## Adding an ADR

1. Reserve the number: `hack/claim.sh adr` creates the ref `claim/adr-NNNN` and prints `NNNN`, so two
   sessions cannot take one number. Gaps are fine; duplicates fail `hack/adr-lint.sh`.
2. Copy [`0000-template.md`](0000-template.md) to `NNNN-short-title.md`. The first line must be
   `# NNNN. Title` and the file needs a `- Status:` line: the index is generated from them.
3. Fill in every section; mark reconstructed alternatives and inferred consequences.
4. If it changes an earlier ADR, add "Amends" here and "Amended by" there, or "Superseded by".
5. Run `hack/adr-index.sh` to regenerate the index below (never edit the table by hand; CI checks it
   is current) and update `docs/design.md` in the same PR when the decision is in the overview.
6. Open a pull request.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-full-multi-tenancy.md) | Full multi-tenancy from the start | Accepted |
| [0002](0002-namespace-per-org-isolation.md) | One namespace per org, mapped to one OpenShell workspace | Accepted |
| [0003](0003-postgres-source-of-truth.md) | Postgres via a Go API is the source of truth | Accepted; amended 2026-10-26 (below) |
| [0004](0004-human-identity.md) | Human identity: generic OIDC with roles in Postgres | Accepted; amended 2026-10-23, 2026-10-26 and 2026-10-30 (below); platform admin audit moved by [0100](0100-install-audit-chain.md) (note of 2026-11-07 below) |
| [0005](0005-agent-identity.md) | Agents act as themselves | Accepted |
| [0006](0006-identity-layers.md) | Identity layers: per-agent ServiceAccount, no usable token in the sandbox | Accepted; amended by [0043](0043-workload-identity-adapters-and-koinon-issuer.md), [0044](0044-in-sandbox-credential-exception.md), [0063](0063-forge-credential-classes.md), [0065](0065-work-model-tasks-inbox.md) and [0073](0073-agent-passport.md) |
| [0007](0007-connector-model.md) | Connector model: type, connection, grant | Accepted |
| [0008](0008-approvals-and-risk-tiers.md) | Approvals: risk tiers per scope, enforced at the boundary | Accepted; amended by [0042](0042-ask-tier-park-and-execute.md) |
| [0009](0009-agent-runtime.md) | Agent runtime: persistent agent, ephemeral compute | Accepted; amended 2026-10-30 and 2026-11-07 (below) |
| [0010](0010-eventing.md) | Eventing: the ticket board is the queue | Superseded by [0065](0065-work-model-tasks-inbox.md) and [0066](0066-triggers-and-scheduling.md) |
| [0011](0011-agent-to-agent.md) | Agent-to-agent: A2A, persisted as tickets | Accepted; amended by [0055](0055-chat-as-its-own-object.md), [0065](0065-work-model-tasks-inbox.md) |
| [0012](0012-cross-org-sharing.md) | Cross-org sharing through links with exposure profiles | Accepted |
| [0013](0013-model-access.md) | Model access through one gateway with hierarchical budgets | Accepted; amended by [0040](0040-agentgateway-model-and-mcp-gateway.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0044](0044-in-sandbox-credential-exception.md) and 2026-10-30 (below) |
| [0014](0014-mcp-and-tools.md) | MCP and tools: shared servers outside sandboxes, one progressive endpoint per agent | Accepted; amended by [0040](0040-agentgateway-model-and-mcp-gateway.md) and [0057](0057-mcp-servers.md) |
| [0015](0015-agent-environment-and-browser.md) | Agent environment and browser | Accepted; amended by [0044](0044-in-sandbox-credential-exception.md) |
| [0016](0016-harness-and-deployment.md) | Harness and deployment: ExecutionBackend, agentd over ACP, dedicated agents cluster | Accepted |
| [0017](0017-board-and-chat-interaction-model.md) | Board and chat are two views of one ticket | Superseded by [0055](0055-chat-as-its-own-object.md) and [0065](0065-work-model-tasks-inbox.md) |
| [0018](0018-v1-screens.md) | v1 screens and automatic personal orgs | Accepted |
| [0019](0019-personal-agents-and-teams.md) | Personal agents call team agents through membership | Accepted |
| [0020](0020-agent-builder-and-templates.md) | Agent builder proposes, admins activate | Accepted |
| [0021](0021-connector-setup-guides.md) | Machine-readable connector setup guides and secure inputs | Accepted |
| [0022](0022-frontend-architecture.md) | React SPA served by the Go API over connect-rpc | Accepted |
| [0023](0023-observability-and-audit.md) | Optional observability, hash-chained audit on by default | Accepted; amended 2026-10-26, 2026-10-28, 2026-10-30 and 2026-10-31 (below); run traces by [0074](0074-run-traces-trace-router.md); install-level audit chain and anchors by [0100](0100-install-audit-chain.md) (note of 2026-11-07 below) |
| [0024](0024-roadmap.md) | Release roadmap from v0.1 to v1.0 | Accepted |
| [0025](0025-testing-strategy.md) | Deterministic, free CI with a continuous sandbox escape suite | Accepted; amended 2026-11-02 (below); the lab as e2e evidence amended by [0105](0105-merge-queue-route.md) |
| [0026](0026-home-lab-and-license.md) | Home lab for real isolation testing; Apache-2.0 with DCO | Accepted; amended 2026-11-02 (below) |
| [0027](0027-repo-and-versioning.md) | Monorepo, one version, supply-chain hygiene | Accepted; amended 2026-10-25 (below) |
| [0028](0028-name-koinon.md) | Name the project Koinon | Accepted |
| [0029](0029-run-level-resume.md) | Run-level resume through the harness session | Accepted; amended by [0051](0051-session-home-generations.md) |
| [0030](0030-run-state-machine.md) | Postgres run state machine with an ownership epoch | Accepted |
| [0031](0031-artifacts-in-object-storage.md) | Artifacts in S3-compatible object storage | Accepted |
| [0032](0032-agent-home-checkpoints.md) | Agent home on a PVC, checkpointed incrementally to object storage | Accepted |
| [0033](0033-file-based-memory.md) | File-based, scoped memory stores | Accepted |
| [0034](0034-project-identity.md) | Repository, module path, domain and API groups | Accepted; amended 2026-10-26 (below) |
| [0035](0035-v01-flavors.md) | v0.1 flavors: OpenCode, Hermes and pi over ACP | Accepted; amended by [0059](0059-v01-opencode-only.md) |
| [0036](0036-v01-demo.md) | v0.1 demo: a 15-minute quickstart ending in one approved GitHub action | Accepted |
| [0037](0037-tooling.md) | Tooling: goose, Starlight, ACP Go SDK, Tilt | Accepted |
| [0038](0038-governance.md) | Governance: BDFL with a written path to maintainership | Accepted |
| [0039](0039-development-setup.md) | Development setup: a maintainer's Mac plus a Talos+Kata cluster | Accepted |
| [0040](0040-agentgateway-model-and-mcp-gateway.md) | agentgateway as the model gateway and MCP gateway | Accepted; amended by [0048](0048-openshell-spike-results.md) and 2026-10-26 (below) |
| [0041](0041-koinon-spend-ledger-and-budget-stops.md) | Koinon spend ledger and budget stops | Accepted; amended 2026-10-30 (below) |
| [0042](0042-ask-tier-park-and-execute.md) | Ask tier: park the run, Koinon executes the approved call | Accepted; amended by [0048](0048-openshell-spike-results.md), [0065](0065-work-model-tasks-inbox.md) and [0084](0084-leak-guard.md) |
| [0043](0043-workload-identity-adapters-and-koinon-issuer.md) | Workload identity adapters and a Koinon OIDC issuer | Accepted |
| [0044](0044-in-sandbox-credential-exception.md) | In-sandbox credentials as a gated exception | Accepted |
| [0045](0045-openshell-hardening-defaults.md) | OpenShell hardening defaults | Accepted; amended by [0048](0048-openshell-spike-results.md) and [0075](0075-isolation-tiers.md) |
| [0046](0046-adr-conventions.md) | ADR conventions | Accepted |
| [0047](0047-repository-scaffold.md) | Repository scaffold, data model foundations and CI gates | Accepted; amended 2026-10-26 (below) |
| [0048](0048-openshell-spike-results.md) | OpenShell spike results | Accepted (2026-10-26, accepted after implementation) |
| [0049](0049-v01-scope-decisions.md) | v0.1 scope: template, approvals, gateway modes and agentd placement | Accepted |
| [0050](0050-spend-ledger-ingestion-and-net-limits.md) | Spend ledger ingestion and net limits | Accepted (2026-10-26, accepted after implementation); amended by [0056](0056-model-providers-pricing-and-budgets-per-org.md) and 2026-10-23 (below) |
| [0051](0051-session-home-generations.md) | Session home generations | Accepted; amended by [0054](0054-unkillable-sandbox-process.md), [0065](0065-work-model-tasks-inbox.md) and below (2026-10-23, 2026-10-24, 2026-10-26) |
| [0052](0052-opencode-v2-flavor.md) | OpenCode flavor on OpenCode v2 | Accepted |
| [0053](0053-hermes-context-file-patch.md) | Hermes context-file build patch: redo on every upgrade, with a guard test | Superseded by [0059](0059-v01-opencode-only.md) |
| [0054](0054-unkillable-sandbox-process.md) | A sandbox with an unkillable process is recreated | Accepted; amended 2026-10-24 (below): detection, saving the work, the loop guard |
| [0055](0055-chat-as-its-own-object.md) | Chat is its own object | Accepted |
| [0056](0056-model-providers-pricing-and-budgets-per-org.md) | Model providers, models, pricing and budgets per org | Accepted; amended 2026-10-23, 2026-10-24, 2026-10-28, 2026-10-29, 2026-10-31 and 2026-11-06 (below); price order amended by [0098](0098-price-catalogue-updates-from-public-sources.md) |
| [0057](0057-mcp-servers.md) | MCP servers: remote and container, catalog, allowlists and discovery | Accepted; amended 2026-10-24, 2026-10-30, 2026-10-31 and 2026-11-01 by [0084](0084-leak-guard.md) (below) |
| [0058](0058-gateway-modes.md) | Gateway modes: standalone, kubernetes and shared | Accepted; amended 2026-10-25 and 2026-10-26 (below) |
| [0059](0059-v01-opencode-only.md) | v0.1 ships one harness: OpenCode v2 | Accepted |
| [0060](0060-migrations-as-a-job.md) | Migrations run as a Job, not on start; an Argo CD Sync hook under Argo | Accepted |
| [0061](0061-image-pull-secrets-and-registry-mirrors.md) | Image pull secrets and registry mirrors: Koinon rewrites its images; OpenShell pulls for sandboxes | Accepted |
| [0062](0062-code-host-connectors.md) | Code-host connectors: one interface per provider, capabilities as data | Accepted |
| [0063](0063-forge-credential-classes.md) | Code-host credential classes: allow weaker credentials, with compensation | Accepted |
| [0064](0064-work-tracker-connectors.md) | Work-tracker connectors: tools and triggers, no sync | Accepted |
| [0065](0065-work-model-tasks-inbox.md) | Work model: A2A-shaped tasks, the inbox, and asking people and agents | Accepted |
| [0066](0066-triggers-and-scheduling.md) | Triggers and scheduling: DBOS on Koinon's Postgres, triggers on the agent version | Accepted |
| [0067](0067-concierge.md) | The concierge: a setup and management assistant in koinon-api | Accepted |
| [0068](0068-prompt-caching-always-on.md) | Prompt caching is always on for models that support it | Accepted |
| [0069](0069-native-model-formats.md) | Models speak their family's native API format | Accepted. The OpenRouter live check (decision 7) passed in the rc.7 home-lab re-run on 2026-10-29 (`anthropic/claude-haiku-5.5` in the `anthropic` format, 5 requests, charged 31% of the uncached list price so cache reads were priced, no `run.uncached` events), and the OpenRouter pin rows are deleted (old issue 242): OpenRouter `anthropic/*` models default to `anthropic` like every other Claude model. Thinking: see the amendments of 2026-10-29 and 2026-10-31. |
| [0070](0070-model-metadata.md) | Model metadata: a reviewed snapshot from models.dev and OpenRouter | Accepted (procoduck, 2026-10-28; built after v0.1.0, with the UI milestone); decisions 4 and 8 amended by [0098](0098-price-catalogue-updates-from-public-sources.md) |
| [0071](0071-skill-library.md) | The skill library: pinned, scanned skills delivered by agentd | Accepted (procoduck, 2026-10-28; built with the UI milestone after v0.1.0) |
| [0072](0072-agent-model-run-now-echo-check.md) | The agent's model setting, "run now" for schedules, and the provider echo check | Accepted |
| [0073](0073-agent-passport.md) | Agent passport: a verifiable identity for every agent call | Accepted (delivery after v0.1.0) |
| [0074](0074-run-traces-trace-router.md) | Run traces: a Koinon trace router to each org's OTLP backend | Accepted; option C (decision 11 step 1) ships in v0.1.0-rc.9; amended 2026-10-30 and 2026-10-31 (below); platform destination and ceiling audit moved by [0100](0100-install-audit-chain.md) (note of 2026-11-07 below) |
| [0075](0075-isolation-tiers.md) | Isolation tiers: vm, hardened and dev | Accepted (pending qualification on Bottlerocket) |
| [0076](0076-database-modes.md) | Database modes: bring your own, existing CNPG, or standalone Postgres | Accepted (delivery after v0.1.0, in v0.1.x) |
| [0077](0077-pinned-openshell-and-installer.md) | Koinon pins OpenShell and Agent Sandbox per release, and ships an installer | Accepted (delivery after v0.1.0, in v0.1.x) |
| [0078](0078-sandbox-observer.md) | Sandbox observer: get-only reads of sandbox pods to explain why a sandbox never became ready | Accepted (delivery in rc.7) |
| [0079](0079-dependency-cooldown.md) | Dependency cooldown: three days everywhere, and a checked fast path for security fixes | Accepted |
| [0080](0080-tool-set-matches-egress-and-escape-hosts.md) | The tool set matches the sandbox's egress, and escape cases observe proxy decisions | Accepted |
| [0081](0081-run-limits.md) | Run limits: model requests, spend and time; tokens are reported, not enforced | Accepted |
| [0082](0082-work-claims-and-ready-queue.md) | Work claims and the ready queue: a claim branch is the lock, the board is the queue | Accepted; amended by [0094](0094-work-claims-on-the-public-repository.md) (2026-11-05) and [0104](0104-work-claims-reduced-to-the-lock.md) (2026-11-09: no lease, heartbeat, sweeper, takeover or board mirror; holder `<login>#<lane>`); decision 9 (approval on sensitive paths) amended by [0103](0103-recorded-review-verdict-check.md) once the owner's ruleset step is done |
| [0083](0083-sandbox-resources.md) | Sandbox resources: template defaults, platform bounds, requests and limits | Accepted |
| [0084](0084-leak-guard.md) | Leak guard: a taint check on every MCP tool call, in koinon-api | Accepted (decisions by procoduck, 2026-10-31; delivery before v0.1.0); amended by [0091](0091-leak-guard-by-destination.md) (2026-11-03: the main protection is where a call sends data; the text check is a frozen secondary alarm) |
| [0085](0085-upgrade-compatibility-and-recovery-policy.md) | Upgrade, compatibility and recovery policy | Accepted (scope decided by procoduck, 2026-10-31; decisions of 2026-11-01; a v0.1.0 gate; amended 2026-11-06: the schema promise starts at v0.3, see [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md)) |
| [0086](0086-stored-org-default-model.md) | The org default model is stored, not derived | Accepted |
| [0087](0087-turn-policy-is-a-pure-state-machine.md) | Turn policy is a pure state machine | Accepted |
| [0088](0088-cli-stack.md) | The command-line stack: cobra, pflag and viper, with Charm on a terminal only | Accepted |
| [0089](0089-run-outcome-writer.md) | One writer ends a run | Accepted |
| [0090](0090-scripts-require-bash-5.md) | Every repository script requires bash 5 | Accepted |
| [0091](0091-leak-guard-by-destination.md) | Leak guard by destination: sink classes, session legs and a byte cap | Accepted (decisions by procoduck, 2026-11-03); amended by [0092](0092-leak-guard-judge-model.md) (2026-11-03: the both-legs `external-high` approval becomes rules, a judge model, then a person) |
| [0092](0092-leak-guard-judge-model.md) | Leak guard judge: rules, then a cheap judge model, then a person | Accepted (decisions by procoduck, 2026-11-03) |
| [0093](0093-koinon-core-opencode-plugin.md) | koinon.core: one Koinon-owned OpenCode plugin instead of a fork | Proposed |
| [0094](0094-work-claims-on-the-public-repository.md) | Work claims on the public repository: one lock, claims by comment, a conflict check, the native merge queue | Accepted (procoduck, 2026-11-05, after the grilling session and the probes, which changed no decision) |
| [0096](0096-mcp-oauth-sign-in.md) | MCP sign-in: OAuth-authorized remote MCP servers | Accepted (2026-11-06) |
| [0097](0097-koinon-owned-gateway.md) | A Koinon-owned gateway, in three steps | Accepted (2026-11-06) |
| [0098](0098-price-catalogue-updates-from-public-sources.md) | Update the model price catalogue from public sources | Accepted (2026-11-06) |
| [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md) | No schema or upgrade promise before v0.3 | Accepted (maintainer decision 2026-11-07) |
| [0100](0100-install-audit-chain.md) | An install-level audit chain for platform actions | Accepted (maintainer decision 2026-11-07; anchors in v0.1, signed checkpoints after v0.1) |
| [0101](0101-agent-substrate-and-kagent-not-adopted.md) | Agent Substrate and kagent 1.x evaluated, not adopted for v0.x | Accepted (maintainer decision 2026-11-04) |
| [0102](0102-merge-queue-runs-the-kind-e2e.md) | The merge queue runs the kind e2e on the queue commit | Accepted (procoduck, 2026-11-09); decisions 2, 3 and 6 superseded in part by [0105](0105-merge-queue-route.md) |
| [0103](0103-recorded-review-verdict-check.md) | A recorded review verdict, checked by `review`, replaces the second-owner approval outside `.github/` | Accepted (direction by procoduck, 2026-11-09; details by the implementing lane) |
| [0104](0104-work-claims-reduced-to-the-lock.md) | Work claims reduced to the lock: no lease, no heartbeat, holder is login and lane | Accepted (procoduck, 2026-11-09: "reduce claims to the lock") |
| [0105](0105-merge-queue-route.md) | One route to main: the merge queue, and drafts run nothing | Proposed |
| [0106](0106-install-mark-owns-openshell-objects.md) | An install mark decides which OpenShell objects an install owns | Accepted |
