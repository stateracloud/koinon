-- SPDX-License-Identifier: Apache-2.0
--
-- A stuck sandbox, the work saved from it, and the end-of-turn summary and fresh-session
-- handoff (plan item AG1; ADRs 0051, 0054).
--
-- Every query on an org-owned table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: MarkSandboxUnhealthy :one
-- agentd found the sandbox stuck: the reconciler recreates it. The mark clears when the
-- reconciler records another home generation.
UPDATE agent_sandboxes
SET unhealthy_at = now(), unhealthy_reason = @reason::text, unhealthy_generation = coalesce(nullif(@generation::text, ''), home_generation, '')
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: CountRecentSandboxRecreations :one
-- Recreations for stuck sandboxes in the last 24 hours, since the agent was last resumed.
SELECT count(*)::bigint FROM sandbox_incidents i
JOIN agents a ON a.org_id = i.org_id AND a.id = i.agent_id
WHERE i.org_id = @org_id AND i.agent_id = @agent_id AND i.outcome = 'recreate'
  AND i.detected_at > now() - interval '24 hours'
  AND (a.resumed_at IS NULL OR i.detected_at > a.resumed_at);

-- name: InsertSandboxIncident :one
INSERT INTO sandbox_incidents (org_id, agent_id, run_id, ticket_id, processes, outcome, work, work_note, saved_work_id)
VALUES (@org_id, @agent_id, @run_id, @ticket_id, @processes, @outcome, @work, @work_note, sqlc.narg(saved_work_id))
RETURNING *;

-- name: MarkAgentUnhealthy :one
UPDATE agents
SET unhealthy_at = now(), unhealthy_reason = @reason::text
WHERE org_id = @org_id AND id = @id AND unhealthy_at IS NULL
RETURNING *;

-- name: ResumeAgent :one
-- Clears the unhealthy mark and starts a new count of recreations. No row: not unhealthy.
UPDATE agents
SET unhealthy_at = NULL, unhealthy_reason = NULL, resumed_at = now()
WHERE org_id = @org_id AND id = @id AND unhealthy_at IS NOT NULL
RETURNING *;

-- name: ListTicketsWithParkedRunOfAgent :many
-- Tickets whose run is parked in the agent's sandbox: they resume in the new sandbox.
SELECT DISTINCT ticket_id FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state = 'parked';

-- name: ListTicketsWithWaitingRunOfAgent :many
-- Tickets with a queued or parked run of the agent: they wait while the agent is unhealthy.
SELECT DISTINCT ticket_id FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state IN ('queued', 'parked');

-- name: InsertSavedWork :one
INSERT INTO saved_work (org_id, id, ticket_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
VALUES (@org_id, @id, @ticket_id, @run_id, @agent_id, @repo, @base_sha, @branch, @manifest, @size_bytes, @envelope, @expires_at)
RETURNING *;

-- name: GetSavedWork :one
SELECT * FROM saved_work
WHERE org_id = @org_id AND id = @id;

-- name: GetSavedWorkManifest :one
-- Everything but the sealed files.
SELECT org_id, id, ticket_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, status, created_at, expires_at
FROM saved_work
WHERE org_id = @org_id AND id = @id;

-- name: MarkSavedWorkPushed :execrows
-- The files are dropped once they are on GitHub.
UPDATE saved_work SET status = 'pushed', envelope = NULL
WHERE org_id = @org_id AND id = @id AND status = 'saved';

-- name: ExpireSavedWork :many
-- Saved work is kept for 14 days; the sealed files go with it.
UPDATE saved_work SET status = 'expired', envelope = NULL
WHERE org_id = @org_id AND status = 'saved' AND expires_at < now()
RETURNING org_id, id, ticket_id, run_id;

-- name: CreateSystemApprovalRequest :one
-- A request Koinon makes itself (saved work to push). No row: it exists.
INSERT INTO approval_requests (org_id, ticket_id, run_id, agent_id, connector, tool, target,
                               args_json, args_sha256, risk, decision, status, expires_at, system)
VALUES (@org_id, @ticket_id, @run_id, @agent_id, @connector, @tool, @target,
        @args_json, @args_sha256, '{}', 'ask', 'pending', @expires_at, true)
ON CONFLICT (org_id, run_id, tool, target, args_sha256) DO NOTHING
RETURNING *;

-- name: SetRunSummarizing :execrows
-- Fenced by the run's epoch.
UPDATE runs SET summarizing = @summarizing::boolean
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'running';

-- name: SetTicketSummary :execrows
-- Stores the agent's end-of-turn summary. Fenced by the run epoch like SetTicketHarnessSession.
UPDATE tickets t
SET agent_summary = @summary::text, agent_summary_at = now()
WHERE t.org_id = @org_id AND t.id = @id
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.org_id = @org_id AND r.ticket_id = t.id AND r.id = @run_id
        AND r.epoch = @epoch AND r.state IN ('starting', 'running'));

-- name: RequestSessionRestartForAgent :many
-- Activating a version with "apply to open tickets": every open ticket of the agent that has a
-- session starts a new one at its next run.
UPDATE tickets
SET session_restart_pending = true
WHERE org_id = @org_id AND agent_id = @agent_id AND status <> 'done'
  AND harness_session_id IS NOT NULL AND NOT session_restart_pending
RETURNING id;

-- name: ListRecentAgentReplies :many
-- The agent's latest replies on the ticket, newest first.
SELECT * FROM ticket_events
WHERE org_id = @org_id AND ticket_id = @ticket_id AND kind = 'message' AND actor_kind = 'agent'
ORDER BY seq DESC
LIMIT @max_rows;

-- name: InsertUploadNonce :execrows
-- Zero rows: the nonce was used before (a replay).
INSERT INTO saved_work_upload_nonces (org_id, run_id, nonce)
VALUES (@org_id, @run_id, @nonce)
ON CONFLICT DO NOTHING;

-- name: GetRunForUpload :one
-- The run a saved-work upload names, if it still owns the sandbox (starting or running) under that epoch.
SELECT * FROM runs
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state IN ('starting', 'running');

-- name: DeleteOldUploadNonces :execrows
-- A nonce only matters while its token could still be good (5 minutes); older ones go.
DELETE FROM saved_work_upload_nonces
WHERE org_id = @org_id AND created_at < now() - interval '10 minutes';
