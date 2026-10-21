# 0048. OpenShell spike results

- Status: Proposed
- Date: 2026-10-21
- Amends: [0040](0040-agentgateway-model-and-mcp-gateway.md), [0042](0042-ask-tier-park-and-execute.md),
  [0045](0045-openshell-hardening-defaults.md); resolves the spike verification of
  [0006](0006-identity-layers.md), [0008](0008-approvals-and-risk-tiers.md),
  [0016](0016-harness-and-deployment.md), 0040, 0042 and 0045
- Verification: done by the spike itself ([#3](https://github.com/procoduck/koinon/issues/3))

## Context

The spike (`docs/spikes/openshell.md`) asked whether the execution layer Koinon reuses keeps its
core promise: an agent in a sandbox can do useful work, but cannot see real credentials, cannot
leave its policy, and must get human approval for risky actions. It ran on 2026-10-21 on the home
lab (Talos 1.14.4, Kubernetes 1.37.1, Cilium, Kata on two Proxmox workers) with OpenShell 0.1.5,
Agent Sandbox v1.0.7, agentgateway 1.6.0, the home-lab OpenBao 2.7.1, Gitea 28.2 and Ollama
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
   fallback is not needed.
2. **Install requirements**, documented and checked by Koinon's preflight:
   - Kata with **guest seccomp enabled** (`disable_guest_seccomp = false`). Without it the pod's
     `RuntimeDefault` profile is not applied inside the VM and OpenShell's default-allow filter
     leaves `keyctl`/`add_key` open (8.12).
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
     runtime-class and pod-shape rules on the **requesting identity** (the Agent Sandbox
     controller's service account), not on pod labels the creator chooses; any other pod in a
     sandbox namespace is denied (8.14 finding);
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
   through a ConfigMap). Kubernetes mode has only token or request buckets in proxy memory. For HA
   the budget database is Postgres (the upstream `agentgateway-standalone` chart's `database` mode).
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

10. The reconciler uses the Go SDK, plus the generated gRPC client for provider profiles; it
    watches `agents.x-k8s.io` Sandbox objects in workspace namespaces for status, and
    garbage-collects OpenShell-labelled Sandbox objects the gateway no longer lists (the
    `--no-keep` orphan bug); it decides whether to restart sandboxes that OpenShell stopped after
    an infrastructure failure.

## Alternatives considered

- **OpenSandbox credential vault** (the plan's fallback): not needed; every credential, isolation
  and policy criterion passed in the intended configuration.
- **Accept 8.12 as a hardening item without a platform fix**: rejected by Andre in favour of
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
  verification" → verified by 0048) and the ADR index once PR #12 merges; run
  `hack/spike/teardown.sh` when Andre says the spike is over.

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

Upstream issue drafts for 6 and 7 are in `hack/spike/results/1-install.md` and `8-escape.md`; none
is filed until Andre reviews them.

## Open questions

- Should Koinon narrow the supervisor pod's egress with its own NetworkPolicy (union of policy
  destinations plus the gateway), at the cost of re-rendering on each policy change?
- Which Ollama or hosted model is the v0.1 default? The 4B model completed every task but needed
  explicit instructions and produced empty or truncated replies.

## Upstream verification

Every result above was observed on the cluster on 2026-10-21; versions: OpenShell 0.1.5 (chart
digest `sha256:e88977ce…`, SDK commit `bdb956f`), Agent Sandbox v1.0.7 (manifest SHA-256
`136c10c9…`), agentgateway 1.6.0, Kata from the Talos `kata-containers` extension with guest
seccomp enabled, OpenCode 1.19.3, Hermes Agent 0.22.3, pi 1.3.0 with pi-acp 0.0.34,
`coder/acp-go-sdk` v0.13.5, `modelcontextprotocol/go-sdk` v1.8.0.

## Links

- [Spike plan](../spikes/openshell.md) and [results](../../hack/spike/results/)
- [0045](0045-openshell-hardening-defaults.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0042](0042-ask-tier-park-and-execute.md),
  [0016](0016-harness-and-deployment.md)
