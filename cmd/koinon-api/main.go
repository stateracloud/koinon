// SPDX-License-Identifier: Apache-2.0

// Command koinon-api serves the Koinon API and web UI.
//
// Every flag can also be set by an environment variable: KOINON_ plus the
// flag name in upper case with dashes as underscores (--database-url is
// KOINON_DATABASE_URL). Secrets are read from files, never from flags.
//
// "koinon-api migrate" applies the database migrations and exits; the Helm
// chart runs it as a hook Job with the migration role.
//
// Besides the API on --addr, koinon-api serves the GitHub connector's MCP
// tools (internal/githubtools, plan item C3) on --tools-addr, a separate
// listener that only agentgateway may reach: the tools trust the gateway's
// agent header, so the listener must never be exposed through the API's
// Service or ingress. They run in this process rather than their own
// Deployment because they only record approval requests in Postgres and hold
// no GitHub credential; Koinon's API executes approved calls (P1), and a
// second Deployment would add an image, a database role and RBAC without
// separating any secret.
//
// "koinon-api preflight [flags]" checks the cluster for Koinon's install
// prerequisites and exits non-zero when one is missing (plan item H2); the chart
// runs it as a pre-install and pre-upgrade hook Job. It needs no database.
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/procoduck/koinon/internal/api"
	"github.com/procoduck/koinon/internal/approvals"
	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/cli"
	"github.com/procoduck/koinon/internal/connections"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/githubtools"
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

	publicURL string

	bootstrapLogin        bool
	bootstrapUsername     string
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

	otlpAddr         string
	toolsAddr        string
	secretsNamespace string
	kubeconfig       string
	githubAPIURL     string
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

	fs.StringVar(&c.publicURL, "public-url", "",
		"URL browsers reach Koinon at, e.g. https://koinon.example.com; an http:// URL drops the "+
			"cookies' Secure attribute (for installs without TLS); default https")

	fs.BoolVar(&c.bootstrapLogin, "bootstrap-login", true,
		"allow the bootstrap admin to log in with a password; disable once OIDC works")
	fs.StringVar(&c.bootstrapUsername, "bootstrap-admin-username", auth.DefaultBootstrapAdminUsername,
		"the bootstrap admin's login name")
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
		"OIDC redirect URL; default <public-url>/auth/oidc/callback")
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

	fs.StringVar(&c.toolsAddr, "tools-addr", ":8081",
		"listen address of the GitHub tools MCP server, for agentgateway only; empty disables it")
	fs.StringVar(&c.secretsNamespace, "secrets-namespace", "",
		"namespace of connection Secrets (default $POD_NAMESPACE); without one, connections cannot be created")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config when in a cluster")
	fs.StringVar(&c.githubAPIURL, "github-api-url", github.DefaultBaseURL, "GitHub REST API base URL")

	if err := cli.BindEnv(fs, nil); err != nil {
		return c, err
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
	if c.secretsNamespace == "" {
		c.secretsNamespace = os.Getenv("POD_NAMESPACE")
	}
	if c.oidcRedirectURL == "" && c.publicURL != "" {
		c.oidcRedirectURL = strings.TrimRight(c.publicURL, "/") + "/auth/oidc/callback"
	}
	return c, nil
}

// secretStore returns the Kubernetes Secret store for connections, or nil
// when koinon-api runs outside a cluster without a kubeconfig.
func secretStore(cfg config) (connections.SecretStore, error) {
	var (
		rc  *rest.Config
		err error
	)
	switch {
	case cfg.kubeconfig != "":
		rc, err = clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		rc, err = rest.InClusterConfig()
	default:
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	if cfg.secretsNamespace == "" {
		return nil, errors.New("--secrets-namespace (or $POD_NAMESPACE) is required with Kubernetes access")
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return connections.KubeSecrets{Client: client, Namespace: cfg.secretsNamespace}, nil
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
	if len(os.Args) > 1 && os.Args[1] == "preflight" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runPreflight(ctx, os.Args[2:], os.Stdout)
	}
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
		return fmt.Errorf("unknown arguments %q; the subcommands are \"migrate\" and \"preflight\" (first)", cfg.args)
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
		Username:               cfg.bootstrapUsername,
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
		PublicURL:          cfg.publicURL,
	}
	if cfg.oidcIssuer != "" {
		secret, err := auth.ReadSecretFile(cfg.oidcClientSecretFile)
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
	secrets, err := secretStore(cfg)
	if err != nil {
		return err
	}
	if secrets == nil {
		slog.Warn("no Kubernetes access: connections cannot be created (their secrets go to Kubernetes Secrets)")
	}
	// One GitHub client, so connection checks and approved calls share its token cache.
	gh := github.NewClient(github.Options{BaseURL: cfg.githubAPIURL})
	approvalSvc := approvals.New(pool, approvals.Options{Secrets: secrets, GitHub: gh, Logger: slog.Default()})
	go approvalSvc.Sweeper(bgCtx, cfg.sweepInterval)
	svc := services.New(pool, services.Options{
		Hub: hub, WatchPollInterval: cfg.watchPoll, Secrets: secrets, GitHub: gh, Approvals: approvalSvc,
	})

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
	servers := []*http.Server{srv}
	toolsErr := make(chan error, 1)
	if cfg.toolsAddr != "" {
		tools, err := githubtools.New(pool, githubtools.Options{
			// Trusted-preset calls run at once instead of on the next sweep.
			Execute: func(ctx context.Context, org, id pgtype.UUID) {
				ctx, cancel := context.WithTimeout(ctx, approvals.DefaultExecutionLease)
				defer cancel()
				if _, err := approvalSvc.Execute(ctx, org, id); err != nil {
					slog.Warn("executing an allowed call failed; the sweeper retries", "approval", id.String(), "err", err)
				}
			},
		})
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/mcp", tools.Handler())
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		toolsSrv := &http.Server{Addr: cfg.toolsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, toolsSrv)
		go func() {
			slog.Info("serving GitHub tools", "addr", cfg.toolsAddr)
			if err := toolsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				toolsErr <- err
				stop()
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	}()

	slog.Info("starting koinon-api", "version", project.Version, "addr", cfg.addr,
		"oidc", cfg.oidcIssuer != "", "web_ui", web.Dist() != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	select {
	case err := <-toolsErr:
		return fmt.Errorf("github tools server: %w", err)
	default:
		return nil
	}
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
