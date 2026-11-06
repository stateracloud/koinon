-- SPDX-License-Identifier: Apache-2.0
--
-- Lost credentials (ADR 0085 decision 13, #85). When a KEK is lost, `koinon-api keys forget <id>`
-- marks every credential it wrapped as lost: the envelope is cleared and credential_lost_at says
-- since when. The UI shows "credential lost, enter it again", and nothing serves a lost credential
-- (the gateway catalog, MCP serving, trace export; runs that need a lost connection fail with the
-- reason). Expand-only: new nullable columns, a relaxed CHECK and a trigger.
--
-- Invariants, held by the database so that every writer keeps them, an N-1 binary's included:
-- - A lost row holds no envelope (CHECK).
-- - Writing an envelope clears the mark (trigger credential_lost_clear): re-entering the key, an
--   update of a declared row from its Secret, anything that stores a credential again.

-- +goose Up

ALTER TABLE providers ADD COLUMN credential_lost_at timestamptz;
ALTER TABLE providers ADD CONSTRAINT providers_credential_lost
    CHECK (credential_lost_at IS NULL OR api_key IS NULL);

ALTER TABLE connections ADD COLUMN credential_lost_at timestamptz;
ALTER TABLE connections ADD CONSTRAINT connections_credential_lost
    CHECK (credential_lost_at IS NULL OR secret_envelope IS NULL);

ALTER TABLE mcp_servers ADD COLUMN credential_lost_at timestamptz;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_credential_lost
    CHECK (credential_lost_at IS NULL OR credential IS NULL);
-- A lost MCP credential keeps its header name and prefix, so the form asks for the value only.
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_check;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_credential_header
    CHECK ((credential IS NULL) = (credential_header IS NULL)
           OR (credential IS NULL AND credential_lost_at IS NOT NULL));

ALTER TABLE trace_destinations ADD COLUMN credential_lost_at timestamptz;
ALTER TABLE trace_destinations ADD CONSTRAINT trace_destinations_credential_lost
    CHECK (credential_lost_at IS NULL OR credentials IS NULL);

ALTER TABLE platform_trace_destination ADD COLUMN credential_lost_at timestamptz;
ALTER TABLE platform_trace_destination ADD CONSTRAINT platform_trace_destination_credential_lost
    CHECK (credential_lost_at IS NULL OR credentials IS NULL);

-- credential_lost_clear(<envelope column>): an update that writes a new envelope clears the mark.
-- keys forget clears the envelope and sets the mark in one update, which this leaves alone; an
-- update that sets the mark but leaves the old envelope is the CHECK's to refuse.
-- +goose StatementBegin
CREATE FUNCTION credential_lost_clear() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    new_env jsonb := to_jsonb(NEW) -> TG_ARGV[0];
BEGIN
    IF NEW.credential_lost_at IS NOT NULL
       AND jsonb_typeof(new_env) IS DISTINCT FROM 'null' AND new_env IS NOT NULL
       AND new_env IS DISTINCT FROM to_jsonb(OLD) -> TG_ARGV[0] THEN
        NEW.credential_lost_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Named so they run before the set_updated_at triggers (alphabetical order), which do not care.
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON providers
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('api_key');
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON connections
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('secret_envelope');
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('credential');
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON trace_destinations
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('credentials');
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON platform_trace_destination
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('credentials');
