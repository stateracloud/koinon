-- SPDX-License-Identifier: Apache-2.0
--
-- Home checkpoints (plan item HC1; ADR 0032): the kopia repository per agent, the checkpoint
-- records koinon-agentd's writer works through, and the restore a recreated sandbox waits for.
--
-- Every query on an org-owned table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: GetHomeRepository :one
SELECT * FROM home_repositories
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: InsertHomeRepository :one
-- No row: another writer created it first; read it with GetHomeRepository.
INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope)
VALUES (@org_id, @agent_id, @prefix, @password_envelope)
ON CONFLICT (org_id, agent_id) DO NOTHING
RETURNING *;

-- name: RequestHomeCheckpoint :one
-- No row: the agent already has an open request (home_checkpoints_open); read it with
-- GetOpenHomeCheckpoint.
INSERT INTO home_checkpoints (org_id, agent_id, reason, sandbox_id, generation)
VALUES (@org_id, @agent_id, @reason::checkpoint_reason, @sandbox_id, @generation)
ON CONFLICT (org_id, agent_id) WHERE status IN ('requested', 'running') DO NOTHING
RETURNING *;

-- name: GetOpenHomeCheckpoint :one
SELECT * FROM home_checkpoints
WHERE org_id = @org_id AND agent_id = @agent_id AND status IN ('requested', 'running');

-- name: GetHomeCheckpoint :one
SELECT * FROM home_checkpoints
WHERE org_id = @org_id AND id = @id;

-- name: LatestHomeCheckpointOfSandbox :one
-- The newest checkpoint of any reason taken for this sandbox. The reconciler's recreate gate
-- reads it: a complete one newer than the sandbox's last activity lets the recreation go on, a
-- failed one blocks it.
SELECT * FROM home_checkpoints
WHERE org_id = @org_id AND agent_id = @agent_id AND sandbox_id = @sandbox_id
ORDER BY requested_at DESC, id DESC
LIMIT 1;

-- name: LatestCompleteHomeCheckpointOfSandbox :one
-- The newest complete checkpoint (hourly, stop or recreate) taken for this sandbox. The recreate
-- gate uses it for a sandbox that cannot be read (phase Error or Completed), which no new
-- checkpoint can be taken of.
SELECT * FROM home_checkpoints
WHERE org_id = @org_id AND agent_id = @agent_id AND sandbox_id = @sandbox_id AND status = 'complete'
ORDER BY finished_at DESC, id DESC
LIMIT 1;

-- name: ListCompleteHomeCheckpoints :many
-- Newest first, for the restore. A checkpoint from a higher ownership epoch than any run of the
-- agent has seen is not restored (ADR 0032).
SELECT c.* FROM home_checkpoints c
WHERE c.org_id = @org_id AND c.agent_id = @agent_id AND c.status = 'complete'
  AND c.epoch <= coalesce((SELECT max(r.epoch) FROM runs r WHERE r.org_id = c.org_id AND r.agent_id = c.agent_id), 0)
ORDER BY c.finished_at DESC, c.id DESC
LIMIT @max_rows::int;

-- name: ClaimHomeCheckpoint :one
-- The writer's next request: a requested one that is due, or a running one whose writer died
-- (started more than @lease_seconds ago), for an agent whose sandbox is ready. No row: nothing to
-- do.
UPDATE home_checkpoints AS h
SET status = 'running', started_at = now(), attempts = h.attempts + 1
WHERE h.org_id = @org_id AND (h.org_id, h.id) = (
    SELECT c.org_id, c.id FROM home_checkpoints c
    WHERE c.org_id = @org_id
      AND ((c.status = 'requested' AND c.requested_at <= now())
           OR (c.status = 'running' AND c.started_at < now() - make_interval(secs => @lease_seconds::int)))
      -- The writer reads the home over exec, so the sandbox must be ready.
      AND EXISTS (SELECT 1 FROM agent_sandboxes s
                  WHERE s.org_id = @org_id AND s.agent_id = c.agent_id AND s.state = 'ready')
    ORDER BY c.requested_at, c.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1)
RETURNING *;

-- name: CompleteHomeCheckpoint :one
UPDATE home_checkpoints
SET status = 'complete', snapshot_id = @snapshot_id, size_bytes = @size_bytes, epoch = @epoch,
    generation = @generation, error = NULL, finished_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'running'
RETURNING *;

-- name: ReleaseHomeCheckpoint :one
-- A failed attempt that will be tried again after @delay_seconds.
UPDATE home_checkpoints
SET status = 'requested', error = @error, requested_at = now() + make_interval(secs => @delay_seconds::int)
WHERE org_id = @org_id AND id = @id AND status = 'running'
RETURNING *;

-- name: FailHomeCheckpoint :one
UPDATE home_checkpoints
SET status = 'failed', error = @error, finished_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('requested', 'running')
RETURNING *;

-- name: RequestHourlyHomeCheckpoints :many
-- Ephemeral agents have no home that outlives a run and are not checkpointed (ADR 0065 13c).
-- One request per agent whose sandbox is ready, has no restore pending (an empty home must not
-- become the newest checkpoint) and has had no checkpoint requested for @interval_seconds.
INSERT INTO home_checkpoints (org_id, agent_id, reason, sandbox_id, generation)
SELECT s.org_id, s.agent_id, 'hourly', coalesce(s.sandbox_id, ''), coalesce(s.home_generation, '')
FROM agent_sandboxes s
JOIN agents a ON a.org_id = s.org_id AND a.id = s.agent_id
LEFT JOIN agent_versions v ON v.org_id = a.org_id AND v.id = a.active_version_id
WHERE s.org_id = @org_id AND s.state = 'ready' AND s.unhealthy_at IS NULL
  AND s.restore_checkpoint_id IS NULL AND a.status = 'active'
  AND (v.config->>'ephemeral')::boolean IS NOT TRUE
  AND NOT EXISTS (
      SELECT 1 FROM home_checkpoints c
      WHERE c.org_id = s.org_id AND c.agent_id = s.agent_id
        AND (c.status IN ('requested', 'running')
             OR c.requested_at > now() - make_interval(secs => @interval_seconds::int)))
ON CONFLICT (org_id, agent_id) WHERE status IN ('requested', 'running') DO NOTHING
RETURNING *;

-- name: MaxAgentEpoch :one
SELECT coalesce(max(epoch), 0)::bigint FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: SetSandboxRestore :one
-- The sandbox is being deleted for a recreation whose home is in this checkpoint: the next
-- sandbox keeps the home generation and the first run restores it (RecordAgentSandbox).
UPDATE agent_sandboxes
SET restore_checkpoint_id = @checkpoint_id, restore_claimed_at = NULL
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: ClaimSandboxRestore :one
-- One run restores at a time; a claim older than @lease_seconds is taken over. No row: nothing
-- to restore, or another run is restoring.
UPDATE agent_sandboxes
SET restore_claimed_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND restore_checkpoint_id IS NOT NULL
  AND (restore_claimed_at IS NULL OR restore_claimed_at < now() - make_interval(secs => @lease_seconds::int))
RETURNING *;

-- name: ReleaseSandboxRestore :exec
UPDATE agent_sandboxes SET restore_claimed_at = NULL
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: ClearSandboxRestore :exec
UPDATE agent_sandboxes SET restore_checkpoint_id = NULL, restore_claimed_at = NULL
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: FailStaleHomeCheckpoints :many
-- Requests that never started because the sandbox was not ready for the configured wait.
UPDATE home_checkpoints
SET status = 'failed', finished_at = now(),
    error = @error
WHERE org_id = @org_id AND status = 'requested' AND attempts = 0
  AND requested_at < now() - make_interval(secs => @wait_seconds::int)
RETURNING *;
