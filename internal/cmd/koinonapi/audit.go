// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
)

// newAuditCommand is "koinon-api audit".
func newAuditCommand() *cobra.Command {
	audit := &cobra.Command{
		Use:   "audit",
		Short: "Audit-log tools",
		Long:  "Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api audit verify",
	}
	var dbURL, orgID string
	var installOnly bool
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Walk the audit hash chain (ADR 0023) and report the first broken link",
		Long: "Walks the audit hash chain of one org (default: every org) and reports the first broken link; " +
			"exits non-zero when a chain does not hold.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			return verifyAudit(cmd.Context(), pool, orgID, installOnly, cmd.OutOrStdout())
		},
	}
	verify.Flags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	verify.Flags().StringVar(&orgID, "org", "", "verify only this org's chain (default: the install chain and every org)")
	verify.Flags().BoolVar(&installOnly, "install", false, "verify only the install chain")
	cli.EnvAlias(verify, "database-url", "DATABASE_URL")
	mustRequire(verify, "database-url")
	audit.AddCommand(verify)
	return audit
}

// verifyAudit verifies the install chain and every org's chain, or one org's with orgID, or the
// install chain alone with installOnly, and prints the result. It returns audit.ErrBroken when a
// chain does not hold.
func verifyAudit(ctx context.Context, pool *pgxpool.Pool, orgID string, installOnly bool, out io.Writer) error {
	if installOnly && orgID != "" {
		return errors.New("--install and --org are exclusive")
	}
	var org pgtype.UUID
	if orgID != "" {
		if err := org.Scan(orgID); err != nil || !org.Valid {
			return fmt.Errorf("--org %q is not a UUID", orgID)
		}
	}
	q := store.New(pool)
	ok := true
	if orgID == "" {
		res, err := audit.VerifyInstall(ctx, q)
		if err != nil {
			return err
		}
		ok = audit.WriteInstallReport(out, res)
	}
	if !installOnly {
		results, err := audit.Verify(ctx, q, org)
		if err != nil {
			return err
		}
		if !audit.WriteReport(out, results) {
			ok = false
		}
	}
	if !ok {
		return audit.ErrBroken
	}
	return nil
}
