-- SPDX-License-Identifier: Apache-2.0
--
-- The audit log (plan item PV1, ADR 0023): the read queries. Append-only: there is no update or
-- delete query, and the table refuses them. The inserts are not here: they live in
-- internal/audit/internal/auditstore, so only internal/audit can write (ADR 0100 decision 1.7).
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

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

-- name: LockPlatformSettings :exec
-- Serialises the start-up record of the Helm-set platform settings across API replicas for the rest
-- of the transaction (ADR 0100 decision 3). It is a domain lock, taken before the install head.
SELECT pg_advisory_xact_lock(hashtext('koinon.platform_settings'));

-- name: ListInstallSettingsEvents :many
-- Every platform.settings.observed event, oldest first. An event holds only the keys that changed, so
-- the last recorded settings are the events applied in turn (ADR 0100 decision 3). A few rows per install.
SELECT * FROM install_audit_events
WHERE action = 'platform.settings.observed'
ORDER BY chain_seq;

-- name: ListAuditChainHeads :many
-- Every org's chain head, a plain read of committed rows (an in-flight org write holds its head row
-- and is not seen): the heads an install audit anchor commits to (ADR 0100 decision 4.1).
SELECT org_id, seq, hash FROM audit_chain_heads
ORDER BY org_id;

-- name: TryLockAuditAnchor :one
-- The domain lock of the anchor job, taken before the install head (ADR 0100 decision 1.7): one
-- anchor is written at a time across API replicas. Returns false when another transaction holds it.
SELECT pg_try_advisory_xact_lock(hashtext('koinon.audit_anchor'))::boolean AS locked;

-- name: LockAuditAnchor :exec
-- TryLockAuditAnchor, waiting for the lock (an anchor made on demand).
SELECT pg_advisory_xact_lock(hashtext('koinon.audit_anchor'));

-- name: GetLastInstallAnchor :one
-- The newest audit.anchor install event and its age by the database's clock.
SELECT id::text AS id_text, chain_seq, extract(epoch FROM now() - created_at)::float8 AS age_seconds
FROM install_audit_events
WHERE action = 'audit.anchor'
ORDER BY chain_seq DESC
LIMIT 1;

-- name: ListInstallAnchors :many
-- A page of audit.anchor events in chain order, from after_seq (exclusive) to up_to_seq (inclusive),
-- with the summary as Postgres prints it.
SELECT id::text AS id_text, chain_seq, summary::text AS summary_text
FROM install_audit_events
WHERE action = 'audit.anchor' AND chain_seq > @after_seq AND chain_seq <= @up_to_seq
ORDER BY chain_seq
LIMIT @page_size;

-- name: ListInstallAnchorHeadsInRange :many
-- The anchored heads of the anchors whose event has a chain_seq in [from_seq, up_to_seq], in anchor
-- order then org order: the install export's header (ADR 0100 decision 5).
SELECT h.anchor_event_id::text AS anchor_text, h.org_id::text AS org_text, h.chain_seq, h.hash
FROM install_audit_anchor_heads h
JOIN install_audit_events e ON e.id = h.anchor_event_id
WHERE e.chain_seq >= @from_seq AND e.chain_seq <= @up_to_seq
ORDER BY e.chain_seq, h.org_id;

-- name: CheckAnchoredHeads :many
-- One anchor's heads with the hash the org chain has at the anchored chain_seq (null when the row is
-- gone): the verifier compares the two (ADR 0100 decision 4.1).
SELECT h.org_id::text AS org_text, h.chain_seq, h.hash, e.hash AS org_hash
FROM install_audit_anchor_heads h
LEFT JOIN audit_events e ON e.org_id = h.org_id AND e.chain_seq = h.chain_seq
WHERE h.anchor_event_id = @anchor_event_id
ORDER BY h.org_id;

-- name: CheckOrgAnchoredHeads :many
-- One org's head in every anchor that names it, oldest anchor first, with the hash the org chain has at
-- the anchored chain_seq (null when the row is gone): the per-org verifier (verify --org) reads this
-- instead of every anchor's heads (ADR 0100 decision 4.1).
SELECT e.id::text AS anchor_text, e.chain_seq AS anchor_seq, h.chain_seq, h.hash, a.hash AS org_hash
FROM install_audit_anchor_heads h
JOIN install_audit_events e ON e.id = h.anchor_event_id
LEFT JOIN audit_events a ON a.org_id = h.org_id AND a.chain_seq = h.chain_seq
WHERE h.org_id = @org_id
ORDER BY e.chain_seq;
