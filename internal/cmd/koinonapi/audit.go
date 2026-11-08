// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/safego"
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

// installWriteRetry is how often koinon-api retries a startup write that the install audit chain
// refused (retryInstallWrite).
const installWriteRetry = 30 * time.Second

// installRetry names a startup write that may be refused by the install audit chain.
type installRetry struct {
	// Goroutine names the background goroutine; What names the write in the log lines.
	Goroutine, What string
	// AnyError retries on every error, not only on a refused install write (the key-set check).
	AnyError bool
}

// retryInstallWrite is the one startup rule for a write that records an install audit event (ADR 0100):
// run it now; if the chain refuses it (audit.ErrInstallWriteDenied), retry it in the background until it
// lands, and do not fail the start. first is the error of the run that has just happened. It returns
// nil when the write landed or is now retried, and first when it failed for another reason (unless
// r.AnyError). On an upgrade of a CloudNativePG cluster the writer role is granted by the chart's
// post-upgrade hook, which Helm runs only once koinon-api is ready, so the start must not wait for it.
func retryInstallWrite(bg context.Context, stop func(), r installRetry, interval time.Duration, first error, write func(context.Context) error) error {
	if first == nil {
		return nil
	}
	denied := errors.Is(first, audit.ErrInstallWriteDenied)
	if !denied && !r.AnyError {
		return first
	}
	if denied {
		slog.Error("the "+r.What+" is not recorded yet: the database refuses koinon-api's install audit event; retrying until the "+
			"install audit writer role is granted (koinon-api migrate --grants-only, the chart's post-upgrade hook)", "what", r.What, "err", first)
	} else {
		slog.Error("not recorded; retrying", "what", r.What, "err", first)
	}
	safego.GoOrStop(bg, slog.Default(), r.Goroutine, stop, func(ctx context.Context) {
		installWriteLoop(ctx, r.What, interval, write)
	})
	return nil
}

// installWriteLoop calls write every interval until it succeeds or ctx ends. Each failed attempt is
// logged as an error, so the state is never silent; a change of state (landed, or a refusal turning into
// another failure) is a line of its own.
func installWriteLoop(ctx context.Context, what string, interval time.Duration, write func(context.Context) error) {
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		err := write(ctx)
		if err == nil {
			slog.Info("recorded after a retry", "what", what)
			return
		}
		if ctx.Err() != nil {
			return
		}
		state := "failed"
		if errors.Is(err, audit.ErrInstallWriteDenied) {
			state = "denied"
		}
		if state == last {
			continue
		}
		last = state
		if state == "denied" {
			slog.Error("still not recorded: the database refuses koinon-api's install audit event; "+
				"grant the install audit writer role (koinon-api migrate --grants-only, the chart's post-upgrade hook)", "what", what, "err", err)
			continue
		}
		slog.Error("still not recorded; retrying", "what", what, "err", err)
	}
}
