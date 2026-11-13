// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/restorestatus"
)

// newRestoreCommand is "koinon-api restore", the operator's view of a restored copy (ADR 0110 decision
// 7, issue #516). It reads the database and the declarative Secrets and calls nothing outward, so it is
// safe on a held install.
func newRestoreCommand() *cobra.Command {
	var dbURL, installMark, namespace, kubeconfig string
	var asJSON, accept bool
	root := &cobra.Command{
		Use:   "restore",
		Short: "Inspect a restored install before its hold is released",
	}
	report := func(cmd *cobra.Command, bestEffort bool) (restorestatus.Report, error) {
		pool, err := pgxpool.New(cmd.Context(), dbURL)
		if err != nil {
			return restorestatus.Report{}, fmt.Errorf("connect to postgres: %w", err)
		}
		defer pool.Close()
		owner := ""
		if installMark != "" {
			owner = execution.OwnerID(installMark)
		}
		opt := restorestatus.Options{AcceptImporterDeletions: accept, BestEffort: bestEffort}
		kube, err := kubeClient(config{kubeconfig: kubeconfig, secretsNamespace: namespace})
		if err != nil {
			return restorestatus.Report{}, err
		}
		if kube != nil {
			opt.PlanRemovals = func(ctx context.Context) ([]installbinding.Removal, error) {
				return declarative.PlanRemovals(ctx, pool, kube, namespace)
			}
		}
		return restorestatus.Build(cmd.Context(), pool, owner, opt)
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show what the install would do outward and what its release starts",
		Long: "Prints the install binding state (same, copy or unknown) and whether a release is pending, the API tokens " +
			"and sessions this install refuses because another install issued them, every outward target with its " +
			"disabled switch, the removals the declarative importer would make, and what a release starts: the schedule " +
			"fires of the first tick, the DBOS workflows, the runs and the run limits that apply, per org. It reads the " +
			"database and the declarative Secrets (in a cluster). The install mark is read from --install-mark " +
			"(KOINON_INSTALL_MARK), the value the api Deployment carries.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := report(cmd, false)
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
	check := &cobra.Command{
		Use:   "release-check",
		Short: "Fail when a release would remove declared items and the removals are not accepted",
		Long: "The pre-upgrade check of the chart (the restore-release-check Job, ADR 0110 decision 1): prints the report and " +
			"exits non-zero when a release is pending and the importer plan holds a removal while " +
			"restore.acceptImporterDeletions is not true, so helm upgrade stops before any Deployment changes. It exits 0 " +
			"when the schema does not exist yet (a first install) and when nothing blocks.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			var have bool
			err = pool.QueryRow(cmd.Context(), `SELECT to_regclass('install_binding') IS NOT NULL AND to_regclass('restore_accepted_removals') IS NOT NULL`).Scan(&have)
			if err != nil {
				pool.Close()
				return err
			}
			if !have {
				pool.Close()
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "restore release check: the schema does not exist yet (a first install); nothing to check")
				return nil
			}
			// The gate reads only the binding row and the removal list. On a live install with no
			// pending release and no waiting removal the Job exits 0 here, so no unrelated read can block
			// an upgrade (ADR 0110 decision 1: the Job refuses nothing else).
			owner := ""
			if installMark != "" {
				owner = execution.OwnerID(installMark)
			}
			row, st, err := installbinding.New(pool, owner, false, nil).Read(cmd.Context())
			var waiting []installbinding.Removal
			if err == nil {
				waiting, _, err = installbinding.ListRemovals(cmd.Context(), pool)
			}
			pool.Close()
			if err != nil {
				return err
			}
			if !installbinding.Pending(owner, row, st != installbinding.StateUnknown) {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restore release check: no release is pending; nothing to check (%d removal(s) wait for restore.acceptImporterDeletions and block no upgrade)"+"\n", len(waiting))
				return nil
			}
			r, err := report(cmd, true)
			if err != nil {
				return err
			}
			restorestatus.Print(cmd.OutOrStdout(), r)
			if b := r.Blocking(); len(b) > 0 {
				return fmt.Errorf("the release would remove %d declared item(s) of the source (listed above) and restore.acceptImporterDeletions is not true: "+
					"apply their Secrets, or set restore.acceptImporterDeletions=true for this upgrade and remove it afterwards", len(b))
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&dbURL, "database-url", "", "Postgres connection string")
	root.PersistentFlags().StringVar(&installMark, "install-mark", "", "this install's mark (the Secret value the api Deployment carries); empty: the state is unknown")
	root.PersistentFlags().StringVar(&namespace, "secrets-namespace", "", "namespace of the declarative Secrets (default $POD_NAMESPACE); without Kubernetes access the importer plan is left out")
	root.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config when in a cluster")
	root.PersistentFlags().BoolVar(&accept, "restore-accept-importer-deletions", false, "restore.acceptImporterDeletions as the chart has it")
	cli.EnvAlias(root, "database-url", "DATABASE_URL")
	cli.EnvAlias(root, "secrets-namespace", "POD_NAMESPACE")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := cli.Resolve(cmd, nil); err != nil {
			return err
		}
		if dbURL == "" {
			return errors.New("--database-url is required (or KOINON_DATABASE_URL, or its alias DATABASE_URL)")
		}
		return nil
	}
	root.AddCommand(status, check)
	return root
}
