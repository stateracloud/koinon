-- SPDX-License-Identifier: Apache-2.0
--
-- A per-agent model setting and the provider echo check (rc.6).
--
-- agents.model is a mutable agent setting, not part of an agent version: an admin changes it
-- without proposing and activating a version, and every change is audited. NULL means "default":
-- the version's model (templates set spec.model), then the org default. A set value names a model
-- the org serves and has a price for; koinon-api checks that when it is set and koinon-agentd
-- checks it again when a run starts, because the catalog changes after the setting does.
--
-- provider_echo_checks keeps the latest result of the on-demand echo check of each provider: one
-- tiny request through the gateway that asks the model to repeat a line exactly, to catch masking
-- of names and addresses on the provider's side (for example an OpenRouter account setting). The
-- row holds what was sent and received, never a credential.

-- +goose Up

ALTER TABLE agents
    ADD COLUMN model text CHECK (model IS NULL OR (length(model) BETWEEN 1 AND 200 AND model <> 'default'));

CREATE TABLE provider_echo_checks (
    org_id      uuid        NOT NULL,
    provider_id uuid        NOT NULL,
    model       text        NOT NULL,
    -- true when the reply was the line, character for character.
    unchanged   boolean     NOT NULL,
    sent        text        NOT NULL,
    received    text        NOT NULL,
    checked_at  timestamptz NOT NULL DEFAULT now(),
    checked_by  uuid        REFERENCES users (id),
    PRIMARY KEY (org_id, provider_id),
    FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE CASCADE
);
