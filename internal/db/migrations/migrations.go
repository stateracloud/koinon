// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the goose SQL migrations. File names are
// zero-padded (NNNNN_name.sql) so goose and sqlc read them in the same order.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
)

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS

// InstallAuditWriterRole is the NOLOGIN group role that may insert into the install audit chain
// (ADR 0100 decision 1.1a). The chart creates it; the migrate role has no CREATEROLE.
const InstallAuditWriterRole = "koinon_install_audit_writer"

// Execer is what GrantInstallAuditWriter needs of *sql.DB, *sql.Conn and *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckInstallAuditWriter reports whether the role InstallAuditWriterRole exists (nil when it does).
// It needs no table, so "koinon-api migrate --require-install-writer" runs it before the
// migrations: a failed Job leaves the schema as it was. The migration does not check koinon-api's
// membership: it connects as another role, often another identity (workload identity, a pooler), and
// cannot know koinon-api's login; koinon-api checks its own (audit.InstallWriterCheck, ADR 0100
// decision 1.1a, maintainer decision 2026-11-07).
func CheckInstallAuditWriter(ctx context.Context, db Execer) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, InstallAuditWriterRole).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the role %s does not exist", InstallAuditWriterRole)
	}
	return nil
}

// GrantInstallAuditWriter gives InstallAuditWriterRole its privileges on the install audit tables
// and reports whether it did: false when the role or the tables (migrations 00066, 00068) do not exist yet.
// It runs as the table owner after the goose migrations on every migrate, and alone in "koinon-api
// migrate --grants-only", and is idempotent. It is not part of migration 00066 because on a Helm
// upgrade of a CloudNativePG cluster the migrate hook runs before the updated Cluster (which adds the
// role as a managed role) reaches the API server: a migration that needs the role would abort the
// upgrade before the role can appear. So the chart's post-upgrade hook (and Argo CD PostSync hook)
// runs --grants-only, which waits for the role and then grants (ADR 0100 decision 1.1a).
func GrantInstallAuditWriter(ctx context.Context, db Execer) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)
		AND to_regclass('install_audit_events') IS NOT NULL
		AND to_regclass('install_audit_chain_head') IS NOT NULL
		AND to_regclass('install_audit_anchor_heads') IS NOT NULL`, InstallAuditWriterRole).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	for _, stmt := range []string{
		`GRANT INSERT ON install_audit_events TO ` + InstallAuditWriterRole,
		`GRANT SELECT ON install_audit_events, install_audit_chain_head TO ` + InstallAuditWriterRole,
		// The anchors' heads (migration 00068, ADR 0100 decision 4.1).
		`GRANT INSERT, SELECT ON install_audit_anchor_heads TO ` + InstallAuditWriterRole,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}

// GatewayRole is the NOLOGIN group role of koinon-gateway's database login (ADR 0097 decision 5,
// #278). It is the gateway's only database privilege: it reads what the leak guard decides on,
// writes the guard's own state and inserts the call log, and nothing else. It cannot read a
// provider key, a secret or an approval. On mcp_servers and connections it holds column-level grants
// that leave out the sealed credential columns (mcp_servers.credential, connections.secret_envelope):
// the gateway is the only process that may later hold the key to open them (ADR 0097 decision 5), so
// it needs no read of them, and a column added later is not readable until it is named here. The
// chart creates the role and makes the gateway's login a member (S4b of #278). The grant runs on every
// migrate and in --grants-only without waiting for the role, so a role that appears later gets its
// grants at the next run; until then the gateway refuses every org call (fail closed). The migrate
// role has no CREATEROLE.
const GatewayRole = "koinon_gateway"

// GatewayMCPServerColumns are the columns of mcp_servers the gateway may read: every column except the
// sealed `credential`. The guard's query GetServedMCPServerForGuard selects exactly these; specs in
// internal/mcpguard keep the query, the row conversion and the database equal to it.
var GatewayMCPServerColumns = []string{
	"org_id", "id", "name", "url", "transport", "credential_header", "credential_prefix", "source",
	"managed_by_secret", "declared_version", "removed_at", "disabled_reason", "health", "health_reason",
	"health_checked_at", "health_since", "consecutive_failures", "tools_synced_at", "created_by",
	"created_at", "updated_at", "kind", "exposes_untrusted_input", "exposes_private_data",
	"code_host_connection_id", "credential_lost_at", "admin_disabled_at", "admin_disabled_by",
	"admin_disabled_note", "admin_disabled_under_by", "admin_disabled_under_note", "ca_bundle",
	"insecure_skip_verify",
}

// GatewayConnectionColumns are the columns of connections the gateway may read. The guard joins
// connections to the code-host scope (ListOrgCodeHostRepos) and reads only these four: it never reads
// the sealed secret_envelope, the secret's name or the connection's config.
var GatewayConnectionColumns = []string{"org_id", "id", "removed_at", "secret_fingerprint"}

// gatewayGrants are the privileges of GatewayRole. Each line names why the gateway needs it.
var gatewayGrants = []string{
	// The guard identifies the server and reads the code-host connection's scope (mcpguard,
	// orgsettings.LeakGuardOf), by named columns: the sealed credential columns are not among them.
	`GRANT SELECT (` + strings.Join(GatewayMCPServerColumns, ", ") + `) ON mcp_servers TO ` + GatewayRole,
	`GRANT SELECT (` + strings.Join(GatewayConnectionColumns, ", ") + `) ON connections TO ` + GatewayRole,
	// The guard identifies the caller, reads the tool declarations, the org's leak-guard settings and the
	// code-host scope (mcpguard, orgsettings.LeakGuardOf).
	`GRANT SELECT ON orgs, agents, mcp_tools, runs, agent_sandboxes,
		org_settings, org_fetch_trusted_hosts, org_code_host_repos, org_code_host_readings TO ` + GatewayRole,
	// The guard's own state: sessions, fingerprints, open calls, the feed, the decision record.
	`GRANT SELECT, INSERT, UPDATE, DELETE ON leak_guard_sessions, leak_guard_fingerprints, leak_guard_runs, leak_guard_open_calls, leak_guard_decisions TO ` + GatewayRole,
	// The call log: insert only.
	`GRANT INSERT ON mcp_call_log TO ` + GatewayRole,
	// The gateway's own key (ADR 0097 12.3): the public rows it checks its private key against, the
	// install ID that binds a KMS-wrapped key, and its report that it loaded a key. The report is an
	// insert and a refresh of one column.
	`GRANT SELECT ON gateway_recipient_keys, install_identity, kek_bound_only TO ` + GatewayRole,
	`GRANT SELECT, INSERT ON gateway_recipient_key_loads TO ` + GatewayRole,
	`GRANT UPDATE (loaded_at) ON gateway_recipient_key_loads TO ` + GatewayRole,
}

// GatewayKeyAdminRole is the NOLOGIN group role of the gateway key-admin Job's login (ADR 0097 12.3,
// 12.4). It is the only role that may INSERT a public key or move its state. It has no read of the
// sealed credential column: it asks gateway_recipient_key_in_use for a count.
const GatewayKeyAdminRole = "koinon_gateway_key_admin"

// GatewayResealRole is the NOLOGIN group role of the re-seal Job's login (ADR 0097 12.4). Its grant is
// the one the compare-and-swap needs: SELECT on org_id, id and credential and UPDATE of credential on
// mcp_servers, and the public rows to find the active key. Nothing else.
const GatewayResealRole = "koinon_gateway_reseal"

var gatewayKeyAdminGrants = []string{
	// Insert a pending key, move its state, read the gateway's reports, and bind the wrapped key to the
	// install (the install ID and the bound-only marker are read by the wrapper).
	`GRANT SELECT, INSERT ON gateway_recipient_keys TO ` + GatewayKeyAdminRole,
	`GRANT UPDATE (state, activated_at, retiring_at, retired_at) ON gateway_recipient_keys TO ` + GatewayKeyAdminRole,
	`GRANT SELECT ON gateway_recipient_key_loads, install_identity, kek_bound_only TO ` + GatewayKeyAdminRole,
	`GRANT EXECUTE ON FUNCTION gateway_recipient_key_in_use(text) TO ` + GatewayKeyAdminRole,
}

var gatewayResealGrants = []string{
	`GRANT SELECT ON gateway_recipient_keys, install_identity, kek_bound_only TO ` + GatewayResealRole,
	`GRANT SELECT (org_id, id, credential) ON mcp_servers TO ` + GatewayResealRole,
	`GRANT UPDATE (credential) ON mcp_servers TO ` + GatewayResealRole,
}

// GrantGatewayKeyRoles gives GatewayKeyAdminRole and GatewayResealRole their privileges and returns the
// roles it granted. A role that does not exist is skipped (the chart creates them only with the
// gateway key switch), so the next migrate or --grants-only run grants it once it does.
func GrantGatewayKeyRoles(ctx context.Context, db Execer) ([]string, error) {
	var granted []string
	for _, r := range []struct {
		role   string
		grants []string
	}{{GatewayKeyAdminRole, gatewayKeyAdminGrants}, {GatewayResealRole, gatewayResealGrants}} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, r.role).Scan(&exists); err != nil {
			return granted, err
		}
		if !exists {
			continue
		}
		for _, stmt := range r.grants {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return granted, err
			}
		}
		granted = append(granted, r.role)
	}
	return granted, nil
}

// GrantGateway gives GatewayRole its privileges and reports whether it did: false when the role does
// not exist yet. Like GrantInstallAuditWriter it runs as the table owner after the goose migrations on
// every migrate and in "migrate --grants-only", and is idempotent: on a Helm upgrade of a
// CloudNativePG cluster the role may appear only after the migrate hook.
func GrantGateway(ctx context.Context, db Execer) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, GatewayRole).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	// An earlier version granted whole-table SELECT on mcp_servers and connections. Drop it, so a database
	// that has it ends with the column grants and nothing wider. Only when it is there: a revoke also
	// removes the column grants, and a running gateway must not lose them on every migrate.
	for _, table := range []string{"mcp_servers", "connections"} {
		var wide bool
		if err := db.QueryRowContext(ctx, `SELECT has_table_privilege($1, $2, 'SELECT')`, GatewayRole, table).Scan(&wide); err != nil {
			return false, err
		}
		if wide {
			if _, err := db.ExecContext(ctx, `REVOKE SELECT ON `+table+` FROM `+GatewayRole); err != nil {
				return false, err
			}
		}
	}
	for _, stmt := range gatewayGrants {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}
