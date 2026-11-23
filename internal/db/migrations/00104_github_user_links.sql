-- SPDX-License-Identifier: Apache-2.0
--
-- GitHub sign-in links (issue #1175, ADR 0005 amendment 2026-11-23). A Koinon user links their
-- GitHub login once, through the org App's user authorization. Koinon keeps the login and the
-- numeric GitHub user ID, never the user token, and requests the requester's review on an agent
-- pull request. A link belongs to one GitHub host (github.com or a GitHub Enterprise Server), so a
-- login is used only on that host. The user or an org admin removes a link; both are audited.

-- +goose Up

CREATE TABLE github_user_links (
    org_id         uuid        NOT NULL REFERENCES orgs (id),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The host's web URL, for example https://github.com.
    host           text        NOT NULL CHECK (host ~ '^https?://[^/]+(/.*)?$' AND length(host) <= 2048),
    github_user_id bigint      NOT NULL CHECK (github_user_id > 0),
    github_login   text        NOT NULL CHECK (length(github_login) BETWEEN 1 AND 100),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id, host),
    -- One Koinon user per GitHub account of a host in an org.
    UNIQUE (org_id, host, github_user_id)
);

-- A started link: single use, bound to the user who started it, valid for ten minutes. Only the
-- SHA-256 of the state is stored. The PKCE verifier is useless without the code GitHub sends to
-- the same user's browser.
CREATE TABLE github_link_flows (
    org_id        uuid        NOT NULL REFERENCES orgs (id),
    state_hash    bytea       NOT NULL CHECK (length(state_hash) = 32),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id uuid        NOT NULL,
    code_verifier text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (org_id, state_hash),
    FOREIGN KEY (org_id, connection_id) REFERENCES connections (org_id, id) ON DELETE CASCADE
);
