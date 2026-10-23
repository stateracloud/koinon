-- SPDX-License-Identifier: Apache-2.0
--
-- Authentication: the bootstrap admin's local credential, server-side
-- sessions and in-flight OIDC logins (plan items A1 and A2).
-- None of these tables is org-owned: they belong to a user or to no one yet.
-- Secrets are never stored in the clear: passwords as argon2id PHC strings,
-- session tokens and OIDC state as SHA-256 hashes.

-- +goose Up

CREATE TYPE auth_method AS ENUM ('password', 'oidc');

-- Local username/password login. In v0.1 only the bootstrap admin has one; it
-- is created from a mounted Secret and must be changed at first login.
CREATE TABLE local_credentials (
    user_id              uuid        PRIMARY KEY REFERENCES users (id),
    username             text        NOT NULL UNIQUE
                                     CHECK (length(username) BETWEEN 1 AND 64),
    password_hash        text        NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    must_change_password boolean     NOT NULL DEFAULT true,
    password_changed_at  timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER local_credentials_set_updated_at BEFORE UPDATE ON local_credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The cookie holds a random token; only its SHA-256 is stored, so a database
-- read does not yield usable sessions. A session is valid while it is not
-- revoked and both the idle and the absolute deadline are in the future.
CREATE TABLE sessions (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    token_hash      bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    user_id         uuid        NOT NULL REFERENCES users (id),
    auth_method     auth_method NOT NULL,
    created_at      timestamptz NOT NULL,
    last_seen_at    timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    CHECK (idle_expires_at <= expires_at),
    CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_id ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expires_at ON sessions (expires_at);

-- An OIDC authorization request between /auth/oidc/start and the callback.
-- Rows live for minutes and are deleted when the callback consumes them.
CREATE TABLE oidc_login_flows (
    state_hash    bytea       PRIMARY KEY CHECK (length(state_hash) = 32),
    code_verifier text        NOT NULL,
    nonce         text        NOT NULL,
    return_to     text        NOT NULL,
    created_at    timestamptz NOT NULL,
    expires_at    timestamptz NOT NULL
);

CREATE INDEX oidc_login_flows_expires_at ON oidc_login_flows (expires_at);
