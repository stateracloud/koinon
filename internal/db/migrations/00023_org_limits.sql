-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org concurrency and rate limits (plan item DB2, ADR 0066, #146 2026-10-26):
--   * max_concurrent_runs caps the runs an org has owned (starting or running) at once. It is
--     enforced where runs are claimed, in run_claimable, so ClaimNextRun and the reconciler's
--     demand test agree: a queued run held back by the cap is not demand.
--   * trigger_concurrency and trigger_rate_per_minute tune the org's partition of the DBOS queue
--     that carries trigger, webhook and task-creation work (internal/durable).
-- A NULL column is the platform default (20 runs; the queue defaults are set in the API).

-- +goose Up

ALTER TABLE org_settings
    ADD COLUMN max_concurrent_runs       integer CHECK (max_concurrent_runs BETWEEN 1 AND 10000),
    ADD COLUMN trigger_concurrency       integer CHECK (trigger_concurrency BETWEEN 1 AND 1000),
    ADD COLUMN trigger_rate_per_minute   integer CHECK (trigger_rate_per_minute BETWEEN 1 AND 100000);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION run_claimable(p_org_id uuid, p_run_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1
        FROM runs r
        JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id
        WHERE r.org_id = p_org_id AND r.id = p_run_id
          AND r.state IN ('queued', 'parked')
          AND r.ready_at IS NOT NULL
          AND a.status = 'active'
          AND a.unhealthy_at IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM runs act
              WHERE act.org_id = r.org_id AND act.agent_id = r.agent_id
                AND act.state IN ('starting', 'running'))
          -- One harness session per task (ADR 0029): a task's next run waits while another
          -- run of the same task is owned or parked.
          AND NOT EXISTS (
              SELECT 1 FROM runs sib
              WHERE sib.org_id = r.org_id AND sib.task_id = r.task_id AND sib.id <> r.id
                AND sib.state IN ('starting', 'running', 'parked'))
          -- Per-org run concurrency (ADR 0066, DB2): the org's owned runs stay under its cap.
          -- Parked runs are not counted; they hold no executor. Claims by different agents of
          -- one org that overlap may overshoot the cap by the number of racing claimers.
          AND (SELECT count(*) FROM runs own
               WHERE own.org_id = r.org_id AND own.state IN ('starting', 'running'))
              < coalesce((SELECT s.max_concurrent_runs FROM org_settings s WHERE s.org_id = r.org_id), 20)
    )
$$;
-- +goose StatementEnd
