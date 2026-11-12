# 0031. Artifacts in S3-compatible object storage

- Status: Accepted
- Date: 2026-10-21
- Area: work-model
- Tags: object-storage, tasks, mcp
- Decision: [design.md#31](../design.md)

## Context

Agents produce files that outlive a run: reports, patches, generated documents, screenshots, logs.
These need to be:

- referenced from tickets and from A2A task results, since A2A defines artifacts as part of a task
  ([0011](0011-agent-to-agent.md))
- shared under permission checks
- kept for an org-defined period

Postgres is the system of record ([0003](0003-postgres-source-of-truth.md)), but large blobs don't belong in
it. Agent homes are already checkpointed to object storage ([0032](0032-agent-home-checkpoints.md)),
so Koinon depends on an S3-compatible store in any case.

## Decision

- **Storage:** artifact contents go to S3-compatible storage (AWS S3, MinIO, SeaweedFS, or any S3
  API), under a per-org key prefix: `orgs/<org_id>/artifacts/<artifact_id>`.
- **Metadata** lives in Postgres:
  - `artifacts(id, org_id, ticket_id, run_id, agent_id, name, media_type, size, sha256,
    created_at, retention_until)`
- **Publishing:** agents publish through a `publish_artifact` MCP tool served outside the sandbox
  ([0040](0040-agentgateway-model-and-mcp-gateway.md)). The sandbox never gets object storage
  credentials.
- **References:** tickets and A2A results refer to artifacts by ID. A2A results carry the reference
  as quoted data, not as an inline blob ([0011](0011-agent-to-agent.md)).
- **Downloads:** after a permission check, the API returns a short-lived signed URL.
- **Retention:** set per org; a sweeper deletes expired objects and marks the metadata.

## Alternatives considered

- **Blobs in Postgres** (bytea or large objects) _(reconstructed)_: transactional, but backups and
  replication bloat, and large downloads load the API.
- **Files on the agent home volume** _(reconstructed)_: zone-bound and tied to one agent; can't be
  shared across orgs or survive agent deletion.
- **A Kubernetes-native store** (PVC per org) _(reconstructed)_: no signed URLs, harder retention.

## Consequences

- Positive:
  - A standard pattern; works on any cloud or on-prem with MinIO or SeaweedFS.
  - Matches the A2A artifact model.
  - The org prefix makes per-org export and deletion easy.
- Negative:
  - Two writes (object and row) are not atomic _(inferred)_. Write the object first, then the row;
    a sweeper removes orphaned objects.
  - Signed URLs are bearer links for their lifetime, so keep them short (minutes) _(inferred)_.
- Follow-ups:
  - An `ObjectStore` interface shared with home checkpoints.
  - A `publish_artifact` tool with size limits and media-type detection.
  - Per-org retention settings in org settings ([0018](0018-v1-screens.md)).
  - Optional server-side encryption settings documented in the Helm values _(inferred)_.

## Open questions

- Whether the v0.1 demo needs artifacts at all, or whether they land in v0.2.
- Default retention value, and whether legal hold is needed before v1.0.
- Can the ticket UI render artifacts inline (images, markdown) through the API proxy instead of
  signed URLs?

## Upstream verification

None beyond the S3 API contract; MinIO and SeaweedFS both implement presigned URLs (standard,
not re-checked 2026-10-21).

## Links

- [0011 Agent-to-agent](0011-agent-to-agent.md), [0032 Agent home](0032-agent-home-checkpoints.md)
- [0040 agentgateway](0040-agentgateway-model-and-mcp-gateway.md)
