-- SPDX-License-Identifier: Apache-2.0
--
-- The home-written clock (issue #329, ADR 0032 amendment). The checkpoint gates (recreate, repair,
-- idle stop, archive) need "the home was last written", not "the sandbox was last active": a wake
-- or a start does not write the home, so it must not make an older backup stale. The home is
-- written by a harness, so home_dirtied_at is the last time a harness in the current sandbox
-- started or stopped running (a run entering or leaving 'running'). NULL: no harness ever ran in
-- this sandbox. A new sandbox ID resets it (RecordAgentSandbox).

-- +goose Up

ALTER TABLE agent_sandboxes ADD COLUMN home_dirtied_at timestamptz;

-- +goose StatementBegin
CREATE FUNCTION runs_mark_home_dirtied() RETURNS trigger AS $$
BEGIN
    UPDATE agent_sandboxes SET home_dirtied_at = now()
    WHERE org_id = NEW.org_id AND agent_id = NEW.agent_id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER runs_home_dirtied AFTER UPDATE OF state ON runs
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state AND (OLD.state = 'running' OR NEW.state = 'running'))
    EXECUTE FUNCTION runs_mark_home_dirtied();

-- Existing sandboxes: the newest run that ended or parked, or now when one is running.
UPDATE agent_sandboxes s SET home_dirtied_at = (
    SELECT max(greatest(r.finished_at, r.parked_at, CASE WHEN r.state = 'running' THEN now() END))
    FROM runs r WHERE r.org_id = s.org_id AND r.agent_id = s.agent_id);
