-- SPDX-License-Identifier: Apache-2.0
--
-- The write queries of the audit tables (ADR 0100 decision 1.7, #371). sqlc generates them into
-- internal/audit/internal/auditstore, which Go's internal rule makes unreachable outside
-- internal/audit: the audit batch (audit.InTx, audit.InPgTx, audit.WriteInTx) and audit.RecordAfter
-- are the only ways to write an audit event. The read queries stay in internal/db/queries/audit.sql.
-- chain_seq, prev_hash and hash are set by the tables' triggers, which also move the chain heads.
--
-- Every query on an org-owned table takes @org_id.

-- name: InsertAuditEvent :one
INSERT INTO audit_events (org_id, actor_kind, actor_id, action, target_kind, target_id, summary, request_id)
VALUES (@org_id, @actor_kind, @actor_id, @action, @target_kind, @target_id, @summary, @request_id)
RETURNING *;

-- name: InsertInstallAuditEvent :one
-- The install chain (ADR 0100): no org.
INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary, request_id)
VALUES (@actor_kind, @actor_id, @action, @target_kind, @target_id, @summary, @request_id)
RETURNING *;
