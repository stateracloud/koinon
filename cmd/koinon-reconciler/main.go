// SPDX-License-Identifier: Apache-2.0

// Command koinon-reconciler renders agents into OpenShell sandboxes, provider records and
// agentgateway keys (plan item R2, package internal/reconciler).
//
// Settings come from the chart's config file (--config, KOINON_CONFIG) and can be overridden
// by flags; every flag can also be set by an environment variable: KOINON_ plus the flag name
// in upper case with dashes as underscores (--idle-timeout is KOINON_IDLE_TIMEOUT). Secrets are
// never flags: the database password comes from PGPASSWORD, the OpenShell client certificate
// from files.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/procoduck/koinon/internal/execution/openshell"
	"github.com/procoduck/koinon/internal/gateway/standalone"
	"github.com/procoduck/koinon/internal/project"
	"github.com/procoduck/koinon/internal/reconciler"
)

func main() {
	if err := run(); err != nil {
		slog.Error("koinon-reconciler failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseConfig(os.Args[1:], os.LookupEnv)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting koinon-reconciler", "version", project.Version)

	endpoint, err := cfg.gatewayEndpoint()
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	kube, err := kubeClient(cfg.kubeconfig)
	if err != nil {
		return err
	}

	backend, err := dialOpenShell(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()

	if cfg.gatewayMode != project.GatewayStandalone {
		return fmt.Errorf("gateway mode %q is not implemented yet (plan item G2); use standalone", cfg.gatewayMode)
	}
	driver := &standalone.Driver{
		Options: standalone.Options{Port: cfg.gatewayPort, DatabaseURL: cfg.gatewayDBURL},
		Deliverer: standalone.ConfigMapDeliverer{
			Client: kube, Namespace: cfg.namespace, Name: cfg.gatewayConfigMap, FieldManager: reconciler.ManagedByValue,
		},
	}
	if cfg.gatewayAdminURL != "" {
		driver.Waiter = standalone.AdminWaiter{AdminURL: cfg.gatewayAdminURL, StatsURL: cfg.gatewayStatsURL}
	}

	r, err := reconciler.New(pool, backend, driver,
		reconciler.KubeSecrets{Client: kube, Namespace: cfg.namespace},
		reconciler.Config{
			Gateway:          endpoint,
			Models:           cfg.models,
			Flavors:          cfg.flavors(),
			RuntimeClassName: cfg.runtimeClass,
			IdleTimeout:      cfg.idleTimeout,
			ResyncInterval:   cfg.resyncInterval,
			HomeRetention:    reconciler.Retention(cfg.homeRetention),
			Logger:           log,
		})
	if err != nil {
		return err
	}
	err = r.Run(ctx)
	log.Info("stopping koinon-reconciler")
	return err
}

func kubeClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		rc  *rest.Config
		err error
	)
	if kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes client config: %w", err)
	}
	return kubernetes.NewForConfig(rc)
}

// openShellBackend is an OpenShell backend with the connection it owns.
type openShellBackend struct {
	*openshell.Backend
	conn *grpc.ClientConn
}

func (b openShellBackend) Close() error {
	if b.conn != nil {
		return b.conn.Close()
	}
	return b.Backend.Close()
}

func dialOpenShell(cfg config) (openShellBackend, error) {
	if cfg.openshellTLSDir != "" {
		b, err := openshell.Dial(openshell.Config{
			Endpoint: cfg.openshellAddress,
			CertFile: filepath.Join(cfg.openshellTLSDir, "tls.crt"),
			KeyFile:  filepath.Join(cfg.openshellTLSDir, "tls.key"),
			CAFile:   filepath.Join(cfg.openshellTLSDir, "ca.crt"),
		})
		return openShellBackend{Backend: b}, err
	}
	// The chart allows an empty client TLS Secret (for example a gateway behind a mesh that
	// terminates mTLS); then the connection is plaintext.
	conn, err := grpc.NewClient(cfg.openshellAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return openShellBackend{}, errors.Join(errors.New("connect to the OpenShell gateway"), err)
	}
	return openShellBackend{Backend: openshell.New(conn), conn: conn}, nil
}
