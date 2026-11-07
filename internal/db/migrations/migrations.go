// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the goose SQL migrations. File names are
// zero-padded (NNNNN_name.sql) so goose and sqlc read them in the same order.
package migrations

import (
	"context"
	"database/sql"
	"embed"
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

// GrantInstallAuditWriter gives InstallAuditWriterRole its privileges on the install audit tables
// and reports whether the role exists. It runs after the goose migrations, on every migrate, as the
// table owner, and is idempotent. It is not part of migration 00066 because on a Helm upgrade of a
// CloudNativePG cluster the migrate hook runs before the updated Cluster (which adds the role as a
// managed role) reaches the API server: a migration that needs the role would abort the upgrade
// before the role can appear. When the role is missing nothing is granted and the caller warns; the
// next migrate (the next upgrade or Argo CD sync) grants once the role exists.
func GrantInstallAuditWriter(ctx context.Context, db Execer) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, InstallAuditWriterRole).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	for _, stmt := range []string{
		`GRANT INSERT ON install_audit_events TO ` + InstallAuditWriterRole,
		`GRANT SELECT ON install_audit_events, install_audit_chain_head TO ` + InstallAuditWriterRole,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}
