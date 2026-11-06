-- SPDX-License-Identifier: Apache-2.0
--
-- Home checkpoints (plan item HC1; ADR 0032): the kopia repositories of each agent (one per
-- repository epoch, ADR 0085 decision 13), the checkpoint records koinon-agentd's writer works
-- through, and the restore a recreated sandbox waits for.
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

-- name: LockHomeRepository :one
-- Serializes the start of a repository epoch for the agent.
SELECT * FROM home_repositories
WHERE org_id = @org_id AND agent_id = @agent_id
FOR UPDATE;

-- name: GetHomeRepositoryEpoch :one
SELECT * FROM home_repository_epochs
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch;

-- name: LatestHomeRepositoryEpoch :one
-- The agent's current repository when it has epochs; no row: epoch 0 (home_repositories) is current.
SELECT * FROM home_repository_epochs
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY repository_epoch DESC
LIMIT 1;

-- name: InsertHomeRepositoryEpoch :one
INSERT INTO home_repository_epochs (org_id, agent_id, repository_epoch, prefix, password_envelope, reason)
VALUES (@org_id, @agent_id, @repository_epoch, @prefix, @password_envelope, @reason)
RETURNING *;

-- name: RetireHomeRepository :execrows
-- Epoch 0 was superseded: its blobs may be deleted after @delete_after.
UPDATE home_repositories
SET retired_at = now(), delete_after = @delete_after
WHERE org_id = @org_id AND agent_id = @agent_id AND retired_at IS NULL;

-- name: RetireHomeRepositoryEpoch :execrows
UPDATE home_repository_epochs
SET retired_at = now(), delete_after = @delete_after
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch AND retired_at IS NULL;

-- name: ListDueHomeRepositories :many
-- Retired repositories whose blobs may now be deleted (the sweeper's work), epoch 0 and later,
-- longest overdue first so a row that keeps failing does not hold back the others. A repository past
-- its deadline is kept while it is the agent's newest restorable home: it goes only once its
-- password is lost (nothing restores from it any more) or a later epoch holds a complete checkpoint
-- (a new epoch starts empty, so until its first backup the retired one is all there is). That later
-- epoch must itself still restore: neither lost nor deleted (a later epoch is always a
-- home_repository_epochs row, since epoch 0 is the first). Its checkpoint must also be at least as
-- new as the retired one's newest: a backup that was running when the epoch changed can finish into
-- the retired repository after a seed (#86), and that home is then the newest. And no repository
-- of the agent goes while one of its checkpoints runs: that backup may have opened the retired
-- repository before the new epoch started, and its snapshot would be written after the delete
-- (lost to the restore, and left in the bucket under the retired password). A running checkpoint
-- ends within its lease.
SELECT agent_id, repository_epoch, prefix FROM (
    SELECT h.agent_id, 0::integer AS repository_epoch, h.prefix, h.delete_after, h.credential_lost_at
    FROM home_repositories h
    WHERE h.org_id = @org_id AND h.deleted_at IS NULL AND h.delete_after <= now()
    UNION ALL
    SELECT e.agent_id, e.repository_epoch, e.prefix, e.delete_after, e.credential_lost_at
    FROM home_repository_epochs e
    WHERE e.org_id = @org_id AND e.deleted_at IS NULL AND e.delete_after <= now()
) due
WHERE NOT EXISTS (SELECT 1 FROM home_checkpoints r
                  WHERE r.org_id = @org_id AND r.agent_id = due.agent_id AND r.status = 'running')
  AND (due.credential_lost_at IS NOT NULL
   OR EXISTS (SELECT 1 FROM home_checkpoints c
              JOIN home_repository_epochs later
                ON later.org_id = c.org_id AND later.agent_id = c.agent_id AND later.repository_epoch = c.repository_epoch
              WHERE c.org_id = @org_id AND c.agent_id = due.agent_id AND c.status = 'complete'
                AND c.repository_epoch > due.repository_epoch
                AND later.credential_lost_at IS NULL AND later.deleted_at IS NULL
                AND c.finished_at >= coalesce(
                    (SELECT max(d.finished_at) FROM home_checkpoints d
                     WHERE d.org_id = @org_id AND d.agent_id = due.agent_id
                       AND d.repository_epoch = due.repository_epoch AND d.status = 'complete'),
                    '-infinity'::timestamptz)))
ORDER BY due.delete_after, due.agent_id, due.repository_epoch
LIMIT @max_rows::int;

-- name: RequestHomeRepositorySeed :execrows
-- The new epoch is to hold the agent's newest home (keys new-repositories, #86): koinon-agentd
-- copies it there from @seed_from_epoch or older.
UPDATE home_repository_epochs
SET seed_from_epoch = @seed_from_epoch, seed_after = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch
  AND reason = 'key_compromised' AND seed_from_epoch IS NULL;

-- name: ClaimHomeRepositorySeed :one
-- The next seed due in the org, held for @lease_seconds: a writer that dies leaves it to another
-- after the lease. A superseded epoch (retired: a later one started) is not seeded; the later
-- one is. No row: nothing to do.
UPDATE home_repository_epochs AS e
SET seed_after = now() + make_interval(secs => @lease_seconds::int), seed_attempts = e.seed_attempts + 1
WHERE e.org_id = @org_id AND (e.org_id, e.agent_id, e.repository_epoch) = (
    SELECT s.org_id, s.agent_id, s.repository_epoch FROM home_repository_epochs s
    WHERE s.org_id = @org_id AND s.seed_from_epoch IS NOT NULL AND s.seeded_at IS NULL
      AND s.seed_failed_at IS NULL AND s.seed_after <= now() AND s.retired_at IS NULL
      AND s.credential_lost_at IS NULL
    ORDER BY s.seed_after, s.agent_id
    FOR UPDATE SKIP LOCKED
    LIMIT 1)
RETURNING *;

-- name: CompleteHomeRepositorySeed :execrows
UPDATE home_repository_epochs
SET seeded_at = now(), seed_error = NULL
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch
  AND seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND seed_failed_at IS NULL AND retired_at IS NULL;

-- name: ReleaseHomeRepositorySeed :execrows
-- A failed try, tried again after @delay_seconds.
UPDATE home_repository_epochs
SET seed_error = @error, seed_after = now() + make_interval(secs => @delay_seconds::int)
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch
  AND seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND seed_failed_at IS NULL;

-- name: FailHomeRepositorySeed :execrows
-- Given up: the retired epoch keeps the home (the sweeper's guard) until another epoch starts.
UPDATE home_repository_epochs
SET seed_failed_at = now(), seed_error = @error
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch
  AND seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND seed_failed_at IS NULL;

-- name: CountHomeRepositorySeeds :one
-- The org's seeds still to do and those given up on, for koinon-agentd's gauge. A superseded epoch
-- is neither.
SELECT count(*) FILTER (WHERE seed_failed_at IS NULL)::bigint AS pending,
       count(*) FILTER (WHERE seed_failed_at IS NOT NULL)::bigint AS failed
FROM home_repository_epochs
WHERE org_id = @org_id AND seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND retired_at IS NULL
  AND credential_lost_at IS NULL;

-- name: InsertSeededHomeCheckpoint :one
-- The copy of checkpoint @source_id in the new epoch's repository: the same home, taken when the
-- source was, so it sorts with it (and just before it: a newer id) and the recreate gate reads the
-- same times. No row: the source is gone or not complete.
INSERT INTO home_checkpoints (org_id, agent_id, reason, status, sandbox_id, generation, epoch, snapshot_id,
                              size_bytes, requested_at, started_at, finished_at, repository_epoch)
SELECT c.org_id, c.agent_id, c.reason, 'complete', c.sandbox_id, c.generation, c.epoch, @snapshot_id,
       @size_bytes, c.requested_at, c.started_at, c.finished_at, @repository_epoch
FROM home_checkpoints c
WHERE c.org_id = @org_id AND c.id = @source_id AND c.status = 'complete'
RETURNING *;

-- name: MarkHomeRepositoryDeleted :execrows
UPDATE home_repositories SET deleted_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND deleted_at IS NULL AND delete_after <= now();

-- name: MarkHomeRepositoryEpochDeleted :execrows
UPDATE home_repository_epochs SET deleted_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND repository_epoch = @repository_epoch
  AND deleted_at IS NULL AND delete_after <= now();

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

-- name: LatestCompleteHomeCheckpointOfGeneration :one
-- The newest complete checkpoint of the home's lineage, whichever sandbox took it. A sandbox no
-- harness ever ran in (home_dirtied_at NULL) holds exactly what this checkpoint holds, so it is
-- the restore of a recreation or archive of that sandbox.
SELECT * FROM home_checkpoints
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation AND status = 'complete'
ORDER BY finished_at DESC, id DESC
LIMIT 1;

-- name: ListCompleteHomeCheckpoints :many
-- Newest first, for the restore. A checkpoint from a higher ownership epoch than any run of the
-- agent has seen is not restored (ADR 0032).
SELECT c.* FROM home_checkpoints c
WHERE c.org_id = @org_id AND c.agent_id = @agent_id AND c.status = 'complete'
  AND c.epoch <= coalesce((SELECT max(r.epoch) FROM runs r WHERE r.org_id = c.org_id AND r.agent_id = c.agent_id), 0)
  -- A checkpoint whose repository was deleted is gone.
  AND NOT EXISTS (SELECT 1 FROM home_repositories h
                  WHERE h.org_id = c.org_id AND h.agent_id = c.agent_id AND c.repository_epoch = 0 AND h.deleted_at IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM home_repository_epochs e
                  WHERE e.org_id = c.org_id AND e.agent_id = c.agent_id AND e.repository_epoch = c.repository_epoch
                    AND e.deleted_at IS NOT NULL)
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
    generation = @generation, repository_epoch = @repository_epoch, error = NULL, finished_at = now()
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
