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
