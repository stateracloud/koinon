// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// newPricesCommand is "koinon-api prices", the platform price list tools (ADR 0098). Run it in a
// koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api prices proposals list.
func newPricesCommand() *cobra.Command {
	var dbURL string
	root := &cobra.Command{
		Use:   "prices",
		Short: "Platform price list: fetch public sources, review proposals, roll back",
		Long: "The platform price list sits between an org's own prices and the default list built into the binary " +
			"(ADR 0098). fetch reads a public price source, import takes a file made by fetch --out (for an install " +
			"that cannot reach the source), proposals lists and decides what the source proposes, and platform rollback " +
			"restores an earlier version. Every decision is audited.",
	}
	open := func(cmd *cobra.Command) (*prices.Refresher, func(), error) {
		pool, err := pgxpool.New(cmd.Context(), dbURL)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to postgres: %w", err)
		}
		r, err := newRefresher(pool, nil)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		return r, pool.Close, nil
	}

	var out string
	fetch := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch the public price sources and make proposals (or write the snapshots with --out)",
		Long: "Fetches OpenRouter's model list (one unauthenticated GET; nothing about your install is sent). " +
			"Without --out it stores the proposals; nothing is applied, because a person decides here. With --out " +
			"it writes the snapshots to a file and touches no database, to carry to an install behind a firewall " +
			"and load there with \"prices import\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out != "" {
				return fetchToFile(cmd.Context(), out, cmd.OutOrStdout())
			}
			if dbURL == "" {
				return fmt.Errorf("--database-url is required without --out")
			}
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			res, err := r.Run(cmd.Context(), prices.TriggerCLI, false)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fetched %v, %d proposal(s) made or refreshed\n", res.Fetched, res.Proposals)
			return err
		},
	}
	fetch.Flags().StringVar(&out, "out", "", "write the snapshots to this file instead of the database (no database needed)")

	imp := &cobra.Command{
		Use:   "import <file>",
		Short: "Make proposals from a file written by \"prices fetch --out\"",
		Long: "Imported proposals are never applied automatically: you vouch for the file. Its snapshots go " +
			"through the same checks as a fetch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0]) // #nosec G304 -- the operator names the file
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			file, err := pricefetch.DecodeFile(f)
			if err != nil {
				return err
			}
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			n, err := r.Import(cmd.Context(), audit.CLI("prices import"), file)
			if err == nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "imported %d snapshot(s), %d proposal(s) made or refreshed\n", len(file.Snapshots), n)
			}
			return err
		},
	}

	proposals := &cobra.Command{Use: "proposals", Short: "List, accept and dismiss price proposals"}
	var state, class string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List proposals, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			ps, err := r.Service().ListProposals(cmd.Context(), state, class, limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, p := range ps {
				_, _ = fmt.Fprintf(w, "%s  %-12s %-12s %-9s %s (%s)\n", p.ID, p.State, p.Class, p.Source, p.Model, p.CreatedAt.Format(time.DateOnly))
				for _, c := range p.Changes {
					_, _ = fmt.Fprintf(w, "    %s: %s -> %s\n", c.Bucket, c.From, c.To)
				}
				if p.FailedRules != "" {
					_, _ = fmt.Fprintf(w, "    why: %s\n", p.FailedRules)
				}
			}
			if len(ps) == 0 {
				_, _ = fmt.Fprintln(w, "no proposals")
			}
			return nil
		},
	}
	list.Flags().StringVar(&state, "state", "open", "open, accepted, dismissed or superseded (empty: all)")
	list.Flags().StringVar(&class, "class", "", "auto, needs_review, confirm or refused (empty: all)")
	list.Flags().IntVar(&limit, "limit", 100, "how many to list")

	var confirm string
	accept := &cobra.Command{
		Use:   "accept <proposal id>",
		Short: "Apply a proposal to the platform list from now on",
		Long: "A proposal of class confirm (a change of more than 3x either way, or a rate above the sanity limit) " +
			"needs --confirm <model id>, typed out. A refused proposal cannot be accepted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			res, err := r.Accept(cmd.Context(), audit.CLI("prices proposals accept"), args[0], confirm)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "platform price version %d written\n", res.Version.Version)
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", w)
			}
			return nil
		},
	}
	accept.Flags().StringVar(&confirm, "confirm", "", "the model id, for a proposal of class confirm")

	dismiss := &cobra.Command{
		Use:   "dismiss <proposal id>",
		Short: "Close a proposal; the same values from the same source do not come back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			return r.Dismiss(cmd.Context(), audit.CLI("prices proposals dismiss"), args[0])
		},
	}
	proposals.AddCommand(list, accept, dismiss)

	platform := &cobra.Command{Use: "platform", Short: "The platform price list"}
	rollback := &cobra.Command{
		Use:   "rollback <version>",
		Short: "Write a new version equal to an earlier one, from now on",
		Long:  "Versions are never edited or deleted; a rollback is a new version that copies an earlier one.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v int64
			if _, err := fmt.Sscanf(args[0], "%d", &v); err != nil || v < 1 {
				return fmt.Errorf("version %q is not a version number", args[0])
			}
			r, closeFn, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			nv, err := r.Service().Rollback(cmd.Context(), audit.CLI("prices platform rollback"), v)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "platform price version %d written, equal to version %d\n", nv.Version, v)
			return nil
		},
	}
	platform.AddCommand(rollback)

	root.AddCommand(fetch, imp, proposals, platform)
	// --database-url is persistent: "prices fetch --out" needs none, every other subcommand does.
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if dbURL == "" && (cmd.Name() != "fetch" || out == "") {
			return fmt.Errorf("--database-url is required (or KOINON_DATABASE_URL)")
		}
		return nil
	}
	return root
}

// newRefresher builds the refresher over the OpenRouter adapter. metrics may be nil.
func newRefresher(pool *pgxpool.Pool, metrics *prices.Metrics) (*prices.Refresher, error) {
	return prices.NewRefresher(prices.New(pool, slog.Default(), pricing.Embedded()), prices.RefreshConfig{
		Adapters: []pricefetch.Adapter{pricefetch.NewOpenRouter(httpClient())},
		Metrics:  metrics,
	})
}

func httpClient() *http.Client { return &http.Client{Timeout: 60 * time.Second} }

// fetchToFile fetches every source and writes the snapshots to path, without a database.
func fetchToFile(ctx context.Context, path string, w io.Writer) error {
	a := pricefetch.NewOpenRouter(httpClient())
	snap, err := a.Fetch(ctx, time.Now())
	if err != nil {
		return err
	}
	if err := pricefetch.CheckFloor(snap, pricefetch.OpenRouterMinModels, 0); err != nil {
		return err
	}
	b, err := pricefetch.EncodeFile([]pricefetch.Snapshot{snap})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "wrote %d models from %s to %s\n", snap.Count, snap.Source, path)
	return err
}
