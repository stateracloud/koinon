# 0032. Agent home on a PVC, checkpointed incrementally to object storage

- Status: Accepted
- Amended by: [0065](0065-work-model-tasks-inbox.md) (checkpoints in v0.1; archive tier); HC1 (kopia, exec-stream runner, hourly points)
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

_Answered by the 2026-10-27 amendment below (kopia, exec stream with a fallback pod, hourly points)._

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

## Amendment (2026-10-26): checkpoints in v0.1

[0065](0065-work-model-tasks-inbox.md) builds checkpoints now. The home (session, notes, scratch
work) is saved when a sandbox stops or before it is recreated, and restored afterwards, so long
waits and long tasks survive drains and image bumps. This answers "Is checkpointing needed in
v0.1?": yes.

Still open: restic or kopia; sidecar or Job; whether the park, run-end and periodic points above
also ship in v0.1; and which paths "scratch work" covers, given the exclusion of repository
checkouts above.

## Amendment (2026-10-26, #146 A7): archive tier in v0.1

After N days stopped (default 7), once a home checkpoint is complete, Koinon deletes the sandbox
and its volume and restores the home into a fresh one on the next run. The generation follows the
checkpoint lineage, so the next run reloads the same session
([0051](0051-session-home-generations.md) amendment). Ephemeral agents have no home and are not
archived ([0065](0065-work-model-tasks-inbox.md)).

Prior art (ideas only, no code; its servers are AGPL-3.0): Daytona v0.190.0 archives a sandbox
after 7 days stopped and only once its latest backup is complete, and restores newest backup first
(`sandbox.manager.ts:173-230`; research notes `daytona.md` §3).

## Amendment (2026-10-27, #146 HC1 decisions): kopia, the exec-stream runner, hourly points

Andre's HC1 decisions on #146 (2026-10-26) answer the open questions above; plan item HC1 builds
them. Research with sources and spike numbers: kopia full backup 6.2 s, incremental 0.7 s and
restore 7.2 s on a 186 MB home.

- **Tool: kopia** 0.23.1 or later (it fixes the index-compaction data-loss bug kopia #5371),
  embedded as a Go library, zstd compression. One repository per agent at
  `orgs/<org>/homes/<agent>/` in one S3 bucket per install, so each agent has its own retention
  and deletion and there are no cross-agent locks. Each repository has a random password, sealed
  by the org's key in the secret store (`home_repositories`, an envelope bound to its row); only
  the writer opens it, and it is never logged and never in a sandbox. Retention: the latest 24
  snapshots, one per day for 14 days, one per week for 8 weeks (a default, set in the repository's
  global policy). restic was equally good as a CLI but has no importable API.
- **Runner: exec stream plus a fallback pod; a sidecar is ruled out.** OpenShell 0.1.5 renders
  exactly one container, and under Kata a sidecar would put the keys in the agent's VM.
  - *Normal path (built):* koinon-agentd, the writer, reads the home as tar over OpenShell exec
    (`tar -C /sandbox`, as saved work is read, ADR 0054) and writes it to kopia. The same writer
    holds the S3 credentials, because the home lab's Garage NetworkPolicy admits only agentd and
    API pods. The stream comes from a sandbox, so it is untrusted: it is extracted through an
    `os.Root` (no path, symbolic link included, leaves the scratch directory), with absolute
    names, links to other files, devices and anything over 4 GiB or 2 million entries refused.
    Restore streams a tar back in over exec before the harness starts.
  - *Fallback (not built in this item):* after stop, for a stuck or crashed sandbox where exec is
    impossible, the reconciler would mount the stopped volume in a pinned pod that streams the tar
    to agentd's writer without S3 credentials, under a narrow admission exemption for exactly that
    pod shape. Until it exists, a sandbox that cannot be read is not recreated (see below).
- **Exclusions:** package-manager, build and harness caches (`node_modules`, `.cache`, `.npm`,
  `.pnpm-store`, `.cargo/registry`, `go/pkg/mod`, and similar, see `internal/checkpoint/exclude.go`).
  Repository checkouts are *not* excluded: uncommitted work in a checkout is the scratch work
  worth saving, and the sandbox's own `tar` and the writer both apply the list.
- **Cadence (settles the periodic-points question):** a checkpoint every hour while the sandbox
  is ready, before an idle stop, and before a recreation. Run park and run end are not separate
  points: the hourly point and the stop point cover them.
- **Rules (practice B3):**
  - A sandbox is deleted for a recreation (changed spec, stuck, failed) only after a checkpoint
    taken since its last activity is complete. The reconciler asks (`home_checkpoints` request),
    agentd writes it, and a stopped sandbox is started to be read.
  - A failed latest backup (three attempts, growing delay) blocks the recreation and marks the
    agent unhealthy with the reason; an admin resumes it and a new backup is requested.
  - An idle stop waits for its checkpoint, but a failed one, or a wait over five minutes, does not
    hold it: stopping keeps the volume.
  - Restore: the first run after a recreation restores before the harness starts, newest
    checkpoint first, then up to two older ones, two tries each. If none restores, the run fails
    and the restore stays owed; the harness never starts on an empty home.
  - Ephemeral agents are skipped (`checkpoint.Ephemeral`, a stub until EP1).
- **Epoch:** each checkpoint records the agent's highest run epoch (ADR 0030); a restore skips a
  checkpoint from a higher epoch than any run of the agent has seen.
- **Home generation:** it follows the checkpoint lineage ([0051](0051-session-home-generations.md)):
  a sandbox recreated from a checkpoint keeps `agent_sandboxes.home_generation`, and the new
  column `sandbox_id` carries the OpenShell ID the unhealthy mark compares with.
- **Storage:** chart values `checkpoints.enabled` (default off) and `checkpoints.s3.*`: a Secret with
  `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `endpoint` and `region`, plus the bucket. The S3
  client (minio-go) addresses path-style outside AWS and sends no checksum trailers. On the home
  lab: Secret `koinon/koinon-homes-s3`, bucket `koinon-homes`, region `garage`, a plain HTTP
  endpoint. No off-site copy in v0.1; one (for example R2) can be added later as configuration.

- **2026-10-27, review fixes (HC1):**
  - *No KEK in agentd.* The writer holds the S3 credentials but not the key-encryption keys (ADR
    0054 stands): koinon-api creates or opens an agent's repository password and returns it to
    agentd per checkpoint at `/internal/home-repository` on the saved-work listener, which only
    agentd may reach. The request is authenticated with an HMAC under the saved-work key (a
    separate message prefix; org, agent and an expiry of at most a minute). The password is
    released to the writer only, with `Cache-Control: no-store`, and never logged. Conservative
    choice: the credential is not single-use (unlike a saved-work post), because the answer is
    one password for one agent and the lifetime is a minute.
  - *A sandbox that cannot be read* (phase Error or Completed) files no request, since agentd
    reads a home only from a ready sandbox and the request would never be claimed. The recreation
    uses the sandbox's newest complete checkpoint if it was taken since the sandbox's last
    activity, and otherwise is blocked at once: the agent is marked unhealthy with the reason and
    the home is kept. A resume repeats this until the fallback pod exists.
  - *Restore claim and scratch space.* The restore claim lasts as long as the run waits for it
    (`Timeout * (tries * (1 + fallbacks) + 1)`), so a second run never extracts into the same
    home. Restores take one slot each (`RestoreConcurrency`, default 1), and the default
    `checkpoints.scratchSize` is 14Gi: (2 backups + 1 restore) * 4 GiB + 2 GiB of kopia caches.
  - *Hard links:* the backup runs GNU tar with `--hard-dereference`, and Stage copies a link to an
    already staged file, so no name of a linked file is lost.

Still open: the fallback pod (above); an admin override of a blocked recreation with an audit
entry; kopia maintenance (reclaiming space of expired snapshots) and a periodic test restore;
exec throughput on the home lab, which has to be measured.

## Links

- [0009 Agent runtime](0009-agent-runtime.md), [0029 Run-level resume](0029-run-level-resume.md)
- [0030 Run state machine](0030-run-state-machine.md), [0033 Memory](0033-file-based-memory.md)
- [0044 In-sandbox credential exception](0044-in-sandbox-credential-exception.md)
