# 0049. v0.1 scope: template, approvals, gateway modes and agentd placement

- Status: Accepted
- Date: 2026-10-22
- Amends: [0007](0007-connector-model.md), [0016](0016-harness-and-deployment.md),
  [0036](0036-v01-demo.md); extends [0048](0048-openshell-spike-results.md) (Proposed, #15)
- Verification: Pending: agentgateway Kubernetes-mode `rawConfig` budgets (plan item S2 in
  [docs/plans/v0.1.md](../plans/v0.1.md))

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
  - The v0.1 CRDs (`agentgateway-crds` v1.6.0) cover API keys, AI backends with request overrides,
    MCP authorization and guardrails, access-log attributes and a model cost catalog, but have no
    USD budget. Plan item S2 tests whether the standalone budget configuration works through
    `AgentgatewayParameters.rawConfig`. If it does not, Kubernetes mode stops spend from Koinon's
    ledger by revoking the agent's key, bounded by a token rate limit, and the docs state the larger
    overshoot.
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
- **v0.1 is private.** Images and the OCI chart go to private `ghcr.io/procoduck` packages.
  Signing and SBOM attestations are wired in the release workflow but switched on when the
  repository goes public, because keyless cosign writes names and digests to the public Rekor log.
  The repository stays `procoduck/koinon`; the org name is chosen when going public.
- **v0.1 is done** when the quickstart passes on the home lab with Kata and a real model (recorded
  run) and the same steps pass as a kind e2e test in CI without Kata, against the fake model server.
- **Work order:** walking skeleton, then GitHub and approvals, then the remaining flavors, ledger
  and docs, then e2e, escape suite and release ([docs/plans/v0.1.md](../plans/v0.1.md)).
- Issue [#5](https://github.com/procoduck/koinon/issues/5) closes as "build": Multica's licence
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
- Negative: two gateway drivers to test; Kubernetes mode may have weaker budget stops.
- Negative: every run depends on a long-lived exec stream through the OpenShell gateway; a gateway
  restart costs a `session/load` per active run _(inferred)_.
- Negative: approvers see agent PRs twice (Koinon card, GitHub review), for different questions.
- Follow-ups: the issues in [docs/plans/v0.1.md](../plans/v0.1.md); ADR 0048 decision 6 (standalone
  mode) gains the Kubernetes mode above when #15 merges.

## Open questions

- Whether `rawConfig` carries budgets in Kubernetes mode (plan item S2).
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
- GitHub Actions: workflows on `push` and on `pull_request` from same-repository branches receive
  repository secrets; pull requests from forks do not.
  https://docs.github.com/en/actions/security-for-github-actions/security-guides/using-secrets-in-github-actions

## Links

- [docs/plans/v0.1.md](../plans/v0.1.md)
- ADRs [0004](0004-human-identity.md), [0007](0007-connector-model.md), [0016](0016-harness-and-deployment.md),
  [0020](0020-agent-builder-and-templates.md), [0036](0036-v01-demo.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0042](0042-ask-tier-park-and-execute.md), [0048](0048-openshell-spike-results.md)
- Spike results: `hack/spike/results/` (#15)
