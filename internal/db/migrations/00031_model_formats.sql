-- SPDX-License-Identifier: Apache-2.0
--
-- Models speak their family's native API format (ADR 0069, decisions 2 and 5).
--
-- models.format is the wire format the gateway and the harness use for the model: 'openai' (chat
-- completions) or 'anthropic' (Messages). internal/modelformat is the one table of defaults; the
-- CHECK lists the known values and grows with it. models.format_overridden records that an admin
-- set the format: koinon-api re-derives every other model from the table at startup.
--
-- The backfill is the one fact SQL can state without the table: models of a provider of type
-- 'anthropic' speak Messages. Every other model keeps 'openai' until koinon-api's startup
-- re-derivation applies the table (OpenRouter anthropic/* models stay on 'openai' for now).
--
-- providers.anthropic_workspace_id is the workspace the gateway names in the anthropic-workspace-id
-- header, for an API key not scoped to a workspace. It is an identifier, not a credential.

-- +goose Up

ALTER TABLE models
    ADD COLUMN format            text    NOT NULL DEFAULT 'openai' CHECK (format IN ('openai', 'anthropic')),
    ADD COLUMN format_overridden boolean NOT NULL DEFAULT false;

UPDATE models m
SET format = 'anthropic'
FROM providers p
WHERE p.org_id = m.org_id AND p.id = m.provider_id AND p.type = 'anthropic';

ALTER TABLE providers
    ADD COLUMN anthropic_workspace_id text
        CONSTRAINT providers_anthropic_workspace_id_format CHECK (anthropic_workspace_id ~ '^wrkspc_[A-Za-z0-9]+$' AND length(anthropic_workspace_id) <= 128),
    ADD CONSTRAINT providers_anthropic_workspace_id_type CHECK (anthropic_workspace_id IS NULL OR type = 'anthropic');
