// SPDX-License-Identifier: Apache-2.0

// Package koinonreconciler is the command tree of koinon-reconciler, which renders agents into
// OpenShell sandboxes, provider records and agentgateway keys (plan item R2, package
// internal/reconciler).
//
// Settings come from the chart's config file (--config, KOINON_CONFIG) and can be overridden
// by flags; every flag can also be set by an environment variable: KOINON_ plus the flag name in
// upper case with dashes as underscores (--idle-timeout is KOINON_IDLE_TIMEOUT). Secrets are
// never flags: the database password comes from PGPASSWORD, the OpenShell client certificate
// from files. The command tree (commands.go) follows ADR 0088; without a subcommand
// koinon-reconciler serves.
package koinonreconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/kekhealth"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/schemagate"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
	"github.com/stateracloud/koinon/pricing"
)

// Main runs koinon-reconciler with args and returns the process exit code.
func Main(ctx context.Context, args []string) int {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
	return cli.Execute(ctx, NewRootCommand(), args)
}

// serve is "koinon-reconciler serve".
func serve(ctx context.Context, cfg config) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// stop ends the process's work, as a signal does: a background goroutine that panics calls it.
	ctx, stop := context.WithCancel(ctx)
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
	audit.WarnSharedWriterLogin(ctx, pool, "koinon-reconciler", slog.Default())

	kube, err := kubeClient(cfg.kubeconfig)
	if err != nil {
		return err
	}

	backend, err := cli.OpenShell{Address: cfg.openshellAddress, TLSDir: cfg.openshellTLSDir}.Dial()
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	// The backend owns what it makes (issue 461): objects carry this install's mark, and an object
	// that carries another install's is never adopted, changed or deleted. The mark is a random
	// value the chart keeps in a Secret, not in Postgres, so a restored database does not share it.
	if cfg.installMark == "" {
		log.Warn("no install mark: OpenShell objects are not marked with this install and a second install on the gateway can adopt them (set --install-mark)")
	} else {
		backend.WithOwner(execution.OwnerID(cfg.installMark))
	}

	driver, err := newGatewayDriver(cfg, kube, log)
	if err != nil {
		return err
	}

	// koinon-api creates the KEK Secret on its first start, so on a fresh install it may not be there
	// (or mounted) yet: wait for it.
	// The pod is not ready while it waits, and the wait is logged at warn level every minute.
	var ready atomic.Bool
	// Also not ready while the schema is outside the goose range this build supports (ADR 0085).
	gate := schemagate.New(pool, schemagate.Range{Min: migrations.MinSupported, Latest: migrations.MustLatest()}, log)
	safego.Go(ctx, log, "schema-gate", gate.Run)
	if cfg.healthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
			if !ready.Load() {
				http.Error(w, "waiting for the key-encryption keys", http.StatusServiceUnavailable)
				return
			}
			gate.ReadyHandler()(w, r)
		})
		health := &http.Server{Addr: cfg.healthAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		safego.Go(ctx, log, "health-listener", func(context.Context) {
			if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("health listener failed", "err", err)
			}
		})
		defer func() { _ = health.Close() }()
	}
	// The KEKs are unwrapped as cfg.kekWrapping says (ADR 0085 decision 12): the default local KEK,
	// or an opted-in KMS. The reconciler only unwraps.
	var keyring *secretstore.Keyring
	wrappers, err := wrapping.New(ctx, cfg.kekWrapping, wrapping.Service, installid.BoundOnlySource(pool))
	if err != nil {
		return err
	}
	// KMS-wrapped KEKs are bound to the install ID the database holds (old issue 615).
	wrappers = wrappers.WithInstallID(installid.Source(pool))
	// The monitor sets koinon_kek_active_reachable from the watch loop; the reconciler records nothing.
	// The keyring loads before the metrics registry exists, so the gauge is registered below.
	kekMetrics := kekhealth.NewMetrics(nil)
	kekMonitor := &kekhealth.Monitor{Wrappers: wrappers, Metrics: kekMetrics, Log: log}
	for lastWarn := (time.Time{}); keyring == nil; {
		if keyring, err = secretstore.LoadDir(ctx, cfg.kekDir, wrappers, kekMonitor.Observer()); err != nil {
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
	safego.GoOrStop(ctx, log, "keyring-watch-dir", stop, func(ctx context.Context) {
		keyring.WatchDir(ctx, cfg.kekDir, wrappers, time.Minute, log, kekMonitor.Observer())
	})

	observer, err := newSandboxObserver(cfg, kube, log)
	if err != nil {
		return err
	}
	netpol := newSandboxNetworkPolicy(cfg, kube, log)

	reg := metrics.NewRegistry()
	audit.RegisterMetrics(reg)
	reg.MustRegister(kekMetrics.Collectors()...)
	if err := metrics.Serve(ctx, cfg.metricsAddr, reg, log); err != nil {
		return err
	}
	restorehold.Metric(reg, cfg.restoreHold)
	// The install binding (ADR 0110 decision 0): the reconciler owns no outward target column. It claims
	// the row when it is the first binary to start unheld, and a held reconciler re-asserts the hold.
	if cfg.installMark != "" {
		binding := installbinding.New(pool, execution.OwnerID(cfg.installMark), cfg.restoreHold, log)
		if _, err := binding.Claim(ctx, installbinding.Targets{}); err != nil {
			// The schema may not be migrated yet; Run retries the claim and credentials are refused until it succeeds.
			log.Warn("install binding: first claim failed, retrying in the background", "err", err)
		}
		binding.StateMetric(reg)
		safego.Go(ctx, log, "install-binding", binding.Run)
	}
	if cfg.restoreHold {
		// Nothing converges: no sandbox is created, stopped or recreated, no provider key is applied, and the
		// spend settler and the provider cost reconciliation do not run (issue #462).
		err = restorehold.Wait(ctx, log, "koinon-reconciler")
		log.Info("stopping koinon-reconciler")
		return err
	}
	// One price list for the reconciler and the settler.
	priceResolver := prices.NewResolver(pricing.Embedded())
	if err := catalog.ValidateAllowedHosts(splitHosts(cfg.egressHosts + "," + cfg.allowedHosts)); err != nil {
		return err
	}
	var supervisor reconciler.SupervisorSource
	if cfg.openshellConfigMap != "" {
		supervisor = reconciler.KubeSupervisorImage{Client: kube, Namespace: cfg.openshellConfigNS, ConfigMap: cfg.openshellConfigMap}
	} else {
		log.Warn("no OpenShell gateway ConfigMap is configured: a new supervisor image does not recreate existing sandboxes")
	}
	var gatewayIDs reconciler.GatewayIDSource
	if cfg.openshellConfigMap != "" {
		gatewayIDs = reconciler.KubeGatewayID{Client: kube, Namespace: cfg.openshellConfigNS, ConfigMap: cfg.openshellConfigMap}
	}
	r, err := reconciler.New(pool, backend, driver,
		reconciler.KubeSecrets{Client: kube, Namespace: cfg.namespace},
		reconciler.Config{
			Gateway:             endpoint,
			Prices:              priceResolver,
			Keys:                keyring,
			ProviderKeys:        reconciler.KubeProviderKeys{Client: kube, Namespace: cfg.namespace, Name: cfg.providerKeysSecret},
			ProviderKeyDir:      cfg.providerKeysDir,
			URLPolicy:           catalog.URLPolicy{Namespace: cfg.namespace, AllowedHosts: splitHosts(cfg.egressHosts + "," + cfg.allowedHosts)},
			ProviderKeySettle:   cfg.keySettle,
			Flavors:             cfg.flavors(),
			RuntimeClassName:    cfg.runtimeClass,
			SandboxResources:    &cfg.sandboxResources,
			IdleTimeout:         cfg.idleTimeout,
			ParkWarm:            cfg.parkWarm,
			ArchiveAfter:        cfg.archiveAfter,
			SandboxJobs:         cfg.sandboxJobs,
			KeepWarmPerOrg:      cfg.keepWarmPerOrg,
			Supervisor:          supervisor,
			GatewayID:           cfg.openshellGateway,
			GatewayIDs:          gatewayIDs,
			MaxRecreatesPerPass: cfg.maxRecreates,
			ResyncInterval:      cfg.resyncInterval,
			HomeRetention:       reconciler.Retention(cfg.homeRetention),
			Checkpoints:         cfg.checkpoints,
			ToolServers:         cfg.toolServers(),
			MCPAskURL:           cfg.mcpAskURL,
			// repo.read grants (plan item C2): tokens minted with the App key opened from the
			// connection's stored secret fields.
			GitHub: reconciler.GitHubApps{
				Keys:   keyring,
				Client: github.NewClient(github.Options{BaseURL: cfg.githubAPIURL}),
			},
			Observer:      observer,
			NetworkPolicy: netpol,
			Logger:        log,
			Metrics:       reconciler.NewMetrics(reg),
		})
	if err != nil {
		return err
	}
	// Responses cut short are settled with their provider's own usage (ADR 0056, amendment
	// 2026-10-28). It runs on every replica: a settlement is claimed with SKIP LOCKED.
	settler := &ledger.Settler{DB: pool, Keys: keyring, Prices: priceResolver, Log: log}
	if err := settler.Validate(); err != nil {
		return err
	}
	safego.Go(ctx, log, "spend-settler", settler.Run)
	// Direct provider keys with a cost API key are reconciled against the provider's cost API once
	// a day (ADR 0056, amendment 2026-11-06); one replica at a time (an advisory lock).
	costSources := providerusage.DefaultCosts(nil)
	costs := &ledger.CostReconciler{
		DB: pool, Keys: keyring, Sources: costSources, Log: log,
		Metrics: ledger.NewCostMetrics(reg, costSources.Types()),
	}
	safego.Go(ctx, log, "cost-reconciliation", costs.Run)
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

// newSandboxObserver builds the sandbox observer (ADR 0078), or nil when it is off.
func newSandboxObserver(cfg config, kube kubernetes.Interface, log *slog.Logger) (*reconciler.SandboxObserver, error) {
	if !cfg.sandboxObserver {
		log.Warn("the sandbox observer is off: a sandbox that cannot pull its image or keeps crashing shows only OpenShell's reason, so its wait cause stays unknown")
		return nil, nil
	}
	rc, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &reconciler.SandboxObserver{
		Kube: kube, Dynamic: dyn, GatewayID: cfg.openshellGateway, ClusterRole: cfg.sandboxObserverClusterRole,
		ServiceAccount: cfg.sandboxObserverServiceAccount, ServiceAccountNamespace: cfg.namespace, Log: log,
	}, nil
}

// newSandboxNetworkPolicy builds the sandbox default-deny NetworkPolicy writer (spike 8.15), or nil when it is off.
func newSandboxNetworkPolicy(cfg config, kube kubernetes.Interface, log *slog.Logger) *reconciler.SandboxNetworkPolicy {
	if !cfg.sandboxNetworkPolicy {
		log.Warn("the sandbox default-deny NetworkPolicy is off: sandbox namespaces rely on OpenShell's own policies alone")
		return nil
	}
	return &reconciler.SandboxNetworkPolicy{
		Kube: kube, GatewayID: cfg.openshellGateway, Name: cfg.sandboxNetworkPolicyName, Log: log,
	}
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
	if err := gateway.ValidateGuardURL(cfg.mcpGuardURL); err != nil {
		return nil, fmt.Errorf("--mcp-guard-url: %w", err)
	}
	if cfg.mcpGuardURL == "" {
		log.Warn("no --mcp-guard-url: the gateway refuses every call to an org's MCP server (the leak guard fails closed)")
	}
	switch cfg.gatewayMode {
	case project.GatewayStandalone:
		driver := &standalone.Driver{
			Options: standalone.Options{Port: cfg.gatewayPort, DatabaseURL: cfg.gatewayDBURL, GuardURL: cfg.mcpGuardURL},
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
			if cfg.otlpTokenFile == "" {
				return nil, errors.New("--gateway-access-log-otlp-token-file is required with --gateway-access-log-otlp: the OTLP receiver refuses requests without its bearer token")
			}
			driver.Options.AccessLogOTLP = &standalone.OTLPExport{HostPort: cfg.accessLogOTLP, TokenFile: cfg.otlpTokenFile}
		} else {
			log.Warn("no --gateway-access-log-otlp: the spend ledger records nothing and spend limits are not netted")
		}
		// Reloads are detected on the data plane, with the revision's probe key; agentgateway's
		// admin listener stays on localhost.
		// agentgateway reports a refused config only in its log, so the waiter reads the gateway
		// pods' logs for the reason (ADR 0050, amendment 2026-10-29).
		driver.Waiter = standalone.ProbeWaiter{
			GatewayURL: cfg.gatewayURL, StatsURL: cfg.gatewayStatsURL, Log: log,
			Reload: standalone.PodLogReload{Client: kube, Namespace: cfg.namespace, Deployment: cfg.gatewayDeployment},
		}
		return driver, nil
	case project.GatewayKubernetes:
		// FROZEN (ADR 0097 decision 4, ADR 0058, issue #274): Kubernetes mode keeps passing the
		// gateway behaviour suite and gets no new features. Koinon's own gateway never gets this
		// mode, and it is deleted with agentgateway in ADR 0097 step 3 (v0.3). Put new gateway behaviour in the
		// standalone driver and the ModelGateway contract only.
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
				GuardURL: cfg.mcpGuardURL,
			},
			Store:  gwk8s.DynamicStore{Client: dyn, FieldManager: reconciler.ManagedByValue},
			Events: kube,
			Waiter: standalone.ProbeWaiter{GatewayURL: cfg.gatewayURL, Log: log},
			Log:    log,
		}
		if cfg.accessLogOTLP != "" {
			if cfg.otlpTokenSecret == "" {
				return nil, errors.New("--gateway-access-log-otlp-token-secret is required with --gateway-access-log-otlp: the OTLP receiver refuses requests without its bearer token")
			}
			export := &standalone.OTLPExport{HostPort: cfg.accessLogOTLP, TokenSecret: cfg.otlpTokenSecret, TokenSecretKey: "token"}
			if _, _, err := export.SplitHostPort(); err != nil {
				return nil, fmt.Errorf("--gateway-access-log-otlp: %w; the token cannot be attached to a malformed receiver", err)
			}
			driver.Options.AccessLogOTLP = export
		} else {
			log.Warn("no --gateway-access-log-otlp: the spend ledger records nothing, so budgets cannot revoke keys")
		}
		return driver, nil
	}
	return nil, fmt.Errorf("gateway mode %q is not supported", cfg.gatewayMode)
}
