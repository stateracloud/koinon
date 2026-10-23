-- SPDX-License-Identifier: Apache-2.0
--
-- Home generations: which home volume a ticket's harness session lives in.
--
-- OpenShell 0.1.5 deletes a sandbox's home volume together with the sandbox,
-- so when the reconciler recreates a sandbox (a spec change, or a sandbox that
-- disappeared) the harness's stored sessions are gone. agentd loads a ticket's
-- session only in the home it was created in, and starts a new session
-- otherwise.

-- +goose Up

-- The identifier of the sandbox's current home volume: OpenShell's ID of the
-- sandbox, which is new for every sandbox created and kept across stop and
-- start (both keep the home volume). NULL until the reconciler observes it.
ALTER TABLE agent_sandboxes
    ADD COLUMN home_generation text CHECK (home_generation <> '');

-- The home generation the ticket's harness session was created in. NULL for
-- sessions recorded before generations existed: those are not loaded again.
ALTER TABLE tickets
    ADD COLUMN harness_session_generation text CHECK (harness_session_generation <> ''),
    ADD CONSTRAINT tickets_harness_session_generation_session
        CHECK (harness_session_generation IS NULL OR harness_session_id IS NOT NULL);
