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
-- origin is interactive unless the caller says scheduled (LT1: interactive work is claimed first).
INSERT INTO runs (org_id, agent_id, agent_version_id, task_id, ready_at,
                  max_wall_clock_seconds, max_model_requests, max_spend_usd, origin)
SELECT a.org_id, a.id, a.active_version_id, @task_id, now(),
       @max_wall_clock_seconds, sqlc.narg(max_model_requests), (sqlc.narg(max_spend_usd)::text)::numeric,
       coalesce(sqlc.narg(origin)::run_origin, 'interactive')
FROM agents a
WHERE a.org_id = @org_id AND a.id = @agent_id AND a.status = 'active'
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs
WHERE org_id = @org_id AND id = @id;

-- name: ListRunsForTask :many
SELECT * FROM runs
WHERE org_id = @org_id AND task_id = @task_id
ORDER BY created_at, id;

-- name: ClaimNextRun :one
-- queued -> starting, or a resolved parked -> starting. Takes the oldest
-- claimable run (by ready_at) of an active agent that has no active run and
-- whose task has no other owned or parked run, and
-- makes the caller its owner under a new epoch. Locking the agent row with
-- SKIP LOCKED serialises claims per agent, so concurrent claimers move on to
-- other agents, and FIFO holds per agent. runs_one_active_per_agent backstops
-- the rare race where two claims for one agent overlap; the loser gets a
-- unique violation and retries.
--
-- The run row is locked too. A claim whose snapshot predates a concurrent
-- winner's commit, but which locks the agent row after it, re-checks only the
-- rows it locks against their latest version. Were the run not among them, the
-- claim would still see it as claimable, bump the agent's epoch, and then
-- update no run: the next owner would get a skipped epoch.
WITH next AS (
    SELECT r.id AS run_id, r.agent_id
    FROM runs r
    JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id
    WHERE r.org_id = @org_id
      AND r.ready_at IS NOT NULL
      -- Active and healthy are repeated inline on the row locked below: after waiting for a lock,
      -- Postgres re-checks the quals on the locked rows against their latest version (EvalPlanQual),
      -- while run_claimable reads its own snapshot and would not see a concurrent change.
      AND a.status = 'active'
      AND a.unhealthy_at IS NULL
      -- The full claim conditions live in run_claimable, which GetAgentRunActivity shares for
      -- has_demand. It is evaluated per candidate run, in ready_at order, as the planner reaches it.
      AND run_claimable(r.org_id, r.id)
    -- Interactive work before scheduled work (LT1), then FIFO.
    ORDER BY (r.origin = 'scheduled'), r.ready_at, r.id
    LIMIT 1
    FOR UPDATE OF a, r SKIP LOCKED
), agent AS (
    UPDATE agents
    SET epoch = agents.epoch + 1
    FROM next
    WHERE agents.org_id = @org_id AND agents.id = next.agent_id
    RETURNING agents.id, agents.epoch
)
UPDATE runs
SET state = 'starting',
    summarizing = false, -- a summary left by a lost owner must not silence the tools (ADR 0051)
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
    started_at = coalesce(runs.started_at, now()),
    claimed_at = now()
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
    summarizing = false,
    takeovers = runs.takeovers + 1,
    owner_id = @owner_id::text,
    -- A starting run's claim time is when this owner began waiting for the sandbox, so a later
    -- requeue does not credit the dead owner's lease time to parked_seconds.
    claimed_at = CASE WHEN runs.state = 'starting' THEN now() ELSE runs.claimed_at END,
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

-- name: RequeueRun :one
-- starting -> queued, for the owner (epoch compare-and-set): the sandbox was not ready within the
-- org's wait (LT1, ADR 0066), so the run waits in the queue instead of failing. It goes to the back
-- of its origin's queue (ready_at = now) and the next claim gives it a new epoch.
-- sandbox_wait_started_at is kept, so the total wait is capped (ADR 0066 amendment
-- 2026-10-28). The time since the
-- claim was spent waiting for the sandbox, so it is credited to parked_seconds and does not count
-- against the wall-clock cap.
--
-- One queued run per task (runs_one_queued_per_task, issue 678). When another run of the task is
-- queued already (a message arrived while this run was starting, and queued a run that will see
-- every message), this run does not wait a second time: it is canceled as superseded, which is an
-- end state for it and not an error. The caller decides once (HasQueuedRunForTask, under the task
-- row lock, so that no enqueue slips in between the check and the write) and passes the result as
-- @superseded; the index is the backstop.
UPDATE runs
SET state = CASE WHEN @superseded::boolean THEN 'canceled'::run_state ELSE 'queued'::run_state END,
    parked_seconds = parked_seconds + CASE
        WHEN claimed_at IS NULL THEN 0
        ELSE greatest(0, floor(extract(epoch FROM now() - claimed_at)))::bigint END,
    claimed_at = NULL,
    ready_at = CASE WHEN @superseded::boolean THEN NULL ELSE now() END,
    outcome_reason = CASE WHEN @superseded::boolean THEN 'superseded by a queued run of the task' ELSE outcome_reason END,
    finished_at = CASE WHEN @superseded::boolean THEN now() ELSE finished_at END,
    owner_id = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'starting'
RETURNING *;

-- name: MarkRunSandboxWait :one
-- Records when the run started waiting for its sandbox, once per wait (the first requeue keeps it).
-- Fenced by the epoch. Returns the recorded start.
UPDATE runs
SET sandbox_wait_started_at = coalesce(sandbox_wait_started_at, @started::timestamptz)
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state IN ('starting', 'running')
RETURNING sandbox_wait_started_at;

-- name: CarrySandboxWaitToQueued :execrows
-- A run that ended as superseded (RequeueRun, issue 678) hands its sandbox wait start to the queued
-- run of the task, which takes over its place in the queue. The earlier start wins, so the total
-- wait stays capped (ADR 0066 amendment 2026-10-28: kept across requeues).
-- Skips a queued run that another transaction holds (SKIP LOCKED): the caller holds the task row
-- lock, and whoever holds the queued run's lock holds no task lock (the task-first paths wait on
-- ours), so it is a cancel or a claim of that run. A claim in that moment means the carry is
-- skipped and the claimed run starts a new wait clock. That is rare and accepted: waiting for the
-- lock here would put a queued-run lock after the task lock, the order that deadlocks (issue 1015).
WITH queued AS (
    SELECT r.id FROM runs r
    WHERE r.org_id = @org_id AND r.task_id = @task_id AND r.id <> @id AND r.state = 'queued'
    FOR UPDATE OF r SKIP LOCKED
)
UPDATE runs
SET sandbox_wait_started_at = least(coalesce(runs.sandbox_wait_started_at, @started::timestamptz), @started::timestamptz)
FROM queued
WHERE runs.org_id = @org_id AND runs.id = queued.id;

-- name: ClearSandboxWaitForAgent :execrows
-- The agent's sandbox was found ready (by any of its runs), or an admin resumed the agent: every
-- run of the agent starts a fresh wait the next time it finds the sandbox not ready. Uses
-- runs_sandbox_waiting. Locks the runs in id order (see CancelOpenRunsForAgent).
WITH locked AS (
    SELECT r.id FROM runs r
    WHERE r.org_id = @org_id AND r.agent_id = @agent_id AND r.sandbox_wait_started_at IS NOT NULL
    ORDER BY r.id
    FOR UPDATE
)
UPDATE runs SET sandbox_wait_started_at = NULL
FROM locked
WHERE runs.org_id = @org_id AND runs.id = locked.id;

-- name: MarkRunWrapUpNotice :execrows
-- Disarms the wrap-up notice (sent, or suppressed because the harness ignored the cancel); once per
-- run. Only a sent notice has a run.wrap_up_notice event. Fenced by the epoch.
UPDATE runs SET wrap_up_notice_at = now()
WHERE org_id = @org_id AND id = @id AND epoch = @epoch AND state = 'running' AND wrap_up_notice_at IS NULL;

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
    failure_class = sqlc.narg(failure_class)::run_failure_class,
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
    failure_class = sqlc.narg(failure_class)::run_failure_class,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
WHERE org_id = @org_id AND id = @id
  AND state IN ('queued', 'starting', 'running', 'parked')
RETURNING *;

-- name: CancelQueuedRunsForTask :many
-- A task that reaches an end state cancels its queued runs (plan item A4).
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL
WHERE org_id = @org_id AND task_id = @task_id AND state = 'queued'
RETURNING *;

-- name: HasQueuedRunForTask :one
-- Whether the task already has a queued run, which will see every message
-- appended before it starts (plan item A4).
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE org_id = @org_id AND task_id = @task_id AND state = 'queued');

-- name: CancelOpenRunsForAgent :many
-- Archiving an agent cancels every run it has not finished. It locks the runs in id order (the
-- rule for every statement that locks more than one run row, see scheduler.LockTask), so it
-- cannot wait for a run in a cycle with Owner.Requeue, which holds a run and wants the queued one.
WITH locked AS (
    SELECT r.id FROM runs r
    WHERE r.org_id = @org_id AND r.agent_id = @agent_id
      AND r.state IN ('queued', 'starting', 'running', 'parked')
    ORDER BY r.id
    FOR UPDATE
)
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
FROM locked
WHERE runs.org_id = @org_id AND runs.id = locked.id
RETURNING runs.*;

-- name: CancelRunsOverWallClock :many
-- Cancels owned runs that have been owned longer than their wall-clock cap
-- (plan item A4). Parked time is excluded through parked_seconds. Safe to run
-- from every API replica: a run canceled by one sweep no longer matches. Locks the runs in id
-- order (see CancelOpenRunsForAgent).
WITH locked AS (
    SELECT r.id FROM runs r
    WHERE r.org_id = @org_id
      AND r.state IN ('starting', 'running')
      AND r.started_at + make_interval(secs => r.max_wall_clock_seconds + r.parked_seconds) < now()
    ORDER BY r.id
    FOR UPDATE
)
UPDATE runs
SET state = 'canceled',
    outcome_reason = @outcome_reason::text,
    finished_at = now(),
    ready_at = NULL,
    lease_expires_at = NULL,
    heartbeat_at = NULL
FROM locked
WHERE runs.org_id = @org_id AND runs.id = locked.id
RETURNING runs.*;

-- name: AddRunTokens :one
-- Adds reported token usage to an unfinished run. Tokens are reported, not enforced (ADR 0081):
-- the run limits are requests, spend and time. No row: the run is finished or absent.
UPDATE runs
SET tokens_used = tokens_used + @tokens::bigint
WHERE org_id = @org_id AND id = @id
  AND state IN ('queued', 'starting', 'running', 'parked')
RETURNING *;

-- name: ReleaseRunLease :one
-- The owner gives the run up without finishing it, for example because the
-- exec stream to the harness dropped: the lease ends now, so any agentd can
-- take the run over at once (TakeOverExpiredRun) and resume the session.
UPDATE runs
SET lease_expires_at = now() - interval '1 second'
WHERE org_id = @org_id AND id = @id AND epoch = @epoch
  AND state IN ('starting', 'running')
RETURNING *;

-- name: SetRunPromptThrough :one
-- Records, before the prompt is sent, the task timeline seq through which
-- the task's messages are in it.
UPDATE runs
SET prompt_through_seq = @prompt_through_seq::bigint
WHERE org_id = @org_id AND id = @id AND epoch = @epoch
  AND state IN ('starting', 'running')
RETURNING *;

-- name: GetTaskPromptedThrough :one
-- The highest task timeline seq already sent to the harness by the task's
-- other runs; messages after it are new for the next run.
SELECT coalesce(max(prompt_through_seq), 0)::bigint AS prompted_through
FROM runs
WHERE org_id = @org_id AND task_id = @task_id AND id <> @run_id;

-- name: LockRun :one
-- Serialises a park decision (agentd) with the resolution of the run's
-- approvals (the executor), so neither misses the other.
SELECT * FROM runs
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: ListExpiredParkedRuns :many
-- Parked runs whose park has expired (ADR 0049: 7 days for an approval).
SELECT * FROM runs
WHERE org_id = @org_id AND state = 'parked' AND park_expires_at < now()
ORDER BY park_expires_at, id;
