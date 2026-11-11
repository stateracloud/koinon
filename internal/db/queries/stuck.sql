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
SET unhealthy_at = now(), unhealthy_reason = @reason::text, unhealthy_generation = coalesce(nullif(@generation::text, ''), sandbox_id, home_generation, '')
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: CountRecentSandboxRecreations :one
-- Recreations in the last 24 hours, since the agent was last resumed: ADR 0054's loop guard,
-- shared by agentd (stuck sandboxes) and the reconciler (sandboxes in Error or Completed). A
-- stuck sandbox counts from its incident, which agentd records before the reconciler recreates
-- it; one in Error or Completed counts from the audited sandbox.recreate event that carries a
-- phase. The audit event of a stuck recreation has none, so nothing is counted twice.
SELECT (
    (SELECT count(*) FROM sandbox_incidents i
     WHERE i.org_id = a.org_id AND i.agent_id = a.id AND i.outcome = 'recreate'
       AND i.detected_at > now() - interval '24 hours'
       AND (a.resumed_at IS NULL OR i.detected_at > a.resumed_at))
    +
    (SELECT count(*) FROM audit_events e
     WHERE e.org_id = a.org_id AND e.action = 'sandbox.recreate'
       AND e.target_kind = 'agent' AND e.target_id = a.id::text
       AND e.summary -> 'after' ? 'phase'
       AND e.created_at > now() - interval '24 hours'
       AND (a.resumed_at IS NULL OR e.created_at > a.resumed_at))
)::bigint
FROM agents a
WHERE a.org_id = @org_id AND a.id = @agent_id;

-- name: InsertSandboxIncident :one
INSERT INTO sandbox_incidents (org_id, agent_id, run_id, task_id, processes, outcome, work, work_note, saved_work_id)
VALUES (@org_id, @agent_id, @run_id, @task_id, @processes, @outcome, @work, @work_note, sqlc.narg(saved_work_id))
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

-- name: ListTasksWithParkedRunOfAgent :many
-- Tasks whose run is parked in the agent's sandbox: they resume in the new sandbox.
SELECT DISTINCT task_id FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state = 'parked';

-- name: ListTasksWithWaitingRunOfAgent :many
-- Tasks with a queued or parked run of the agent: they wait while the agent is unhealthy.
SELECT DISTINCT task_id FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state IN ('queued', 'parked');

-- name: InsertSavedWork :one
INSERT INTO saved_work (org_id, id, task_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, envelope, expires_at)
VALUES (@org_id, @id, @task_id, @run_id, @agent_id, @repo, @base_sha, @branch, @manifest, @size_bytes, @envelope, @expires_at)
RETURNING *;

-- name: GetSavedWork :one
SELECT * FROM saved_work
WHERE org_id = @org_id AND id = @id;

-- name: GetSavedWorkManifest :one
-- Everything but the sealed files.
SELECT org_id, id, task_id, run_id, agent_id, repo, base_sha, branch, manifest, size_bytes, status, created_at, expires_at
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
RETURNING org_id, id, task_id, run_id;

-- name: CreateSystemApprovalRequest :one
-- A request Koinon makes itself (saved work to push). No row: it exists.
INSERT INTO approval_requests (org_id, task_id, run_id, agent_id, connector, tool, target,
                               args_json, args_sha256, risk, decision, status, expires_at, system)
VALUES (@org_id, @task_id, @run_id, @agent_id, @connector, @tool, @target,
        @args_json, @args_sha256, '{}', 'ask', 'pending', @expires_at, true)
ON CONFLICT (org_id, run_id, tool, target, args_sha256) DO NOTHING
RETURNING *;

-- name: SetRunSummarizing :execrows
-- Fenced by the run's epoch.
UPDATE runs SET summarizing = @summarizing::boolean
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'running';

-- name: SetTaskSummary :execrows
-- Stores the agent's end-of-turn summary. Fenced by the run epoch like SetTaskHarnessSession.
UPDATE tasks t
SET agent_summary = @summary::text, agent_summary_at = now()
WHERE t.org_id = @org_id AND t.id = @id
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.org_id = @org_id AND r.task_id = t.id AND r.id = @run_id
        AND r.epoch = @epoch AND r.state IN ('starting', 'running'));

-- name: RequestSessionRestartForAgent :many
-- Activating a version with "apply to open tasks": every open task of the agent that has a
-- session starts a new one at its next run. A task a run completed counts as open: a chat thread is
-- completed by each turn's run and reopened by the next message, so it is still in use. Its last
-- status.changed event tells it apart from one a person completed: the system's, with the reason
-- run_succeeded (runoutcome.ReasonRunSucceeded). A person's own change, a later reopen or no event at
-- all leaves a completed task alone.
UPDATE tasks t
SET session_restart_pending = true
WHERE t.org_id = @org_id AND t.agent_id = @agent_id
  AND t.harness_session_id IS NOT NULL AND NOT t.session_restart_pending
  AND (t.state NOT IN ('completed', 'canceled', 'failed')
    OR (t.state = 'completed' AND EXISTS (
      SELECT 1 FROM (
        SELECT e.actor_kind, e.payload FROM task_events e
        WHERE e.org_id = t.org_id AND e.task_id = t.id AND e.kind = 'status.changed'
        ORDER BY e.seq DESC
        LIMIT 1
      ) last
      WHERE last.actor_kind = 'system' AND last.payload ->> 'reason' = 'run_succeeded')))
RETURNING t.id;

-- name: ListRecentAgentReplies :many
-- The agent's latest replies on the task, newest first.
SELECT * FROM task_events
WHERE org_id = @org_id AND task_id = @task_id AND kind = 'message' AND actor_kind = 'agent'
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
