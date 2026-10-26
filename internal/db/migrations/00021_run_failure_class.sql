-- SPDX-License-Identifier: Apache-2.0
--
-- Failure classes (plan item FC1; ADR 0030, amendment of 2026-10-26). A run that failed, or was
-- stopped by a budget, records the class of the error where it was first understood. Retries and
-- the message to the person follow from the class. NULL: the run did not fail, or it failed before
-- this column existed.

-- +goose Up

CREATE TYPE run_failure_class AS ENUM (
    'transient', 'throttled', 'context_overflow', 'budget', 'policy', 'fatal'
);

ALTER TABLE runs ADD COLUMN failure_class run_failure_class;
