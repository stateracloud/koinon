# 0032. Agent home on a PVC, checkpointed incrementally to object storage

- Status: Accepted
- Amended by: [0065](0065-work-model-tasks-inbox.md) (checkpoints in v0.1; archive tier); HC1 (kopia, exec-stream runner, hourly points); old issue 169 (restore through the upload channel); AR1 (archive tier built)
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

## Amendment (2026-10-26, old issue 146 A7): archive tier in v0.1

After N days stopped (default 7), once a home checkpoint is complete, Koinon deletes the sandbox
and its volume and restores the home into a fresh one on the next run. The generation follows the
checkpoint lineage, so the next run reloads the same session
([0051](0051-session-home-generations.md) amendment). Ephemeral agents have no home and are not
archived ([0065](0065-work-model-tasks-inbox.md)).

Prior art (ideas only, no code; its servers are AGPL-3.0): Daytona v0.190.0 archives a sandbox
after 7 days stopped and only once its latest backup is complete, and restores newest backup first
(`sandbox.manager.ts:173-230`; research notes `daytona.md` §3).

## Amendment (2026-10-27, old issue 146 HC1 decisions): kopia, the exec-stream runner, hourly points

procoduck's HC1 decisions on old issue 146 (2026-10-26) answer the open questions above; plan item HC1 builds
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
    holds the S3 credentials, because the S3 store's NetworkPolicy admits only agentd and
    API pods. The stream comes from a sandbox, so it is untrusted: it is extracted through an
    `os.Root` (no path, symbolic link included, leaves the scratch directory), with absolute
    names, links to other files, devices and anything over 4 GiB or 2 million entries refused.
    Restore streams a tar back in before the harness starts (over exec here; amended 2026-10-27,
    old issue 169: through OpenShell's upload channel, see the last amendment).
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
    taken since the home was last written is complete (see the 2026-10-31 amendment: not the idle
    clock). The reconciler asks (`home_checkpoints` request),
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
  client (minio-go) addresses path-style outside AWS and sends no checksum trailers. A maintainer's cluster used an S3-compatible store over plain HTTP. No off-site copy in v0.1; one (for example R2) can be added later as configuration.

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
    uses the sandbox's newest complete checkpoint if it was taken since the home was last
    written, and otherwise is blocked at once: the agent is marked unhealthy with the reason and
    the home is kept. A resume repeats this until the fallback pod exists.
  - *Restore claim and scratch space.* The restore claim lasts as long as the run waits for it
    (`Timeout * (tries * (1 + fallbacks) + 1)`), so a second run never extracts into the same
    home. Restores take one slot each (`RestoreConcurrency`, default 1), and the default
    `checkpoints.scratchSize` is 14Gi: (2 backups + 1 restore) * 4 GiB + 2 GiB of kopia caches.
  - *Hard links:* the backup runs GNU tar with `--hard-dereference`, and Stage copies a link to an
    already staged file, so no name of a linked file is lost.

Still open: the fallback pod (above); an admin override of a blocked recreation with an audit
entry; kopia maintenance (reclaiming space of expired snapshots) and a periodic test restore.
Throughput was measured on a maintainer's cluster on 2026-10-27; see the measurement amendment below.

## Links

- [0009 Agent runtime](0009-agent-runtime.md), [0029 Run-level resume](0029-run-level-resume.md)
- [0030 Run state machine](0030-run-state-machine.md), [0033 Memory](0033-file-based-memory.md)
- [0044 In-sandbox credential exception](0044-in-sandbox-credential-exception.md)

## Amendment (2026-10-27, wave-2 integration): ephemeral agents

Ephemeral agents are skipped everywhere: the reconciler (`agentconfig.IsEphemeral` replaces the
`checkpoint.Ephemeral` stub), `RequestHourlyHomeCheckpoints` (the active version's `ephemeral` flag)
and agentd's `restoreHome` (clears a pending restore). A persistent agent that becomes ephemeral is
backed up by the recreate gate before its home is deleted; that checkpoint is kept, is not restored
into a task sandbox, and serves a later persistent version. Moving the row to a task's sandbox
clears any pending restore and the sandbox lineage, so nothing carries over between tasks (ADR 0065
13c).

## Amendment (2026-10-27, old issue 169): the restore goes through OpenShell's upload channel

**Decision (procoduck, 2026-10-27, old issue 169).** The restore no longer pipes tar into `tar -x` over
exec. koinon-agentd uploads it through OpenShell's flow-controlled `openshell-upload-v1` SSH
subsystem (NVIDIA/OpenShell#4448, in v0.1.4 and later). The backup stays tar over exec.

- **Why exec cannot restore.** The supervisor keeps at most 4 MiB of stdin that the command has not
  read (`MAX_PENDING_INPUT`, OpenShell #3846, v0.1.3 and later) and kills the exec with exit status
  74 and no stderr when a write would pass it. Exec stdin has no consumption signal, and an SSH
  client cannot slow down, since russh grants receive credit before the application reads the data
  (OpenShell #4430). A home of more than a few MiB therefore failed to restore. The 4 MiB cap is
  why Koinon cannot fix this by chunking or pacing the exec stream.
- **Why the backup is unaffected.** It reads stdout, which has SSH flow control.
- **How agentd reaches the channel, with no CLI and no secret in the sandbox.**
  1. `CreateSshSession` returns a short-lived token bound to the sandbox, the sandbox's
     `host_key_fingerprint` and an expiry.
  2. `ForwardTcp` opens the relay with a first frame `TcpForwardInit{sandbox, workspace, target:
     ssh, authorization_token}`; data frames then carry SSH. The gateway checks the token (not
     revoked, this sandbox, not expired) and allows 3 connections per token and 20 per sandbox.
  3. agentd speaks SSH over the relay with `golang.org/x/crypto/ssh`. The supervisor accepts any
     client that reaches it, so agentd authenticates the sandbox instead: it refuses a host key
     whose OpenSSH `SHA256:` fingerprint differs from the one the gateway returned, and a gateway
     that returns none.
  4. It requests the subsystem without a PTY and sends the plan line
     (`{"destination_directory":"/sandbox","expected_destination":null}`), waits for the first
     total (zero), then sends the tar in chunks, never more than 1 MiB (`WINDOW_BYTES`) past the
     last total the supervisor reported. The supervisor reports every 256 KiB, as a big-endian
     `u64` on stdout. After EOF the final total must equal the bytes sent, and the exit status is
     the result: 0 is success, anything else fails the restore with the code and the extractor's
     stderr tail.
  5. `RevokeSshSession` runs on every path. The token stays in agentd: it is not put into the
     sandbox, a log or an error.
- **Koinon shapes the archive.** Upstream's extract command is fixed
  (`mkdir -p -- DEST && cat | tar xf - -C DEST`, as the sandbox user). `checkpoint.WriteTar`
  therefore writes no root entry (tar would try to chmod `/sandbox`), skips every path
  `checkpoint.Excluded` names (so a checkpoint made before the list grew restores cleanly), and
  writes no owners.
- **`lost+found`** (top level only) joins the excluded paths: it is the volume filesystem's root
  directory, which the sandbox user cannot extract into. The backup's `TarExcludeArgs` leaves it
  out too. `work/lost+found` stays a user's directory.
- **Old OpenShell.** A supervisor without the subsystem answers the request with a failure; the
  restore then fails at once, without retries or older checkpoints, with "this sandbox supervisor
  does not support openshell-upload-v1; OpenShell v0.1.4 or later is needed". Expired tokens and
  connection limits are reported as such. The restore gate is unchanged otherwise: a failed
  restore fails the run, the restore stays owed, retries and fallback to older checkpoints stand.
- **Timeouts.** The upload has no timeout of its own: the caller's context bounds it, which for the
  restore is `Checkpoints.Timeout` per try. A backend-level default would cap that setting
  silently, so there is none. When the upload fails before it starts, agentd cancels the writer
  at once instead of waiting for kopia to finish staging the snapshot.
- **Caveats.** Upstream calls the protocol internal, but the subsystem name is versioned. Extraction
  is not atomic, which is acceptable because the harness does not start unless the restore
  succeeded. Throughput is bounded by 1 MiB per round trip; the measured numbers are below. The Go SDK has no upload transport (it returns `ErrTransportNotAvailable`), which is why
  agentd carries the client.
- **Tests.** The fake OpenShell gateway serves the subsystem as upstream does (window enforcement
  with exit 74, acknowledgments, plan parsing) and caps exec stdin at 4 MiB, so the old failure
  reproduces in a test.

### 2026-10-27: measured on a maintainer's cluster

Measured on main e520d87 on a maintainer's cluster (Kata sandboxes, an S3-compatible store), with
200 MiB of random data in an agent's home (210 MB with the rest of the home), old issue 169:

| | Time |
| --- | --- |
| Backup, first (tar over exec, kopia, S3-compatible store) | 11.4 s and 14.2 s on two runs |
| Backup, unchanged home | 6.4–7.5 s |
| Restore through `openshell-upload-v1` (kopia staging and upload, from run queued to `run.home_restored`) | 16 s |

The restored files' SHA-256 sums matched the originals. Before the fix, every restore failed with
exit 74; a prototype that sent 3 MiB pieces over exec took 19.9 s and needed twice the disk space.
A home at the 4 GiB stage limit would take about 5 minutes at these rates.

## Amendment (2026-10-27, old issue 146 A7, plan item AR1): the archive tier

The archive tier above is built in the reconciler (`internal/reconciler/archive.go`).

- **When.** A sandbox that has been stopped for `reconciler.archiveAfter` (default 168h = 7 days,
  counted from the later of `stopped_at` and the sandbox's last activity) is deleted with its
  volume. Needs `checkpoints.enabled`; a negative value turns the tier off. Conservative choice: the
  window is one platform value (chart and `--archive-after`), not an org setting; a per-org
  override can be added to `org_settings` later without changing the rules below.
- **Gate (practice B3).** The sandbox's latest checkpoint must be complete and finished since the
  home was last written (`fresh`, the freshness rule of `homeGate`; the 2026-10-31 amendment). A failed, requested, running or
  older checkpoint means no archive. Unlike a recreation, the archive never starts the sandbox,
  files a request or marks the agent unhealthy: it waits for the next stop backup. A sandbox that
  still owes a restore (`restore_checkpoint_id`) holds nothing newer than that checkpoint and may
  be archived.
- **Never archived:** ephemeral agents (`checkpointsOn`, nothing to keep) and any sandbox with an
  active or claimable run (it is not stopped then). Decision: a **parked** run does not prevent
  the archive. The restore keeps the home generation ([0051](0051-session-home-generations.md)),
  so when the park resolves the run is claimed, a sandbox is created, the home is restored and the
  session is reloaded with `session/load`.
- **Effect.** The checkpoint becomes the restore (`SetSandboxRestore`), the sandbox is deleted,
  `agent_sandboxes.archived_at` is set (migration 00029), the row is held in state `deleting`
  (phase `Archived`, message "archived, restores on next run"; agentd waits on that state rather
  than failing the run) and `sandbox.archive` is audited. No sandbox is created while nothing
  wants one. A queued run, a claimable park or a pre-wake signal creates a fresh sandbox, which
  keeps the home generation; the first run restores the home (HC1, unchanged) and reloads the same
  session. Any observed starting, ready, stopping, stopped or error sandbox clears `archived_at`.
- **Shown** on the agent page ("archived, restores on next run", `Agent.sandbox_archived_at`).

## Amendment (2026-10-31, old issue 329): the gates count from when the home was last written

Every gate (recreate, repair of a stuck or failed sandbox, idle stop, archive) compared its
checkpoint with `activitySince`: the latest of the sandbox's start, the last run activity and the
last pre-wake. That is the idle clock, not "the home was written". A wake or a start writes
nothing, yet it made the hourly backup look stale, and a sandbox in phase Error (unreadable, so no
new backup is possible) was then blocked and its agent marked unhealthy, although its home held
exactly what the backup holds.

- **The clock** is `agent_sandboxes.home_dirtied_at` (migration 00043): the last time a harness in
  the current sandbox started or stopped running (a run entering or leaving `running`, set by a
  trigger on `runs`). NULL means no harness ever ran in this sandbox. A new sandbox ID resets it
  (`RecordAgentSandbox`, `SetAgentSandboxName`). A run that fails from `starting` never ran a
  harness and does not count.
- **Wake and start count only toward idle.** `activitySince` stays the clock of the idle stop and of
  the archive window; the gates use `homeDirtiedSince`.
- **An untouched home recreates freely.** With the clock NULL no new backup is requested, waited
  for or required, whatever the sandbox's phase, and an idle stop goes ahead. The newest complete
  checkpoint of the sandbox's home generation (`LatestCompleteHomeCheckpointOfGeneration`) becomes
  the restore, so the lineage is kept; with none, the next sandbox starts without a restore.
  Visible change: a sandbox no harness ever ran in is stopped, recreated or archived without
  checkpoints, and an unreadable one is recreated at once instead of blocking the agent.
- agentd fails a run whose sandbox is in Error, Deleted or Retained with the agent unhealthy with
  the class `sandbox_unavailable` (it was `fatal`).

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
