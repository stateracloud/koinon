// SPDX-License-Identifier: Apache-2.0

// Command koinon-api serves the Koinon API and web UI.
//
// Every flag can also be set by an environment variable: KOINON_ plus the
// flag name in upper case with dashes as underscores (--database-url is
// KOINON_DATABASE_URL). Secrets are read from files, never from flags.
//
// "koinon-api migrate" applies the database migrations and exits; the Helm
// chart runs it as a hook Job with the migration role.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose

	"github.com/procoduck/koinon/internal/api"
	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/ledger"
	"github.com/procoduck/koinon/internal/project"
	"github.com/procoduck/koinon/internal/runwatch"
	"github.com/procoduck/koinon/internal/scheduler"
	"github.com/procoduck/koinon/internal/services"
	"github.com/procoduck/koinon/templates"
	"github.com/procoduck/koinon/web"
)

type config struct {
	addr           string
	databaseURL    string
	migrate        bool
	wait           time.Duration
	trustedOrigins string
	args           []string

	bootstrapLogin        bool
	bootstrapPasswordFile string
	defaultOrgMode        string
	defaultOrgGroups      string

	oidcIssuer           string
	oidcClientID         string
	oidcClientSecretFile string
	oidcRedirectURL      string
	oidcGroupsClaim      string
	oidcScopes           string

	sessionIdle time.Duration
	sessionMax  time.Duration

	sweepInterval time.Duration
	watchPoll     time.Duration

	otlpAddr string
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("koinon-api", flag.ContinueOnError)
	fs.StringVar(&c.addr, "addr", ":8080", "listen address")
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string")
	fs.BoolVar(&c.migrate, "migrate", false, "apply database migrations before starting")
	fs.DurationVar(&c.wait, "wait", 5*time.Minute, "how long migrations wait for the database to come up")
	fs.StringVar(&c.trustedOrigins, "trusted-origins", "",
		"comma-separated extra origins allowed to make state-changing requests (CSRF)")

	fs.BoolVar(&c.bootstrapLogin, "bootstrap-login", true,
		"allow the bootstrap admin to log in with a password; disable once OIDC works")
	fs.StringVar(&c.bootstrapPasswordFile, "bootstrap-password-file", "",
		"file holding the bootstrap admin's one-time password (a mounted Secret)")
	fs.StringVar(&c.defaultOrgMode, "default-org-identity-mode", "hybrid",
		"identity mode of the default org: platform, idp or hybrid")
	fs.StringVar(&c.defaultOrgGroups, "default-org-group-mappings", "",
		"IdP group to default-org role mappings, e.g. koinon-admins=admin,staff=member")

	fs.StringVar(&c.oidcIssuer, "oidc-issuer-url", "", "OIDC issuer URL; empty disables OIDC login")
	fs.StringVar(&c.oidcClientID, "oidc-client-id", "", "OIDC client ID")
	fs.StringVar(&c.oidcClientSecretFile, "oidc-client-secret-file", "", "file holding the OIDC client secret")
	fs.StringVar(&c.oidcRedirectURL, "oidc-redirect-url", "",
		"OIDC redirect URL, https://<host>/auth/oidc/callback")
	fs.StringVar(&c.oidcGroupsClaim, "oidc-groups-claim", "groups", "ID token claim holding the user's groups")
	fs.StringVar(&c.oidcScopes, "oidc-scopes", "email,profile", "comma-separated scopes requested besides openid")

	fs.DurationVar(&c.sessionIdle, "session-idle-timeout", auth.DefaultSessionIdleTimeout,
		"end sessions after this long without requests")
	fs.DurationVar(&c.sessionMax, "session-max-lifetime", auth.DefaultSessionMaxLifetime,
		"end sessions this long after login")

	fs.DurationVar(&c.sweepInterval, "run-cap-sweep-interval", 30*time.Second,
		"how often to cancel runs past their wall-clock cap")
	fs.DurationVar(&c.watchPoll, "watch-poll-interval", services.DefaultWatchPollInterval,
		"how often a WatchRun stream re-reads its run without a notification (a backstop for missed NOTIFYs)")
	fs.StringVar(&c.otlpAddr, "otlp-addr", ":4318",
		"listen address of the spend ledger's OTLP/HTTP logs receiver, for agentgateway's access log; "+
			"it has no authentication, so restrict it to agentgateway with a NetworkPolicy; empty disables it")

	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		key := "KOINON_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(key); ok {
			if err := f.Value.Set(v); err != nil && envErr == nil {
				envErr = fmt.Errorf("%s: %w", key, err)
			}
		}
	})
	if envErr != nil {
		return c, envErr
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.args = fs.Args()
	if c.databaseURL == "" {
		c.databaseURL = os.Getenv("DATABASE_URL") // as set by the dev manifests
	}
	if c.databaseURL == "" {
		return c, errors.New("--database-url is required")
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	if err := run(); err != nil {
		slog.Error("koinon-api failed", "err", err)
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

	switch {
	case len(cfg.args) == 1 && cfg.args[0] == "migrate":
		return migrate(ctx, cfg.databaseURL, cfg.wait)
	case len(cfg.args) > 0:
		return fmt.Errorf("unknown arguments %q; the only subcommand is \"migrate\"", cfg.args)
	case cfg.migrate:
		if err := migrate(ctx, cfg.databaseURL, cfg.wait); err != nil {
			return err
		}
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	mode, err := auth.ParseIdentityMode(cfg.defaultOrgMode)
	if err != nil {
		return err
	}
	mappings, err := auth.ParseGroupMappings(cfg.defaultOrgGroups)
	if err != nil {
		return err
	}
	if len(mappings) > 0 && cfg.oidcIssuer == "" {
		return errors.New("--default-org-group-mappings needs --oidc-issuer-url")
	}
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapConfig{
		PasswordFile:           cfg.bootstrapPasswordFile,
		LoginEnabled:           cfg.bootstrapLogin,
		DefaultOrgIdentityMode: mode,
		OIDCIssuer:             cfg.oidcIssuer,
		GroupMappings:          mappings,
	}, time.Now())
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	slog.Info("bootstrap done", "created_admin", res.CreatedAdmin, "created_org", res.CreatedOrg,
		"created_credential", res.CreatedCredential, "revoked_bootstrap_sessions", res.RevokedSessions,
		"bootstrap_login", cfg.bootstrapLogin)

	if res.MissingPasswordFile {
		slog.Warn("bootstrap login is enabled but no password is set; set --bootstrap-password-file")
	}

	authCfg := auth.Config{
		SessionIdleTimeout: cfg.sessionIdle,
		SessionMaxLifetime: cfg.sessionMax,
		BootstrapLogin:     cfg.bootstrapLogin,
	}
	if cfg.oidcIssuer != "" {
		secret, err := readSecretFile(cfg.oidcClientSecretFile)
		if err != nil {
			return fmt.Errorf("oidc client secret: %w", err)
		}
		authCfg.OIDC = &auth.OIDCConfig{
			IssuerURL:    cfg.oidcIssuer,
			ClientID:     cfg.oidcClientID,
			ClientSecret: secret,
			RedirectURL:  cfg.oidcRedirectURL,
			GroupsClaim:  cfg.oidcGroupsClaim,
			Scopes:       splitList(cfg.oidcScopes),
		}
	}
	authSvc, err := auth.New(pool, authCfg)
	if err != nil {
		return err
	}
	purgeCtx, cancelPurge := context.WithCancel(ctx)
	defer cancelPurge() // before pool.Close
	go authSvc.RunPurger(purgeCtx, time.Hour, 7*24*time.Hour)

	// Templates embedded in the binary become rows, versioned by content hash.
	embedded, err := templates.All()
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}
	if _, err := services.SyncTemplates(ctx, pool, embedded); err != nil {
		return err
	}
	slog.Info("templates synced", "count", len(embedded))

	bgCtx, cancelBg := context.WithCancel(ctx)
	defer cancelBg() // before pool.Close
	hub := runwatch.New(pool.Config().ConnConfig, slog.Default())
	go hub.Run(bgCtx)
	go scheduler.Sweeper(bgCtx, pool, cfg.sweepInterval, slog.Default())
	svc := services.New(pool, services.Options{Hub: hub, WatchPollInterval: cfg.watchPoll})

	if cfg.otlpAddr != "" {
		// The spend ledger's receiver (plan item L1). It runs on its own listener, outside
		// the session-authenticated API handler.
		go func() {
			slog.Info("starting the spend ledger's OTLP receiver", "addr", cfg.otlpAddr)
			if err := ledger.Serve(bgCtx, cfg.otlpAddr, &ledger.Ingester{DB: pool}, slog.Default()); err != nil {
				slog.Error("spend ledger OTLP receiver failed", "err", err)
				stop()
			}
		}()
	}

	handler, err := api.NewHandler(api.Options{
		Auth:           authSvc,
		TrustedOrigins: splitList(cfg.trustedOrigins),
		Services:       []func(*connect.Server){svc.Register},
		UI:             web.Dist(),
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("starting koinon-api", "version", project.Version, "addr", cfg.addr,
		"oidc", cfg.oidcIssuer != "", "web_ui", web.Dist() != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// migrate applies pending migrations, first waiting up to wait for the
// database: on a fresh install it may still be starting.
func migrate(ctx context.Context, dsn string, wait time.Duration) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err = db.PingContext(waitCtx)
		if err == nil {
			break
		}
		slog.Info("waiting for the database", "err", err)
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("database not reachable: %w", err)
		case <-time.After(2 * time.Second):
		}
	}

	results, err := migrations.Up(ctx, db)
	for _, r := range results {
		slog.Info("applied migration", "version", r.Source.Version, "duration", r.Duration)
	}
	return err
}

// readSecretFile reads a secret mounted as a file. An empty path yields an
// empty secret, for public OIDC clients that rely on PKCE alone.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
