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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/project"
)

type config struct {
	databaseURL     string
	openshellAddr   string
	openshellTLSDir string
	openshellName   string
	pool            agentd.Config
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("koinon-agentd", flag.ContinueOnError)
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&c.openshellAddr, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "",
		"directory with the OpenShell client bundle (tls.crt, tls.key, ca.crt); empty connects without TLS")
	fs.StringVar(&c.openshellName, "openshell-server-name", "", "name checked against the gateway's certificate")
	fs.StringVar(&c.pool.SavedWorkURL, "saved-work-url", "",
		"koinon-api's tools listener, where the uncommitted work of a stuck sandbox is posted to be sealed (ADR 0054); empty: not saved")
	fs.StringVar(&c.pool.GatewayURL, "gateway-url", "",
		"agentgateway's base URL as sandboxes reach it; flavors get <url>/v1 and <url>/mcp")
	fs.StringVar(&c.pool.DefaultModel, "default-model", "", "model of agents that name none")
	fs.StringVar(&c.pool.OwnerID, "owner-id", "", "this replica's name in run leases (default $POD_NAME, else the host name)")
	fs.IntVar(&c.pool.Workers, "workers", 8, "runs driven at once")
	fs.DurationVar(&c.pool.Lease, "lease", 30*time.Second, "run lease; another replica takes a run over when it expires")
	fs.DurationVar(&c.pool.Heartbeat, "heartbeat", 0, "lease renewal and cancel check interval (default lease/3)")
	fs.DurationVar(&c.pool.Poll, "poll", time.Second, "how often idle workers look for runs")
	fs.DurationVar(&c.pool.SandboxTimeout, "sandbox-timeout", 5*time.Minute, "how long a run waits for its sandbox")
	fs.DurationVar(&c.pool.CancelGrace, "cancel-grace", 10*time.Second, "how long a harness gets to end a cancelled turn")
	fs.IntVar(&c.pool.MaxTakeovers, "max-takeovers", 5, "fail a run taken over more often than this")
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
	switch {
	case c.databaseURL == "":
		return c, errors.New("--database-url is required")
	case c.openshellAddr == "":
		return c, errors.New("--openshell-address is required")
	case c.pool.GatewayURL == "":
		return c, errors.New("--gateway-url is required")
	}
	return c, nil
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

	pool, err := agentd.New(db, backend, cfg.pool)
	if err != nil {
		return err
	}
	slog.Info("starting koinon-agentd", "version", project.Version, "owner", cfg.pool.OwnerID, "workers", cfg.pool.Workers)
	err = pool.Run(ctx)
	slog.Info("stopping koinon-agentd")
	return err
}
