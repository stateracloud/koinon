// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// newOrgLimitsCommand is "koinon-api org-limits", the operator's setter for an org's run limits
// (issue #462). Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api org-limits
// show --org acme. It is the tool of the restore runbook: while the install is held
// (restoreHold), tighten an org's cap so a restored backlog does not all start at once on release.
func newOrgLimitsCommand() *cobra.Command {
	var dbURL, org string
	root := &cobra.Command{
		Use:   "org-limits",
		Short: "Show and set an org's run limits (concurrent runs, trigger queue share)",
		Long: "The limits are the org's cap on runs starting or running at once and its share of the trigger queue " +
			"(ADR 0066). The cap is read at every run claim, so a change also reaches runs that are already queued. " +
			"Every change is audited as org.run_limits.set.",
	}
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	root.PersistentFlags().StringVar(&org, "org", "", "the org's slug or ID")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// This hook replaces the root's own, which resolves flags from the environment (cobra runs only the
		// nearest persistent pre-run), so resolve here or DATABASE_URL is never read.
		if err := cli.Resolve(cmd, nil); err != nil {
			return err
		}
		if dbURL == "" {
			return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
		}
		if org == "" {
			return errors.New("--org is required: the org's slug or ID")
		}
		return nil
	}
	open := func(cmd *cobra.Command) (*orgsettings.Service, pgtype.UUID, func(), error) {
		pool, err := pgxpool.New(cmd.Context(), dbURL)
		if err != nil {
			return nil, pgtype.UUID{}, nil, fmt.Errorf("connect to postgres: %w", err)
		}
		id, err := resolveOrg(cmd.Context(), store.New(pool), org)
		if err != nil {
			pool.Close()
			return nil, pgtype.UUID{}, nil, err
		}
		return orgsettings.NewService(pool), id, pool.Close, nil
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "Print the org's effective run limits",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, id, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			l, err := svc.RunLimits(cmd.Context(), id)
			if err != nil {
				return err
			}
			printLimits(cmd.OutOrStdout(), l)
			return nil
		},
	}

	var maxRuns, trigConc, trigRate, creations int
	set := &cobra.Command{
		Use:   "set",
		Short: "Change the org's run limits; a limit you do not name stays, 0 returns it to the platform default",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var p orgsettings.RunLimitsPatch
			f := cmd.Flags()
			if f.Changed("max-concurrent-runs") {
				p.MaxConcurrentRuns = &maxRuns
			}
			if f.Changed("trigger-concurrency") {
				p.TriggerConcurrency = &trigConc
			}
			if f.Changed("trigger-rate-per-minute") {
				p.TriggerRatePerMinute = &trigRate
			}
			if f.Changed("sandbox-creations-per-minute") {
				p.SandboxCreationsPerMinute = &creations
			}
			if p == (orgsettings.RunLimitsPatch{}) {
				return errors.New("name a limit to change: --max-concurrent-runs, --trigger-concurrency, --trigger-rate-per-minute or --sandbox-creations-per-minute")
			}
			svc, id, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := svc.SetRunLimits(cmd.Context(), id, audit.CLI("org-limits set"), p); err != nil {
				return err
			}
			l, err := svc.RunLimits(cmd.Context(), id)
			if err != nil {
				return err
			}
			printLimits(cmd.OutOrStdout(), l)
			return nil
		},
	}
	set.Flags().IntVar(&maxRuns, "max-concurrent-runs", 0, fmt.Sprintf("runs the org may have starting or running at once (%d to %d; default %d)",
		orgsettings.MinMaxConcurrentRuns, orgsettings.MaxMaxConcurrentRuns, orgsettings.DefaultMaxConcurrentRuns))
	set.Flags().IntVar(&trigConc, "trigger-concurrency", 0, fmt.Sprintf("trigger, webhook and task-creation items of the org that run at once (%d to %d; default %d)",
		orgsettings.MinTriggerConcurrency, orgsettings.MaxTriggerConcurrency, orgsettings.DefaultTriggerConcurrency))
	set.Flags().IntVar(&trigRate, "trigger-rate-per-minute", 0, fmt.Sprintf("trigger items of the org that may start per minute (%d to %d; default %d)",
		orgsettings.MinTriggerRatePerMinute, orgsettings.MaxTriggerRatePerMinute, orgsettings.DefaultTriggerRatePerMinute))
	set.Flags().IntVar(&creations, "sandbox-creations-per-minute", 0, fmt.Sprintf("sandboxes the reconciler creates or starts for the org per minute; more wait (%d to %d; default %d)",
		orgsettings.MinSandboxCreationsPerMinute, orgsettings.MaxSandboxCreationsPerMinute, orgsettings.DefaultSandboxCreationsPerMinute))
	root.AddCommand(show, set)
	return root
}

func printLimits(w io.Writer, l orgsettings.RunLimits) {
	_, _ = fmt.Fprintf(w, "max-concurrent-runs          %d\ntrigger-concurrency          %d\ntrigger-rate-per-minute      %d\nsandbox-creations-per-minute %d\n",
		l.MaxConcurrentRuns, l.TriggerConcurrency, l.TriggerRatePerMinute, l.SandboxCreationsPerMinute)
}

// resolveOrg takes the --org value as an org ID, else as a slug.
func resolveOrg(ctx context.Context, q *store.Queries, ref string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(ref); err == nil && id.Valid {
		return id, nil
	}
	o, err := q.GetOrgBySlug(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("no org with the slug or ID %q", ref)
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	return o.ID, nil
}
