// SPDX-License-Identifier: Apache-2.0

// Package koinongateway is the command tree of koinon-gateway (ADR 0088); cmd/koinon-gateway only
// calls Main. It is Koinon's own gateway, MCP path (ADR 0097 step 2, issue 278): an aggregating MCP
// server (package mcpgateway) that follows the gateway State stream of koinon-api, so each replica
// serves the newest config the reconciler published and confirms each revision it applied.
//
// The process holds no database login, no key-encryption key and no Kubernetes API access. Its only
// inputs are the State stream (a bearer token, read from a file on every request), the credential
// files of the MCP targets (a mounted Secret) and its listen addresses.
//
// Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper case
// with dashes as underscores (--state-url is KOINON_STATE_URL).
package koinongateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/stateclient"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpgateway/pgaudit"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/safego"
)

// mcpPath is where the MCP endpoint is served. It is the path agentgateway served it at, so an
// agent's URL does not change with the driver.
const mcpPath = "/mcp"

type config struct {
	listenAddr  string
	healthAddr  string
	metricsAddr string

	stateURL       string
	stateTokenFile string
	podName        string
	// databaseURLFile holds the connection string of the gateway's own database login (role
	// migrations.GatewayRole). Empty: no guard, so org targets are not served (fail closed).
	databaseURLFile string

	drain time.Duration
	// shutdownDelay is how long the gateway keeps serving after SIGTERM, with /readyz at 503.
	shutdownDelay time.Duration
}

func (c *config) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.listenAddr, "listen-addr", ":8080", "address of the MCP listener, where agents connect with their gateway key (path "+mcpPath+")")
	fs.StringVar(&c.healthAddr, "health-addr", ":15021", "address of the health listener (/healthz, /readyz); empty disables it")
	fs.StringVar(&c.metricsAddr, "metrics-addr", ":15020", "address of the Prometheus listener (/metrics); empty disables it")
	fs.StringVar(&c.stateURL, "state-url", "",
		"base URL of koinon-api's internal listener that serves the gateway State stream (for example http://koinon-api.koinon.svc:4318)")
	fs.StringVar(&c.stateTokenFile, "state-token-file", "",
		"file with the gateway's bearer token for that listener (the OTLP token Secret, key token, mounted read-only); read on every request, so a rotation needs no restart")
	fs.StringVar(&c.databaseURLFile, "database-url-file", "",
		"file with the Postgres connection string of the gateway's own login (a member of the role "+migrations.GatewayRole+"). "+
			"With it the leak guard runs in the process and org MCP targets are served; without it the gateway serves platform targets only")
	fs.StringVar(&c.podName, "pod-name", "", "base of this process's copy id on the State stream (the pod name); a random boot nonce is appended")
	fs.DurationVar(&c.shutdownDelay, "shutdown-delay", 5*time.Second,
		"on SIGTERM /readyz turns 503 at once and the gateway keeps serving for this long, so the Service and clients stop sending requests to it; then it drains")
	fs.DurationVar(&c.drain, "drain-timeout", 30*time.Second,
		"on SIGTERM the gateway refuses new requests, then lets calls in flight finish for up to this long")
}

// validate holds the rules between flags, which cobra's required-flag check does not know.
func (c *config) validate() error {
	if c.stateURL == "" || c.stateTokenFile == "" {
		return errors.New("--state-url and --state-token-file are required: the gateway has no config without the State stream")
	}
	u, err := url.Parse(c.stateURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return errors.New("--state-url must be an http(s) URL with a host")
	}
	if c.listenAddr == "" {
		return errors.New("--listen-addr must not be empty")
	}
	if c.shutdownDelay < 0 {
		return errors.New("--shutdown-delay must not be negative")
	}
	if c.drain <= 0 {
		return errors.New("--drain-timeout must be positive")
	}
	return nil
}

// NewRootCommand returns the koinon-gateway command.
func NewRootCommand() *cobra.Command {
	c := &config{}
	cmd := &cobra.Command{
		Use:   "koinon-gateway",
		Short: "Koinon's gateway, MCP path",
		Long: "koinon-gateway serves the MCP path of Koinon's gateway (ADR 0097 step 2): agents connect with their gateway key, " +
			"and the gateway forwards each tool call to the MCP server that owns the tool, after the grant check and the guard. " +
			"It follows the State stream of koinon-api for its config and holds no key-encryption key. Its only database access is the leak guard's own login (--database-url-file).\n\n" +
			"Every flag can also be set by an environment variable: KOINON_ plus the flag name in upper case with " +
			"dashes as underscores (--state-url is KOINON_STATE_URL). Secrets are read from files, never from flags.",
		Args:    cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error { return c.validate() },
		RunE:    func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), *c) },
	}
	cli.Bind(cmd, cli.NewViper())
	cli.AllowEnv(cmd, "KOINON_MEMORY_LIMIT_BYTES")
	c.flags(cmd.Flags())
	cli.EnvAlias(cmd, "pod-name", "POD_NAME")
	for _, n := range []string{"state-url", "state-token-file"} {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("koinon-gateway: %v", err))
		}
	}
	return cmd
}

// Main runs koinon-gateway with args and returns the process exit code.
func Main(ctx context.Context, args []string) int {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
	return cli.Execute(ctx, NewRootCommand(), args)
}

// readToken reads the bearer token from file, trimmed of the trailing newline.
func readToken(file string) (string, error) { return readSecretFile(file, "the State stream token") }

// readSecretFile reads a secret from file, trimmed. An error names the file, never its content.
func readSecretFile(file, what string) (string, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the path is the operator's flag
	if err != nil {
		return "", fmt.Errorf("read %s: %w", what, err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the file of %s is empty", what)
	}
	return t, nil
}

func run(ctx context.Context, cfg config) error {
	log := slog.Default()
	if _, err := readToken(cfg.stateTokenFile); err != nil {
		return err // fail at start, not on the first request
	}
	reg := metrics.NewRegistry()
	gopts := mcpgateway.Options{
		Metrics: mcpgateway.NewMetrics(reg),
		Tracer:  otel.Tracer("koinon-gateway"),
		Audit:   logAudit{log: log},
		Logger:  log,
	}
	if cfg.databaseURLFile != "" {
		guard, audit, closeDB, err := openGuard(ctx, cfg.databaseURLFile, log, reg)
		if err != nil {
			return err
		}
		defer closeDB()
		gopts.Guard, gopts.Audit = guard, multiAudit{logAudit{log: log}, audit}
	}
	gw := mcpgateway.New(gopts)
	copyID, err := stateclient.NewCopyID(cfg.podName)
	if err != nil {
		return err
	}
	sc, err := stateclient.New(stateclient.Config{
		URL: cfg.stateURL, Token: func() (string, error) { return readToken(cfg.stateTokenFile) },
		CopyID: copyID, MCP: gw, Metrics: stateclient.NewMetrics(reg), Log: log,
	})
	if err != nil {
		return err
	}
	if err := metrics.Serve(ctx, cfg.metricsAddr, reg, log); err != nil {
		return err
	}

	// The health listener outlives SIGTERM, as agentd's does: a liveness probe that fails while the
	// gateway drains would get the pod killed. /readyz answers 503 from the signal on, so the pod
	// leaves the Service first.
	healthCtx, stopHealth := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHealth()
	if err := serveHealth(healthCtx, ctx, cfg.healthAddr, gw, log); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(mcpPath, gw.Handler())
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("MCP listener: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	served := make(chan error, 1)
	safego.Go(ctx, log, "mcp-listener", func(context.Context) { served <- srv.Serve(ln) })
	safego.Go(ctx, log, "mcp-sweep", func(ctx context.Context) {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				gw.Sweep()
			}
		}
	})
	safego.Go(ctx, log, "state-stream", func(ctx context.Context) { _ = sc.Run(ctx) })
	log.Info("starting koinon-gateway", "version", project.Version, "listen", ln.Addr().String(), "copy", copyID)

	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("MCP listener: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	// Endpoint removal lags behind SIGTERM: /readyz is 503 already, and the gateway keeps serving
	// until the Service and the clients have stopped sending requests here.
	log.Info("shutdown delay before the drain", "delay", cfg.shutdownDelay.String())
	time.Sleep(cfg.shutdownDelay)
	// Drain: new requests get 503 and the pool retires; a call in flight finishes first.
	log.Info("draining koinon-gateway", "timeout", cfg.drain.String())
	gw.Drain()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.drain)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		log.Warn("drain deadline passed: open requests were closed", "err", err)
	}
	stopHealth()
	log.Info("stopped koinon-gateway")
	return nil
}

// serveHealth serves /healthz (the process runs) and /readyz (the gateway applied a config and is
// not draining) on addr. An empty addr serves nothing.
func serveHealth(life, term context.Context, addr string, gw *mcpgateway.Gateway, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	var applied atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		// A replica with no config would refuse every key: it is not ready until the first
		// revision applied. Once one has, a later State stream outage keeps the config in force.
		if !applied.Load() && gw.Revision() != "" {
			applied.Store(true)
		}
		if term.Err() != nil || !applied.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	ln, err := (&net.ListenConfig{}).Listen(life, "tcp", addr)
	if err != nil {
		return fmt.Errorf("health listener: %w", err)
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
	return nil
}

// logAudit writes one structured log line per tool call. It names the agent, target, tool, outcome,
// guard reason and credential kind, and never an argument, a result or a token (ADR 0097 decision
// 5). It is written beside the Postgres row (pgaudit) when the gateway has a database login.
type logAudit struct{ log *slog.Logger }

func (a logAudit) Record(ctx context.Context, r mcpgateway.AuditRow) error {
	a.log.LogAttrs(ctx, slog.LevelInfo, "mcp tool call",
		slog.String("audit", "mcp_call"),
		slog.String("org", r.Agent.Org), slog.String("agent", r.Agent.Name),
		slog.String("target", r.Target), slog.String("tool", r.Tool),
		slog.String("outcome", string(r.Outcome)), slog.String("verdict", r.Verdict),
		slog.String("credential", r.CredKind), slog.Duration("duration", r.Duration))
	return nil
}

// openGuard opens the gateway's database login and builds the leak guard on it, with the Postgres audit
// writer. The guard's route stays closed (every message and call is refused) until the database has
// answered once; a later outage refuses on its own, because a decision that cannot be recorded is a
// refusal (ADR 0120 section 2).
func openGuard(ctx context.Context, urlFile string, log *slog.Logger, reg prometheus.Registerer) (*mcpguard.GatewayGuard, mcpgateway.Audit, func(), error) {
	dsn, err := readSecretFile(urlFile, "the database URL")
	if err != nil {
		return nil, nil, nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open the guard's database login: %w", errors.New("the connection string does not parse"))
	}
	gm := mcpguard.NewMetrics(reg)
	g := mcpguard.New(pool, mcpguard.Options{
		Logger: log, Metrics: gm,
		// Koinon's own tool servers: the code-host tools and the platform's own tools.
		OwnTargets: append(providers.Default().IDs(), gateway.KoinonTarget),
		// No Approve in this slice: a call that needs a person's decision is refused (fail closed).
	})
	ga := mcpguard.NewGatewayGuard(g, log)
	safego.Go(ctx, log, "guard-load", func(ctx context.Context) {
		for ctx.Err() == nil {
			if err := ga.Load(ctx); err == nil {
				log.Info("the leak guard reached its database")
				return
			}
			log.Warn("the leak guard cannot reach its database; org targets stay closed")
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	})
	return ga, pgaudit.New(pool), pool.Close, nil
}

// multiAudit writes every row to each Audit. One failing writer does not stop the others, and the
// first error is returned.
type multiAudit []mcpgateway.Audit

func (m multiAudit) Record(ctx context.Context, r mcpgateway.AuditRow) error {
	var first error
	for _, a := range m {
		if err := a.Record(ctx, r); err != nil && first == nil {
			first = err
		}
	}
	return first
}
