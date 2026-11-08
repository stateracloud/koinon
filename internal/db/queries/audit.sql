-- SPDX-License-Identifier: Apache-2.0
--
-- The audit log (plan item PV1, ADR 0023). Append-only: there is no update or
-- delete query, and the table refuses them.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: InsertAuditEvent :one
INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind, target_id, summary, request_id)
VALUES (@org_id, @actor_kind, @actor_id, @action, @target_kind, @target_id, @summary, @request_id)
RETURNING *;

-- name: ListAuditEvents :many
SELECT * FROM audit_events
WHERE org_id = @org_id
  AND (sqlc.narg(before_id)::uuid IS NULL OR id < sqlc.narg(before_id)::uuid)
ORDER BY id DESC
LIMIT @page_size;

-- name: ListAuditEventsFiltered :many
-- The read path (AuditService.List, GET /admin/v1/audit): newest first by id, optionally narrowed
-- by time range, action prefix and actor. before_id is the cursor, the last id of the previous page.
SELECT * FROM audit_events
WHERE org_id = @org_id
  AND (sqlc.narg(before_id)::uuid IS NULL OR id < sqlc.narg(before_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR created_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(action_prefix)::text IS NULL OR starts_with(action, sqlc.narg(action_prefix)::text))
  AND (sqlc.narg(actor_kind)::text IS NULL OR actor_kind = sqlc.narg(actor_kind)::text)
  AND (sqlc.narg(actor_id)::text IS NULL OR actor_id = sqlc.narg(actor_id)::text)
ORDER BY id DESC
LIMIT @page_size;

-- name: ListAuditEventsAllOrgs :many
-- ListAuditEventsFiltered across every org, for platform admins who name no org.
SELECT * FROM audit_events
WHERE (sqlc.narg(before_id)::uuid IS NULL OR id < sqlc.narg(before_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR created_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(action_prefix)::text IS NULL OR starts_with(action, sqlc.narg(action_prefix)::text))
  AND (sqlc.narg(actor_kind)::text IS NULL OR actor_kind = sqlc.narg(actor_kind)::text)
  AND (sqlc.narg(actor_id)::text IS NULL OR actor_id = sqlc.narg(actor_id)::text)
ORDER BY id DESC
LIMIT @page_size;

-- name: ListAuditChainOrgs :many
-- Every org that has a chain, for the verifier.
SELECT org_id FROM audit_events
UNION
SELECT org_id FROM audit_chain_heads
ORDER BY org_id;

-- name: ListAuditChainRows :many
-- One page of an org's chain in chain order, with the fields the hash covers as text, so the
-- verifier recomputes the hash without trusting audit_event_hash().
SELECT org_id::text AS org_text, id::text AS id_text, chain_seq, actor_kind, actor_id, action,
       target_kind, target_id, summary::text AS summary_text, request_id,
       (extract(epoch FROM created_at) * 1000000)::bigint AS created_micros,
       prev_hash, hash
FROM audit_events
WHERE org_id = @org_id AND chain_seq > @after_seq
ORDER BY chain_seq
LIMIT @page_size;

-- name: GetAuditChainHead :one
SELECT * FROM audit_chain_heads WHERE org_id = @org_id;

-- name: InsertInstallAuditEvent :one
-- The install chain (ADR 0100): no org. chain_seq, prev_hash and hash are set by the trigger.
INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary, request_id)
VALUES (@actor_kind, @actor_id, @action, @target_kind, @target_id, @summary, @request_id)
RETURNING *;

-- name: ListInstallAuditEventsFiltered :many
-- The install-scope read path (AuditService.List with AUDIT_SCOPE_INSTALL): the filters of
-- ListAuditEventsAllOrgs over the install chain.
SELECT * FROM install_audit_events
WHERE (sqlc.narg(before_id)::uuid IS NULL OR id < sqlc.narg(before_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR created_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(action_prefix)::text IS NULL OR starts_with(action, sqlc.narg(action_prefix)::text))
  AND (sqlc.narg(actor_kind)::text IS NULL OR actor_kind = sqlc.narg(actor_kind)::text)
  AND (sqlc.narg(actor_id)::text IS NULL OR actor_id = sqlc.narg(actor_id)::text)
ORDER BY id DESC
LIMIT @page_size;

-- name: ListInstallAuditChainRows :many
-- One page of the install chain in chain order, with the fields the hash covers as text. The
-- install ID is read from install_identity, which never changes.
SELECT (SELECT install_id::text FROM install_identity) AS scope_text, id::text AS id_text, chain_seq,
       actor_kind, actor_id, action, target_kind, target_id, summary::text AS summary_text, request_id,
       (extract(epoch FROM created_at) * 1000000)::bigint AS created_micros,
       prev_hash, hash
FROM install_audit_events
WHERE chain_seq > @after_seq
ORDER BY chain_seq
LIMIT @page_size;

-- name: GetInstallAuditChainHead :one
SELECT * FROM install_audit_chain_head;

-- name: GetLastInstallKeyEvent :one
-- The newest key event whose "after" is a key-set state (ADR 0100 decision 3, "Keys: not atomic, so
-- checked on start"): keys.rewrap, keys.reencrypt and keys.forget carry counts, not a key set.
SELECT * FROM install_audit_events
WHERE starts_with(action, 'keys.') AND jsonb_exists(summary->'after', 'keks')
ORDER BY chain_seq DESC
LIMIT 1;
