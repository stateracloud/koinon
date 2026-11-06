// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/stateracloud/koinon/internal/checkpointscli"
	"github.com/stateracloud/koinon/internal/cli"
)

// newCheckpointsCommand is "koinon-api checkpoints", the home checkpoint tools of the restore
// runbook (ADR 0085 decision 17).
func newCheckpointsCommand() *cobra.Command {
	checkpoints := &cobra.Command{
		Use:   "checkpoints",
		Short: "Home checkpoint tools",
		Long:  "Run it in a koinon-api pod: kubectl exec deploy/koinon-api -- koinon-api checkpoints resync --all",
	}
	var (
		dbURL     string
		all, wait bool
		agents    []string
		timeout   time.Duration
	)
	resync := &cobra.Command{
		Use:   "resync",
		Short: "After a database restore: rebuild the agents' home checkpoints from the snapshots in the bucket",
		Long: "Asks koinon-agentd to compare each agent's home checkpoints with the snapshots in its repositories: " +
			"a snapshot the database lacks becomes a checkpoint, and a checkpoint whose snapshot is gone is marked failed " +
			"(none is deleted). Each request is audited in the agent's org. With --wait (the default) the command waits " +
			"for koinon-agentd and prints a summary; it fails when a resync was given up or --timeout passed first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connect to postgres: %w", err)
			}
			defer pool.Close()
			t := &checkpointscli.Tool{Pool: pool, Out: cmd.OutOrStdout()}
			return t.Resync(cmd.Context(), all, agents, wait, timeout)
		},
	}
	f := resync.Flags()
	f.StringVar(&dbURL, "database-url", "", "Postgres connection string")
	f.BoolVar(&all, "all", false, "every agent with a home repository")
	f.StringSliceVar(&agents, "agent", nil, "an agent ID (repeat it, or separate IDs with commas)")
	f.BoolVar(&wait, "wait", true, "wait for koinon-agentd to resync each agent and print a summary")
	f.DurationVar(&timeout, "timeout", 30*time.Minute, "with --wait, how long to wait; the requests stay when it passes")
	resync.MarkFlagsMutuallyExclusive("all", "agent")
	resync.MarkFlagsOneRequired("all", "agent")
	cli.EnvAlias(resync, "database-url", "DATABASE_URL")
	mustRequire(resync, "database-url")
	checkpoints.AddCommand(resync)
	return checkpoints
}
