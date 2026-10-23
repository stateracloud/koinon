-- SPDX-License-Identifier: Apache-2.0
--
-- Tickets, runs and their event streams (plan item D1; ADRs 0010, 0017, 0029,
-- 0030 and 0049).

-- +goose Up

-- Board columns (ADR 0049): To do, In progress, Waiting on you, Done.
CREATE TYPE ticket_status AS ENUM ('todo', 'in_progress', 'waiting_on_you', 'done');

CREATE TABLE tickets (
    org_id              uuid          NOT NULL REFERENCES orgs (id),
    id                  uuid          NOT NULL DEFAULT uuidv7(),
    title               text          NOT NULL CHECK (length(title) BETWEEN 1 AND 500),
    status              ticket_status NOT NULL DEFAULT 'todo',
    -- The assigned agent; NULL while unassigned.
    agent_id            uuid,
    created_by          uuid          REFERENCES users (id),
    -- ADR 0029 / plan item AG1: one harness session per ticket. session/new on
    -- the first run, session/load afterwards. OpenCode rejects a load without
    -- the session's original cwd, so it is kept alongside.
    harness_session_id  text,
    harness_session_cwd text,
    -- Counter for ticket_events.seq.
    last_event_seq      bigint        NOT NULL DEFAULT 0 CHECK (last_event_seq >= 0),
    done_at             timestamptz,
    created_at          timestamptz   NOT NULL DEFAULT now(),
    updated_at          timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    CHECK ((status = 'done') = (done_at IS NOT NULL)),
    CHECK ((harness_session_id IS NULL) = (harness_session_cwd IS NULL))
);

CREATE INDEX tickets_org_id_status ON tickets (org_id, status, created_at);
CREATE INDEX tickets_org_id_agent_id ON tickets (org_id, agent_id);

CREATE TRIGGER tickets_set_updated_at BEFORE UPDATE ON tickets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ADR 0030 states. Transitions are listed in runs_check_transition below.
CREATE TYPE run_state AS ENUM (
    'queued', 'starting', 'running', 'parked', 'succeeded', 'failed', 'canceled'
);

CREATE TYPE run_park_reason AS ENUM ('approval', 'budget', 'a2a', 'input_required');

CREATE TABLE runs (
    org_id                 uuid            NOT NULL REFERENCES orgs (id),
    id                     uuid            NOT NULL DEFAULT uuidv7(),
    agent_id               uuid            NOT NULL,
    -- The approved agent version this run executes.
    agent_version_id       uuid            NOT NULL,
    ticket_id              uuid            NOT NULL,
    state                  run_state       NOT NULL DEFAULT 'queued',
    -- ADR 0030: the agent epoch at the moment the current owner took the run.
    -- Every owner write is a compare-and-set on it.
    epoch                  bigint          NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    -- The agentd replica that holds (or last held) the run.
    owner_id               text,
    lease_expires_at       timestamptz,
    heartbeat_at           timestamptz,
    -- When the run became claimable: set on enqueue and when a park is
    -- resolved; NULL otherwise. Claims take the oldest first (FIFO).
    ready_at               timestamptz,
    park_reason            run_park_reason,
    parked_at              timestamptz,
    park_expires_at        timestamptz,
    -- Why the run failed or was canceled.
    outcome_reason         text,
    -- Caps from the template (plan item A4).
    max_wall_clock_seconds integer         NOT NULL DEFAULT 1800 CHECK (max_wall_clock_seconds > 0),
    max_tokens             bigint          NOT NULL CHECK (max_tokens > 0),
    -- Counter for run_events.seq.
    last_event_seq         bigint          NOT NULL DEFAULT 0 CHECK (last_event_seq >= 0),
    started_at             timestamptz,
    finished_at            timestamptz,
    created_at             timestamptz     NOT NULL DEFAULT now(),
    updated_at             timestamptz     NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    -- Target for references that must also match the ticket.
    UNIQUE (org_id, ticket_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, agent_id, agent_version_id)
        REFERENCES agent_versions (org_id, agent_id, id),
    FOREIGN KEY (org_id, ticket_id) REFERENCES tickets (org_id, id),
    -- An owned run holds a lease.
    CHECK (state NOT IN ('starting', 'running')
           OR (owner_id IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (state IN ('starting', 'running') OR lease_expires_at IS NULL),
    CHECK (state <> 'parked' OR (park_reason IS NOT NULL AND parked_at IS NOT NULL)),
    CHECK (CASE state
               WHEN 'queued' THEN ready_at IS NOT NULL
               WHEN 'parked' THEN true
               ELSE ready_at IS NULL
           END),
    CHECK ((state IN ('succeeded', 'failed', 'canceled')) = (finished_at IS NOT NULL))
);

-- One active run per agent (ADRs 0009, 0049). Parked runs do not hold the
-- agent, so a 7-day approval wait does not block the agent's queue; resuming a
-- parked run goes through the same claim and so waits for the slot.
CREATE UNIQUE INDEX runs_one_active_per_agent ON runs (org_id, agent_id)
    WHERE state IN ('starting', 'running');

CREATE INDEX runs_claimable ON runs (org_id, ready_at, id) WHERE ready_at IS NOT NULL;
CREATE INDEX runs_leases ON runs (org_id, lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE INDEX runs_org_id_ticket_id ON runs (org_id, ticket_id, created_at);

CREATE TRIGGER runs_set_updated_at BEFORE UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Backstop for the run state machine. Queries already guard each transition
-- with its source state (and the epoch), so a racing or stale write matches no
-- rows; this trigger turns any other illegal write into an error. ADR 0030's
-- diagram plus cancel from every non-terminal state (user cancel, Done ticket,
-- run caps: plan items A3, A4).
-- +goose StatementBegin
CREATE FUNCTION runs_check_transition() RETURNS trigger
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

CREATE TRIGGER runs_check_transition BEFORE INSERT OR UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION runs_check_transition();

-- Session updates and lifecycle notes for one run, in write order. seq is
-- assigned under the run's row lock, so it is gap-free and strictly increasing
-- per run; clients stream "after seq N" and replay without duplicates.
CREATE TABLE run_events (
    org_id     uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT uuidv7(),
    run_id     uuid        NOT NULL,
    seq        bigint      NOT NULL CHECK (seq >= 1),
    -- The run epoch of the writer.
    epoch      bigint      NOT NULL,
    kind       text        NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]{0,63}$'),
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, run_id, seq),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id)
);

CREATE TYPE ticket_actor_kind AS ENUM ('user', 'agent', 'system');

-- The ticket timeline (ADR 0017): messages, column moves, approvals and run
-- notes such as "canceled: wall-clock cap". Ordered per ticket like run_events.
CREATE TABLE ticket_events (
    org_id         uuid              NOT NULL,
    id             uuid              NOT NULL DEFAULT uuidv7(),
    ticket_id      uuid              NOT NULL,
    seq            bigint            NOT NULL CHECK (seq >= 1),
    kind           text              NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]{0,63}$'),
    actor_kind     ticket_actor_kind NOT NULL,
    actor_user_id  uuid              REFERENCES users (id),
    actor_agent_id uuid,
    -- The run the event is about, if any; it must belong to the same ticket.
    run_id         uuid,
    payload        jsonb             NOT NULL DEFAULT '{}',
    created_at     timestamptz       NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, ticket_id, seq),
    FOREIGN KEY (org_id, ticket_id) REFERENCES tickets (org_id, id),
    FOREIGN KEY (org_id, actor_agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, ticket_id, run_id) REFERENCES runs (org_id, ticket_id, id),
    CHECK ((actor_kind = 'user') = (actor_user_id IS NOT NULL)),
    CHECK ((actor_kind = 'agent') = (actor_agent_id IS NOT NULL))
);
