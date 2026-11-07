# Spike: OpenShell, Agent Sandbox and agentgateway on the maintainer cluster

- Status: Run complete (steps 1–8, re-runs and gap checks on 2026-10-21); ADR 0048 proposed; teardown pending;
  results in `hack/spike/results/`)
- Tracking: [koinon-private#3](https://github.com/stateracloud/koinon-private/issues/3)
- Results: [ADR 0048](../adr/0048-openshell-spike-results.md)
- Decisions it verifies: ADR 0002, 0006, 0008, 0009, 0013, 0016, 0029, 0035, 0040–0045

## Goal

Find out whether the execution layer Koinon reuses keeps the core promise: **an agent in a sandbox
can do useful work, but cannot see real credentials, cannot leave its policy, and must get human
approval for risky actions.** Each step below has pass/fail criteria. A result is one of:

- **pass**: the criterion holds, with evidence recorded;
- **fail**: the criterion does not hold in Koinon's intended configuration;
- **blocked**: an upstream bug prevents testing it; the upstream issue is drafted and shown to
  procoduck before filing.

## Rules

- Time box: about 3 working sessions.
- Any **fail** on credentials (step 2), isolation (step 8) or policy (step 3) **stops the spike**.
  Claude then writes up the evidence and proposes the fallback (OpenSandbox credential vault behind
  the `ExecutionBackend` interface, ADR 0016) for procoduck to decide.
- **partial** (added 2026-10-21 by procoduck's decision after the code review): enforcement held, but a
  non-enforcement part of the criterion (logging, API coverage, configuration) is missing. A partial
  does not trigger the stop rule and is listed as a gap in ADR 0048.
- A criterion that fails and passes on re-run after a platform fix is recorded as **pass after
  platform fix**, keeps its fail history, and the fix becomes a Koinon install requirement.
- Claude asks procoduck before any cluster-level change not listed here, including temporary ones.
- The spike starts once procoduck reports that the spike registry and **OpenBao** are
  installed (both running since 2026-10-21) and gives the go-ahead. Nothing cluster-wide changes
  beyond the CRDs and namespaces listed below.
- No paid model calls. Models come from a local Ollama.
- Throwaway spike code lives in `hack/spike/`; nothing there is product code.

## Environment

| Item | Value |
| --- | --- |
| Cluster | a Talos+Kata cluster with 2 workers, the spike cluster context |
| Runtime classes | `kata` (Cloud Hypervisor, default), `kata-qemu` (fallback if nested virtualization misbehaves) |
| CNI | Cilium (Koinon must not depend on it; only standard NetworkPolicy is used) |
| Storage | the default storage class |
| Platform | Managed by a maintainer's GitOps setup; the spike does not modify it |
| Registry | A registry at `registry.example.invalid` |
| Secret store | OpenBao (raft); OpenShell `vault` credential driver |
| Models | Ollama at `http://ollama.example.invalid:11434`, model `qwen3.5:4b`, OpenAI-compatible `/v1` |
| Git target | The platform's Gitea (in-cluster, also at `git.example.invalid`). The spike works only inside a Gitea org `koinon-spike` |

### Components installed by the spike

| Component | Version | Namespace | Notes |
| --- | --- | --- | --- |
| Kubernetes Agent Sandbox | v1.0.7 | `agent-sandbox-system` | core `agents.x-k8s.io` CRDs plus `extensions.agents.x-k8s.io` |
| NVIDIA OpenShell gateway | v0.1.5 | `openshell` | Helm chart from `deploy/helm/openshell`; Kubernetes compute driver |
| agentgateway | 1.6.0 | `agentgateway-system` | model gateway and MCP gateway (ADR 0040); standalone mode |
| agentgateway controller (Kubernetes mode, 5.6 only) | 1.6.0 chart | `agentgateway-system` | its CRDs and own GatewayClass; approved 2026-10-21 |
| Spike tools | — | `koinon-spike` | approval stub, ExtMCP processor (6.5), escape probes, ACP driver (`hack/spike/`) |

### Approved deviations

Recorded 2026-10-21 after the code review; all approved by procoduck.

- Agent Sandbox ships plain manifests, not a Helm chart: `hack/spike/agent-sandbox.sh` applies the
  pinned release manifest after a SHA-256 check.
- OpenBao: a dedicated KV mount and policy (see "OpenBao
  setup"), set up by procoduck.
- Platform repo (procoduck): label `openshell.ai/sandbox-attachable: "true"` on RuntimeClasses `kata`
  and `kata-qemu`; Kata guest seccomp enabled on both workers through a node
  node CRI customization pointing both handlers at copies of the Kata
  config with `disable_guest_seccomp = false`.
- 8.14: Pod Security `enforce=privileged` was set on `openshell-openshell-spike-b` for a few
  minutes so that server-side dry runs reached the VAP, then removed.

Exact chart versions and values are recorded in `hack/spike/` during the run; `hack/spike/teardown.sh`
removes every component, namespace and CRD the spike created, and deletes the `koinon-spike` Gitea
org and the spike's OpenBao paths.

### Gitea setup

- An admin token (procoduck's) is read from the a local keychain entry by the setup and
  teardown scripts on the Mac; it never enters a sandbox or the approval stub. (A copy in secret
  `koinon-spike/gitea-token` was created early on and is unused.)
- Setup creates org `koinon-spike`, repo `koinon-spike/hello` with a protected `main`, and a
  non-admin user `koinon-spike-bot` with write access to that repo only.
- The bot's token is the spike's real credential: it lives only in OpenBao and reaches Gitea
  through the OpenShell provider (step 2) or the approval stub (step 6).

### OpenShell configuration

- `server.drivers.kubernetes.workspaceMode: managed` (one namespace per workspace, ADR 0002)
- `default_runtime_class_name: kata`
- Authentication: mTLS (default); the spike client holds a client certificate
- `credential_drivers = ["vault"]` against OpenBao (KV v2, Kubernetes auth)

### OpenBao setup

Created by procoduck on 2026-10-21; the spike changes nothing else in OpenBao.

- All spike secrets live under KV v2 mount `secret/`, path `koinon-spike/` (the bot's Gitea token and
  the random probe secrets for step 2).
- The gateway does not read `koinon-spike/`. OpenShell's `vault` driver stores every credential it
  is given (and each sandbox's SSH identity) itself, under the fixed prefix
  `openshell/provider-credentials/`, so it gets its own KV v2 mount, outside the
  platform's secret store. A dedicated policy (create, read, update on
  `<mount>/data/openshell/provider-credentials/*`, delete on the matching metadata path) is
  bound through a dedicated Kubernetes auth role to service account `openshell/openshell`.
  The driver requires HTTPS, so it uses `https://secrets.example.invalid`. (Revised during
  step 1; see `hack/spike/results/1-install.md`.)
- A dedicated admin policy (create, read, update, delete on that path only) backs an orphan token
  with a 7-day TTL in a local keychain entry. Setup, rotation (2.7) and teardown
  scripts on the Mac use it; it never enters the cluster.
- Teardown deletes `secret/koinon-spike/`; the dedicated mount, policies and role are
  procoduck's to remove.
- The platform's secret store has no namespace conditions, so step 8 also checks that
  sandbox identities cannot create `ExternalSecret` objects.
- Every rendered L7 endpoint uses `enforcement: enforce` (ADR 0045)
- `agent_policy_proposals_enabled: false` (default)

## Steps

### 1. Install and lifecycle

Install Agent Sandbox, then OpenShell with the configuration above. Create a workspace `spike-a`
and a sandbox from the default image.

| # | Criterion | How |
| --- | --- | --- |
| 1.1 | Agent Sandbox controller and CRDs are healthy | `kubectl get crd`, controller logs, a plain `Sandbox` reaches `Ready` |
| 1.2 | OpenShell creates an `agents.x-k8s.io/v1beta1` `Sandbox` in the managed workspace namespace | inspect the created object and namespace name `openshell-{gateway}-spike-a` |
| 1.3 | The sandbox pod runs with `runtimeClassName: kata` and is a Kata VM | pod spec; guest `uname -r` differs from the node kernel |
| 1.4 | Stop suspends instead of deleting: `spec.operatingMode: Suspended`, pod gone, PVC kept | stop via gateway, check objects |
| 1.5 | Start resumes with the same PVC and files written before the stop | write a file, stop, start, read it |
| 1.6 | Cold start and resume times are recorded (no threshold; input for ADR 0009) | timestamps from gateway events |

### 2. Credentials

Create a provider with the `koinon-spike-bot` Gitea token as `SPIKE_TOKEN`, stored in OpenBao and
bound to the Gitea host only. Inside the sandbox, a probe script searches for the real value.

| # | Criterion | How |
| --- | --- | --- |
| 2.1 | The agent process sees only a placeholder (`openshell:resolve:env:…`) | `env`, `/proc/*/environ` of every process the agent user can read |
| 2.2 | The real value is not in any file the agent can read | `grep -r` of `/`, the home volume and `/tmp` for the value and its base64 |
| 2.3 | The real value is not in the agent's process memory | dump the agent's own processes' memory (as the agent user) and search |
| 2.4 | The supervisor's memory is not readable from the agent | `ptrace` / `/proc/<supervisor>/mem` attempts fail |
| 2.5 | The value reaches Gitea only for the allowed host, port and path | Gitea access log shows the real token; a request to another host gets the placeholder or a 403 |
| 2.6 | The value never appears in OpenShell or sandbox logs | search gateway, supervisor and pod logs |
| 2.7 | Rotating the secret in OpenBao takes effect without restarting the sandbox | rotate, repeat 2.5 |
| 2.8 | Exception paths are understood: `SandboxSpec.environment`, template `--env` and `allow_uninspected_credentials` expose real values as documented | confirm, so ADR 0045 and the VAP forbid them; not a fail |

**Strict bar:** any real secret value observable inside the sandbox in the intended configuration
(2.1–2.6) is a **fail**.

### 3. Policy

Render a policy for the sandbox that expresses "Gitea read allowed, PR creation needs approval":
allow `GET` on the Gitea API for the agent's binaries; deny `POST /api/v1/repos/*/*/pulls` from the
sandbox; deny everything else.

| # | Criterion | How |
| --- | --- | --- |
| 3.1 | Allowed reads succeed (`GET /api/v1/repos/{owner}/{repo}/contents/...`) | curl and `git clone` from inside the sandbox |
| 3.2 | Direct PR creation is denied with a 403 and logged | `POST .../pulls` from inside; OCSF event present |
| 3.3 | Hosts not in policy are denied (HTTP and HTTPS) | curl to Ollama directly, to a public site |
| 3.4 | Rules match by binary: the same request from a binary not in the rule is denied | curl vs. an allowed binary |
| 3.5 | Policy defaults to deny when the policy is empty | sandbox with no network policy |
| 3.6 | Hot reload: changing a network rule takes effect without a restart, within 30 s | update via gateway, retry |
| 3.7 | With `enforcement: audit` the request passes but is logged (confirms ADR 0045 must force `enforce`) | flip one endpoint, observe; not a fail |
| 3.8 | Every decision is logged with sandbox, binary, host, method, path and verdict | OCSF JSONL export |

### 4. Gateway API from Go

A throwaway Go program in `hack/spike/gatewayctl` uses `github.com/NVIDIA/OpenShell/sdk/go` with an
mTLS client certificate.

| # | Criterion | How |
| --- | --- | --- |
| 4.1 | Create, stop, start and delete a sandbox | program output, objects in the cluster |
| 4.2 | Create a sandbox template with image, runtime class and resources | `CreateSandboxTemplate`, sandbox from it |
| 4.3 | Create and update a provider (custom profile for agentgateway) and push a new credential value (`provider update`) | value visible to the supervisor, not the agent |
| 4.4 | Set and update a per-sandbox network policy (`UpdateConfig`) | step 3 rules applied from Go |
| 4.5 | Watch sandbox status and stream logs | events received for 4.1 |
| 4.6 | Workspace and member management (`AddWorkspaceMember`) | a second workspace `spike-b` with its own admin |
| 4.7 | Gaps are listed: anything the reconciler needs that the API lacks | written into ADR 0048 |

### 5. Inference through agentgateway

agentgateway runs with an OpenAI-compatible backend pointing at Ollama. Each sandbox gets a
provider whose credential is its own agentgateway API key; the harness base URL points at
agentgateway (ADR 0040).

| # | Criterion | How |
| --- | --- | --- |
| 5.1 | A chat completion from inside the sandbox reaches Ollama through agentgateway | curl with the placeholder key |
| 5.2 | The agentgateway key in the sandbox is a placeholder; Ollama is not reachable directly | step 2 probes; step 3.3 |
| 5.3 | Token usage and `llm.cost` are recorded per key, including for streaming responses | agentgateway logs/metrics; set a nominal price for `qwen3.5:4b` in the catalog |
| 5.4 | A per-key limit stops requests once reached (429), with at most one response of overshoot | low limit, loop requests |
| 5.5 | Koinon can read per-key spend (logs, OTLP or API) for the ledger (ADR 0041) | export works |
| 5.6 | Kubernetes mode vs standalone mode: which supports per-key USD limits pushed by Koinon | try both; record the choice |

### 6. Approval routing (ask tier)

A stub approval service in `hack/spike/approver` registers as an MCP server behind agentgateway
and exposes `create_pull_request`. agentgateway routes that tool to the stub with CEL rules
(or ExtMCP); direct PR creation stays denied by OpenShell (3.2).

| # | Criterion | How |
| --- | --- | --- |
| 6.1 | The agent sees `create_pull_request` through agentgateway and nothing that bypasses it | MCP `tools/list` |
| 6.2 | Calling it returns "approval requested" immediately and records the exact tool, arguments and target | stub log |
| 6.3 | On approval, the stub creates exactly the approved PR on Gitea with an idempotency key; repeating the approval does not create a second PR | Gitea PR list |
| 6.4 | Changing any argument after the request (e.g. a different base branch) is rejected | tamper and replay |
| 6.5 | CEL vs ExtMCP: which routing works, and how long an MCP call can stay open (for the record only) | both tried |

### 7. Harnesses over ACP

Flavor images for OpenCode, Hermes and pi (via `pi-acp`) are built on the OpenShell base image and
pushed to the registry. Until `agentd` exists, `hack/spike/acpdrive` (Go, `github.com/coder/acp-go-sdk`)
starts the harness through the gateway exec stream and talks ACP over its stdin/stdout; if exec
cannot carry stdin both ways, a small relay inside the sandbox is used instead. Each harness is
configured with agentgateway as its OpenAI-compatible base URL and the approval stub as an MCP
server.

Per harness:

| # | Criterion | How |
| --- | --- | --- |
| 7.1 | `initialize` succeeds and reports `loadSession` | ACP response |
| 7.2 | `session/new` and one `session/prompt` round trip with streamed `session/update` | transcript |
| 7.3 | `session/load` works after the sandbox is suspended and resumed (session files on the PVC) | stop, start, load, prompt again |
| 7.4 | At least one tool call is attempted | transcript |
| 7.5 | Simple task: in a Gitea repo, create `hello.md` on a branch and open a PR through the approval tool; the PR appears after approval. Up to 3 attempts | Gitea |
| 7.6 | Known caveats are confirmed or refuted: OpenCode needs the original `cwd` on load; Hermes starts a new session for an unknown ID; pi-acp lacks `session/resume` and does not forward client MCP servers | record per harness |

A harness passes with 7.1–7.5. A failure caused by the 4B model's ability (not by plumbing) is
recorded as such and re-run with a stronger Ollama model if one is available.

### 8. Escape checks

A probe image runs the checks from inside a sandbox as the agent user. Every item must be
**blocked**.

**Network**

| # | Attempt |
| --- | --- |
| 8.1 | Direct TCP and UDP to an IP outside policy (public, LAN, node IPs) |
| 8.2 | Cloud and link-local metadata (`169.254.169.254`, `fd00:ec2::254`) |
| 8.3 | DNS exfiltration: queries to an arbitrary resolver, and long subdomains through the cluster resolver |
| 8.4 | Kubernetes API (`kubernetes.default.svc`, node IPs on 6443 and 10250) |
| 8.5 | Postgres, OpenBao, the OpenShell gateway's admin port, agentgateway's admin port, Gitea except through policy |
| 8.6 | Another sandbox in the same and in another workspace |

**Identity and secrets**

| # | Attempt |
| --- | --- |
| 8.7 | Read a ServiceAccount token (`/var/run/secrets/...`, projected volumes) |
| 8.8 | Secrets in env, `/proc/*/environ`, files or logs (step 2 probes) |
| 8.9 | Read supervisor memory or attach with `ptrace` |

**Host and runtime**

| # | Attempt |
| --- | --- |
| 8.10 | Confirm a Kata VM: guest kernel differs from the host; no host PID namespace |
| 8.11 | Mount, `hostPath`, `hostNetwork`, privileged operations, raw sockets, new capabilities |
| 8.12 | Landlock: write outside allowed paths; seccomp: blocked syscalls (`mount`, `unshare`, `bpf`, `keyctl`) |
| 8.13 | `kubectl debug` ephemeral container on a sandbox pod is rejected by the ValidatingAdmissionPolicy matching `pods/ephemeralcontainers` |
| 8.14 | A sandbox requesting a non-allowed runtime class or `hostNetwork` is rejected by the VAP |

**Defense in depth**

| # | Attempt |
| --- | --- |
| 8.15 | A standard Kubernetes NetworkPolicy on sandbox namespaces allows egress only to the OpenShell gateway, agentgateway and DNS; with the supervisor proxy bypassed (a probe process that opens sockets directly), 8.1–8.6 stay blocked |
| 8.16 | A sandbox pod annotated `io.katacontainers.config.runtime.disable_guest_seccomp: "true"` does not run with guest seccomp off: the VAP rejects `io.katacontainers.*` annotations on sandbox pods (tested with and without the rule) |

## Evidence

For each criterion, record the command, the relevant output and a timestamp in
`hack/spike/results/<step>.md`. Logs that could contain secrets are redacted before commit; the
test secret is random and rotated after the spike.

Deviation (approved 2026-10-21): the first run of steps 1–6 and 8 is recorded as summary rows.
Every later run (re-runs, gap checks, step 7) records the full command, output and timestamp in
`hack/spike/results/raw/`, with secrets redacted.

## Results

Summarised in ADR 0048: a table of every criterion with pass, fail or blocked, measured numbers
(cold start, resume, hot reload, overshoot), API gaps from 4.7, the agentgateway mode from 5.6,
the routing method from 6.5, per-harness caveats, and any decision changes. Open items from the
design that this spike closes:

- Credential mechanism: placeholders vs env injection (design.md "Verify before relying on it")
- Kubernetes path maturity and AppArmor
- Policy granularity for risk tiers and exact-request approvals
- Gateway API coverage for the reconciler

## Fallback

If OpenShell fails a credential, isolation or policy criterion, the proposal to procoduck is the
OpenSandbox credential vault (Apache-2.0; egress sidecar injecting headers, Kata supported) behind
the same `ExecutionBackend` interface, with the failing criteria re-run against it. agentgateway
and the approval design (ADR 0040–0042) do not depend on the execution backend.
