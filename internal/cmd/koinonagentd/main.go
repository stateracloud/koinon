// SPDX-License-Identifier: Apache-2.0

// Package koinonagentd is the command tree of koinon-agentd (ADR 0088); cmd/koinon-agentd only
// calls Main. It drives runs: it claims them from Postgres and speaks ACP to the
// harness in each agent's sandbox over OpenShell's non-TTY exec stream (ADR 0049, plan item
// AG1). It holds no credentials besides its own database and OpenShell client identities.
//
// Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper
// case with dashes as underscores (--gateway-url is KOINON_GATEWAY_URL).
package koinonagentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/mcpcheck"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/schemagate"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/pricing"
)

type config struct {
	databaseURL      string
	openshellAddr    string
	openshellTLSDir  string
	installMark      string
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

	// restoreHold keeps the process from claiming or driving any run (issue #462).
	restoreHold bool
}

// flags registers the settings of koinon-agentd.
func (c *config) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string")
	fs.StringVar(&c.openshellAddr, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "",
		"directory with the OpenShell client bundle (tls.crt, tls.key, ca.crt); empty connects without TLS")
	fs.StringVar(&c.installMark, "install-mark", "", "this install's random mark (the chart's koinon-install-mark Secret); agentd then refuses to exec in or upload to a sandbox another install owns")
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
	restorehold.Flag(fs, &c.restoreHold)
	fs.StringVar(&c.pool.SessionCwd, "session-cwd", "/sandbox/work", "harness session directory in the sandbox")
}

// NewRootCommand is koinon-agentd's command (ADR 0088). It has no subcommands: the Deployment runs
// it with flags and environment variables only. The chart's contract test (test/chartsmoke) builds
// it to check what the rendered container passes.
func NewRootCommand() *cobra.Command {
	var c config
	cmd := &cobra.Command{
		Use:   "koinon-agentd",
		Short: "Koinon's run driver",
		Long: "koinon-agentd drives runs: it claims them from Postgres and speaks ACP to the harness in each agent's " +
			"sandbox over OpenShell's non-TTY exec stream (ADR 0049). It holds no credentials besides its own " +
			"database and OpenShell client identities.\n\n" +
			"Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper case with " +
			"dashes as underscores (--gateway-url is KOINON_GATEWAY_URL). Secrets are read from files, never from flags.",
		Args:    cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error { return c.validate() },
		RunE:    func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), c) },
	}
	cli.Bind(cmd, cli.NewViper())
	cli.AllowEnv(cmd, "KOINON_MEMORY_LIMIT_BYTES")
	c.flags(cmd.Flags())
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	cli.EnvAlias(cmd, "owner-id", "POD_NAME")
	for _, n := range []string{"database-url", "openshell-address", "gateway-url"} {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("koinon-agentd: %v", err))
		}
	}
	return cmd
}

// validate holds the rules between flags, which cobra's required-flag check does not know.
func (c *config) validate() error {
	// MarkFlagRequired only checks that a flag was set, and an empty environment variable counts as
	// set, so an empty value is refused here. An empty database URL would make pgx fall back to the
	// libpq defaults.
	if c.databaseURL == "" {
		return errors.New("--database-url is required")
	}
	if c.openshellAddr == "" {
		return errors.New("--openshell-address is required")
	}
	if c.pool.GatewayURL == "" {
		return errors.New("--gateway-url is required")
	}
	if c.pool.OwnerID == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("owner id: %w", err)
		}
		c.pool.OwnerID = h
	}
	if c.otlpEndpoint != "" || c.otlpTokenFile != "" {
		if err := checkOTLP(c.otlpEndpoint, c.otlpTokenFile); err != nil {
			return err
		}
	}
	if c.checkpoints && (c.pool.SavedWorkURL == "" || c.savedWorkKeyFile == "") {
		return errors.New("--saved-work-url and --saved-work-key-file are required with --checkpoints: koinon-api hands out the repository passwords")
	}
	return nil
}

// checkOTLP checks the shape of the exporter's settings at start: both are set, and the endpoint is
// an http(s) URL without a path (the exporter adds /v1/traces). It does not open the token file,
// which is a mounted Secret the chart's contract test cannot see; run does, through readOTLPToken,
// before anything is served.
func checkOTLP(endpoint, tokenFile string) error {
	if endpoint == "" || tokenFile == "" {
		return errors.New("--otlp-endpoint and --otlp-token-file are required together: the OTLP listener refuses requests without its bearer token")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return errors.New("--otlp-endpoint must be an http(s) URL with a host and no path, for example http://koinon-api.koinon.svc:4318")
	}
	return nil
}

// readOTLPToken checks that the token file holds a valid token, so a missing token fails the pod at
// start instead of every export. The value is never reported.
func readOTLPToken(tokenFile string) error {
	if _, err := otlpauth.ReadToken(tokenFile); err != nil {
		return fmt.Errorf("--otlp-token-file: %w", err)
	}
	return nil
}

// Main runs koinon-agentd with args and returns the process exit code.
func Main(ctx context.Context, args []string) int {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
	return cli.Execute(ctx, NewRootCommand(), args)
}

func run(ctx context.Context, cfg config) error {
	if cfg.otlpEndpoint != "" {
		if err := readOTLPToken(cfg.otlpTokenFile); err != nil {
			return err
		}
	}
	db, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	audit.WarnSharedWriterLogin(ctx, db, "koinon-agentd", slog.Default())

	backend, err := cli.OpenShell{Address: cfg.openshellAddr, TLSDir: cfg.openshellTLSDir, ServerName: cfg.openshellName}.Dial()
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	if cfg.installMark != "" {
		backend.WithOwner(execution.OwnerID(cfg.installMark))
	} else {
		slog.Warn("no install mark: agentd does not check who owns a sandbox (set --install-mark)")
	}

	if cfg.savedWorkKeyFile != "" {
		cfg.pool.SavedWorkKey = func() ([]byte, error) { return os.ReadFile(cfg.savedWorkKeyFile) }
		if cfg.pool.SavedWorkURL != "" {
			// The live check of an agent's MCP tools (old issue 488) asks koinon-api, which holds the
			// agent's gateway key; koinon-agentd never does.
			cfg.pool.MCPTools = mcpcheck.Remote{HTTP: &http.Client{Timeout: time.Minute}, BaseURL: cfg.pool.SavedWorkURL, Key: cfg.pool.SavedWorkKey}
		}
	}
	if cfg.checkpoints {
		cp, err := checkpointsFrom(cfg)
		if err != nil {
			return err
		}
		cfg.pool.Checkpoints = cp
	}
	reg := metrics.NewRegistry()
	audit.RegisterMetrics(reg)
	cfg.pool.Metrics = agentd.NewMetrics(reg)
	restorehold.Metric(reg, cfg.restoreHold)
	// The MCP leak guard's feed (ADR 0091 decision 4): agentd fingerprints the sandbox's own tool
	// results in this process and stores the fingerprints only.
	cfg.pool.Taint = mcpguard.NewFeed(db, nil)
	cfg.pool.Prices = prices.NewResolver(pricing.Embedded())
	if err := metrics.Serve(ctx, cfg.metricsAddr, reg, slog.Default()); err != nil {
		return err
	}
	// The health listener outlives SIGTERM: agentd drains for up to the drain deadline, and a
	// liveness probe that fails meanwhile gets the pod killed. It stops once the drain is over.
	healthCtx, stopHealth := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHealth()
	if _, err := serveHealth(healthCtx, ctx, cfg.healthAddr, db, slog.Default()); err != nil {
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
	// The install binding (ADR 0110 decision 0). agentd owns the checkpoint target columns, which the
	// runtime guard of part b2 fills; today it claims the row and a held agentd re-asserts the hold.
	if cfg.installMark != "" {
		binding := installbinding.New(db, execution.OwnerID(cfg.installMark), cfg.restoreHold, slog.Default())
		if _, err := binding.Claim(ctx, installbinding.Targets{}); err != nil {
			// The schema may not be migrated yet; Run retries the claim and credentials are refused until it succeeds.
			slog.Default().Warn("install binding: first claim failed, retrying in the background", "err", err)
		}
		binding.StateMetric(reg)
		safego.Go(ctx, slog.Default(), "install-binding", binding.Run)
	}
	slog.Info("starting koinon-agentd", "version", project.Version, "owner", cfg.pool.OwnerID, "workers", cfg.pool.Workers)
	if cfg.restoreHold {
		// No worker starts: no run is claimed, no expired lease is taken over, no sandbox is touched and no
		// checkpoint is written. Only "koinon-api checkpoints resync" is served, because the runbook runs it
		// before the release. The health listener and /metrics above keep answering.
		slog.Warn("restore-hold: koinon-agentd claims no run and touches no sandbox until the hold is released (restoreHold=false in the chart); it only serves checkpoints resync")
		err = pool.RunHeld(ctx)
	} else {
		err = pool.Run(ctx)
	}
	stopHealth()
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
	// Never the key: its ID's fingerprint, to check a rotation took (the secret keys page of the docs site, project.SecretKeysDocURL).
	slog.Info("checkpoint storage", "bucket", s3.Bucket, "access_key", s3.AccessKeyFingerprint())
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
			Key: func() ([]byte, error) { return os.ReadFile(cfg.savedWorkKeyFile) },
		},
		Interval: cfg.checkpointInterval,
	}, nil
}

// serveHealth serves /healthz and /readyz on addr and returns the address it listens on (empty
// when addr is empty and nothing is served). koinon-agentd is not Ready while the database
// schema is outside the goose range this build supports (ADR 0085): a rollout of a release the
// schema has outgrown then stalls on the new pod instead of replacing working ones.
//
// The listener runs until life ends. term is the termination signal: from then on /readyz answers
// 503 (the pod leaves the Service) but /healthz keeps answering, because agentd is still draining
// and the kubelet must not kill a pod that is busy finishing its runs.
func serveHealth(life, term context.Context, addr string, db schemagate.Querier, log *slog.Logger) (string, error) {
	if addr == "" {
		return "", nil
	}
	gate := schemagate.New(db, schemagate.Range{Min: migrations.MinSupported, Latest: migrations.MustLatest()}, log)
	safego.Go(life, log, "schema-gate", gate.Run)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ready := gate.ReadyHandler()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if term.Err() != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		ready(w, r)
	})
	ln, err := (&net.ListenConfig{}).Listen(life, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("health listener: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	safego.Go(life, log, "health-shutdown", func(context.Context) {
		<-life.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(life), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})
	safego.Go(life, log, "health-listener", func(context.Context) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health listener failed", "addr", addr, "err", err)
		}
	})
	return ln.Addr().String(), nil
}
