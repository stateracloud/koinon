// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	root := &cobra.Command{
		Use:   "audit",
		Short: "Audit-log tools",
		Long:  "Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api audit verify",
	}
	var dbURL, orgID, file, after string
	var installOnly bool
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Walk the audit hash chain (ADR 0023, 0100) and report the first broken link",
		Long: "Walks the audit hash chain of the install and of one org (default: every org) and reports the first " +
			"broken link; exits non-zero when a chain does not hold.\n\n" +
			"With --file it checks an export (koinon-api audit export) instead, with no database: the same hashing, " +
			"run anywhere. A partial export (--from-seq) is checked against the end of an earlier export given with --after.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file != "" {
				// A database URL is ignored here: the pod's DATABASE_URL reaches --database-url through
				// the root's environment binding, so it is no sign the operator asked for the database.
				if orgID != "" || installOnly {
					return errors.New("--file checks a file alone; it does not take --org or --install")
				}
				return verifyAuditFile(file, after, cmd.OutOrStdout())
			}
			if after != "" {
				return errors.New("--after needs --file")
			}
			if dbURL == "" {
				return errors.New(`required flag(s) "database-url" not set (or give --file to check an export)`)
			}
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
	verify.Flags().StringVar(&file, "file", "", "check this export file (\"-\" for stdin) instead of the database; needs no database")
	verify.Flags().StringVar(&after, "after", "", "with --file: an earlier export that ends where this partial export starts")
	cli.EnvAlias(verify, "database-url", "DATABASE_URL")
	root.AddCommand(verify)

	var scope, out string
	var fromSeq int64
	export := &cobra.Command{
		Use:   "export",
		Short: "Write an audit chain as JSON lines that verify off the cluster (ADR 0100)",
		Long: "Writes the install chain or one org's chain as JSON lines (format koinon.audit.export.v1): a header, then " +
			"one line per event in chain_seq order. Check a copy anywhere with koinon-api audit verify --file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := exportScope(scope, orgID)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if out != "" && out != "-" {
				f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the output file
				if err != nil {
					return fmt.Errorf("create %s: %w", out, err)
				}
				defer func() { _ = f.Close() }()
				w = f
			}
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			return audit.Export(cmd.Context(), store.New(pool), w, sc, fromSeq)
		},
	}
	export.Flags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	export.Flags().StringVar(&scope, "scope", "", "install or org")
	export.Flags().StringVar(&orgID, "org", "", "the org to export (with --scope org)")
	export.Flags().Int64Var(&fromSeq, "from-seq", 1, "start at this chain_seq (a partial export; verify it with --after)")
	export.Flags().StringVar(&out, "out", "-", "write to this new file instead of stdout")
	cli.EnvAlias(export, "database-url", "DATABASE_URL")
	mustRequire(export, "database-url", "scope")
	root.AddCommand(export)
	return root
}

// exportScope reads --scope and --org of "audit export".
func exportScope(scope, orgID string) (audit.ExportScope, error) {
	switch scope {
	case "install":
		if orgID != "" {
			return audit.ExportScope{}, errors.New("--org is for --scope org; the install chain has no org")
		}
		return audit.ExportScope{Install: true}, nil
	case "org":
		var org pgtype.UUID
		if err := org.Scan(orgID); orgID == "" || err != nil || !org.Valid {
			return audit.ExportScope{}, fmt.Errorf("--scope org needs --org <org id> (a UUID), got %q", orgID)
		}
		return audit.ExportScope{Org: org}, nil
	}
	return audit.ExportScope{}, fmt.Errorf("--scope must be install or org, got %q", scope)
}

// verifyAuditFile checks an export file, with no database. after is the path of an earlier export
// that a partial file continues. It returns audit.ErrBroken when the file does not hold.
func verifyAuditFile(path, after string, out io.Writer) error {
	var opt audit.ExportVerifyOptions
	if after != "" {
		if path == "-" && after == "-" {
			return errors.New("--file and --after cannot both be stdin")
		}
		prev, err := checkExportFile(after)
		if err != nil {
			return fmt.Errorf("--after %s: %w", after, err)
		}
		if !prev.OK() {
			audit.WriteExportReport(out, prev)
			return fmt.Errorf("--after %s does not verify: %w", after, audit.ErrBroken)
		}
		opt.After = &prev.End
	}
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the operator names the file to check
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	res, err := audit.VerifyExport(r, opt)
	if err != nil {
		return err
	}
	if !audit.WriteExportReport(out, res) {
		return audit.ErrBroken
	}
	return nil
}

// checkExportFile verifies one export file alone.
func checkExportFile(path string) (audit.ExportResult, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the file to check
	if err != nil {
		return audit.ExportResult{}, err
	}
	defer func() { _ = f.Close() }()
	return audit.VerifyExport(f, audit.ExportVerifyOptions{})
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

// installWriteFirstAttempt bounds the first, inline attempt of a startup write (startInstallWrite): a slow
// or hung database must not hold the start. An attempt that times out is an ordinary failure.
const installWriteFirstAttempt = 10 * time.Second

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
	state := installWriteState(first)
	if denied {
		slog.Error("the "+r.What+" is not recorded yet: the database refuses koinon-api's install audit event; retrying until the "+
			"install audit writer role is granted (koinon-api migrate --grants-only, the chart's post-upgrade hook)", "what", r.What, "err", first)
	} else {
		slog.Error("not recorded; retrying", "what", r.What, "err", first)
	}
	safego.GoOrStop(bg, slog.Default(), r.Goroutine, stop, func(ctx context.Context) {
		installWriteLoop(ctx, r.What, state, interval, write)
	})
	return nil
}

// startInstallWrite is retryInstallWrite for a write that has not run yet: it makes the first attempt
// itself, bounded by firstAttempt, so a database that does not answer cannot block the start. The
// background retries use the server's context, without that bound.
func startInstallWrite(bg context.Context, stop func(), r installRetry, interval, firstAttempt time.Duration, write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(bg, firstAttempt)
	defer cancel()
	return retryInstallWrite(bg, stop, r, interval, write(ctx), write)
}

// installWriteState names the state of a failed install write: "denied" when the chain refused it
// (audit.ErrInstallWriteDenied), "failed" for any other error.
func installWriteState(err error) string {
	if errors.Is(err, audit.ErrInstallWriteDenied) {
		return "denied"
	}
	return "failed"
}

// installWriteLoop calls write every interval until it succeeds or ctx ends. A line is logged on a
// change of state, not on every attempt: last is the state already logged for the first failure (the
// caller logged it), a retry that fails in the same state stays quiet, a refusal turning into another
// failure (or back) is a line of its own, and so is the write landing.
func installWriteLoop(ctx context.Context, what, last string, interval time.Duration, write func(context.Context) error) {
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
		state := installWriteState(err)
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
