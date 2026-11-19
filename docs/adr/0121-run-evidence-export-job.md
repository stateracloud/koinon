---
status: proposed
date: 2026-11-19
area: install-operations
tags: [audit, kubernetes, secrets]
relates: ["0023", "0056", "0057", "0109"]
---
# 0121. Run-evidence export as a chart Job

- Status: Proposed
- Date: 2026-11-19
- Area: install-operations
- Tags: audit, kubernetes, secrets
- Relates to: [0023](0023-observability-and-audit.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0057](0057-mcp-servers.md), [0109](0109-automated-restore-drill.md)
- Issue: [#966](https://github.com/stateracloud/koinon/issues/966)

## Context

`koinon-api run-evidence export` (#926) writes the scrubbed evidence of one recorded run. It had one
way to read the key-encryption keys (KEKs): `--kek-dir`, a mounted directory. On a running install
that does not work. The api image is distroless: it has no shell, no tar and no `kubectl cp`. The api
pod mounts no KEK directory, because koinon-api reads the KEKs through the Kubernetes API (ADR 0056).
`--no-org-secrets` is not allowed when the org stores credentials. The lab used a one-off Job.

Prior art in this repository: the `key-admin` Job and the `koinon-api keys` commands already read the
KEK Secret with `--kek-secret`. Other platforms use one of three routes: an admin Job in the chart,
an authenticated API download, or a CLI that streams the bundle.

## Decision

1. `run-evidence export` gets `--kek-secret`, `--namespace` and `--kubeconfig`. It reads the KEK
   Secret through the Kubernetes API with a read-only `get`, as the `keys` commands do. `--kek-dir`
   stays. The two flags exclude each other.
2. The chart gets an opt-in Job, `runEvidence.export` (off by default). It runs the command with the
   koinon-api service account, which already reads the KEK Secret and holds the KMS decrypt role. It
   adds no RBAC and no service account. The cost: that account can also read other Secrets of the
   namespace and update the KEK Secret, so only the code keeps the export read-only (a `get`). A
   separate account (the key-admin pattern) needs its own KMS role or IAM trust. Follow-up if an
   operator wants it. The Job has no TTL: a deleted Job would run again on the next upgrade. It writes to an S3 prefix. S3 keys come from an existing
   Secret. No KEK material enters logs, env or values. The S3 keys are in env (static keys from the Secret). An export overwrites files that exist in its prefix: use one prefix for each export.
3. The Job is not a hook. Its name carries a hash of its pod template: a new org, run or prefix makes
   a new Job. The operator turns the values off after the export.
4. The evidence rules do not change (ADR 0023, ADR 0057): no tokens, no arguments or results, host
   names masked, org secrets scrubbed. An export that cannot open a stored credential fails.

## Alternatives considered

- An authenticated API endpoint that streams the bundle. It adds a public surface that returns
  run data, and it needs a new permission and a size limit. _(reconstructed)_ A Job needs neither.
- A directory output in the Job. A Job has no durable volume, so the files would be lost. _(reconstructed)_
- A new service account with its own RBAC. The api account already has the access the Job needs.
  _(reconstructed)_

## Consequences

- Positive: an operator exports a run with `helm upgrade` and an S3 read. No exec into a pod.
- Negative: the operator needs an S3 bucket. The export is not interactive.
- Follow-ups: if a download from the API is wanted, write a new ADR.
