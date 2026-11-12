# 0048. OpenShell spike results

- Status: Accepted (2026-10-26, accepted after implementation)
- Date: 2026-10-21
- Area: runtime-sandbox
- Tags: openshell, isolation, credentials, agentgateway, approvals
- Amended by: [0052](0052-opencode-v2-flavor.md), [0049](0049-v01-scope-decisions.md) (extends it), [0075](0075-isolation-tiers.md) (decision 2), [0078](0078-sandbox-observer.md) (decision 3) and 2026-10-26 (below)
- Amends: [0040](0040-agentgateway-model-and-mcp-gateway.md), [0042](0042-ask-tier-park-and-execute.md),
  [0045](0045-openshell-hardening-defaults.md); resolves the spike verification of
  [0006](0006-identity-layers.md), [0008](0008-approvals-and-risk-tiers.md),
  [0016](0016-harness-and-deployment.md), 0040, 0042 and 0045
- Verification: done by the spike itself (old issue 3)

## Context

The spike (`docs/spikes/openshell.md`) asked whether the execution layer Koinon reuses keeps its
core promise: an agent in a sandbox can do useful work, but cannot see real credentials, cannot
leave its policy, and must get human approval for risky actions. It ran on 2026-10-21 on a
maintainer's Talos+Kata cluster with OpenShell 0.1.5,
Agent Sandbox v1.0.7, agentgateway 1.6.0, OpenBao, Gitea and Ollama
(`qwen3.5:4b`). Evidence is in `hack/spike/results/` (one file per step, full command logs for
re-runs in `raw/`).

### Results

| Step | Result | Headline |
| --- | --- | --- |
| 1 Install and lifecycle | pass | v1beta1 Sandboxes in managed workspace namespaces on Kata; stop keeps the PVC. Cold start 25 s (cached images), stop 6 s, resume 22 s |
| 2 Credentials | pass (2.7 conditional) | the agent only ever holds placeholders; the real token is in none of its files, environment, memory, logs or Secrets; rotation without restart needs `stable_placeholder: true` |
| 3 Policy | pass (3.8 partial) | deny by default at DNS, TCP and L7; binary-scoped rules; hot reload in 9 s; audit mode lets violations through; HTTP decision records lack the binary |
| 4 Gateway API from Go | pass (4.2 partial) | SDK covers lifecycle, templates, providers, policy, watch, logs, workspaces; gaps below |
| 5 Inference | pass | placeholder key → agentgateway → Ollama; per-key USD cost; budget stop with less than one response of overshoot; standalone mode chosen after running both modes |
| 6 Approval routing | pass | park, record exact call, execute once after approval, reject changed arguments; CEL and ExtMCP both work |
| 7 Harnesses over ACP | pass | OpenCode, Hermes and pi: initialize, prompt, tool use, load after stop/start, PR through the approval tool |
| 8 Escape checks | pass (8.12 after platform fix) | 48 network, identity, host and admission attempts blocked; keyring syscalls were open until Kata guest seccomp was enabled; 8.16 added |

The architecture differs from what the design assumed: OpenShell runs the **supervisor** (proxy,
policy, credential injection) in its own pod outside the Kata VM, and the **workload** pod has no
network egress at all; the supervisor relays its traffic through a connection into the VM.

## Decision

### Execution layer

1. OpenShell 0.1.5 on Agent Sandbox stays Koinon's execution backend (ADR 0016). The OpenSandbox
   fallback is not needed. _(Changed by [0077](0077-pinned-openshell-and-installer.md), amendment of 2026-11-09: the pin is OpenShell 0.1.7.)_
2. **Install requirements**, documented and checked by Koinon's preflight:
   - Kata with **guest seccomp enabled** (`disable_guest_seccomp = false`). Without it the pod's
     `RuntimeDefault` profile is not applied inside the VM and OpenShell's default-allow filter
     leaves `keyctl`/`add_key` open (8.12). Preflight starts a test Kata pod and checks
     `Seccomp: 2` and `keyctl` → `EPERM`; on failure it **warns** (a lasting platform-admin warning
     and an audit entry) but does not block installation (procoduck, 2026-10-21).
   - The label `openshell.ai/sandbox-attachable: "true"` on every RuntimeClass Koinon uses.
   - A CNI that enforces egress NetworkPolicy (the workload fence depends on it).
   - The OpenShell `vault` credential driver on its **own KV mount**, outside any broadly usable
     External Secrets store, reached over **HTTPS**; its policy needs create/read/update on
     `<mount>/data/openshell/provider-credentials/*` and delete on the metadata path.
3. **Hardening defaults added to ADR 0045** (reconciler, Helm chart and VAP):
   - every credential Koinon rotates sets `stable_placeholder: true` (2.7);
   - `landlock.compatibility: hard_requirement` (3, 8.12);
   - OpenShell telemetry off;
   - the sandbox-namespace VAP rejects `io.katacontainers.*` annotations (8.16), and keys its
     rules on the **requesting identity**: only the Agent Sandbox controller may create pods in
     sandbox namespaces (and OpenShell's gateway only its Secrets); every other pod is denied,
     including from cluster admins, with no break-glass; debugging goes through OpenShell exec and
     logs (procoduck, 2026-10-21; 8.14 finding);
   - unchanged from 0045: `enforcement: enforce`, no secrets in sandbox env, managed workspaces,
     `kata` default, ephemeral containers denied (confirmed by 3.7, 2.8, 1.2, 8.13).
4. **Flavor image contract** (ADR 0016, 0035): OpenShell does not pass the image's `ENV` (not even
   `PATH`) to the processes it starts, so each flavor ships an entry script that sets its
   environment and execs the ACP agent by absolute path. Configuration is read-only under
   `/etc/koinon/`; state goes under the sandbox home (`/sandbox`). MCP servers are configured in
   the harness's own config (Hermes and pi-acp do not take client MCP servers); pi's approval
   tools use `exposure: direct`.
5. **`agentd` talks ACP over the gateway's non-TTY exec stdin** (`exec --no-tty` works for all
   three harnesses; the Go SDK lacks that path today, see gaps).

### Model and MCP gateway (amends ADR 0040)

6. agentgateway runs in **standalone mode**: per-key USD budgets in a database, `keyHash` entries
   so Koinon never writes key values into gateway config, and hot reload of pushed limits (11 s
   through a ConfigMap). Kubernetes mode has only token or request buckets in proxy memory.
   Budget store (procoduck, 2026-10-21): **SQLite on a PVC by default** (one replica), with two
   opt-ins: an external Postgres URL from a Secret (RDS or any Postgres), or a CNPG-managed
   database when the operator is present. The chart refuses more than one replica with SQLite.
   Koinon cannot assume CNPG or a cloud database. The upstream `agentgateway-standalone` chart
   renders its `database` mode URL into a ConfigMap, so Koinon passes the Postgres URL from a
   Secret instead.
7. Koinon's ledger (ADR 0041) reads per-request key, agent, tokens and USD from agentgateway's JSON
   access log; budget windows are epoch-aligned, so Koinon keeps its own never-reset totals.

### Ask tier (amends ADR 0042)

8. Tool **visibility** is gated twice: OpenShell `protocol: mcp` endpoints allowlist tool names
   inside the sandbox boundary, and agentgateway CEL `mcpAuthorization` hides and rejects tools.
   **Argument** rules (target branch, repository) run in an agentgateway ExtMCP processor; denials
   reach the agent as ordinary tool errors. ExtMCP calls have a fixed 10 s timeout, which the
   park-and-execute design never approaches.
9. Ask-tier tools carry everything the approved action writes (for a pull request: branch, files,
   title), so the agent needs no direct write path to the forge.

### Reconciler

10. The reconciler uses the Go SDK, plus the generated gRPC client for provider profiles (the SDK
    fix is proposed upstream after a facts check); it
    watches `agents.x-k8s.io` Sandbox objects in workspace namespaces for status, and
    garbage-collects OpenShell-labelled Sandbox objects the gateway no longer lists (the
    `--no-keep` orphan bug); it decides whether to restart sandboxes that OpenShell stopped after
    an infrastructure failure.

## Alternatives considered

- **OpenSandbox credential vault** (the plan's fallback): not needed; every credential, isolation
  and policy criterion passed in the intended configuration.
- **Accept 8.12 as a hardening item without a platform fix**: rejected by procoduck in favour of
  enabling guest seccomp and re-running.
- **agentgateway Kubernetes mode**: run and rejected for budgets (no USD unit, in-memory buckets).
- **CEL only, or ExtMCP only, for the ask tier**: CEL cannot see arguments; ExtMCP alone would
  leave tool visibility to a remote call. Both are used for what each does well.
- **A relay process inside the sandbox for ACP**: unnecessary, `exec --no-tty` carries stdio.

## Consequences

- Positive: the execution layer is verified end to end on the target platform, including real
  harnesses; Koinon's own code shrinks to rendering policy, pushing credentials and keys, and the
  approval service.
- Positive: the workload fence (no egress) is stronger than the design asked for.
- Negative: Koinon depends on platform settings (Kata guest seccomp, RuntimeClass labels, CNI
  egress enforcement, a dedicated OpenBao mount) that it must check rather than assume.
- Negative: the supervisor pod has unrestricted egress and holds the per-sandbox interception CA
  key; it is OpenShell's trusted component and outside Koinon's VAP shape rules.
- Follow-ups: issues below; update ADRs 0006, 0008, 0016, 0040, 0042, 0045 statuses ("Pending
  verification" → verified by 0048) and the ADR index once PR old issue 12 merges; run
  `hack/spike/teardown.sh` when procoduck says the spike is over.

## Gaps for Koinon to close or file upstream

From 4.7 and later steps:

1. The Go SDK's `ProviderProfile` endpoint type drops `access`, `rules` and `enforcement`
   (upstream fix or generated client).
2. Stored sandbox templates cannot set a runtime class (per-sandbox only).
3. No workspace-wide sandbox watch in the gateway API.
4. No non-TTY stdin exec in the Go SDK (`Exec().Stream` has no stdin; `Interactive` allocates a TTY).
5. HTTP decision events (OCSF 4002) lack the binary; Koinon joins them to connection events.
6. `sandbox create --no-keep` with a fast-exiting command orphans the Sandbox object and PVC.
7. OpenShell's seccomp filter does not block the kernel keyring syscalls.
8. Workspace-scoped profiles need `ProviderSpec.ProfileWorkspace` on provider create.
9. MCP clients must speak a revision OpenShell permits (`2025-11-25` by default).

Upstream (procoduck, 2026-10-21, after checking OpenShell's contribution rules and existing issues):

- 7 is already known upstream: [#578](https://github.com/NVIDIA/OpenShell/issues/578) asked to block
  `keyctl` and was closed as not planned ("defense-in-depth seccomp hardening"); in
  [PR #891](https://github.com/NVIDIA/OpenShell/pull/891) a maintainer proposed blocking the keyring
  calls as a follow-up on an internal tracker. Current `main` still does not block them. Not filed;
  Koinon relies on Kata guest seccomp and its preflight warning.
- 6 was not reproduced in five runs with gateway debug logging (`raw/1-no-keep-orphan.md`); the
  suspected cause from reading the code (a 409 on delete treated like 404) is unconfirmed. Not
  filed. OpenShell's rules also require a first-hand user story from the human and an
  OpenShell-only reproduction for any bug report.

## Open questions

- Narrowing the supervisor pod's egress with Koinon's own NetworkPolicy: not in v0.1; tracked as
  a hardening issue (procoduck, 2026-10-21).
- Which model is the v0.1 default? The 4B model completed every task but needed explicit
  instructions and produced empty or truncated replies. Decided (procoduck, 2026-10-21): hosted
  testing goes through **OpenRouter**, set to zero data retention for every model group, no
  training, prompt logging off, and an allowlist of audited hosts (`fireworks`, `together`,
  `deepinfra`, `amazon-bedrock`, `google-vertex`), behind one key with a hard credit limit.
  Compared and not chosen: Fireworks direct (fewest parties, fewer models), Anthropic direct
  (30-day retention), Hugging Face Inference Providers (no ZDR filter, provider allowlist or
  spending cap on personal accounts). The budget amount is procoduck's; CI never calls it.

## Upstream verification

Every result above was observed on the cluster on 2026-10-21; versions: OpenShell 0.1.5 (chart
digest `sha256:e88977ce…`, SDK commit `bdb956f`), Agent Sandbox v1.0.7 (manifest SHA-256
`136c10c9…`), agentgateway 1.6.0, Kata from the `kata-containers` extension with guest
seccomp enabled, OpenCode 1.19.3, Hermes Agent 0.22.3, pi 1.3.0 with pi-acp 0.0.34,
`coder/acp-go-sdk` v0.13.5, `modelcontextprotocol/go-sdk` v1.8.0.

## Amendment (2026-10-26): decision 10, the Sandbox watch and orphan collection are deferred

Decision 10 says the reconciler watches `agents.x-k8s.io` Sandbox objects and garbage-collects
OpenShell-labelled Sandbox objects the gateway no longer lists. Neither is built: nothing in `internal/`
or `cmd/` reads or deletes Sandbox objects (the only reference to the group is the preflight's CRD
check, `internal/preflight/preflight.go:37-44`). The reconciler learns sandbox status from the
OpenShell gateway API. The chart still grants the reconciler read, watch and delete on Sandboxes
cluster-wide (`charts/koinon/templates/rbac.yaml:131-133`). That grant is unused and is being
removed (old issue 110).

Orphan garbage collection is deferred. When it is built, it adds back only the Sandbox access it
needs, scoped to the namespaces it works in, with its own tests.

## Amendment (2026-10-29, rc.7): the reconciler reads sandbox pods, get only

The 2026-10-26 amendment left the reconciler without Kubernetes access to Agent Sandbox objects. rc.7
adds one narrow, used grant: [0078](0078-sandbox-observer.md). A ClusterRole with `get` on pods and
`agents.x-k8s.io` sandboxes is bound to the reconciler per workspace namespace, by RoleBindings it creates
(`create` on rolebindings and `bind` on that one ClusterRole), and a native ValidatingAdmissionPolicy limits
those bindings to OpenShell workspace namespaces with a fixed name, role and subject (decision 3's identity
keyed rules, extended). Nothing in sandbox namespaces is written by a new identity.

## Amendment (2026-11-10): the admission policies cover their own gateway's namespaces, old issue 460

The `openshell.ai/managed-by` namespace selector matched every OpenShell namespace on the cluster, so a
second Koinon release with another gateway ID was policed by the first release's identity lists. The
chart's bindings now also select `openshell.ai/gateway-id=<openshell.gatewayId>` by default;
`sandboxPolicy.scopeToGateway=false` restores the old scope (every OpenShell namespace). The preflight
fails when `openshell.gatewayId` differs from the gateway's `gateway_id` (the policies would bind to no
namespace of that gateway and the boundary would fail open), and when the key is unset and
`openshell.gatewayId` is not OpenShell's default `openshell`. Namespaces of a gateway with no Koinon
release are no longer policed by this release. A namespace with `openshell.ai/managed-by` but no
`openshell.ai/gateway-id` label stays covered: each policy has a second binding (`<name>-unlabelled`) for
namespaces without that label, so no OpenShell namespace is left without a policy.

## Links

- [Spike plan](../spikes/openshell.md) and [results](../../hack/spike/results/)
- [0045](0045-openshell-hardening-defaults.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0042](0042-ask-tier-park-and-execute.md),
  [0016](0016-harness-and-deployment.md)

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
