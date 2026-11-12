-- SPDX-License-Identifier: Apache-2.0
--
-- Run artifacts: a body Koinon keeps for a run (#22). The first kind is 'input': an input the
-- context guard did not send to the model because it was larger than the cap (agentd, 80% of the
-- model's context window). The full body is kept here; Koinon never truncates it silently.
--
-- The rows live in Postgres until artifacts move to object storage (ADR 0031, not built yet); the
-- size check keeps one row bounded. Expand only (ADR 0085): a new table, nothing existing changes.

-- +goose Up

CREATE TABLE run_artifacts (
    org_id     uuid        NOT NULL REFERENCES orgs (id),
    id         uuid        NOT NULL DEFAULT uuidv7(),
    run_id     uuid        NOT NULL,
    task_id    uuid        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('input')),
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    media_type text        NOT NULL DEFAULT 'text/plain; charset=utf-8',
    size_bytes bigint      NOT NULL CHECK (size_bytes >= 0),
    sha256     text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    body       bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id)
);

CREATE INDEX run_artifacts_run ON run_artifacts (org_id, run_id);
