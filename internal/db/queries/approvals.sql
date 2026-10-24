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
                               args_json, args_sha256, risk, decision, status, expires_at,
                               approvals_required, call_binding)
VALUES (@org_id, @ticket_id, @run_id, @agent_id, @connector, @tool, @target,
        @args_json, @args_sha256, @risk, @decision, @status, @expires_at,
        COALESCE(sqlc.narg(approvals_required)::smallint, 1), COALESCE(sqlc.narg(call_binding)::jsonb, '{}'))
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

-- name: GetApprovalRequest :one
SELECT * FROM approval_requests
WHERE org_id = @org_id AND id = @id;

-- name: LockApprovalRequest :one
-- Serialises decisions on one request.
SELECT * FROM approval_requests
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: ListPendingApprovalRequests :many
-- The org's requests waiting for a human (Waiting on you), oldest first.
-- Trusted-preset calls are recorded as approved and never listed here.
SELECT * FROM approval_requests
WHERE org_id = @org_id AND status = 'pending'
ORDER BY created_at, id;

-- name: DecideApprovalRequest :one
-- pending -> approved | denied by a human. No row: already decided.
UPDATE approval_requests
SET status = @status::approval_status,
    decided_by = @decided_by::uuid,
    decided_at = now(),
    decision_note = sqlc.narg(decision_note)
WHERE org_id = @org_id AND id = @id AND status = 'pending'
  AND @status::approval_status IN ('approved', 'denied')
RETURNING *;

-- name: ListExecutableApprovalRequests :many
-- Approved requests no executor holds: Trusted-preset calls, and calls whose
-- executor died or failed to record the outcome.
SELECT * FROM approval_requests
WHERE org_id = @org_id AND status = 'approved'
  AND (execution_lease_until IS NULL OR execution_lease_until < now())
ORDER BY updated_at, id
LIMIT @max_requests;

-- name: ClaimApprovalExecution :one
-- Takes an approved request for execution for lease_seconds. No row: it is
-- not approved, or another executor holds it.
UPDATE approval_requests
SET executions = executions + 1,
    execution_lease_until = now() + make_interval(secs => @lease_seconds::integer)
WHERE org_id = @org_id AND id = @id AND status = 'approved'
  AND (execution_lease_until IS NULL OR execution_lease_until < now())
RETURNING *;

-- name: CompleteApprovalExecution :one
-- approved -> executed | failed, by the executor that claimed it (fenced on
-- executions). No row: another executor took it over.
UPDATE approval_requests
SET status = @status::approval_status,
    result = sqlc.narg(result),
    error = sqlc.narg(error),
    executed_at = now(),
    execution_lease_until = NULL
WHERE org_id = @org_id AND id = @id AND status = 'approved' AND executions = @executions
  AND @status::approval_status IN ('executed', 'failed')
RETURNING *;

-- name: ExpirePendingApprovalRequests :many
-- pending -> expired once expires_at has passed (ADR 0049: 7 days).
UPDATE approval_requests
SET status = 'expired'
WHERE org_id = @org_id AND status = 'pending' AND expires_at < now()
RETURNING *;

-- name: CancelPendingApprovalRequestsForRun :many
-- pending -> canceled, for a run that finished without them (for example a
-- user canceled the parked run).
UPDATE approval_requests
SET status = 'canceled'
WHERE org_id = @org_id AND run_id = @run_id AND status = 'pending'
RETURNING *;

-- name: ListPendingApprovalRequestsOfFinishedRuns :many
SELECT a.* FROM approval_requests a
JOIN runs r ON r.org_id = a.org_id AND r.id = a.run_id
WHERE a.org_id = @org_id AND a.status = 'pending'
  AND r.state IN ('succeeded', 'failed', 'canceled')
ORDER BY a.created_at, a.id;

-- name: ListUndeliveredApprovalRequestsForRun :many
-- A run's requests whose outcome the agent has not been sent yet.
SELECT * FROM approval_requests
WHERE org_id = @org_id AND run_id = @run_id AND delivered_at IS NULL
ORDER BY created_at, id;

-- name: MarkApprovalRequestsDelivered :execrows
-- Records that the outcomes were sent to the agent.
UPDATE approval_requests
SET delivered_at = now()
WHERE org_id = @org_id AND id = ANY(@ids::uuid[]) AND delivered_at IS NULL
  AND status IN ('executed', 'failed', 'denied', 'expired', 'canceled');

-- name: SetApprovalFirstApprover :one
-- The first of two approvals: remembered, the request stays pending.
UPDATE approval_requests
SET first_approver = @approver::uuid
WHERE org_id = @org_id AND id = @id AND status = 'pending' AND first_approver IS NULL AND approvals_required = 2
RETURNING *;
