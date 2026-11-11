# 0083. Sandbox resources: template defaults, platform bounds, requests and limits

- Status: Accepted
- Date: 2026-10-31
- Relates to: [0045](0045-openshell-hardening-defaults.md), [0075](0075-isolation-tiers.md), [0020](0020-agent-builder-and-templates.md), [0027](0027-repo-and-versioning.md)
- Issue: old issue 314 (decision by procoduck, 2026-10-31); this ADR covers the first slice

## Context

On the rc.9 home-lab run 22 sandbox pods had no CPU or memory requests or limits. The scheduler
treats such a pod as needing nothing, so 13 of them landed on one 8-vCPU node and starved it, with
the node's other pods. Koinon rendered `limits` only (`execution.SandboxSpec.CPU` and `Memory`) and
nothing ever set them. OpenShell 0.1.5 takes `SandboxTemplate.resources` as a Struct
`{requests: {cpu, memory}, limits: {cpu, memory}}` that lands on the `agent` container; when only
limits are given it copies each limit into the request, which over-reserves CPU (Guaranteed QoS).

The default runtime class is Kata (ADR 0045, 0075). A Kata guest never returns memory to the host
once it has touched it, so a memory request below the limit lets a node be over-committed on memory
that cannot be reclaimed.

## Decision

1. **A sandbox's agent container gets requests and limits for CPU and memory.** `SandboxSpec`
   carries `CPURequest`, `CPULimit`, `MemoryRequest` and `MemoryLimit`, and the OpenShell backend
   renders both maps (only non-empty values). They are part of the reconciler's spec hash, so a
   change recreates the sandbox, as an image change does.
2. **Templates may size their sandbox.** `spec.resources` takes `cpu` and `memory`, each with an
   optional `request` and `limit` (Kubernetes quantities). Parsing validates the quantities and that
   a request does not exceed its limit. The values are copied into the agent version's config
   (`resources`); a version stored before this field existed has none and takes the platform default.
   Shipped sizes (estimates, agent container only): finops-analyst cpu 250m/2 and memory 1.5Gi/2Gi,
   lcm-upgrader cpu 250m/2 and memory 1Gi/1.5Gi, researcher cpu 250m/1 and memory 1Gi/1.5Gi. The
   other templates leave it unset.
3. **The platform sets the default and the bounds** (chart `sandbox.resources`, rendered into
   `config.yaml` and read by the reconciler, which refuses to start on a bad quantity or a minimum
   above a maximum). The default is cpu 250m/2 and memory 1Gi/2Gi. The bounds are cpu request
   100m to 2, cpu limit 100m to 4, memory 512Mi to 8Gi for both. `memoryRequestEqualsLimit` is true
   by default.
4. **The effective size** is the template's value if set, else the default; each of the four values
   is clamped to its bounds; with `memoryRequestEqualsLimit` the memory request becomes the memory
   limit; a request above its limit is lowered to the limit. A clamp is logged once per change, with
   org, agent and the value, never per pass.
5. **Isolation tiers (ADR 0075).** Recommended settings are in `values.yaml`: `vm` memory 512Mi to
   8Gi with the request equal to the limit; `hardened` memory 256Mi to 8Gi with a request below the
   limit allowed; `dev` requests of 50m and 256Mi with a 1Gi limit.

## Alternatives considered

- **Limits only.** Rejected: OpenShell mirrors them into requests, which reserves the full limit of
  CPU on the node for an agent that idles most of the time (the cause of the over-reservation).
- **A cluster LimitRange.** Does not reach the supervisor and cannot know a template's size; a
  separate follow-up for the namespaces OpenShell creates.
- **One fixed size for every agent.** Rejected: a research agent and a repo helper differ by an order
  of magnitude in memory, and the bounds are the guard against a bad value, not the default.

## Consequences

- Positive: the scheduler can place sandboxes by what they need; one node cannot be filled with
  unsized pods; a template author cannot exceed what the operator allows.
- Negative: the spec hash changes, so after the upgrade each existing sandbox is recreated once; its
  home is restored from the checkpoint, as for any recreate. Changing a default, a bound or
  `memoryRequestEqualsLimit` later recreates the sandboxes it changes.
- Negative: with `memoryRequestEqualsLimit`, the default reserves 2Gi of node memory per sandbox.
- Follow-ups, each its own issue: a per-agent override within the platform bounds (API, data model
  and UI), and per-org capacity admission.

### Amendment (2026-11-11)

- A reconciler-owned LimitRange in each OpenShell workspace namespace (#76) supplies default
  requests (50m/64Mi) and limits (250m/256Mi) for type Container (which also covers init
  containers), so the supervisor and init containers are no longer BestEffort. OpenShell still
  cannot size them itself (upstream NVIDIA/OpenShell #3930 and #3415). Chart: `sandboxLimitRange`;
  RBAC is fenced by a ValidatingAdmissionPolicy to this gateway's workspace namespaces and the
  fixed shape.
- A LimitRange sizes pods only at admission: only pods admitted after the LimitRange exists get
  the defaults. OpenShell creates the workspace namespace with the first sandbox, so a missing
  namespace does not hold CreateSandbox; Ensure runs again right after create. Other Ensure failures
  (for example Forbidden) are logged and counted (`koinon_reconciler_sandbox_limit_range_errors_total`)
  and hold that org's sandbox work. When CreateSandbox ran before the LimitRange existed, the
  reconciler deletes that sandbox once so the next pass admits supervisors sized.
- Kata's pod overhead (`podFixed` on the RuntimeClass) is outside these Container defaults. A
  Pod-type LimitRange max would have to include that overhead; this amendment does not add one.
- Turning `sandboxLimitRange.enabled` off stops the chart from rendering the ClusterRole, binding
  and VAP, and the reconciler applies nothing new. Existing LimitRanges stay until removed:
  `kubectl delete limitrange -A -l koinon.dev/sandbox-limit-range=true`.

### Amendment (2026-11-11): per-agent override (#75)

- An org admin or owner can override the four values for one agent (`SetAgentSandboxResources`,
  `GetAgentSandboxResources`; the agent page, "Sandbox size"). The override is a setting of the agent,
  not a new version, stored in four nullable columns of `agents` (migration `00074`, expand-only).
- **Precedence:** the agent's override, else the template's value, else the platform default, per
  value; then the clamp of decision 4. The reconciler clamps again on every pass because the bounds
  can change after the write; a stale out-of-bounds override is logged once and runs clamped.
- **The API refuses** a value outside the bounds, a malformed quantity, and a request above its limit
  (taking the template and the default into account), with `INVALID_ARGUMENT` and a message that
  names the bounds ("memory.limit 16Gi is above the maximum: the platform allows 512Mi to 8Gi").
  Setting a memory request that the install raises to the limit (`memoryRequestEqualsLimit`) is
  allowed; the state reports the effective value and the flag.
- **Audited** as `agent.sandbox_resources.set` with the old and the new override in one transaction;
  a repeat of the stored value writes nothing.
- **The API needs the bounds.** `koinon-api` reads the `sandboxResources` section of `config.yaml`
  (the chart now mounts the file into the API Deployment and rolls it on a change); every other
  setting of the API stays a flag or environment variable. One parser (`sandboxres.FromConfig`)
  serves both binaries.
- A change recreates the sandbox at the next pass (the size is in the spec hash).
- Not done here: the REST admin API (`internal/adminapi`) does not expose it yet.

## Links

- Old issue 314
- #76
