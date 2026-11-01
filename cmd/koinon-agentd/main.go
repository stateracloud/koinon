// SPDX-License-Identifier: Apache-2.0

// Command koinon-agentd drives runs: it claims them from Postgres and speaks ACP to the
// harness in each agent's sandbox over OpenShell's non-TTY exec stream (ADR 0049, plan item
// AG1). It holds no credentials besides its own database and OpenShell client identities.
//
// Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper
// case with dashes as underscores (--gateway-url is KOINON_GATEWAY_URL).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/schemagate"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/pricing"
)

type config struct {
	databaseURL      string
	openshellAddr    string
	openshellTLSDir  string
	openshellName    string
	savedWorkKeyFile string
	metricsAddr      string
	healthAddr       string

	// The internal OTLP listener of koinon-api, where run spans go (ADR 0074): its base URL and
	// the file with the bearer token it requires. Validated at start; the exporter uses them.
	otlpEndpoint  string
	otlpTokenFile string
	pool          agentd.Config

	// Home checkpoints (plan item HC1). koinon-agentd is the writer: it holds the S3 credentials
	// (AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY) but no key-encryption key: it asks koinon-api
	// for each repository's password (--saved-work-url and --saved-work-key-file).
	checkpoints          bool
	checkpointScratch    string
	checkpointInterval   time.Duration
	s3Endpoint, s3Region string
	s3Bucket             string
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("koinon-agentd", flag.ContinueOnError)
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&c.openshellAddr, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "",
		"directory with the OpenShell client bundle (tls.crt, tls.key, ca.crt); empty connects without TLS")
	fs.StringVar(&c.openshellName, "openshell-server-name", "", "name checked against the gateway's certificate")
	fs.StringVar(&c.metricsAddr, "metrics-addr", metrics.DefaultPort, "listen address of /metrics (Prometheus); empty disables it")
	fs.StringVar(&c.healthAddr, "health-addr", ":8082", "listen address of /healthz and /readyz (not ready while the database schema is outside the supported goose range, ADR 0085); empty disables it")
	fs.StringVar(&c.otlpEndpoint, "otlp-endpoint", "",
		"base URL of koinon-api's internal OTLP/HTTP listener (for example http://koinon-api.koinon.svc:4318), where run spans are exported; empty: not exported")
	fs.StringVar(&c.otlpTokenFile, "otlp-token-file", "",
		"file with the bearer token that listener requires (the Secret koinon-otlp-token, key token, mounted read-only); required with --otlp-endpoint")
	fs.StringVar(&c.savedWorkKeyFile, "saved-work-key-file", "", "file with the key that signs saved-work posts (the Secret koinon-api creates, mounted read-only)")
	fs.StringVar(&c.pool.SavedWorkURL, "saved-work-url", "",
		"koinon-api's tools listener, where the uncommitted work of a stuck sandbox is posted to be sealed (ADR 0054); empty: not saved")
	fs.StringVar(&c.pool.GatewayURL, "gateway-url", "",
		"agentgateway's base URL as sandboxes reach it; flavors get <url>/v1 and <url>/mcp")
	fs.StringVar(&c.pool.DefaultModel, "default-model", "", "model of agents that name none")
	fs.StringVar(&c.pool.OwnerID, "owner-id", "", "this replica's name in run leases (default $POD_NAME, else the host name)")
	fs.IntVar(&c.pool.Workers, "workers", 8, "runs driven at once")
	fs.DurationVar(&c.pool.Lease, "lease", 30*time.Second, "run lease; another replica takes a run over when it expires")
	fs.DurationVar(&c.pool.DrainDeadline, "drain-deadline", agentd.DefaultDrainDeadline,
		"on SIGTERM, how long turns in progress get to finish before their runs are handed off (ADR 0085); the pod's terminationGracePeriodSeconds must be this plus 30s")
	fs.DurationVar(&c.pool.Heartbeat, "heartbeat", 0, "lease renewal and cancel check interval (default lease/3)")
	fs.DurationVar(&c.pool.Poll, "poll", time.Second, "how often idle workers look for runs")
	fs.DurationVar(&c.pool.SandboxTimeout, "sandbox-timeout", 5*time.Minute, "how long a run waits for its sandbox")
	fs.DurationVar(&c.pool.ModelServeTimeout, "model-serve-timeout", agentd.DefaultModelServeTimeout, "how long a run waits for the gateway to serve its model (a new provider still reaching the gateway) before it fails; keep it well above koinon-reconciler's --provider-key-settle plus a gateway apply")
	fs.DurationVar(&c.pool.SandboxWaitCap, "sandbox-wait-cap", time.Hour, "the total time a run waits for its sandbox, across requeues, before it fails")
	fs.DurationVar(&c.pool.CancelGrace, "cancel-grace", 10*time.Second, "how long a harness gets to end a cancelled turn")
	fs.IntVar(&c.pool.MaxTakeovers, "max-takeovers", 5, "fail a run taken over more often than this")
	fs.BoolVar(&c.checkpoints, "checkpoints", false, "save each agent's home to S3 (kopia) hourly, before stops and recreations, and restore it into recreated sandboxes")
	fs.StringVar(&c.checkpointScratch, "checkpoints-scratch-dir", "/var/lib/koinon/checkpoints", "scratch space for staging a home (about the size of the largest home)")
	fs.DurationVar(&c.checkpointInterval, "checkpoints-interval", time.Hour, "time between a sandbox's periodic checkpoints")
	fs.StringVar(&c.s3Endpoint, "checkpoints-s3-endpoint", "", "S3 endpoint of the checkpoint bucket, host:port or an http(s) URL")
	fs.StringVar(&c.s3Region, "checkpoints-s3-region", "", "S3 region")
	fs.StringVar(&c.s3Bucket, "checkpoints-s3-bucket", "", "S3 bucket of the checkpoints (credentials: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&c.pool.SessionCwd, "session-cwd", "/sandbox/work", "harness session directory in the sandbox")

	if err := cli.BindEnv(fs, nil); err != nil {
		return c, err
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.databaseURL == "" {
		c.databaseURL = os.Getenv("DATABASE_URL")
	}
	if c.pool.OwnerID == "" {
		c.pool.OwnerID = os.Getenv("POD_NAME")
	}
	if c.pool.OwnerID == "" {
		h, err := os.Hostname()
		if err != nil {
			return c, fmt.Errorf("owner id: %w", err)
		}
		c.pool.OwnerID = h
	}
	if c.otlpEndpoint != "" || c.otlpTokenFile != "" {
		if err := validateOTLP(c.otlpEndpoint, c.otlpTokenFile); err != nil {
			return c, err
		}
	}
	switch {
	case c.databaseURL == "":
		return c, errors.New("--database-url is required")
	case c.openshellAddr == "":
		return c, errors.New("--openshell-address is required")
	case c.pool.GatewayURL == "":
		return c, errors.New("--gateway-url is required")
	case c.checkpoints && (c.pool.SavedWorkURL == "" || c.savedWorkKeyFile == ""):
		return c, errors.New("--saved-work-url and --saved-work-key-file are required with --checkpoints: koinon-api hands out the repository passwords")
	}
	return c, nil
}

// validateOTLP checks the exporter's settings at start, so a missing token fails the pod instead of
// every export: the endpoint is an http(s) URL without a path (the exporter adds /v1/traces) and
// the token file holds a valid token. The value is never reported.
func validateOTLP(endpoint, tokenFile string) error {
	if endpoint == "" || tokenFile == "" {
		return errors.New("--otlp-endpoint and --otlp-token-file are required together: the OTLP listener refuses requests without its bearer token")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return errors.New("--otlp-endpoint must be an http(s) URL with a host and no path, for example http://koinon-api.koinon.svc:4318")
	}
	if _, err := otlpauth.ReadToken(tokenFile); err != nil {
		return fmt.Errorf("--otlp-token-file: %w", err)
	}
	return nil
}

func main() {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
	if err := run(); err != nil {
		slog.Error("koinon-agentd failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	backend, err := cli.OpenShell{Address: cfg.openshellAddr, TLSDir: cfg.openshellTLSDir, ServerName: cfg.openshellName}.Dial()
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()

	if cfg.savedWorkKeyFile != "" {
		cfg.pool.SavedWorkKey = func() ([]byte, error) { return os.ReadFile(cfg.savedWorkKeyFile) } //nolint:gosec // an operator-set path
	}
	if cfg.checkpoints {
		cp, err := checkpointsFrom(cfg)
		if err != nil {
			return err
		}
		cfg.pool.Checkpoints = cp
	}
	reg := metrics.NewRegistry()
	cfg.pool.Metrics = agentd.NewMetrics(reg)
	cfg.pool.Prices = prices.NewResolver(pricing.Embedded())
	if err := metrics.Serve(ctx, cfg.metricsAddr, reg, slog.Default()); err != nil {
		return err
	}
	if err := serveHealth(ctx, cfg.healthAddr, db, slog.Default()); err != nil {
		return err
	}
	if cfg.otlpEndpoint != "" {
		// Run spans (ADR 0074). The service only reads settings and decides runs: agentd holds no
		// key-encryption key and dials no destination; the router in koinon-api does.
		cfg.pool.Tracing = &agentd.Tracing{
			Decider:  agentd.TraceDeciderFrom(tracedest.NewWithClient(db, nil, catalog.URLPolicy{}, nil, slog.Default())),
			Endpoint: cfg.otlpEndpoint, TokenFile: cfg.otlpTokenFile,
		}
	}
	pool, err := agentd.New(db, backend, cfg.pool)
	if err != nil {
		return err
	}
	slog.Info("starting koinon-agentd", "version", project.Version, "owner", cfg.pool.OwnerID, "workers", cfg.pool.Workers)
	err = pool.Run(ctx)
	slog.Info("stopping koinon-agentd")
	return err
}

// checkpointsFrom builds the home checkpoint writer: the S3 bucket, the scratch space and the
// address of koinon-api, which hands out the repositories' passwords.
func checkpointsFrom(cfg config) (*agentd.Checkpoints, error) {
	s3 := checkpoint.S3{
		Endpoint: cfg.s3Endpoint, Region: cfg.s3Region, Bucket: cfg.s3Bucket,
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if err := s3.Validate(); err != nil {
		return nil, fmt.Errorf("checkpoint storage: %w", err)
	}
	if err := os.MkdirAll(cfg.checkpointScratch, 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint scratch directory: %w", err)
	}
	// kopia keeps a few files under the user's cache directory; the root file system is read-only.
	if err := os.Setenv("XDG_CACHE_HOME", cfg.checkpointScratch); err != nil {
		return nil, err
	}
	return &agentd.Checkpoints{
		Store: &checkpoint.Writer{Storage: s3, Scratch: cfg.checkpointScratch},
		Repositories: checkpoint.Remote{
			HTTP: &http.Client{Timeout: time.Minute}, BaseURL: cfg.pool.SavedWorkURL,
			Key: func() ([]byte, error) { return os.ReadFile(cfg.savedWorkKeyFile) }, //nolint:gosec // an operator-set path
		},
		Interval: cfg.checkpointInterval,
	}, nil
}

// serveHealth serves /healthz and /readyz on addr. koinon-agentd is not Ready while the database
// schema is outside the goose range this build supports (ADR 0085): a rollout of a release the
// schema has outgrown then stalls on the new pod instead of replacing working ones.
func serveHealth(ctx context.Context, addr string, db schemagate.Querier, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	gate := schemagate.New(db, schemagate.Range{Min: migrations.MinSupported, Latest: migrations.MustLatest()}, log)
	safego.Go(ctx, log, "schema-gate", gate.Run)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", gate.ReadyHandler())
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("health listener: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	safego.Go(ctx, log, "health-shutdown", func(context.Context) {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})
	safego.Go(ctx, log, "health-listener", func(context.Context) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health listener failed", "addr", addr, "err", err)
		}
	})
	return nil
}
