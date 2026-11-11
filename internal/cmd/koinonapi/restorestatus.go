// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/restorestatus"
)

// newRestoreCommand is "koinon-api restore", the operator's view of a restored copy (ADR 0110 decision
// 7, issue #516). It reads the database only and calls nothing outward, so it is safe on a held install.
func newRestoreCommand() *cobra.Command {
	var dbURL, installMark string
	var asJSON bool
	root := &cobra.Command{
		Use:   "restore",
		Short: "Inspect a restored install before its hold is released",
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what the install would do outward and what its release starts",
		Long: "Prints the install binding state (same, copy or unknown), the API tokens and sessions this install refuses " +
			"because another install issued them, every outward target with its disabled switch, and the runs a release " +
			"picks up. It reads only the database. The install mark is read from --install-mark (KOINON_INSTALL_MARK), " +
			"the value the api Deployment carries.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			owner := ""
			if installMark != "" {
				owner = execution.OwnerID(installMark)
			}
			r, err := restorestatus.Build(cmd.Context(), pool, owner)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			restorestatus.Print(cmd.OutOrStdout(), r)
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	root.PersistentFlags().StringVar(&installMark, "install-mark", "", "this install's mark (the Secret value the api Deployment carries); empty: the state is unknown")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := cli.Resolve(cmd, nil); err != nil {
			return err
		}
		if dbURL == "" {
			return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
		}
		return nil
	}
	root.AddCommand(status)
	return root
}
