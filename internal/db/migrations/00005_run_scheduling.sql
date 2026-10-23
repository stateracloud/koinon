-- SPDX-License-Identifier: Apache-2.0
--
-- Run scheduling and the WatchRun wake-ups (plan items A3, A4).

-- +goose Up

-- Run caps (plan item A4). The wall-clock cap counts time a run is owned
-- (starting or running), not time spent queued or parked: parked_seconds
-- accumulates the parked time and is added to the cap when a parked run is
-- claimed again. tokens_used is the running total reported by whoever records
-- usage (the ledger or agentd); the scheduler cancels the run when it reaches
-- max_tokens.
ALTER TABLE runs
    ADD COLUMN parked_seconds bigint NOT NULL DEFAULT 0 CHECK (parked_seconds >= 0),
    ADD COLUMN tokens_used    bigint NOT NULL DEFAULT 0 CHECK (tokens_used >= 0);

-- Owned runs by start time, for the wall-clock sweeper.
CREATE INDEX runs_owned_started_at ON runs (org_id, started_at)
    WHERE state IN ('starting', 'running');

-- WatchRun wake-ups (ADR 0022): every new run event and every run state change
-- sends NOTIFY koinon_run with '<org_id>/<run_id>'. The payload carries keys
-- only; listeners re-read run_events after their last seq, so a lost or
-- coalesced notification never loses data. NOTIFY is delivered on commit, so
-- writers in any process (API replicas, agentd) wake every API replica.
-- +goose StatementBegin
CREATE FUNCTION notify_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    run uuid;
BEGIN
    IF TG_TABLE_NAME = 'runs' THEN
        run := NEW.id;
    ELSE
        run := NEW.run_id;
    END IF;
    PERFORM pg_notify('koinon_run', NEW.org_id::text || '/' || run::text);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER run_events_notify AFTER INSERT ON run_events
    FOR EACH ROW EXECUTE FUNCTION notify_run_change();

CREATE TRIGGER runs_notify_state AFTER UPDATE OF state ON runs
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION notify_run_change();
