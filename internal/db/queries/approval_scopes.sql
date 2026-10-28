-- SPDX-License-Identifier: Apache-2.0
--
-- Scoped approvals (plan item AP1, ADR 0042 amendment).
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateApprovalScope :one
INSERT INTO approval_scopes (org_id, agent_id, connector, tool, target, kind, task_id, expires_at,
                             created_by, source_approval_id, call_binding)
VALUES (@org_id, @agent_id, @connector, @tool, @target, @kind, sqlc.narg(task_id), @expires_at,
        @created_by, @source_approval_id, sqlc.narg(call_binding))
RETURNING *;

-- name: FindActiveApprovalScope :one
-- The scope that allows this call now: exact on agent, connector, tool and target; a task scope
-- only for its task. A task scope wins over an agent scope (the narrower one is named). When
-- call_binding is given the scope's binding must equal it (a code-host scope covers only the
-- connection and base URL it was granted on).
SELECT * FROM approval_scopes
WHERE org_id = @org_id AND agent_id = @agent_id AND connector = @connector AND tool = @tool
  AND target = @target AND revoked_at IS NULL AND expires_at > @now::timestamptz
  AND (kind = 'agent' OR task_id = @task_id)
  AND (sqlc.narg(call_binding)::jsonb IS NULL OR call_binding = sqlc.narg(call_binding)::jsonb)
ORDER BY (kind = 'task') DESC, created_at, id
LIMIT 1;

-- name: GetApprovalScope :one
SELECT * FROM approval_scopes WHERE org_id = @org_id AND id = @id;

-- name: ListActiveApprovalScopesForAgent :many
SELECT * FROM approval_scopes
WHERE org_id = @org_id AND agent_id = @agent_id AND revoked_at IS NULL AND expires_at > @now::timestamptz
ORDER BY created_at, id;

-- name: RevokeApprovalScope :one
-- No row: unknown, or already revoked.
UPDATE approval_scopes SET revoked_at = now(), revoked_by = @revoked_by
WHERE org_id = @org_id AND id = @id AND revoked_at IS NULL
RETURNING *;

-- name: SetApprovalRequestScope :exec
UPDATE approval_requests SET scope_id = @scope_id WHERE org_id = @org_id AND id = @id;
