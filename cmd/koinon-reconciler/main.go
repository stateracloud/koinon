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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/secretstore"
)

func main() {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
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

	driver, err := newGatewayDriver(cfg, kube, log)
	if err != nil {
		return err
	}

	// koinon-api creates the KEK Secret on its first start, so on a fresh install it may not be there
	// (or mounted) yet: wait for it.
	// The pod is not ready while it waits, and the wait is logged at warn level every minute.
	var ready atomic.Bool
	if cfg.healthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
			if !ready.Load() {
				http.Error(w, "waiting for the key-encryption keys", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		health := &http.Server{Addr: cfg.healthAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("health listener failed", "err", err)
			}
		}()
		defer func() { _ = health.Close() }()
	}
	var keyring *secretstore.Keyring
	for lastWarn := (time.Time{}); keyring == nil; {
		if keyring, err = secretstore.LoadDir(cfg.kekDir); err != nil {
			if time.Since(lastWarn) >= time.Minute {
				log.Warn("waiting for the key-encryption keys (koinon-api creates them on its first start, or restore the KEK Secret)", "err", err)
				lastWarn = time.Now()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	ready.Store(true)
	go keyring.WatchDir(ctx, cfg.kekDir, time.Minute, log)

	reg := metrics.NewRegistry()
	if err := metrics.Serve(ctx, cfg.metricsAddr, reg, log); err != nil {
		return err
	}
	r, err := reconciler.New(pool, backend, driver,
		reconciler.KubeSecrets{Client: kube, Namespace: cfg.namespace},
		reconciler.Config{
			Gateway:           endpoint,
			Keys:              keyring,
			ProviderKeys:      reconciler.KubeProviderKeys{Client: kube, Namespace: cfg.namespace, Name: cfg.providerKeysSecret},
			ProviderKeyDir:    cfg.providerKeysDir,
			URLPolicy:         catalog.URLPolicy{Namespace: cfg.namespace, AllowedHosts: splitHosts(cfg.egressHosts + "," + cfg.allowedHosts)},
			ProviderKeySettle: cfg.keySettle,
			Flavors:           cfg.flavors(),
			RuntimeClassName:  cfg.runtimeClass,
			IdleTimeout:       cfg.idleTimeout,
			ParkWarm:          cfg.parkWarm,
			SandboxJobs:       cfg.sandboxJobs,
			ResyncInterval:    cfg.resyncInterval,
			HomeRetention:     reconciler.Retention(cfg.homeRetention),
			Checkpoints:       cfg.checkpoints,
			ToolServers:       cfg.toolServers(),
			MCPAskURL:         cfg.mcpAskURL,
			// repo.read grants (plan item C2): tokens minted with the App key opened from the
			// connection's stored secret fields.
			GitHub: reconciler.GitHubApps{
				Keys:   keyring,
				Client: github.NewClient(github.Options{BaseURL: cfg.githubAPIURL}),
			},
			Logger:  log,
			Metrics: reconciler.NewMetrics(reg),
		})
	if err != nil {
		return err
	}
	err = r.Run(ctx)
	log.Info("stopping koinon-reconciler")
	return err
}

func restConfig(kubeconfig string) (*rest.Config, error) {
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
	return rc, nil
}

func kubeClient(kubeconfig string) (kubernetes.Interface, error) {
	rc, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(rc)
}

func splitHosts(s string) []string {
	var out []string
	for h := range strings.SplitSeq(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// newGatewayDriver builds the ModelGateway for cfg.gatewayMode: standalone writes agentgateway's
// config file into a ConfigMap and detects the reload with a probe key; kubernetes applies native
// agentgateway CRDs next to the existing controller (plan item G2, ADR 0058) and confirms each
// revision with the same probe key.
func newGatewayDriver(cfg config, kube kubernetes.Interface, log *slog.Logger) (gateway.ModelGateway, error) {
	switch cfg.gatewayMode {
	case project.GatewayStandalone:
		driver := &standalone.Driver{
			Options: standalone.Options{Port: cfg.gatewayPort, DatabaseURL: cfg.gatewayDBURL},
			Deliverer: standalone.ConfigMapDeliverer{
				Client: kube, Namespace: cfg.namespace, Name: cfg.gatewayConfigMap, FieldManager: reconciler.ManagedByValue,
				OwnerDeployment: cfg.gatewayDeployment,
				// The labels of a ConfigMap the reconciler creates: a fixed set, with no instance or
				// chart label (the chart does not own the object, so GitOps tools must not claim it).
				Labels: map[string]string{
					"app.kubernetes.io/name":      project.Name,
					"app.kubernetes.io/component": "gateway",
					"app.kubernetes.io/part-of":   project.Name,
					project.Domain + "/owner":     "reconciler",
				},
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
		return driver, nil
	case project.GatewayKubernetes:
		rc, err := restConfig(cfg.kubeconfig)
		if err != nil {
			return nil, err
		}
		// A pass applies about a dozen objects and lists seven kinds; client-go's default of 5 requests
		// per second would make every Apply take seconds, and a key revocation with it.
		rc.QPS, rc.Burst = 50, 100
		dyn, err := dynamic.NewForConfig(rc)
		if err != nil {
			return nil, err
		}
		driver := &gwk8s.Driver{
			Options: gwk8s.Options{
				Namespace: cfg.namespace, GatewayName: cfg.gatewayName, GatewayClassName: cfg.gatewayClass,
				Port: cfg.gatewayPort, ProviderKeysSecret: cfg.providerKeysSecret, ManagedBy: reconciler.ManagedByValue,
			},
			Store:  gwk8s.DynamicStore{Client: dyn, FieldManager: reconciler.ManagedByValue},
			Events: kube,
			Waiter: standalone.ProbeWaiter{GatewayURL: cfg.gatewayURL, Log: log},
			Log:    log,
		}
		if cfg.accessLogOTLP != "" {
			driver.Options.AccessLogOTLP = &standalone.OTLPExport{HostPort: cfg.accessLogOTLP}
		} else {
			log.Warn("no --gateway-access-log-otlp: the spend ledger records nothing, so budgets cannot revoke keys")
		}
		return driver, nil
	}
	return nil, fmt.Errorf("gateway mode %q is not supported", cfg.gatewayMode)
}
