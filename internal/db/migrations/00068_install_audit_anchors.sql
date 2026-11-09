-- SPDX-License-Identifier: Apache-2.0
--
-- Install audit anchors (ADR 0100 decision 4.1, issue 333): the heads of every org chain that an
-- audit.anchor install event commits to. The event's summary holds org_count and heads_root, a hash
-- over these rows, so the install head commits to every org head. Expand only (ADR 0085): a new table.
--
-- Append-only like the install chain, with the same guards (00066). org_id has no foreign key: an
-- org's last head stays anchored after the org is deleted. Only the koinon_install_audit_writer role
-- may insert, granted by "koinon-api migrate" after goose (migrations.GrantInstallAuditWriter).

-- +goose Up

CREATE TABLE install_audit_anchor_heads (
    anchor_event_id uuid   NOT NULL REFERENCES install_audit_events (id),
    org_id          uuid   NOT NULL,
    chain_seq       bigint NOT NULL CHECK (chain_seq >= 1),
    hash            bytea  NOT NULL CHECK (length(hash) = 32),
    PRIMARY KEY (anchor_event_id, org_id)
);

CREATE TRIGGER install_audit_anchor_heads_no_update BEFORE UPDATE OR DELETE ON install_audit_anchor_heads
    FOR EACH ROW EXECUTE FUNCTION install_audit_append_only();
CREATE TRIGGER install_audit_anchor_heads_no_truncate BEFORE TRUNCATE ON install_audit_anchor_heads
    FOR EACH STATEMENT EXECUTE FUNCTION install_audit_append_only();

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON install_audit_anchor_heads FROM koinon_app;
        GRANT SELECT ON install_audit_anchor_heads TO koinon_app;
    END IF;
END;
$$;
-- +goose StatementEnd
