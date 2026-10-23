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
