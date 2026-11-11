// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/orcatalog"
)

// newModelMetadataCommand is "koinon-api model-metadata", the review of the per-install OpenRouter
// catalog (ADR 0070 amendment of 2026-11-11). The fetch itself is the price refresh's
// (--prices-refresh); this command lists and decides the context and output limits it proposes.
func newModelMetadataCommand() *cobra.Command {
	var dbURL string
	root := &cobra.Command{
		Use:   "model-metadata",
		Short: "Review the OpenRouter catalog this install fetched",
		Long: "While an enabled openrouter provider exists, koinon-api stores OpenRouter's model list in this " +
			"install's database (the same fetch as the price refresh, so --prices-refresh=off makes no request). " +
			"Display fields are stored at once. The context and output limits set compaction thresholds, so they wait " +
			"here for a person; until one is accepted, models.dev's openrouter values apply. An accepted limit is " +
			"recorded now and becomes the served one when the model-metadata reader (#265, #305) reads this catalog.",
	}
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Replaces the root's own hook (cobra runs the nearest one), so resolve the environment here.
		if err := cli.Resolve(cmd, nil); err != nil {
			return err
		}
		if dbURL == "" {
			return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
		}
		return nil
	}
	open := func(cmd *cobra.Command) (*orcatalog.Store, func(), error) {
		pool, err := pgxpool.New(cmd.Context(), dbURL)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to postgres: %w", err)
		}
		return orcatalog.New(pool), pool.Close, nil
	}

	proposals := &cobra.Command{Use: "proposals", Short: "List, accept and dismiss limit proposals"}
	var state string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List limit proposals, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			ps, err := s.ListProposals(cmd.Context(), state, limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, p := range ps {
				cur := "none (models.dev applies)"
				if p.CurrentValue != nil {
					cur = fmt.Sprint(*p.CurrentValue)
				}
				_, _ = fmt.Fprintf(w, "%s  %-10s %s %s: %s -> %d (%s)\n", p.ID, p.State, p.Model, p.Field, cur, p.ProposedValue, p.Source)
			}
			if len(ps) == 0 {
				_, _ = fmt.Fprintln(w, "no proposals")
			}
			return nil
		},
	}
	list.Flags().StringVar(&state, "state", "open", "open, accepted, dismissed or superseded (empty: all)")
	list.Flags().IntVar(&limit, "limit", 100, "how many to list")
	decide := func(use, short string, f func(*orcatalog.Store, *cobra.Command, string) error) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				s, closeFn, err := open(cmd)
				if err != nil {
					return err
				}
				defer closeFn()
				return f(s, cmd, args[0])
			},
		}
	}
	accept := decide("accept <proposal id>", "Accept the proposed limit (served once the model-metadata reader reads it, #265)",
		func(s *orcatalog.Store, cmd *cobra.Command, id string) error {
			return s.Accept(cmd.Context(), audit.CLI("model-metadata proposals accept"), id)
		})
	dismiss := decide("dismiss <proposal id>", "Close a proposal; the same value does not come back",
		func(s *orcatalog.Store, cmd *cobra.Command, id string) error {
			return s.Dismiss(cmd.Context(), audit.CLI("model-metadata proposals dismiss"), id)
		})
	proposals.AddCommand(list, accept, dismiss)
	root.AddCommand(proposals)
	return root
}
