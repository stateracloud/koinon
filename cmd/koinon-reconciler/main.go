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
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/secretstore"
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

	backend, err := cli.OpenShell{Address: cfg.openshellAddress, TLSDir: cfg.openshellTLSDir}.Dial()
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
	if cfg.accessLogOTLP != "" {
		driver.Options.AccessLogOTLP = &standalone.OTLPExport{HostPort: cfg.accessLogOTLP}
	} else {
		log.Warn("no --gateway-access-log-otlp: the spend ledger records nothing and spend limits are not netted")
	}
	// Reloads are detected on the data plane, with the revision's probe key; agentgateway's
	// admin listener stays on localhost.
	driver.Waiter = standalone.ProbeWaiter{GatewayURL: cfg.gatewayURL, StatsURL: cfg.gatewayStatsURL, Log: log}

	keyring, err := secretstore.LoadDir(cfg.kekDir)
	if err != nil {
		return err
	}
	go keyring.WatchDir(ctx, cfg.kekDir, time.Minute, log)

	r, err := reconciler.New(pool, backend, driver,
		reconciler.KubeSecrets{Client: kube, Namespace: cfg.namespace},
		reconciler.Config{
			Gateway:           endpoint,
			Keys:              keyring,
			ProviderKeys:      reconciler.KubeProviderKeys{Client: kube, Namespace: cfg.namespace, Name: cfg.providerKeysSecret},
			ProviderKeyDir:    cfg.providerKeysDir,
			ProviderKeySettle: cfg.keySettle,
			Flavors:           cfg.flavors(),
			RuntimeClassName:  cfg.runtimeClass,
			IdleTimeout:       cfg.idleTimeout,
			ResyncInterval:    cfg.resyncInterval,
			HomeRetention:     reconciler.Retention(cfg.homeRetention),
			ToolServers:       cfg.toolServers(),
			// repo.read grants (plan item C2): tokens minted with the App key opened from the
			// connection's stored secret fields.
			GitHub: reconciler.GitHubApps{
				Keys:   keyring,
				Client: github.NewClient(github.Options{BaseURL: cfg.githubAPIURL}),
			},
			Logger: log,
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
