-- SPDX-License-Identifier: Apache-2.0
--
-- An agent selects a harness, and "flavor" is retired (ADR 0112, decisions 1 and 22; #610).
--
-- agents.harness is the name of a harness definition. Only the built-in definitions exist today, so
-- koinon-api accepts "opencode" alone; the definitions of ADR 0112 turn the name into a reference.
-- An enum cannot hold a harness that an install registers, so the column becomes text and the
-- enum goes. Renaming and dropping are allowed while there is no schema promise (ADR 0099).

-- +goose Up

ALTER TABLE agents RENAME COLUMN flavor TO harness;
ALTER TABLE agents ALTER COLUMN harness TYPE text USING harness::text;
ALTER TABLE agents ADD CONSTRAINT agents_harness_name CHECK (harness ~ '^[a-z][a-z0-9-]{0,62}$');
DROP TYPE agent_flavor;
