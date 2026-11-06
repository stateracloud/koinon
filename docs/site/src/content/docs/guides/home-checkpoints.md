---
title: Home checkpoints and the archive tier
description: Save each agent's home to S3 so a recreated or archived sandbox resumes its session.
---

An agent's home is the part of its sandbox that holds its work: sessions, notes and scratch work.
Checkpoints save it to an S3-compatible bucket and restore it into a sandbox that is recreated, so an
image bump, a stuck sandbox or a long wait does not cost the agent its session. A restored sandbox keeps
the agent's home generation and reloads the same session instead of starting over. Checkpoints are off
by default.
[ADR 0032](https://github.com/stateracloud/koinon/blob/main/docs/adr/0032-agent-home-checkpoints.md)
has the decisions and the measurements.

## Requirements

- An S3-compatible bucket, created first: AWS S3, Garage, SeaweedFS, Ceph RGW or MinIO. The client
  addresses buckets path-style on every endpoint but AWS's.
- A Secret in Koinon's namespace with the keys `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
  `endpoint` (`host:port` or an `http(s)` URL; the scheme decides TLS) and `region`.
- OpenShell v0.1.4 or later. The restore goes through OpenShell's flow-controlled upload channel,
  because the exec channel drops input past 4 MiB, which a home easily exceeds. The backup reads the home
  as a tar stream over exec and needs nothing more.
- Room in `koinon-agentd`, the only component that holds the storage credentials: about 512Mi of memory
  per concurrent backup (`agentd.resources`) and scratch space for the staged homes.
- A NetworkPolicy on the bucket's namespace that admits `koinon-agentd` and `koinon-api` pods and no
  others.

```yaml chart-values
checkpoints:
  enabled: true
  s3:
    existingSecret: koinon-homes-s3
    bucket: koinon-homes
  interval: 1h
  scratchSize: 14Gi
```

`scratchSize` is the size limit of the emptyDir where agentd stages a home. A home stages up to 4Gi, two
backups and one restore run at once, and kopia's caches need about 2Gi, so the default is 14Gi
(3 x 4Gi + 2Gi). Going over the limit evicts the agentd pod, so lower it only together with the per-home
limit.

## When a checkpoint is taken

- every hour while the sandbox is running (`checkpoints.interval`);
- before an idle stop. A failed checkpoint, or a wait of more than five minutes, does not hold the stop:
  stopping keeps the volume;
- before a recreation.

A sandbox is recreated only after a checkpoint taken since its last activity is complete. A failed
backup (three attempts with growing delays) blocks the recreation and marks the agent unhealthy with the
reason; an admin resumes the agent, and a new backup is requested. A sandbox that cannot be read at all
is not recreated either, and its volume is kept.

## What is saved, and what is not

Saved: the whole home, including repository checkouts, because uncommitted work in a checkout is the
scratch work worth keeping. Excluded: package-manager, build and harness caches such as `node_modules`,
`.cache`, `.npm`, `.pnpm-store`, `.cargo/registry` and `go/pkg/mod`, and the home's top-level `tmp` (see [exclude.go](https://github.com/stateracloud/koinon/blob/main/internal/checkpoint/exclude.go) for the full list). Ephemeral agents (one fresh sandbox
per task, no home volume) have no home to save and are skipped.

No secrets are in a home: credentials stay in OpenShell provider records, agentgateway and the secret
store, never in the sandbox. Each agent has its own kopia repository at `orgs/<org>/homes/<agent>/` in
the bucket, compressed with zstd. Its random password is sealed by the org's key; only `koinon-api` can
open it, and `koinon-agentd` asks for it per checkpoint. Retention keeps the latest 24 snapshots, one an hour for 24 hours, one a
day for 14 days and one a week for 8 weeks. There is no off-site copy in v0.1.

## Restore

The first run after a recreation restores the home before the harness starts, newest checkpoint first,
then up to two older ones, two tries each. If none restores, the run fails and the restore stays owed:
the harness never starts on an empty home. A checkpoint from a higher run epoch than the agent has seen
is skipped.

## After a database restore

The database lists the checkpoints, and the bucket holds them. After you restore a database dump, the
list is older than the bucket. It lacks the snapshots written since the dump, and it names snapshots
that retention or the repository sweep has deleted since. Run this once koinon-agentd is up:

```sh
kubectl exec deploy/koinon-api -- koinon-api checkpoints resync --all
```

`--agent <id>` (repeatable) picks agents instead. Each agent's request is audited in its org
(`home.checkpoints_resync_requested`). koinon-agentd then lists every repository of the agent that is
neither deleted nor lost with its key, and makes two kinds of change:

- A snapshot that no checkpoint names becomes a complete checkpoint, with the generation, run epoch,
  reason and sandbox it was written with, and the times of the snapshot. A seed's copy of a checkpoint
  into a newer repository takes its times from the checkpoint it copies, so it is no newer than its
  source.
- A snapshot is left out if its backup is still running or was given up, or if its tags cannot be
  read. It is also left out if it is another attempt at a checkpoint that already has its snapshot in
  the same repository, or an earlier attempt where a later snapshot carries the same tag. Builds
  before checkpoints resync ([#89](https://github.com/stateracloud/koinon/issues/89)), v0.1.0-rc.9 and earlier and main before it merged, saved a snapshot
  before they checked that the home was read whole, and a retry kept the checkpoint's ID. The summary counts the snapshots left out.
- A complete checkpoint whose snapshot is gone is marked failed, with an error that names the snapshot.
  No checkpoint is deleted.

The agent's run epoch is raised to the newest run epoch among the added checkpoints. A restored
database has the dump's run epoch, and a restore takes only checkpoints from run epochs that a run has
seen. With the raise, the agent's next run restores the newest home, not one from before the dump.

A repository that is missing from the bucket counts as empty if it was due for deletion. If it was not
due, the resync of that agent gives up and changes nothing, because the bucket is likely not the one
this database belongs to. A resync also waits while the agent's new repository waits for its seed.
Running it again changes nothing.

By default the command waits (`--wait`, up to `--timeout`, 30 minutes) and prints progress and a summary.
It exits non-zero if a resync was given up or replaced by a newer request, or if the timeout passes
first. Requests that are still pending when the timeout passes stay, and koinon-agentd finishes them.
koinon-agentd audits each outcome (`home.checkpoints_resynced`, `home.checkpoints_resync_failed`).
`koinon_agentd_home_checkpoint_resyncs{state}` counts the pending and failed resyncs, and
`koinon_agentd_home_checkpoint_resync_changes_total{change}` counts the checkpoints added and those
marked missing.

Limits:

- The resync cannot recognize one partial home: a checkpoint written after the dump by a build
  before checkpoints resync ([#89](https://github.com/stateracloud/koinon/issues/89)), whose last attempt failed after its snapshot was saved. The restored database has no row
  that records the failure.
- A repository started after the dump, by `keys forget` or `keys new-repositories`, is not in the
  restored database, and its password was sealed only in the database you lost. The homes saved in it
  are lost with that password. A repository's location in the bucket follows from the agent and the
  repository number, so the restored database's next number can point at that repository. Koinon
  never writes over it: run `checkpoints resync` before you run either command again. The resync
  opens each repository the database knows, and it lists the agent's repository folders in the
  bucket to find those past the newest number. It records each location it finds taken by a
  repository the database has no password for, and the next free number, past every folder that
  holds anything (audit `home.repository_prefix_taken`). Its summary counts the taken locations. `keys forget` and
  `keys new-repositories` then start the agent's next repository at that free number. The taken
  repository's blobs are never deleted by the sweeper; delete them yourself once you are sure
  nothing in them is needed ([#262](https://github.com/stateracloud/koinon/issues/262)).
- If a backup or a seed reaches a taken location first (you ran neither the resync nor the commands),
  it fails at once, records the same, and names the location and the next step: run
  `koinon-api keys new-repositories --agent <id>`, which starts the agent's repository at the free
  number. Until then the agent's backups fail.
- A repository that does not open with the password that wrote its complete checkpoints is not
  taken: koinon-agentd uses the wrong bucket, or the repository's `kopia.repository` is damaged. The
  backup fails with an error that says so, and nothing is recorded. Do not start a new repository
  for that agent: it would retire this one, and the sweeper would delete the homes in it.
- A recreate checkpoint whose snapshot the resync found gone is marked failed with the snapshot
  missing, which is not a failed backup: the sandbox is not blocked, and the next pass requests a new
  backup before it recreates the sandbox ([#263](https://github.com/stateracloud/koinon/issues/263)).

## The archive tier

An agent that has been stopped for N days (7 by default) needs neither its sandbox nor its volume. Once
a complete home checkpoint exists, Koinon deletes the sandbox and its volume, and the next run restores
the home into a fresh sandbox and reloads the same session. An agent whose latest backup is incomplete is
not archived, and ephemeral agents are not archived. Archiving therefore needs checkpoints to be on. The 7 days count from the later of the stop and the
last activity.

The window is the chart value `reconciler.archiveAfter` (default `168h`; the flag is `--archive-after`).
A negative value such as `-1h` turns the archive tier off. It is one value for every org; per-org tuning
is not built.

```yaml
reconciler:
  archiveAfter: 168h
```

This is separate from archiving an agent, which retires it. The chart's `reconciler.homeRetention`
decides what happens to the volume of an agent that was archived by an admin (`delete` or `retain`).
