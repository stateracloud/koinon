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
- Negative: the supervisor pod stays BestEffort. OpenShell 0.1.5 cannot size it (upstream
  NVIDIA/OpenShell #3930 and #3415), so its use is unaccounted for until upstream does.
- Negative: with `memoryRequestEqualsLimit`, the default reserves 2Gi of node memory per sandbox.
- Follow-ups, each its own issue: a per-agent override within the platform bounds (API, data model
  and UI), a LimitRange for the supervisor's namespace, and per-org capacity admission.

## Links

- Old issue 314
