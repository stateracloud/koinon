-- SPDX-License-Identifier: Apache-2.0
--
-- The public keys of Koinon's gateway (ADR 0097 decision 12, #1151, slice 2). koinon-api seals a stored
-- MCP credential to the newest `active` key of this table (envelope v2) and only the gateway holds the
-- private half. A public key is not secret, but WHICH key is `active` is: whoever can insert a row
-- here can make koinon-api seal to a key that person knows. So the table is guarded twice:
--
--  1. Grants. The shared application role (koinon_app, which gets table rights by default privileges
--     in cnpg mode) loses INSERT, UPDATE and DELETE here and keeps SELECT. The chart's separate roles
--     (gateway key-admin, gateway, re-seal; migrations.GrantGatewayRecipientKeys) get what each needs.
--  2. Triggers. A row is never deleted, and only its state moves forward (pending to active to
--     retiring to retired, with the time it did). Everything else in a row is fixed at INSERT. This
--     stops mistakes. It does not stop the table owner (who can DROP the table or DISABLE TRIGGER);
--     with external mode and one shared database role the grant protection does not hold either, and
--     the install guide says so.
--
-- gateway_recipient_key_loads is the gateway's report that it holds the private key of a row (the
-- `pending` to `active` step of a rotation, ADR 0097 12.4): one row per gateway process and key.
--
-- The function gateway_recipient_key_in_use tells the key-admin Job whether a stored MCP credential
-- still names a key, without giving it a read of the sealed column. The last trigger refuses a v2
-- credential whose key is not `active` or `retiring`, so a stale koinon-api pod cannot write a value
-- nobody can open (12.4). No v2 value is written before slice 3; the trigger is the guard that is
-- there when it is.
--
-- Expand only (ADR 0085): new tables, new functions, one new trigger. An N-1 binary ignores them.

-- +goose Up

CREATE TABLE gateway_recipient_keys (
    key_id       text PRIMARY KEY CHECK (key_id ~ '^gk-[0-9a-f]{16}$'),
    recipient    text NOT NULL CHECK (recipient ~ '^hpke-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}$'),
    public_key   bytea NOT NULL CHECK (length(public_key) BETWEEN 32 AND 2048),
    state        text NOT NULL CHECK (state IN ('pending', 'active', 'retiring', 'retired')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    retiring_at  timestamptz,
    retired_at   timestamptz,
    -- The state and its time agree: a time is set exactly when the key reached that state or a later one.
    CONSTRAINT gateway_recipient_keys_times CHECK (
        (state = 'pending' AND activated_at IS NULL AND retiring_at IS NULL AND retired_at IS NULL)
        OR (state = 'active' AND activated_at IS NOT NULL AND retiring_at IS NULL AND retired_at IS NULL)
        OR (state = 'retiring' AND activated_at IS NOT NULL AND retiring_at IS NOT NULL AND retired_at IS NULL)
        OR (state = 'retired' AND retired_at IS NOT NULL)
    )
);

-- At most one key is active: koinon-api seals to the newest active key.
CREATE UNIQUE INDEX gateway_recipient_keys_one_active ON gateway_recipient_keys ((true)) WHERE state = 'active';

-- +goose StatementBegin
CREATE FUNCTION gateway_recipient_keys_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'gateway_recipient_keys rows are never deleted: a key is retired, not removed (ADR 0097 12.4)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.key_id IS DISTINCT FROM OLD.key_id
        OR NEW.recipient IS DISTINCT FROM OLD.recipient
        OR NEW.public_key IS DISTINCT FROM OLD.public_key
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'a gateway recipient key changes only its state (ADR 0097 12.4)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.state = OLD.state THEN
        IF NEW IS DISTINCT FROM OLD THEN
            RAISE EXCEPTION 'a gateway recipient key changes only with its state (ADR 0097 12.4)'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    -- The only moves: pending to active, active to retiring, retiring to retired. A pending key that is
    -- never used may be retired at once (a rotation that was abandoned).
    IF NOT ((OLD.state = 'pending' AND NEW.state IN ('active', 'retired'))
        OR (OLD.state = 'active' AND NEW.state = 'retiring')
        OR (OLD.state = 'retiring' AND NEW.state = 'retired')) THEN
        RAISE EXCEPTION 'a gateway recipient key cannot move from % to % (ADR 0097 12.4)', OLD.state, NEW.state
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- A time already set stays.
    IF (OLD.activated_at IS NOT NULL AND NEW.activated_at IS DISTINCT FROM OLD.activated_at)
        OR (OLD.retiring_at IS NOT NULL AND NEW.retiring_at IS DISTINCT FROM OLD.retiring_at)
        OR OLD.retired_at IS NOT NULL THEN
        RAISE EXCEPTION 'the times of a gateway recipient key never change once set (ADR 0097 12.4)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER gateway_recipient_keys_guard BEFORE UPDATE OR DELETE ON gateway_recipient_keys
    FOR EACH ROW EXECUTE FUNCTION gateway_recipient_keys_guard();
CREATE TRIGGER gateway_recipient_keys_no_truncate BEFORE TRUNCATE ON gateway_recipient_keys
    FOR EACH STATEMENT EXECUTE FUNCTION gateway_recipient_keys_guard();

-- The gateway's report that a process holds the private key of a row. copy_id is the process (pod name
-- and a boot nonce, as on the State stream). The gateway inserts a row and refreshes loaded_at.
CREATE TABLE gateway_recipient_key_loads (
    key_id    text NOT NULL REFERENCES gateway_recipient_keys (key_id),
    copy_id   text NOT NULL CHECK (length(copy_id) BETWEEN 1 AND 253),
    loaded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key_id, copy_id)
);

-- A stored MCP credential names the key it is sealed to (ADR 0097 12.2). Without a v2 value the function
-- counts zero. SECURITY DEFINER: the key-admin role has no read of the sealed column, only of this count.
-- +goose StatementBegin
CREATE FUNCTION gateway_recipient_key_in_use(p_key_id text) RETURNS bigint
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    SELECT count(*) FROM mcp_servers
    WHERE credential ->> 'fmt' = '2' AND credential ->> 'recipient_key_id' = p_key_id
$$;
-- +goose StatementEnd

-- A v2 credential must name a key that is active or retiring (ADR 0097 12.4).
-- +goose StatementBegin
CREATE FUNCTION mcp_servers_credential_key_check() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.credential IS NULL OR NEW.credential ->> 'fmt' IS DISTINCT FROM '2' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.credential IS NOT DISTINCT FROM OLD.credential THEN
        RETURN NEW;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM gateway_recipient_keys
        WHERE key_id = NEW.credential ->> 'recipient_key_id' AND state IN ('active', 'retiring')
    ) THEN
        RAISE EXCEPTION 'a v2 MCP credential must name an active or retiring gateway key (ADR 0097 12.4)'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER mcp_servers_credential_key_check BEFORE INSERT OR UPDATE OF credential ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION mcp_servers_credential_key_check();

-- The shared application role keeps SELECT only. It exists in cnpg mode (and in external installs that
-- follow the guide); where it does not, nothing was granted to it. The function is also closed to it:
-- default privileges grant EXECUTE on functions to koinon_app.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON gateway_recipient_keys FROM koinon_app;
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON gateway_recipient_key_loads FROM koinon_app;
        REVOKE EXECUTE ON FUNCTION gateway_recipient_key_in_use(text) FROM koinon_app;
    END IF;
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION gateway_recipient_key_in_use(text) FROM PUBLIC;
