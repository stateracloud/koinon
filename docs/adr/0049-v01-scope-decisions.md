# 0049. v0.1 scope: template, approvals, gateway modes and agentd placement

- Status: Accepted
- Date: 2026-10-22
- Amended by: [0056](0056-model-providers-pricing-and-budgets-per-org.md) (models, budgets,
  connection secrets; the Helm model values are dropped),
  [0058](0058-gateway-modes.md) (gateway modes), [0051](0051-session-home-generations.md), and on 2026-10-26 by
  [0062](0062-code-host-connectors.md), [0063](0063-forge-credential-classes.md),
  [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md) (v0.1 Foundation),
  and on 2026-10-28 by [0076](0076-database-modes.md) (database modes) and
  [0077](0077-pinned-openshell-and-installer.md) (pinned prerequisites and the installer)
- Amends: [0007](0007-connector-model.md), [0016](0016-harness-and-deployment.md),
  [0036](0036-v01-demo.md); extends [0048](0048-openshell-spike-results.md) (PR 15 in the previous repository)
- Verification: agentgateway Kubernetes-mode `rawConfig` budgets verified by spike S2
  (2026-10-22, below)

## Context

ADR [0036](0036-v01-demo.md) left four questions to the v0.1 plan (template behaviour, GitHub
scopes, UI parts, quickstart prerequisites), and `BOOTSTRAP.md` lists more (first data model, CI,
repository move). The OpenShell spike (ADR [0048](0048-openshell-spike-results.md)) produced
evidence that bears on two accepted decisions:

- Spike step 7 drove OpenCode, Hermes and pi over ACP from **outside** the sandbox through
  `openshell sandbox exec --no-tty`, with no relay inside. ADR [0016](0016-harness-and-deployment.md)
  puts `agentd` inside the sandbox.
- agentgateway's Kubernetes mode has no USD budgets (spike 5.6), so ADR 0048 chose standalone
  mode. Clusters that already run agentgateway in Kubernetes mode still need to be supported.

procoduck answered these in a grilling session on 2026-10-21/22. This ADR records the answers.

## Decision

### Demo and scope

- **Template:** a *repo helper*. A ticket describes a small change to one repository; the agent
  clones it read-only, makes the change in its sandbox and calls `create_pull_request`.
- **UI in v0.1:** board, ticket view (chat plus live run stream), agent-from-template form, approval
  card and a GitHub connection page that follows the setup guide. No org, member or admin screens.
- **Quickstart clock** starts at `helm install`. Agent Sandbox, OpenShell, Kata and the CNPG
  operator are prerequisites with pinned install steps in the docs. A chart preflight fails when a
  prerequisite is missing and warns when Kata guest seccomp is off (ADR 0048).
- **Human login:** generic OIDC (ADR [0004](0004-human-identity.md)) plus a bootstrap admin (a
  one-time password in a Secret), so the quickstart needs no IdP. One default org is created at
  install; there is no org-creation flow in v0.1.
- **Out of v0.1:** artifacts in object storage, agent-home checkpoints, memory stores, A2A,
  routines, webhooks, Slack/Teams mirroring, in-sandbox credentials (ADR 0044), nightly evals
  against real models and Postgres row-level security. The agent home is a plain PVC, which keeps
  harness sessions across sandbox stop and start (spike 7.3).

### Platform

- **Postgres:** `postgres.mode: cnpg` (default; the chart renders a CNPG `Cluster`) or `external`
  (DSN from a Secret). No bundled StatefulSet.
- **Secrets:** Koinon reads and writes only Kubernetes Secrets and has no hard dependency on a
  secret store. Organisations that keep secrets in OpenBao, Vault, AWS Secrets Manager or Azure
  Key Vault sync them into Kubernetes with the External Secrets Operator. Secrets entered in the UI
  (setup-guide secure inputs, ADR [0021](0021-connector-setup-guides.md)) are written by Koinon as
  Kubernetes Secrets. This amends ADR 0007, which kept connection secrets in Vault/OpenBao.
- **Models:** one OpenAI-compatible upstream (base URL, key Secret, model list). Docs cover
  OpenRouter with the routing guard (ADR 0048) and Ollama. CI uses the fake model server.
- **agentgateway, two modes behind one `ModelGateway` interface:**
  - *standalone*: the chart installs a standalone agentgateway that Koinon fully configures (SQLite
    budget store on a PVC, ADR 0048). Used when the cluster has no agentgateway.
  - *kubernetes*: on a cluster that already runs agentgateway's controller, Koinon creates its own
    `Gateway` (class `agentgateway`), `AgentgatewayBackend`, `AgentgatewayPolicy` and
    `AgentgatewayParameters` in its own namespace. The controller runs a dedicated proxy for it;
    Koinon never edits the user's other gateways or their ConfigMaps.
  - *Superseded by the amendment of 2026-10-25 (plan item G2): Kubernetes mode renders native CRDs,
    not `rawConfig`, and the ledger enforces budgets.* The v0.1 CRDs (`agentgateway-crds` v1.6.0) cover API keys, AI backends with request overrides,
    MCP authorization and guardrails, access-log attributes and a model cost catalog, but have no
    USD budget. Spike S2 showed that the standalone LLM configuration (keys with budgets, models,
    budget database, model catalog) works when carried in the Gateway-level
    `AgentgatewayParameters.rawConfig`, on its own bind next to the xDS listeners. So in Kubernetes
    mode Koinon renders the same LLM configuration as in standalone mode into `rawConfig`, and
    exposes that bind through the Parameters' Service overlay. Budget stops are the same in both
    modes.
  - In Kubernetes mode the budget database is SQLite on the proxy's `/tmp` emptyDir: it survives
    container restarts but not pod replacement, so budget windows restart with a new pod. Koinon's
    ledger stays authoritative (ADR 0041) and re-pushes limits net of spend already recorded. A
    persistent volume or an external database through the Parameters' deployment overlay is
    evaluated in G2.
- **Spend:** a minimal ledger (tokens and cost per run, cost per ticket) and one per-agent limit
  pushed to agentgateway. Hierarchical budgets wait (ADR [0041](0041-koinon-spend-ledger-and-budget-stops.md)).
- **`agentd` runs in the control plane**, as the `koinon-agentd` Deployment, and speaks ACP to the
  harness over OpenShell's non-TTY exec stream through the generated gRPC client. Nothing of
  Koinon's runs inside the sandbox; flavor images carry no Koinon binary and the sandbox needs no
  route to Koinon. When the stream drops, the run resumes with `session/load`. This amends ADR 0016.
- **Sandbox lifecycle:** one active run per agent; further tickets queue in FIFO order. A sandbox
  stops after 10 idle minutes, parked runs included, and is started again for the next run or a
  resume, which calls `session/load`. A parked run expires after 7 days.

### GitHub connector and approvals

- **GitHub App permissions:** metadata read, contents write, pull requests write, issues write
  (for comments), and no `workflows` permission.
- **Scopes and tiers:**

  | Scope | Tier | Path |
  | --- | --- | --- |
  | `repo.read` (clone, fetch, REST GET) | read | in the sandbox, through an OpenShell provider record holding an installation token narrowed to read |
  | `pull_request.create` | reversible-write | MCP tool; Koinon executes it |
  | `issue.comment` | reversible-write | MCP tool; Koinon executes it |

  Nothing else is offered in v0.1.
- **Presets:** *Read only* = read; *Standard* = read, ask for reversible-write; *Trusted* = read and
  reversible-write allowed. The repo-helper template uses Standard.
- **What Koinon's approval decides:** whether the agent's code may enter the repository and run in
  CI. A branch in the repository triggers `push` workflows, and a pull request from a same-repo
  branch runs `pull_request` workflows with the repository's secrets, before any GitHub review. So
  the agent never pushes: after approval, Koinon creates the branch, the commit and the pull request
  through the GitHub API with an idempotency key, bound to the hash of the approved arguments
  (spike 6.3). GitHub review stays the merge gate.
- The approval card leads with push-and-run risk (workflow and CI files, scripts, dependency
  manifests, binaries, size); the full diff is collapsed. Changes touching `.github/workflows/` are
  refused before approval, with the reason returned to the agent as a tool error.
- **Approvers:** org admins and owners, the default approver policy on every grant.

### Data, delivery and process

- **Org isolation:** every query takes `org_id`, tables keep the scaffold's `(org_id, id)` keys,
  and an org-isolation test suite runs in CI from v0.1.
- **v0.1 is private.** Images and the OCI chart go to private `ghcr.io/stateracloud` packages (amended 2026-10-26, see [0034](0034-project-identity.md)).
  Signing and SBOM attestations are wired in the release workflow but switched on when the
  repository goes public, because keyless cosign writes names and digests to the public Rekor log.
  The repository stays `stateracloud/koinon` (amended 2026-10-26: it was `procoduck/koinon`); the org name is chosen when going public.
- **v0.1 is done** when the quickstart passes on the home lab with Kata and a real model (recorded
  run) and the same steps pass as a kind e2e test in CI without Kata, against the fake model server.
- **Work order:** walking skeleton, then GitHub and approvals, then the remaining flavors, ledger
  and docs, then e2e, escape suite and release ([docs/plans/v0.1.md](../plans/v0.1.md)).
- Old issue 5 closes as "build": Multica's licence
  restricts hosting, and LiteLLM was dropped in ADR [0040](0040-agentgateway-model-and-mcp-gateway.md).

### Small calls (mine; overrule freely)

- Board columns: To do, In progress, Waiting on you, Done.
- Run caps: 30 minutes wall-clock and a token cap per run, set in the template. The DeepSeek re-run
  of spike step 7 showed an agent without its tools probing for 1 m 43 s before giving up.
- Ledger ingestion: agentgateway's OTLP access-log export to `koinon-api`.
- First flavor for the walking skeleton: OpenCode (advertises MCP capabilities and session resume).
- Flavor images are built with `docker buildx` in CI; Go images with ko.
- The kind e2e uses `runc` in place of Kata.
- UI ingress is an optional chart value, off by default.

## Alternatives considered

- **Agent pushes to `koinon/*` branches, only PR creation is approved.** Rejected: the code is in
  the repository, and CI has run with its secrets, before anyone approves.
- **Koinon's approval as the only review (Koinon merges).** Rejected for v0.1: needs the app to
  bypass required reviews.
- **Pull requests at allow tier, approval on merge.** Rejected: leaves the CI-secrets exposure open.
- **Kubernetes mode only, or standalone only.** Rejected: the first loses USD budgets; the second
  needs a second gateway on clusters that already run agentgateway.
- **`agentd` inside the sandbox (ADR 0016 as written).** Rejected: adds a Koinon binary to every
  flavor image and a control channel into the sandbox; spike 7 showed it is not needed.
- **A Koinon `SecretStore` interface with a Vault driver.** Rejected: ESO already bridges every
  common store.
- **Row-level security from v0.1.** Deferred to v0.4 with full multi-tenancy.

## Consequences

- Positive: the demo exercises credential isolation, park-and-execute and resume end to end.
- Positive: Koinon works next to an existing agentgateway without touching its configuration.
- Negative: two gateway drivers to test; in Kubernetes mode the ledger, not the gateway, stops a key
  that is out of budget (see the amendment of 2026-10-25).
- Negative: every run depends on a long-lived exec stream through the OpenShell gateway; a gateway
  restart costs a `session/load` per active run _(inferred)_.
- Negative: approvers see agent PRs twice (Koinon card, GitHub review), for different questions.
- Follow-ups: the issues in [docs/plans/v0.1.md](../plans/v0.1.md); ADR 0048 decision 6 (standalone
  mode) gains the Kubernetes mode above when PR 15 (in the previous repository) merges.

## Amendment (2026-10-23)

- **Models:** the single Helm-configured upstream is no longer the only source. Orgs manage their own
  providers and models ([0056](0056-model-providers-pricing-and-budgets-per-org.md)).
- **Budgets:** the template's `spendUSD` is only a default; admins set the org monthly cap and the
  per-agent daily limits ([0056](0056-model-providers-pricing-and-budgets-per-org.md)).
- **Gateway modes:** a third, *shared* mode joins standalone and kubernetes in v0.2
  ([0058](0058-gateway-modes.md)).

## Amendment (2026-10-23, plan item PV1)

- **Helm model values dropped.** The chart's `model.*` values (`baseUrl`, `provider`,
  `apiKeySecret`, `models`) and everything that read them (the schema, the config file, the
  gateway's bootstrap config, the key mount, `KOINON_DEFAULT_MODEL`) are gone. Nobody had installed
  Koinon, so there is no migration path from them. Providers and models are rows in Postgres per org,
  entered in the UI or admin API or declared by labelled Secrets
  ([0056](0056-model-providers-pricing-and-budgets-per-org.md)). The chart gains `secrets.kek.*` for
  the key-encryption keys.
- **Connection secrets** are no longer one Kubernetes Secret per connection: they are envelopes in
  `connections.secret_envelope` ([0056](0056-model-providers-pricing-and-budgets-per-org.md)). The
  rule that Koinon writes only Kubernetes Secrets (Secrets bullet above) still holds for the KEKs, the
  gateway's provider keys and agent gateway keys, but no longer for connections.

## Amendment (2026-10-25, plan item G2): Kubernetes mode on native CRDs, budgets by the ledger

procoduck decided this after the research in [agentgateway-k8s-crds](../research/agentgateway-k8s-crds.md)
and a run against controller v1.6.0 on kind. It replaces the `rawConfig` design of the decision
above (spike S2 stays a valid result: `rawConfig` budgets work, but they pull keys, models and the
access log into one file whose every change rolls the proxy pods).

- **Mode and preflight.** `gateway.mode: kubernetes` is explicit, with `gateway.kubernetes.gatewayClassName`
  (default `agentgateway`). The install preflight fails with a clear message when the GatewayClass, the
  four `agentgateway.dev/v1alpha1` CRDs or a v1.6.x controller are missing. There is no silent
  fallback to standalone mode.
- **Native CRDs only, no `rawConfig`,** all in Koinon's namespace, all carrying the labels
  `app.kubernetes.io/managed-by=koinon-reconciler` and `koinon.dev/gateway=<name>`; the driver applies
  and prunes only objects with both, so other gateways are never touched. The mapping is in
  [0058](0058-gateway-modes.md).
- **Budgets use the S2 fallback.** There are no gateway budgets. Koinon's ledger enforces them: when an
  agent, its org or the platform ceiling is spent, the existing path cancels the runs and the reconciler
  removes the agent's keys from the key ConfigMap; the proxy stops accepting them in about 0.1 s,
  without a restart. The key comes back when the window resets or the limit is raised. An org cap or
  the platform ceiling revokes every key of the org the same way. The ledger already pushes limits net
  of recorded spend (`ledger.AgentLimit`); in this mode a net amount of zero is a revoked key and any
  other amount is a key without a gateway budget.
- **Overshoot is bounded by the ingest delay,** not by one response: the gateway no longer stops the key
  by itself. Measured on kind (Apply and proxy side only, no Postgres): the proxy batches its access log
  for about 0.75 to 0.9 s before exporting it, the revoking Apply returns in 50 to 150 ms, and the proxy
  refuses the key about 0.1 s after the ConfigMap changes, so a key is spent for about one second after
  the request that reaches the limit. At 10 requests a second of $0.001 each that was 4 requests ($0.004)
  past a $0.010 limit. Add the ledger's transaction, the reconciler's wake-up (200 ms debounce) and a
  full pass; the pass recomputes every agent, so it takes longer with many agents. The unit is time, not
  one response: a fast, expensive key overshoots by rate times delay. Standalone mode keeps the
  one-response bound.
- **State survives proxy pod replacement** because it never lived in the pod: the key list is in a
  ConfigMap and the spend is in the ledger. A test deletes the proxy pod while a key is revoked and
  checks that it is never served, during or after the replacement.
- **Org-prefixed model names are not needed.** Per-model authorization on the key's org routes one
  plain model name per org natively (verified on kind).
- **MCP isolation is weaker than in standalone mode.** The CRDs have no per-target condition, so an
  org's server is a target for every key. Tool rules deny every call to a target the agent holds no
  grant for (and the driver drops grants that name another org's server), and `tools/list` shows only
  granted tools, but another org's `initialize`, `ping` and list requests may reach a server with its
  owner's credential. Standalone mode does not contact the server at all for other orgs. Accepted by
  procoduck; documented in [0058](0058-gateway-modes.md) and the gateway-modes guide.

## Amendment (2026-10-26): v0.1 Foundation replaces this scope

procoduck replaced v0.1's scope with "v0.1 Foundation" (old issue 146;
[0024](0024-roadmap.md) amendment; [docs/plans/v0.1.md](../plans/v0.1.md)). Changes to the decisions above:

- **Code hosts.** GitHub, GitHub Enterprise Server and Gitea behind a code-host interface
  ([0062](0062-code-host-connectors.md)). Gitea uses a bot per connection with a read token and a
  write token ([0063](0063-forge-credential-classes.md)). The agent still never pushes: GitHub
  commits through its Git Data API, Gitea through a git push from koinon-api. Changes are refused
  under the provider's CI paths, not only `.github/workflows/`.
- **Tickets and board columns** give way to A2A-shaped tasks and the inbox; the board is a
  secondary view ([0065](0065-work-model-tasks-inbox.md)).
- **"Out of v0.1"**: agent-home checkpoints and schedules are now in v0.1. A2A (`ask_agent`),
  webhooks and Slack/Teams stay out, in v0.2 ([0065](0065-work-model-tasks-inbox.md),
  [0066](0066-triggers-and-scheduling.md)).
- **Sandbox lifecycle.** A sandbox whose run waits on `a2a` or `input_required` stays warm for
  15 minutes; otherwise it stops after 10 idle minutes. A run waits in the queue instead of
  failing after 5 minutes. All of this is tunable per org ([0066](0066-triggers-and-scheduling.md)).
- **Release test.** v0.1 is tested on the home lab with Gitea through three stories of old issue 146:
  the daily FinOps cost check (without the hand-off to the lead-engineer agent), a long-running research task, and the weekly LCM upgrade agent.

## Open questions

- Whether OpenShell's Kubernetes driver runs on kind in GitHub's free runners (plan item S1).

## Upstream verification

Checked 2026-10-22:

- `agentgateway-crds` v1.6.0 (`oci://cr.agentgateway.dev/charts/agentgateway-crds`, digest
  `sha256:dd7f965d43e54dffa9c95af089b6f762397a23c963a159ad6d4992f6c582292a`): four CRDs
  (`AgentgatewayBackend`, `AgentgatewayModel`, `AgentgatewayParameters`, `AgentgatewayPolicy`). No
  budget, USD or price field; the only `cost` is a descriptor weight for global rate limits.
  `AgentgatewayParameters.rawConfig` merges raw configuration into the generated config file;
  `modelCatalog` is honoured only on Gateway-level parameters.
- The standalone install on the home lab runs with no `agentgateway.dev` CRDs present.
- **Spike S2** (2026-10-22, home lab, approved by procoduck for one day; manifests in
  `hack/spike/s2/manifests.yaml`): controller `agentgateway` v1.6.0 (chart digest
  `sha256:d3edf2b33cbf2e9d59406c207b8d3554597799707b8803b5f62e698fe5376e28`) in `koinon-spike`; a
  Koinon-owned Gateway with Gateway-level Parameters whose `rawConfig` holds the standalone `config`
  (SQLite database, model catalog), `gateways.default` on port 4000 and `llm` (two keys by hash
  with USD budgets, one Ollama model). The proxy logged `started bind bind="8090/koinon-spike/koinon-s2"`
  and `started bind bind="bind/4000"`. Results on the 4000 bind: no key `401`; agent B (limit
  $0.0005) `200` nine times, then `429 {"code":"budget_exceeded"}`, with the log
  `used=0.000504000 limit_amount=0.0005` (overshoot $0.000004, under one response of about
  $0.000056); agent A still `200`. The controller's Service carries only Gateway listener ports;
  adding port 4000 through `spec.service.spec.ports` in the Parameters exposed the bind, and the
  same results held through the Service. Released and removed the same day.
- GitHub Actions: workflows on `push` and on `pull_request` from same-repository branches receive
  repository secrets; pull requests from forks do not.
  https://docs.github.com/en/actions/security-for-github-actions/security-guides/using-secrets-in-github-actions

## Links

- [docs/plans/v0.1.md](../plans/v0.1.md)
- ADRs [0004](0004-human-identity.md), [0007](0007-connector-model.md), [0016](0016-harness-and-deployment.md),
  [0020](0020-agent-builder-and-templates.md), [0036](0036-v01-demo.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0042](0042-ask-tier-park-and-execute.md), [0048](0048-openshell-spike-results.md)
- Spike results: `hack/spike/results/` (PR 15 in the previous repository)

## Amendment (2026-10-28): agentgateway v1.7.0

The pinned release is v1.7.0 (image `sha256:60216aaf...`, `agentgateway-crds` digest
`sha256:946b739c0fcda4d47a72d1589d184b6fd507772e733cbedf02789bafd0468064`, `agentgateway`
chart digest `sha256:40340d639e35206317c934e9af3e29575cf7c5377b2940679732baac6e108c15`), checked
against the registry on 2026-10-28; the two-week wait was waived. Its breaking changes:

- **MCP session encryption.** Standalone always encrypts MCP session IDs. The gateway Deployment gets
  `SESSION_KEY` (the upstream-recommended form, not `config.session.key`, which would put the key in
  the config ConfigMap) from the Secret `<release>-gateway-session-key`, key `key`, 64 hex characters.
  The ensure-secrets hook Job generates it (`kind: aes256-key`) and never changes it;
  `gateway.standalone.sessionKey.existingSecret` supplies one instead. Without it v1.7.0 would write a
  key into the config file when it is writable, or leave sessions unencrypted when it is not.
- **Prompt guards** now also inspect tool output. Koinon configures no prompt guard, so nothing
  changes; a future guard must say which of request, response and tool output it covers.
- **Dynamic backends** cannot reach loopback or link-local addresses. Koinon's backends are static
  (the `127.0.0.1:9` placeholder MCP target is static and never contacted), and the suites reach
  their fakes through the host address, so nothing changes.
- **`anthropic-version`** is forwarded, not replaced: see ADR 0069's amendment.

Gaps it closes: ADR 0058 (MCP isolation in Kubernetes mode) and ADR 0041 (budgets without a window).

## Amendment (2026-10-28): database modes

[0076](0076-database-modes.md) replaces "Postgres: `cnpg` … or `external` … No bundled StatefulSet".
`postgres.mode` takes `external` (any Postgres 18 or newer), `cnpg` (an existing operator; Koinon
never installs it) or `standalone`: Koinon's own single-replica StatefulSet on the official
`postgres:18` image, pinned by digest, with a `pg_dump` CronJob that also backs up the KEK. There
is no default: the install fails until `postgres.mode` is set.

## Amendment (2026-10-28): pinned prerequisites and the installer

[0077](0077-pinned-openshell-and-installer.md) keeps the quickstart clock at `helm install`, but
Agent Sandbox and OpenShell are no longer manual prerequisites: each Koinon release pins their
versions in `release/versions.yaml`. They install as separate Helm releases, either with plain
`helm install` in a documented order or with `koinon install`, which runs the same steps. Kata, the CNPG operator and the NetworkPolicy engine stay prerequisites.

## Amendment (2026-11-01): a sandbox starts on demand, with an optional keep-warm

Old issue 315 (decision by procoduck, 2026-10-31). Activating an agent no longer creates its sandbox: on
the rc.9 home-lab run, activating 22 agents started 22 sandboxes and they stopped only after the
idle timeout. The reconciler now creates (and starts) a sandbox only when something wants it: a run
that is active, a claimable run (`has_demand`), a pre-wake signal (package wake), or a keep-warm
request. Until then the agent's sandbox row records state `deleting`, phase `Idle` ("no sandbox yet,
it starts with the first run"), as an archived agent's row records `Archived`. Bulk-creating agents
from templates costs nothing until work arrives.

- **Keep-warm** is `agents.keep_warm`, a per-agent setting like `thinking`, set by an org admin
  (migration 00044, query `SetAgentKeepWarm`). A keep-warm agent's sandbox is created when the agent
  is active, is never idle-stopped and is started when found stopped.
- **The operator caps keep-warm per org**: `reconciler.keepWarmPerOrg` in the chart (reconciler
  `--keep-warm-per-org`), default 2, negative for none. The flag is a request: each pass grants it to
  the org's oldest active agents (`created_at`, `id`) up to the cap, so the same agents win from pass
  to pass; the others start on demand. Keep-warm sandboxes count toward resources (old issue 314).
- **Cost.** The first run of an agent now waits for its sandbox to start (image pull, home volume);
  that wait is the one `agentd` already has for a stopped or archived sandbox. Preparing the image
  and the home volume at activation without starting the pod is not built: tracked in #101. A per-org rate limit on
  sandbox starts is tracked in #27.
- **API and UI (#100).** `AgentService.SetAgentKeepWarm` sets or withdraws the request; admins and
  owners only, audited (`agent.keep_warm.set`), refused on an archived agent. `Agent.keep_warm` is the
  request and `Agent.keep_warm_granted` the outcome. Only the reconciler knows the cap, so each pass
  records its decision in `agents.keep_warm_granted` (migration 00076) and the API and the agent page
  read it; a new request reads "not granted" until the next pass. The agent page has a "Keep warm"
  control that says whether the cap granted it. The admin REST API (`/admin/v1`) has no keep-warm
  endpoint yet.
