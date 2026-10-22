# 0032. Agent home on a PVC, checkpointed incrementally to object storage

- Status: Accepted
- Date: 2026-10-21
- Decision: [design.md#32](../design.md)

## Context

Agents persist between runs ([0009](0009-agent-runtime.md)). Their durable state lives in `$HOME`:

- harness sessions, needed for resume ([0029](0029-run-level-resume.md))
- memory stores ([0033](0033-file-based-memory.md))
- self-installed tools and `environment.lock` ([0015](0015-agent-environment-and-browser.md))
- the browser profile
- in-sandbox credentials where allowed ([0044](0044-in-sandbox-credential-exception.md))

Agent Sandbox keeps the PVC through suspend:

- `spec.volumeClaimTemplates` creates a PVC named `<template>-<sandbox>`, owned by the Sandbox.
- That PVC is kept while suspended and deleted with the Sandbox.
- The OpenShell Kubernetes driver suspends on stop and deletes storage only on delete.

PVCs are zone-bound, though, and give no history or rollback. Moving an agent to another zone or
cluster, rolling back a bad self-install, or exporting an agent all need a copy outside the volume.

## Decision

- **Where the home lives:** a PVC managed through the Agent Sandbox `volumeClaimTemplates`.
- **When it's checkpointed** to object storage under the org prefix
  ([0031](0031-artifacts-in-object-storage.md)):
  - when a run parks
  - when a run ends
  - periodically during long runs
- **Format:** checkpoints are incremental and deduplicated, keep history, and use one of restic or
  kopia (chosen in the follow-up). They are encrypted with a per-org key held outside the sandbox.
- **Included:** memory, harness sessions, `environment.lock`, the encrypted browser profile, and any
  allowed in-sandbox credentials. Credentials are always encrypted and treated as secrets.
- **Excluded:** repository checkouts, package caches and build outputs, all of which can be
  rebuilt.
- **Epoch:** each checkpoint carries the agent's ownership epoch
  ([0030](0030-run-state-machine.md)). A restore refuses a checkpoint newer than the current owner's
  view, and a stale owner can't write a checkpoint.
- **Restore:** any checkpoint can be restored into a fresh PVC in any zone, which covers rollback,
  zone moves and export.
- **Who runs it:** the checkpoint job runs outside the agent's process, either in a supervisor-side
  sidecar or a short-lived job mounting the PVC. The agent never holds the storage credentials or
  the encryption key _(inferred from [0006](0006-identity-layers.md))_.

## Alternatives considered

- **CSI volume snapshots** _(reconstructed)_: cheap where supported, but zone- and driver-specific,
  not portable across clusters, and no export.
- **Plain tar to S3** _(reconstructed)_: simple, but full copies every time, with no deduplication
  or history.
- **No checkpoints, PVC only** _(reconstructed)_: loses agents on zone failure, with no rollback.
- **restic** (BSD-2-Clause, v0.19.1) versus **kopia** (Apache-2.0, v0.23.1): both encrypt,
  deduplicate and support S3. kopia adds zstd compression and policy-based retention. Its 0.23.1
  release notes mention a fix for a rare data-loss race, which is worth weighing.

## Consequences

- Positive:
  - Agents survive zone loss.
  - Rollback after a bad install.
  - Export is per org.
  - Fits suspend-between-runs.
- Negative:
  - Checkpoint time adds to park and run-end latency _(inferred)_.
  - Encryption key management per org is needed.
  - Restoring large homes takes time.
- Follow-ups:
  - Spike: restic versus kopia on a realistic home (sessions plus node_modules excluded), measuring
    time to checkpoint at park and restore time.
  - Exclude list as part of the flavor contract ([0035](0035-v01-flavors.md)).
  - Placement of the checkpoint runner (sidecar versus job), and whether OpenShell or Agent
    Sandbox allow adding a sidecar to the Sandbox pod.

## Open questions

- restic or kopia?
- Sidecar or Job for checkpoints, given the OpenShell pod template constraints?
- Default periodic interval, and retention of checkpoint history per org.
- Is checkpointing needed in v0.1, or is the PVC enough until v0.2?

## Upstream verification

Checked 2026-10-21:

- **Agent Sandbox:**
  - The `volumeClaimTemplates` PVC is kept while suspended and garbage-collected on Sandbox delete.
  - The list is immutable.
  - `ShutdownPolicy` is `Delete` or `Retain`.
  - https://github.com/kubernetes-sigs/agent-sandbox/blob/main/api/v1beta1/sandbox_types.go
- **OpenShell Kubernetes driver:** "Stop preserves the Agent Sandbox resource and workspace PVC";
  delete removes storage. Source: the `openshell-driver-kubernetes` README in
  https://github.com/NVIDIA/OpenShell
- **restic:** v0.19.1, BSD-2-Clause, AES-256-CTR with Poly1305. https://restic.readthedocs.io/en/stable/100_references.html
- **kopia:** v0.23.1, Apache-2.0, snapshots "always incremental". https://kopia.io/docs/features/

## Links

- [0009 Agent runtime](0009-agent-runtime.md), [0029 Run-level resume](0029-run-level-resume.md)
- [0030 Run state machine](0030-run-state-machine.md), [0033 Memory](0033-file-based-memory.md)
- [0044 In-sandbox credential exception](0044-in-sandbox-credential-exception.md)
