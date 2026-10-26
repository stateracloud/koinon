-- SPDX-License-Identifier: Apache-2.0
--
-- Latency defaults (plan item LT1, ADRs 0049, 0065, 0066):
--   * per-org tunables for the idle stop, the warm window of a run parked on a2a or input_required,
--     and how long a run waits for its sandbox before it goes back to the queue;
--   * run origin, so interactive work is claimed before scheduled work;
--   * starting -> queued, so a run whose sandbox is not ready in time waits in the queue instead of
--     failing;
--   * agent_sandboxes.woken_at, the pre-wake signal (a task created, a person typing, a cron about
--     to fire).

-- +goose Up

-- A NULL column means the platform default (10 minutes idle, 15 minutes warm, 5 minutes wait).
CREATE TABLE org_settings (
    org_id                uuid        PRIMARY KEY REFERENCES orgs (id),
    idle_timeout_seconds  integer     CHECK (idle_timeout_seconds BETWEEN 60 AND 86400),
    park_warm_seconds     integer     CHECK (park_warm_seconds BETWEEN 60 AND 86400),
    queue_wait_seconds    integer     CHECK (queue_wait_seconds BETWEEN 5 AND 86400),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER org_settings_set_updated_at BEFORE UPDATE ON org_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TYPE run_origin AS ENUM ('interactive', 'scheduled');

ALTER TABLE runs ADD COLUMN origin run_origin NOT NULL DEFAULT 'interactive';

ALTER TABLE agent_sandboxes ADD COLUMN woken_at timestamptz;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION runs_check_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'queued' THEN
            RAISE EXCEPTION 'run % must be created queued, not %', NEW.id, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.epoch < OLD.epoch THEN
        RAISE EXCEPTION 'run % epoch must not decrease (% -> %)', OLD.id, OLD.epoch, NEW.epoch
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.state = OLD.state OR (OLD.state, NEW.state) IN (
        ('queued',   'starting'),
        ('queued',   'canceled'),
        -- The sandbox was not ready in time: the run waits in the queue (ADR 0066).
        ('starting', 'queued'),
        ('starting', 'running'),
        ('starting', 'failed'),
        ('starting', 'canceled'),
        ('running',  'parked'),
        ('running',  'succeeded'),
        ('running',  'failed'),
        ('running',  'canceled'),
        ('parked',   'starting'),
        ('parked',   'canceled'))
    THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'illegal run state transition % -> % (run %)', OLD.state, NEW.state, OLD.id
        USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd
