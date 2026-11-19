// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the goose SQL migrations. File names are
// zero-padded (NNNNN_name.sql) so goose and sqlc read them in the same order.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
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
// provider key, a secret or an approval. It can read the sealed (encrypted) credential columns of
// mcp_servers and connections, because the guard's queries select whole rows; the gateway is the only
// process that may later hold the key to open them (ADR 0097 decision 5), so a column-level grant
// follows when those queries name their columns. The chart creates the role and makes the
// gateway's login a member; the migrate role has no CREATEROLE.
const GatewayRole = "koinon_gateway"

// gatewayGrants are the privileges of GatewayRole. Each line names why the gateway needs it.
var gatewayGrants = []string{
	// The guard identifies the caller and the server, reads the tool declarations, the org's leak-guard
	// settings and the code-host connection's repository list (mcpguard, orgsettings.LeakGuardOf).
	`GRANT SELECT ON orgs, agents, mcp_servers, mcp_tools, runs, agent_sandboxes,
		org_settings, org_fetch_trusted_hosts, org_code_host_repos, org_code_host_readings, connections TO ` + GatewayRole,
	// The guard's own state: sessions, fingerprints, open calls, the feed, the decision record.
	`GRANT SELECT, INSERT, UPDATE, DELETE ON leak_guard_sessions, leak_guard_fingerprints, leak_guard_runs, leak_guard_open_calls, leak_guard_decisions TO ` + GatewayRole,
	// The call log: insert only.
	`GRANT INSERT ON mcp_call_log TO ` + GatewayRole,
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
	for _, stmt := range gatewayGrants {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}
