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
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

type keyAdminOptions struct {
	dbURL, kekSecret, namespace, kubeconfig, addID string
	create, rewrapUnbound, activateIfNoActive      bool
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
			"exist (--kek-create), and adds the KEK --add-key names when the key set does not hold it. Every KEK " +
			"it wraps is bound to the install ID in the database (old issue 615); with --rewrap-unbound it also wraps " +
			"again, bound, each KEK of the primary wrapping key that was wrapped before that. It is idempotent " +
			"and never activates (except with --activate-if-no-active, when there is no active KEK to replace), retires or rewraps values: koinon-api keys does. Needs --kek-backend other " +
			"than local and --database-url.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKeyAdmin(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.dbURL, "database-url", "", "Postgres connection string: the install ID every new KEK is bound to, and (with --kek-create) whether values exist, since a missing Secret is never re-created then")
	f.StringVar(&o.kekSecret, "kek-secret", "", "name of the KEK Secret")
	f.StringVar(&o.namespace, "namespace", "", "namespace of the KEK Secret")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	f.BoolVar(&o.create, "kek-create", false, "create the KEK Secret with one active KMS-wrapped KEK when it does not exist")
	f.StringVar(&o.addID, "add-key", "", "ID of a KEK to add, wrapped by the KMS, unless the key set holds it already")
	f.BoolVar(&o.activateIfNoActive, "activate-if-no-active", false, "with --add-key, when the KEK Secret has no usable active KEK (its active entry is missing or names a KEK the Secret no longer holds, after a lost KMS key), make the added KEK active; otherwise change nothing about which KEK is active")
	f.BoolVar(&o.rewrapUnbound, "rewrap-unbound", false, "wrap again, bound to the install ID, every KEK of the primary wrapping key that is not bound yet (wrapped before old issue 615); a binary older than old issue 615 cannot read the result")
	f.DurationVar(&o.wait, "wait", 5*time.Minute, "how long to wait for the database to come up and be migrated")
	o.wrapping.Flags(f)
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
	// The database is required for the install ID every new KEK is bound to (old issue 615).
	mustRequire(cmd, "kek-secret", "namespace", "database-url")
	return cmd
}

func runKeyAdmin(ctx context.Context, o keyAdminOptions, out io.Writer) error {
	pool, err := waitForPool(ctx, o.dbURL, o.wait)
	if err != nil {
		return err
	}
	defer pool.Close()
	// On a new install the first migration, which creates the install ID, may still be running.
	waitCtx, cancel := context.WithTimeout(ctx, o.wait)
	install, err := installid.Wait(waitCtx, pool, 2*time.Second)
	cancel()
	if err != nil {
		return err
	}
	wrappers, err := wrapping.New(ctx, o.wrapping, wrapping.KeyAdmin, installid.BoundOnlySource(pool))
	if err != nil {
		return err
	}
	wrappers = wrappers.WithInstallID(secretstore.StaticInstallID(install))
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

		ActivateIfNoActive: o.activateIfNoActive,
		RewrapUnbound:      o.rewrapUnbound,
		Marker:             installid.Marker{DB: pool},
		HasValues:          func(ctx context.Context) (bool, error) { return anyEnvelopeYet(ctx, pool) },
		UsedID:             func(ctx context.Context, id string) (bool, error) { return kekIDUsedYet(ctx, pool, id) },
		Out:                out,
		// The Job writes as the koinon_api database role, like koinon-api (the chart gives it the api
		// database Secret). It runs next to the migration Job, so it waits up to --wait for the install
		// audit chain to take the event. It can get the KEK Secret by name and nothing else, so its view of
		// the generated Secrets is that one.
		Audit: &keyscli.Auditor{
			Pool: pool, Actor: keyscli.ActorKeyAdmin, Wait: o.wait,
			Generated: keyscli.GeneratedReader{Client: client, Namespace: o.namespace, Names: []string{o.kekSecret}}.Read,
		},
	}
	_, _ = fmt.Fprintf(out, "Install ID %s: every KEK this Job wraps is bound to it.\n", install)
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

// kekIDUsedYet is keyscli.AddableID (envelopes, or an ID the audit chain ever recorded) for every KEK add
// path (keys add, the Job's plain add and the recovery). It must not guess: treating a missing table as
// empty (as anyEnvelopeYet does) would say "unused" for an ID that later tables still carry, so the check
// fails closed instead and the operator reruns the command once the migrations are done.
func kekIDUsedYet(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	used, err := keyscli.AddableID(ctx, pool, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return false, fmt.Errorf("cannot check whether KEK ID %q is still used: the database is not fully migrated yet; rerun this command once the migrations have finished (%s): %w", id, pgErr.Message, err)
	}
	return used, err
}
