-- SPDX-License-Identifier: Apache-2.0
--
-- Connections (plan item C1; ADRs 0007, 0021, 0049) and ask-tier approval
-- requests (plan item C3; ADRs 0008, 0042).

-- +goose Up

CREATE TYPE connection_status AS ENUM ('untested', 'ok', 'failing');

-- One configured instance of a connector type in an org (ADR 0007), for
-- GitHub one App installation. Secrets are never stored here: the secure-input
-- fields of the setup guide are written straight to the Kubernetes Secret
-- named by secret_name (ADR 0049), and only a public fingerprint is kept.
CREATE TABLE connections (
    org_id             uuid              NOT NULL REFERENCES orgs (id),
    id                 uuid              NOT NULL DEFAULT uuidv7(),
    -- Connector type ID (connectors/<id>/connector.yaml).
    connector          text              NOT NULL
                                         CHECK (connector ~ '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'),
    display_name       text              NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    -- The non-secret setup-guide fields, e.g. {"appId": 1, "installationId": 2}.
    config             jsonb             NOT NULL DEFAULT '{}',
    -- Kubernetes Secret in Koinon's namespace holding the secret fields. The
    -- name derives from (org_id, id), never from user input.
    secret_name        text              NOT NULL UNIQUE
                                         CHECK (secret_name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    -- A public identifier of the secret (for GitHub: the key's SHA-256
    -- fingerprint, as GitHub shows it), so admins can match it.
    secret_fingerprint text              NOT NULL DEFAULT '',
    status             connection_status NOT NULL DEFAULT 'untested',
    last_tested_at     timestamptz,
    -- The last test's per-check results, [{"check":..,"ok":..,"message":..}].
    last_test          jsonb             NOT NULL DEFAULT '[]',
    created_by         uuid              REFERENCES users (id),
    created_at         timestamptz       NOT NULL DEFAULT now(),
    updated_at         timestamptz       NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    CHECK (jsonb_typeof(config) = 'object'),
    CHECK (jsonb_typeof(last_test) = 'array')
);

CREATE TRIGGER connections_set_updated_at BEFORE UPDATE ON connections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ADR 0042 lifecycle. C3 records pending requests; P1 approves, denies,
-- executes and expires them.
CREATE TYPE approval_status AS ENUM (
    'pending', 'approved', 'denied', 'executed', 'failed', 'expired', 'canceled'
);

-- What the grant's preset decided for the call: ask needs a human; allow is
-- executed without one (the Trusted preset).
CREATE TYPE approval_decision AS ENUM ('ask', 'allow');

-- One ask-tier tool call an agent made, recorded exactly (ADR 0042). The
-- approval binds to args_sha256, the SHA-256 of args_json; args_json is kept
-- as text, not jsonb, because jsonb re-orders keys and so could not be hashed
-- back to the same value.
CREATE TABLE approval_requests (
    org_id        uuid              NOT NULL,
    id            uuid              NOT NULL DEFAULT uuidv7(),
    ticket_id     uuid              NOT NULL,
    run_id        uuid              NOT NULL,
    agent_id      uuid              NOT NULL,
    connector     text              NOT NULL
                                    CHECK (connector ~ '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'),
    tool          text              NOT NULL CHECK (tool ~ '^[a-z][a-z0-9_]{0,63}$'),
    -- What the call acts on, e.g. "github:acme/widgets".
    target        text              NOT NULL CHECK (length(target) BETWEEN 1 AND 300),
    -- Canonical JSON of the arguments: sorted keys, no insignificant space.
    args_json     text              NOT NULL,
    args_sha256   text              NOT NULL CHECK (args_sha256 ~ '^[0-9a-f]{64}$'),
    -- Push-and-run risk flags for the approval card (plan item W2).
    risk          jsonb             NOT NULL DEFAULT '{}',
    decision      approval_decision NOT NULL,
    status        approval_status   NOT NULL DEFAULT 'pending',
    decided_by    uuid              REFERENCES users (id),
    decided_at    timestamptz,
    decision_note text,
    -- The executed call's outcome, e.g. {"url": ".../pull/1"}.
    result        jsonb,
    -- ADR 0049: a parked run expires after 7 days.
    expires_at    timestamptz       NOT NULL,
    created_at    timestamptz       NOT NULL DEFAULT now(),
    updated_at    timestamptz       NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    -- The same call twice in one run is one request.
    UNIQUE (org_id, run_id, tool, target, args_sha256),
    FOREIGN KEY (org_id, ticket_id, run_id) REFERENCES runs (org_id, ticket_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    -- The hash is what an approver approves; the database refuses a row whose
    -- hash does not match its arguments.
    CHECK (args_sha256 = encode(sha256(convert_to(args_json, 'UTF8')), 'hex')),
    CHECK (jsonb_typeof(risk) = 'object')
);

CREATE INDEX approval_requests_pending ON approval_requests (org_id, created_at)
    WHERE status = 'pending';
CREATE INDEX approval_requests_ticket ON approval_requests (org_id, ticket_id, created_at);

CREATE TRIGGER approval_requests_set_updated_at BEFORE UPDATE ON approval_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
