-- SPDX-License-Identifier: Apache-2.0
--
-- The oldest release that still works against this schema (ADR 0085, item 8, #408).
--
-- A binary stays not Ready while the schema declares a min_reader_version above the newest
-- migration the binary embeds. The newer schema says so, so a release that shipped earlier never
-- has to predict the future: an expand migration leaves this row alone and every older release
-- stays Ready; a breaking (contract) migration, allowed only in a new minor line, sets it to its
-- own version in the same file, and the older releases then refuse the schema instead of failing
-- at their first query.

-- +goose Up

CREATE TABLE schema_compat (
    singleton          boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    min_reader_version bigint  NOT NULL CHECK (min_reader_version >= 1)
);

INSERT INTO schema_compat (singleton, min_reader_version) VALUES (true, 44);
