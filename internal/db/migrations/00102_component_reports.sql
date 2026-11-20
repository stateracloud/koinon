-- SPDX-License-Identifier: Apache-2.0
--
-- Which formats each running process can read (ADR 0097 decision 12.5 step 1, #1151). A process writes
-- a row for itself on a timer: its component, its instance (the pod name) and the names of the stored
-- formats its code reads. A writer of a new format (the first one is the sealed MCP credential, v2)
-- may start only when every component that must read it reports that it does, so an old pod never
-- meets a value it cannot read. internal/formatgate holds the gate and the list of formats.
--
-- Expand only (ADR 0085): one new table. An N-1 binary ignores it, and writes no row, so the gate
-- also counts the rows of each component against the replicas the chart expects.

-- +goose Up

CREATE TABLE component_reports (
    component   text        NOT NULL CHECK (component ~ '^[a-z][a-z0-9-]{0,31}$'),
    instance    text        NOT NULL CHECK (instance <> '' AND length(instance) <= 253),
    reads       text[]      NOT NULL DEFAULT '{}',
    reported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (component, instance)
);

-- +goose Down

DROP TABLE component_reports;
