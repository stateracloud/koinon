-- SPDX-License-Identifier: Apache-2.0
--
-- Run state machine (ADR 0030). Every transition is a compare-and-set on the
-- source state, and owner writes also on the epoch: a racing, illegal or stale
-- write matches no rows (pgx.ErrNoRows for :one). The runs_check_transition
-- trigger rejects anything else.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateRun :one
-- Queues a run of the agent's approved version. No row: the agent is not
-- active.
INSERT INTO runs (org_id, agent_id, agent_version_id, ticket_id, ready_at,
                  max_wall_clock_seconds, max_tokens)
SELECT a.org_id, a.id, a.active_version_id, @ticket_id, now(),
       @max_wall_clock_seconds, @max_tokens
FROM agents a
WHERE a.org_id = @org_id AND a.id = @agent_id AND a.status = 'active'
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs
WHERE org_id = @org_id AND id = @id;

-- name: ListRunsForTicket :many
SELECT * FROM runs
WHERE org_id = @org_id AND ticket_id = @ticket_id
ORDER BY created_at, id;

-- name: ClaimNextRun :one
-- queued -> starting, or a resolved parked -> starting. Takes the oldest
-- claimable run (by ready_at) of an active agent that has no active run and
-- whose ticket has no other owned or parked run, and
-- makes the caller its owner under a new epoch. Locking the agent row with
-- SKIP LOCKED serialises claims per agent, so concurrent claimers move on to
-- other agents, and FIFO holds per agent. runs_one_active_per_agent backstops
-- the rare race where two claims for one agent overlap; the loser gets a
-- unique violation and retries.
WITH next AS (
    SELECT r.id AS run_id, r.agent_id
    FROM runs r
    JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id
    WHERE r.org_id = @org_id
      AND r.ready_at IS NOT NULL
      AND a.status = 'active'
      AND NOT EXISTS (
          SELECT 1 FROM runs act
          WHERE act.org_id = r.org_id AND act.agent_id = r.agent_id
            AND act.state IN ('starting', 'running'))
      -- One harness session per ticket (ADR 0029): a ticket's next run waits
      -- while another run of the same ticket is owned or parked.
      AND NOT EXISTS (
          SELECT 1 FROM runs sib
          WHERE sib.org_id = r.org_id AND sib.ticket_id = r.ticket_id AND sib.id <> r.id
            AND sib.state IN ('starting', 'running', 'parked'))
    ORDER BY r.ready_at, r.id
    LIMIT 1
    FOR UPDATE OF a SKIP LOCKED
), agent AS (
    UPDATE agents
    SET epoch = agents.epoch + 1
    FROM next
    WHERE agents.org_id = @org_id AND agents.id = next.agent_id
    RETURNING agents.id, agents.epoch
)
UPDATE runs
SET state = 'starting',
    epoch = agent.epoch,
    owner_id = @owner_id::text,
    lease_expires_at = now() + make_interval(secs => @lease_seconds::integer),
    heartbeat_at = now(),
    ready_at = NULL,
    -- Parked time does not count against the wall-clock cap.
    parked_seconds = runs.parked_seconds + CASE
        WHEN runs.state = 'parked' THEN greatest(0, floor(extract(epoch FROM now() - runs.parked_at)))::bigint
        ELSE 0 END,
    park_reason = NULL,
    parked_at = NULL,
    park_expires_at = NULL,
    started_at = coalesce(runs.started_at, now())
FROM next
JOIN agent ON agent.id = next.agent_id
WHERE runs.org_id = @org_id AND runs.id = next.run_id
  AND runs.state IN ('queued', 'parked') AND runs.ready_at IS NOT NULL
RETURNING runs.*;

-- name: TakeOverExpiredRun :one
-- Takes ownership of a starting or running run whose owner's lease expired
-- (for example a killed agentd). The state is unchanged; the new epoch makes
-- every later write of the old owner match no rows.
WITH stale AS (
    SELECT r.id AS run_id, r.agent_id
    FROM runs r
    WHERE r.org_id = @org_id
      AND r.state IN ('starting', 'running')
      AND r.lease_expires_at < now()
    ORDER BY r.lease_expires_at, r.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), agent AS (
    UPDATE agents
    SET epoch = agents.epoch + 1
    FROM stale
    WHERE agents.org_id = @org_id AND agents.id = stale.agent_id
    RETURNING agents.id, agents.epoch
)
UPDATE runs
SET epoch = agent.epoch,
    owner_id = @owner_id::text,
    lease_expires_at = now() + make_interval(secs => @lease_seconds::integer),
    heartbeat_at = now()
FROM stale
JOIN agent ON agent.id = stale.agent_id
WHERE runs.org_id = @org_id AND runs.id = stale.run_id
RETURNING runs.*;

-- name: HeartbeatRun :one
-- Extends the owner's lease. No row: the caller is no longer the owner and
-- must stop (ADR 0030).
UPDATE runs
SET heartbeat_at = now(),
    lease_expires_at = now() + make_interval(secs => @lease_seconds::integer)
WHERE org_id = @org_id AND id = @id AND epoch = @epoch
  AND state IN ('starting', 'running')
RETURNING *;

-- name: MarkRunRunning :one
-- starting -> running once the harness session is ready.
UPDATE runs
SET state = 'running'
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'starting'
RETURNING *;

-- name: ParkRun :one
-- running -> parked. The owner releases the run; a later claim resumes it
-- after ResolveRunPark.
UPDATE runs
SET state = 'parked',
    park_reason = @park_reason::run_park_reason,
    parked_at = now(),
    park_expires_at = sqlc.narg(park_expires_at),
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'running'
RETURNING *;

-- name: ResolveRunPark :one
-- Makes a parked run claimable again (approval decided, input given). It
-- stays parked until an owner claims it: parked -> starting.
UPDATE runs
SET ready_at = now()
WHERE org_id = @org_id AND id = @id AND state = 'parked' AND ready_at IS NULL
RETURNING *;

-- name: FinishRun :one
-- The owner ends the run: running -> succeeded | failed | canceled, or
-- starting -> failed | canceled. No row: stale epoch or illegal transition.
UPDATE runs
SET state = @outcome::run_state,
    outcome_reason = sqlc.narg(outcome_reason),
    finished_at = now(),
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND id = @id AND epoch = @epoch
  AND ((state = 'running' AND @outcome::run_state IN ('succeeded', 'failed', 'canceled'))
       OR (state = 'starting' AND @outcome::run_state IN ('failed', 'canceled')))
RETURNING *;

-- name: CancelRun :one
-- Control-plane cancel from any non-terminal state (user cancel, run caps,
-- park expiry). Needs no epoch; an owner's next write then matches no rows
-- because the state changed.
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND id = @id
  AND state IN ('queued', 'starting', 'running', 'parked')
RETURNING *;

-- name: CancelQueuedRunsForTicket :many
-- A ticket moved to Done cancels its queued runs (plan item A4).
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL
WHERE org_id = @org_id AND ticket_id = @ticket_id AND state = 'queued'
RETURNING *;

-- name: HasQueuedRunForTicket :one
-- Whether the ticket already has a queued run, which will see every message
-- appended before it starts (plan item A4).
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE org_id = @org_id AND ticket_id = @ticket_id AND state = 'queued');

-- name: CancelOpenRunsForAgent :many
-- Archiving an agent cancels every run it has not finished.
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND agent_id = @agent_id
  AND state IN ('queued', 'starting', 'running', 'parked')
RETURNING *;

-- name: CancelRunsOverWallClock :many
-- Cancels owned runs that have been owned longer than their wall-clock cap
-- (plan item A4). Parked time is excluded through parked_seconds. Safe to run
-- from every API replica: a run canceled by one sweep no longer matches.
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id
  AND state IN ('starting', 'running')
  AND started_at + make_interval(secs => max_wall_clock_seconds + parked_seconds) < now()
RETURNING *;

-- name: AddRunTokens :one
-- Adds reported token usage to an unfinished run. The caller cancels the run
-- when tokens_used reaches max_tokens. No row: the run is finished or absent.
UPDATE runs
SET tokens_used = tokens_used + @tokens::bigint
WHERE org_id = @org_id AND id = @id
  AND state IN ('queued', 'starting', 'running', 'parked')
RETURNING *;
