-- SPDX-License-Identifier: Apache-2.0
--
-- Rendered execution state (plan item R2): each org's OpenShell workspace and
-- each agent's sandbox, written by koinon-reconciler and read by koinon-agentd.
-- Postgres stays the source of truth (ADR 0003); these rows record what the
-- reconciler rendered and last observed. No key material is stored here: the
-- agentgateway key lives in a Kubernetes Secret and the OpenShell provider
-- record, and only its ID and hash are kept.

-- +goose Up

-- One OpenShell workspace per org (ADR 0002). In managed mode OpenShell names
-- the namespace, so Koinon stores the mapping instead of deriving it.
CREATE TABLE org_workspaces (
    org_id       uuid        PRIMARY KEY REFERENCES orgs (id),
    -- The OpenShell workspace name, derived from a hash of org_id.
    workspace    text        NOT NULL UNIQUE
                             CHECK (workspace ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    -- OpenShell's ID for the workspace, once observed.
    workspace_id text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER org_workspaces_set_updated_at BEFORE UPDATE ON org_workspaces
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- What koinon-agentd needs to know about a sandbox before it execs into it.
-- pending: nothing rendered yet; starting/ready/stopping/stopped/error follow
-- the OpenShell phase; deleting/deleted after archive; retained: archived with
-- the home volume kept (home retention "retain").
CREATE TYPE sandbox_state AS ENUM (
    'pending', 'starting', 'ready', 'stopping', 'stopped', 'error',
    'deleting', 'deleted', 'retained'
);

-- One row per agent that the reconciler has rendered (ADR 0009: exactly one
-- sandbox per agent), so it is keyed by the agent.
CREATE TABLE agent_sandboxes (
    org_id           uuid          NOT NULL,
    agent_id         uuid          NOT NULL,
    workspace        text          NOT NULL,
    -- OpenShell 0.1.5 limits sandbox names to 19 characters. Names derive from
    -- a hash of (org_id, agent_id), never from display names.
    sandbox_name     text          NOT NULL
                                   CHECK (sandbox_name ~ '^[a-z0-9]([a-z0-9-]{0,17}[a-z0-9])?$'),
    provider_name    text          NOT NULL
                                   CHECK (provider_name ~ '^[a-z0-9]([a-z0-9-]{0,17}[a-z0-9])?$'),
    -- Kubernetes Secret in Koinon's namespace holding the agentgateway key.
    key_secret_name  text          NOT NULL
                                   CHECK (key_secret_name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    -- The current agentgateway key: its ID and SHA-256, never the material.
    gateway_key_id   text,
    gateway_key_hash text          CHECK (gateway_key_hash ~ '^sha256:[0-9a-f]{64}$'),
    state            sandbox_state NOT NULL DEFAULT 'pending',
    -- The last observed OpenShell phase and condition message, for operators.
    phase            text          NOT NULL DEFAULT '',
    message          text          NOT NULL DEFAULT '',
    -- Hash of the rendered sandbox spec (without network rules); a change
    -- recreates the sandbox, network rule changes are applied in place.
    spec_hash        text          NOT NULL DEFAULT '',
    -- When the sandbox last became ready and last stopped; the idle timeout
    -- counts from the later of started_at and the agent's last run activity.
    started_at       timestamptz,
    stopped_at       timestamptz,
    observed_at      timestamptz,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id),
    UNIQUE (workspace, sandbox_name),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id) REFERENCES org_workspaces (org_id),
    CHECK ((gateway_key_id IS NULL) = (gateway_key_hash IS NULL))
);

CREATE TRIGGER agent_sandboxes_set_updated_at BEFORE UPDATE ON agent_sandboxes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Wake-up hints for the reconciler (ADR 0003): NOTIFY is delivered only on
-- commit and is not replayed, so the reconciler also re-reads everything on a
-- timer; the payload is only the org ID.
-- +goose StatementBegin
CREATE FUNCTION notify_reconciler() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('koinon_reconcile', NEW.org_id::text);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agents_notify_reconciler AFTER INSERT OR UPDATE OF status, active_version_id ON agents
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler();

-- A queued run starts a stopped sandbox; a finished or parked run starts the
-- idle timer.
CREATE TRIGGER runs_notify_reconciler AFTER INSERT OR UPDATE OF state, ready_at ON runs
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler();
