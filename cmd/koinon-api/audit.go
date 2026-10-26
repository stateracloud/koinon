// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
)

const auditUsage = `usage: koinon-api audit <command> [flags]

commands:
  verify [--org ID]   walk the audit hash chain of one org (default: every org) and report the
                      first broken link; exits non-zero when a chain does not hold

Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api audit verify
`

// runAudit runs "koinon-api audit".
func runAudit(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(out, auditUsage)
		return nil
	}
	cmd, args := args[0], args[1:]
	if cmd != "verify" {
		return fmt.Errorf("unknown audit command %q\n%s", cmd, auditUsage)
	}
	fs := flag.NewFlagSet("koinon-api audit verify", flag.ContinueOnError)
	var dbURL, orgID string
	fs.StringVar(&dbURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&orgID, "org", "", "verify only this org's chain (default every org)")
	if err := cli.BindEnv(fs, nil); err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		return errors.New("--database-url is required")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()
	return verifyAudit(ctx, pool, orgID, out)
}

// verifyAudit verifies the chain of org (every org when empty) and prints the result. It returns
// audit.ErrBroken when a chain does not hold.
func verifyAudit(ctx context.Context, pool *pgxpool.Pool, orgID string, out io.Writer) error {
	var org pgtype.UUID
	if orgID != "" {
		if err := org.Scan(orgID); err != nil || !org.Valid {
			return fmt.Errorf("--org %q is not a UUID", orgID)
		}
	}
	results, err := audit.Verify(ctx, store.New(pool), org)
	if err != nil {
		return err
	}
	if !audit.WriteReport(out, results) {
		return audit.ErrBroken
	}
	return nil
}
