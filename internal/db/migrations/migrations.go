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

// CheckInstallAuditWriter reports what the database lacks for the install audit chain: the role
// InstallAuditWriterRole and, when member is not empty, member's membership in it, with its
// privileges inherited (koinon-api inserts as its login role, without SET ROLE). It needs no table,
// so "koinon-api migrate --require-install-writer" runs it before the migrations: a failed Job leaves
// the schema as it was. member is koinon-api's login role; the migration connects as another role and
// knows it only when told (--install-writer-member). It returns nil when nothing is missing.
func CheckInstallAuditWriter(ctx context.Context, db Execer, member string) error {
	var writer, memberExists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2)`, InstallAuditWriterRole, member).Scan(&writer, &memberExists); err != nil {
		return err
	}
	switch {
	case !writer:
		return fmt.Errorf("the role %s does not exist", InstallAuditWriterRole)
	case member == "":
		return nil
	case !memberExists:
		return fmt.Errorf("koinon-api's login role %s does not exist", member)
	}
	var inherits bool
	if err := db.QueryRowContext(ctx, `SELECT pg_has_role($1, $2, 'USAGE')`, member, InstallAuditWriterRole).Scan(&inherits); err != nil {
		return err
	}
	if !inherits {
		return fmt.Errorf("koinon-api's login role %s is not a member of %s (or does not inherit its privileges)", member, InstallAuditWriterRole)
	}
	return nil
}

// GrantInstallAuditWriter gives InstallAuditWriterRole its privileges on the install audit tables
// and reports whether it did: false when the role or the tables (migration 00066) do not exist yet.
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
		AND to_regclass('install_audit_chain_head') IS NOT NULL`, InstallAuditWriterRole).Scan(&exists); err != nil {
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
