// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

type keyAdminOptions struct {
	dbURL, kekSecret, namespace, kubeconfig, addID string
	create                                         bool
	wait                                           time.Duration
	wrapping                                       wrapping.Config
}

// newKeyAdminCommand is "koinon-api key-admin", the chart's key-admin Job (ADR 0085 decision 12):
// the only identity that wraps a new KEK with the KMS. koinon-api and the reconciler only decrypt.
func newKeyAdminCommand() *cobra.Command {
	var o keyAdminOptions
	cmd := &cobra.Command{
		Use:   "key-admin",
		Short: "Wrap new KEKs with the KMS (the key-admin Job)",
		Long: "Runs as the chart's key-admin Job, with its own service account, the only one the KMS lets " +
			"encrypt. It creates the KEK Secret with one active KEK wrapped by the KMS when the Secret does not " +
			"exist (--kek-create), and adds the KEK --add-key names when the key set does not hold it. It is " +
			"idempotent and never activates, rewraps or retires: koinon-api keys does. Needs --kek-backend " +
			"other than local.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKeyAdmin(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.dbURL, "database-url", "", "Postgres connection string; required with --kek-create (a missing Secret is never re-created while values exist)")
	f.StringVar(&o.kekSecret, "kek-secret", "", "name of the KEK Secret")
	f.StringVar(&o.namespace, "namespace", "", "namespace of the KEK Secret")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	f.BoolVar(&o.create, "kek-create", false, "create the KEK Secret with one active KMS-wrapped KEK when it does not exist")
	f.StringVar(&o.addID, "add-key", "", "ID of a KEK to add, wrapped by the KMS, unless the key set holds it already")
	f.DurationVar(&o.wait, "wait", 5*time.Minute, "how long to wait for the database to come up")
	o.wrapping.Flags(f)
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
	mustRequire(cmd, "kek-secret", "namespace")
	return cmd
}

func runKeyAdmin(ctx context.Context, o keyAdminOptions, out io.Writer) error {
	if o.create && o.dbURL == "" {
		return errors.New("--database-url is required with --kek-create")
	}
	wrappers, err := wrapping.New(ctx, o.wrapping, wrapping.KeyAdmin)
	if err != nil {
		return err
	}
	rc, err := restConfig(o.kubeconfig)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	job := keyscli.KeyAdmin{
		Store:  keyscli.KubeStore{Client: client, Namespace: o.namespace, Name: o.kekSecret, Wrappers: wrappers},
		Create: o.create,
		AddID:  o.addID,
		Out:    out,
	}
	if o.create {
		pool, err := waitForPool(ctx, o.dbURL, o.wait)
		if err != nil {
			return err
		}
		defer pool.Close()
		job.HasValues = func(ctx context.Context) (bool, error) { return anyEnvelopeYet(ctx, pool) }
	}
	return job.Run(ctx)
}

// waitForPool connects to the database, waiting up to wait for it: on a fresh install it may still
// be starting.
func waitForPool(ctx context.Context, dsn string, wait time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err = pool.Ping(waitCtx)
		if err == nil {
			return pool, nil
		}
		slog.Info("waiting for the database", "err", err)
		select {
		case <-waitCtx.Done():
			pool.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// anyEnvelopeYet is keyscli.AnyEnvelope on a database that may not be migrated yet (the key-admin Job
// and the first migration run side by side on a new install): a table that does not exist holds no
// values.
func anyEnvelopeYet(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	found, err := keyscli.AnyEnvelope(ctx, pool)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return false, nil
	}
	return found, err
}
