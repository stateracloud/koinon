-- SPDX-License-Identifier: Apache-2.0
--
-- Ask-tier approval requests (plan item C3, ADR 0042). C3 records them; P1
-- decides, executes and expires them.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: GetRunningRunForAgent :one
-- The run an agent's tool call belongs to: one active run per agent
-- (runs_one_active_per_agent), and tools are only called while it runs.
SELECT * FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state = 'running';

-- name: CreateApprovalRequest :one
-- No row: the same call was already recorded in this run (see
-- GetApprovalRequestByCall).
INSERT INTO approval_requests (org_id, ticket_id, run_id, agent_id, connector, tool, target,
                               args_json, args_sha256, risk, decision, status, expires_at)
VALUES (@org_id, @ticket_id, @run_id, @agent_id, @connector, @tool, @target,
        @args_json, @args_sha256, @risk, @decision, @status, @expires_at)
ON CONFLICT (org_id, run_id, tool, target, args_sha256) DO NOTHING
RETURNING *;

-- name: GetApprovalRequestByCall :one
SELECT * FROM approval_requests
WHERE org_id = @org_id AND run_id = @run_id AND tool = @tool AND target = @target
  AND args_sha256 = @args_sha256;

-- name: GetApprovalRequestForAgent :one
-- An agent may only read its own requests.
SELECT * FROM approval_requests
WHERE org_id = @org_id AND id = @id AND agent_id = @agent_id;

-- name: ListApprovalRequestsForTicket :many
SELECT * FROM approval_requests
WHERE org_id = @org_id AND ticket_id = @ticket_id
ORDER BY created_at, id;
