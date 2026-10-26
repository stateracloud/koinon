-- SPDX-License-Identifier: Apache-2.0
--
-- One definition of "this run can be claimed now", shared by ClaimNextRun and by the demand test
-- the reconciler keeps a sandbox up for (GetAgentRunActivity.has_demand), so the two cannot drift.
-- A queued run that cannot start (its agent is unhealthy, or its ticket's run is parked) is not
-- demand and must not keep a Kata VM running or start a stopped one.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION run_claimable(p_org_id uuid, p_run_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1
        FROM runs r
        JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id
        WHERE r.org_id = p_org_id AND r.id = p_run_id
          AND r.state IN ('queued', 'parked')
          AND r.ready_at IS NOT NULL
          AND a.status = 'active'
          -- An unhealthy agent (ADR 0054) claims nothing: queued runs wait, and a parked run
          -- whose approval was decided stays parked, until an admin resumes the agent.
          AND a.unhealthy_at IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM runs act
              WHERE act.org_id = r.org_id AND act.agent_id = r.agent_id
                AND act.state IN ('starting', 'running'))
          -- One harness session per ticket (ADR 0029): a ticket's next run waits while another
          -- run of the same ticket is owned or parked.
          AND NOT EXISTS (
              SELECT 1 FROM runs sib
              WHERE sib.org_id = r.org_id AND sib.ticket_id = r.ticket_id AND sib.id <> r.id
                AND sib.state IN ('starting', 'running', 'parked'))
    )
$$;
-- +goose StatementEnd

