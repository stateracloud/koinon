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
// "koinon-api audit verify [--org ID]" walks the audit hash chain (ADR 0023) and exits non-zero at
// the first broken link.
//
// "koinon-api preflight [flags]" checks the cluster for Koinon's install
// prerequisites and exits non-zero when one is missing (plan item H2); the chart
// runs it as a pre-install and pre-upgrade hook Job. It needs no database.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/pressly/goose/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpcheck"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/schemagate"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/tokens"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
	tracerouter "github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/triggers/schedule"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
	"github.com/stateracloud/koinon/web"
)

type config struct {
	addr           string
	databaseURL    string
	migrate        bool
	durable        bool
	executorID     string
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
	oidcPlatformAdmins   string

	sessionIdle time.Duration
	sessionMax  time.Duration

	sweepInterval time.Duration
	watchPoll     time.Duration

	otlpAddr     string
	otlpTokenDir string
	// The trace router's byte budgets; 0 derives each from the container memory limit (its cgroup's,
	// or KOINON_MEMORY_LIMIT_BYTES when that is lower; 20% queues, 5% intake, 3% held roots, 8% requests).
	traceQueueBytes, traceIntakeBytes, traceHeldBytes, traceRequestBytes int
	metricsAddr                                                          string
	toolsAddr                                                            string
	savedWorkAddr                                                        string
	savedWorkSecret                                                      string
	secretsNamespace                                                     string
	kubeconfig                                                           string
	githubAPIURL                                                         string

	kekDir string
	// kekSecret names the Secret with the key-encryption keys. koinon-api creates it on its first
	// start when kekCreate is set and it is missing, and never overwrites it.
	kekSecret string
	kekCreate bool

	// egressAllowedHosts and its deprecated alias providerAllowedHosts are joined: internal hosts that
	// providers and MCP servers may name.
	// gatewayMCPURL is agentgateway's MCP endpoint as koinon-api reaches it, for approved MCP calls.
	gatewayMCPURL string
	// gatewayURL is agentgateway's LLM listener, for the provider echo check.
	gatewayURL           string
	egressAllowedHosts   string
	providerAllowedHosts string
}

// migrateOnly reports whether the command is the migrate subcommand, which applies the migrations
// and exits without serving.
func (c config) migrateOnly() bool { return len(c.args) == 1 && c.args[0] == "migrate" }

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("koinon-api", flag.ContinueOnError)
	fs.StringVar(&c.addr, "addr", ":8080", "listen address")
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string")
	fs.BoolVar(&c.migrate, "migrate", false, "apply database migrations before starting")
	fs.BoolVar(&c.durable, "durable", false,
		"run the DBOS runtime (schedules, durable timers; ADR 0066); needs the DBOS tables that "+
			"\"koinon-api -durable migrate\" creates (the migrate subcommand creates them only with this flag)")
	fs.StringVar(&c.executorID, "executor-id", "",
		"this process's DBOS executor ID, unique among the replicas and never reused (the chart passes the pod name); "+
			"the executors heartbeat into a table and one of them recovers the workflows of a dead one (ADR 0085). "+
			"Empty is DBOS's default, \"local\", which is right for one process only")
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

	fs.StringVar(&c.oidcPlatformAdmins, "oidc-platform-admin-groups", "",
		"comma-separated IdP groups whose members are platform admins; reconciled at every login "+
			"(grants and revokes only what this setting granted)")

	fs.DurationVar(&c.sessionIdle, "session-idle-timeout", auth.DefaultSessionIdleTimeout,
		"end sessions after this long without requests")
	fs.DurationVar(&c.sessionMax, "session-max-lifetime", auth.DefaultSessionMaxLifetime,
		"end sessions this long after login")

	fs.DurationVar(&c.sweepInterval, "run-cap-sweep-interval", 30*time.Second,
		"how often to cancel runs past their wall-clock cap")
	fs.DurationVar(&c.watchPoll, "watch-poll-interval", services.DefaultWatchPollInterval,
		"how often a WatchRun stream re-reads its run without a notification (a backstop for missed NOTIFYs)")
	fs.StringVar(&c.otlpAddr, "otlp-addr", ":4318",
		"listen address of the internal OTLP/HTTP receiver (the spend ledger's logs from agentgateway); "+
			"every request needs the bearer token in --otlp-token-dir; empty disables it")
	fs.StringVar(&c.otlpTokenDir, "otlp-token-dir", "",
		"directory with the OTLP receiver's bearer token (file token) and, during a rotation, the one it replaced (file previous); "+
			"required with --otlp-addr")

	fs.IntVar(&c.traceQueueBytes, "trace-queue-bytes", 0,
		"bytes all run-trace destination queues may hold together (ADR 0074); 0 is 20% of the container memory limit (the cgroup's or KOINON_MEMORY_LIMIT_BYTES, the lower) (128 MiB without one)")
	fs.IntVar(&c.traceIntakeBytes, "trace-intake-bytes", 0,
		"bytes of spans the trace router may hold while attributing them; 0 is 5% of the memory limit (the cgroup's or KOINON_MEMORY_LIMIT_BYTES, the lower) (32 MiB without one)")
	fs.IntVar(&c.traceHeldBytes, "trace-held-bytes", 0,
		"bytes of turn root spans the trace router may hold for the ledger; 0 is 3% of the memory limit (the cgroup's or KOINON_MEMORY_LIMIT_BYTES, the lower) (16 MiB without one)")
	fs.IntVar(&c.traceRequestBytes, "trace-request-bytes", 0,
		"bytes of export requests the trace receiver may read and decode at once; 0 is 8% of the memory limit (the cgroup's or KOINON_MEMORY_LIMIT_BYTES, the lower) (64 MiB without one)")

	fs.StringVar(&c.metricsAddr, "metrics-addr", metrics.DefaultPort,
		"listen address of /metrics (Prometheus); empty disables it")
	fs.StringVar(&c.savedWorkAddr, "saved-work-addr", "",
		"listen address of the saved-work upload endpoint, for koinon-agentd only; empty disables it")
	fs.StringVar(&c.savedWorkSecret, "saved-work-secret", "koinon-saved-work",
		"Secret with the key that signs saved-work uploads; created on first use, never overwritten")
	fs.StringVar(&c.toolsAddr, "tools-addr", ":8081",
		"listen address of the GitHub tools MCP server, for agentgateway only; empty disables it")
	fs.StringVar(&c.secretsNamespace, "secrets-namespace", "",
		"namespace of connection Secrets (default $POD_NAMESPACE); without one, connections cannot be created")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config when in a cluster")
	fs.StringVar(&c.githubAPIURL, "github-api-url", github.DefaultBaseURL, "GitHub REST API base URL")
	fs.StringVar(&c.kekSecret, "kek-secret", "",
		"Secret with the key-encryption keys, read through the Kubernetes API; takes precedence over --kek-dir")
	fs.StringVar(&c.gatewayURL, "gateway-url", "", "agentgateway's LLM listener as koinon-api reaches it (for example http://koinon-gateway.koinon.svc:8080), for the provider echo check; empty disables the check")
	fs.StringVar(&c.gatewayMCPURL, "gateway-mcp-url", "", "agentgateway's MCP endpoint, for the calls humans approved on remote MCP servers; empty disables them")
	fs.StringVar(&c.egressAllowedHosts, "egress-allowed-hosts", "",
		"comma-separated hosts (optionally host:port) that providers and MCP servers may name although they are internal "+
			"(loopback, link-local, metadata and Koinon's own namespace are denied otherwise); platform-level")
	fs.StringVar(&c.providerAllowedHosts, "provider-allowed-hosts", "",
		"deprecated alias of --egress-allowed-hosts, kept for one release")
	fs.BoolVar(&c.kekCreate, "kek-create", true, "create the --kek-secret Secret with a new key when it does not exist")
	fs.StringVar(&c.kekDir, "kek-dir", "",
		"directory with the mounted key-encryption keys Secret (files named by KEK ID plus \"active\"); "+
			"without it, provider keys and connection secrets cannot be stored")

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
	// Only the server opens the OTLP receiver; the migrate subcommand (the chart's migrate Job,
	// which mounts no token) must not need its token.
	if c.otlpAddr != "" && c.otlpTokenDir == "" && !c.migrateOnly() {
		return c, errors.New("--otlp-token-dir is required with --otlp-addr: the OTLP receiver never runs without its bearer token (pass --otlp-addr= to turn the receiver off)")
	}
	if c.secretsNamespace == "" {
		c.secretsNamespace = os.Getenv("POD_NAMESPACE")
	}
	if c.oidcRedirectURL == "" && c.publicURL != "" {
		c.oidcRedirectURL = strings.TrimRight(c.publicURL, "/") + "/auth/oidc/callback"
	}
	return c, nil
}

// kubeClient returns the Kubernetes client, or nil when koinon-api runs outside a cluster
// without a kubeconfig.
func kubeClient(cfg config) (kubernetes.Interface, error) {
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
	return kubernetes.NewForConfig(rc)
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
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
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
	if len(os.Args) > 1 && os.Args[1] == "ensure-secrets" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runEnsureSecrets(ctx, os.Args[2:], os.Stdout)
	}
	if len(os.Args) > 1 && os.Args[1] == "audit" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runAudit(ctx, os.Args[2:], os.Stdout)
	}
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runKeys(ctx, os.Args[2:], os.Stdout)
	}
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case cfg.migrateOnly():
		return migrate(ctx, cfg.databaseURL, cfg.wait, cfg.durable)
	case len(cfg.args) > 0:
		return fmt.Errorf("unknown arguments %q; the subcommands are \"migrate\", \"preflight\", \"ensure-secrets\", \"keys\" and \"audit\" (first)", cfg.args)
	case cfg.migrate:
		if err := migrate(ctx, cfg.databaseURL, cfg.wait, cfg.durable); err != nil {
			return err
		}
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	// Not Ready (/readyz) while the schema is outside the goose range this build supports
	// (ADR 0085): after an app-only rollback the older pods would otherwise fail at their first query.
	gate := schemagate.New(pool, schemagate.Range{Min: migrations.MinSupported, Latest: migrations.MustLatest()}, slog.Default())
	safego.Go(ctx, slog.Default(), "schema-gate", gate.Run)

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
	if cfg.oidcPlatformAdmins != "" && cfg.oidcIssuer == "" {
		return errors.New("--oidc-platform-admin-groups needs --oidc-issuer-url")
	}
	// On a fresh install the database and its schema come up after koinon-api starts (the chart's
	// migration Job waits for CNPG), so bootstrap is retried for up to --wait while the database
	// is unreachable or not migrated. Any other error fails at once.
	res, err := retryBootstrap(ctx, cfg.wait, 2*time.Second, func() (auth.BootstrapResult, error) {
		return auth.Bootstrap(ctx, pool, auth.BootstrapConfig{
			Username:               cfg.bootstrapUsername,
			PasswordFile:           cfg.bootstrapPasswordFile,
			LoginEnabled:           cfg.bootstrapLogin,
			DefaultOrgIdentityMode: mode,
			OIDCIssuer:             cfg.oidcIssuer,
			GroupMappings:          mappings,
		}, time.Now())
	})
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

			PlatformAdminGroups: splitList(cfg.oidcPlatformAdmins),
		}
	}
	authSvc, err := auth.New(pool, authCfg)
	if err != nil {
		return err
	}
	purgeCtx, cancelPurge := context.WithCancel(ctx)
	defer cancelPurge() // before pool.Close
	safego.GoOrStop(purgeCtx, slog.Default(), "auth-purger", stop, func(ctx context.Context) { authSvc.RunPurger(ctx, time.Hour, 7*24*time.Hour) })

	// Templates embedded in the binary become rows, versioned by content hash.
	embedded, err := templates.All()
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}
	priceSvc := prices.New(pool, slog.Default(), pricing.Embedded())
	if err := priceSvc.SyncDefaults(ctx); err != nil {
		return err
	}
	if _, err := priceSvc.CompleteMigrated(ctx); err != nil {
		return err
	}
	// Orgs that existed before the stored default model (migration 00045) get the model they ran on.
	if n, err := agentmodel.BackfillDefaults(ctx, pool, prices.NewResolver(pricing.Embedded()), time.Now()); err != nil {
		return err
	} else if n > 0 {
		slog.Info("backfilled org default models", "orgs", n)
	}
	// Every model no admin set the format of follows the format table (ADR 0069).
	if _, err := catalog.New(pool, nil, slog.Default()).RederiveFormats(ctx); err != nil {
		return err
	}
	if _, err := services.SyncTemplates(ctx, pool, embedded); err != nil {
		return err
	}
	slog.Info("templates synced", "count", len(embedded))

	bgCtx, cancelBg := context.WithCancel(ctx)
	defer cancelBg() // before pool.Close
	hub := runwatch.New(pool.Config().ConnConfig, slog.Default())
	safego.GoOrStop(bgCtx, slog.Default(), "runwatch-hub", stop, func(ctx context.Context) { hub.Run(ctx) })
	safego.GoOrStop(bgCtx, slog.Default(), "scheduler-sweeper", stop, func(ctx context.Context) {
		scheduler.Sweeper(ctx, pool, cfg.sweepInterval, slog.Default())
	})
	// Notifications that follow a commit (pre-wakes) are DBOS steps with --durable, and inline
	// best effort without it (ADR 0066, practice B2).
	var notifier *durable.Notifier
	var durableRT *durable.Runtime // for its metrics, registered once the registry exists
	var sched *schedule.Scheduler
	if !cfg.durable {
		slog.Warn("schedule triggers do not fire: they run on the DBOS runtime (api.durable.enabled, --durable)")
	}
	var directMessages *services.DirectMessageQueue
	var runNow *services.RunNowQueue
	if cfg.durable {
		// The tables come from the migration Job (ADR 0060): DBOS only verifies them here, so a
		// missing schema fails the start instead of the API running without its timers.
		rt := durable.New(durable.Config{
			DatabaseURL: cfg.databaseURL,
			ExecutorID:  cfg.executorID,
			// The default ApplicationVersion is durable.WorkflowVersion, not the release version:
			// an upgrade must not orphan pending workflows (ADR 0085).
			Logger: slog.Default(),
		})
		durableRT = rt
		notifier = durable.NewNotifier(rt, slog.Default())
		services.RegisterNotifications(notifier, pool)
		if err := rt.Register(notifier.Registrar()); err != nil {
			return err
		}
		// The org-partitioned queue for trigger and task-creation work (DB2): each org's limits are
		// its trigger_concurrency and trigger_rate_per_minute settings. Schedule triggers (TR2) enqueue
		// through it with durable.EnqueueForOrg; TR3 will follow.
		orgWork, err := durable.NewOrgWork(rt, durable.OrgQueueConfig{
			Defaults: orgsettings.QueueDefaults(),
			Limits:   orgsettings.QueueLimits(store.New(pool), orgsettings.Defaults()),
		})
		if err != nil {
			return err
		}
		// Schedule triggers (TR2): every replica ticks; the fire workflow and the dedupe key make a
		// due time fire once. It starts below, once the services exist.
		sched = schedule.New(schedule.Config{Logger: slog.Default()}, pool, orgWork)
		if err := rt.Register(sched.Registrar()); err != nil {
			return err
		}
		// Direct messages in the inbox chat (TR3) create their tasks on that queue.
		if directMessages, err = services.RegisterDirectMessages(rt, orgWork); err != nil {
			return err
		}
		// "Run now" for schedule triggers fires on the same queue, so the org's limits bound it.
		if runNow, err = services.RegisterRunNow(rt, orgWork); err != nil {
			return err
		}
		if err := rt.Launch(bgCtx); err != nil {
			return err
		}
		defer func() { // before pool.Close
			if err := rt.Shutdown(30 * time.Second); err != nil {
				slog.Warn("stopping the DBOS runtime", "err", err)
			}
		}()
		slog.Info("DBOS runtime started")
	}
	kube, err := kubeClient(cfg)
	if err != nil {
		return err
	}
	// The secret store (ADR 0056): provider keys and connection secrets are envelopes in
	// Postgres, wrapped by the KEKs of a mounted Secret.
	var keyring *secretstore.Keyring
	switch {
	case cfg.kekSecret != "" && kube != nil:
		// The chart renders no key material (a render would make new keys every time, and a new
		// KEK loses every stored secret): koinon-api creates the Secret once, and only reads it after.
		kekStore := keyscli.KubeStore{Client: kube, Namespace: cfg.secretsNamespace, Name: cfg.kekSecret}
		if cfg.kekCreate {
			created, err := keyscli.EnsureKubeSecret(ctx, kube, cfg.secretsNamespace, cfg.kekSecret, time.Now(),
				func(ctx context.Context) (bool, error) { return keyscli.AnyEnvelope(ctx, pool) })
			if err != nil {
				return err
			}
			if created {
				slog.Warn("created the key-encryption key Secret; back it up now (docs/ops/secret-keys.md)", "secret", cfg.kekSecret)
			}
		}
		if keyring, err = loadKeyring(ctx, kekStore); err != nil {
			return err
		}
		safego.GoOrStop(bgCtx, slog.Default(), "keyring-watch", stop, func(ctx context.Context) { watchKeyring(ctx, keyring, kekStore, time.Minute) })
	case cfg.kekDir != "":
		if keyring, err = secretstore.LoadDir(cfg.kekDir); err != nil {
			return err
		}
		safego.GoOrStop(bgCtx, slog.Default(), "keyring-watch-dir", stop, func(ctx context.Context) {
			keyring.WatchDir(ctx, cfg.kekDir, time.Minute, slog.Default())
		})
	default:
		slog.Warn("no --kek-secret or --kek-dir: provider keys and connections cannot be stored")
	}
	// Connections created before the secret store kept their fields in Kubernetes Secrets: move them.
	if kube != nil && keyring != nil {
		legacy := connections.KubeSecrets{Client: kube, Namespace: cfg.secretsNamespace}
		if n, err := connections.MigrateLegacy(ctx, store.New(pool), legacy, keyring, slog.Default()); err != nil {
			return fmt.Errorf("move connection secrets into the store: %w", err)
		} else if n > 0 {
			slog.Info("moved connection secrets into the store", "count", n)
		}
	}
	// One GitHub client, so connection checks and approved calls share its token cache.
	gh := github.NewClient(github.Options{BaseURL: cfg.githubAPIURL})
	hosts := providers.New(gh)
	var opener connections.Opener
	var keys services.Keys
	if keyring != nil {
		opener, keys = keyring, keyring
	}
	// One egress policy for everything an org points the gateway or Koinon at: model providers and
	// remote MCP servers.
	urlPolicy := catalog.URLPolicy{Namespace: cfg.secretsNamespace, AllowedHosts: splitList(cfg.egressAllowedHosts + "," + cfg.providerAllowedHosts)}
	if err := catalog.ValidateAllowedHosts(urlPolicy.AllowedHosts); err != nil {
		return err
	}
	approvalOpts := approvals.Options{Keys: opener, Hosts: hosts, GatewayMCPURL: cfg.gatewayMCPURL, Logger: slog.Default()}
	if kube != nil {
		// Approved calls to remote MCP servers go through the gateway with the org's executor key,
		// which the reconciler keeps in a Secret; koinon-api never holds a server credential.
		approvalOpts.ExecKeys = mcpservers.KubeExecKeys{Client: kube, Namespace: cfg.secretsNamespace}
	}
	approvalOpts.SavedWorkKeys = keyring // opens the work saved from stuck sandboxes (ADR 0054)
	approvalSvc := approvals.New(pool, approvalOpts)
	safego.GoOrStop(bgCtx, slog.Default(), "approval-sweeper", stop, func(ctx context.Context) { approvalSvc.Sweeper(ctx, cfg.sweepInterval) })
	tokenSvc := tokens.New(pool, nil)
	var catalogKeys catalog.Keys
	if keyring != nil {
		catalogKeys = keyring
	}
	catalogSvc := catalog.New(pool, catalogKeys, slog.Default()).WithURLPolicy(urlPolicy)
	// A server that asks each user to sign in is refused when it is added (v0.2 brings per-user OAuth).
	mcpSvc := mcpservers.New(pool, catalogKeys, slog.Default()).WithURLPolicy(urlPolicy).WithPreflight(mcpservers.OAuthPreflight(urlPolicy))
	// The provider echo check sends one request through the gateway with the org's model-check key,
	// which the reconciler keeps in a Secret (mcpservers.ModelCheckSecretName). Without a gateway URL or Kubernetes
	// access the check answers that it is not configured.
	var echo *modelcheck.Service
	if cfg.gatewayURL != "" && kube != nil {
		echo = modelcheck.New(pool, modelcheck.GatewayChat{
			URL: cfg.gatewayURL, Keys: mcpservers.KubeExecKeys{Client: kube, Namespace: cfg.secretsNamespace, SecretName: mcpservers.ModelCheckSecretName},
			HTTP: &http.Client{Timeout: 90 * time.Second},
		}, prices.NewResolver(pricing.Embedded()))
	} else {
		echo = modelcheck.New(pool, nil, prices.NewResolver(pricing.Embedded()))
	}
	// Run-trace destinations (ADR 0074). The client dials only what the egress policy allows.
	traceSvc, err := tracedest.New(pool, catalogKeys, urlPolicy, slog.Default())
	if err != nil {
		return err
	}
	svc := services.New(pool, services.Options{
		ModelCheck: echo, Traces: traceSvc,
		Hub: hub, WatchPollInterval: cfg.watchPoll, Keys: keys, Tokens: tokenSvc, Hosts: hosts, Approvals: approvalSvc, Catalog: catalogSvc, MCP: mcpSvc, Notifier: notifier, DirectMessages: directMessages, RunNow: runNow, URLPolicy: urlPolicy, Prices: pricing.Embedded(),
	})
	// The notification of an opened question (the park that opens it commits in koinon-agentd).
	safego.GoOrStop(bgCtx, slog.Default(), "question-sweeper", stop, func(ctx context.Context) { svc.QuestionSweeper(ctx, cfg.sweepInterval) })
	if sched != nil {
		sched.Bind(svc)
		safego.GoOrStop(bgCtx, slog.Default(), "schedule-triggers", stop, func(ctx context.Context) { sched.Run(ctx) })
		slog.Info("schedule triggers started")
	}
	if kube != nil {
		var sealer connections.Sealer
		if keyring != nil {
			sealer = keyring
		}
		imp := &declarative.Importer{Client: kube, Namespace: cfg.secretsNamespace, Pool: pool, Catalog: catalogSvc, MCP: mcpSvc, URLPolicy: urlPolicy, Sealer: sealer, Log: slog.Default()}
		safego.GoOrStop(bgCtx, slog.Default(), "declarative-importer", stop, func(ctx context.Context) { imp.Run(ctx) })
	} else {
		slog.Warn("no Kubernetes access: declarative Secrets are not imported")
	}
	adminSvc := adminapi.New(pool, catalogSvc, tokenSvc, slog.Default(), pricing.Embedded()).WithMCP(mcpSvc).WithTriggers(svc).WithModelCheck(echo)

	reg := metrics.NewRegistry()
	if durableRT != nil {
		// Every replica reports the same database-derived counts; alerts aggregate with max (ADR 0085).
		reg.MustRegister(durableRT.Collector())
	}
	if err := metrics.Serve(bgCtx, cfg.metricsAddr, reg, slog.Default()); err != nil {
		return err
	}
	if cfg.otlpAddr != "" {
		// The spend ledger's receiver (plan item L1). It runs on its own listener, outside
		// the session-authenticated API handler.
		otlpTokens, err := otlpauth.NewTokens(cfg.otlpTokenDir, slog.Default())
		if err != nil {
			return fmt.Errorf("--otlp-token-dir: %w", err)
		}
		otlpMetrics := otlpauth.NewMetrics(reg)
		// The trace router (ADR 0074): agentd's spans arrive at /v1/traces, inside the token check.
		exportClient, err := export.NewClient(urlPolicy.DialContext(&net.Dialer{Timeout: 5 * time.Second}), 0)
		if err != nil {
			return err
		}
		traceRouter, err := tracerouter.New(tracerouter.Config{
			Store: tracerouter.NewPGStore(pool), Routes: traceSvc, Secrets: tracerouter.NewPGSecrets(pool, opener, slog.Default()),
			Health: tracerouter.NewPGHealth(pool), Sender: exportClient, MemoryLimit: memlimit.Limit(),
			QueueBytes: cfg.traceQueueBytes, IntakeBytes: cfg.traceIntakeBytes, MaxHeldBytes: cfg.traceHeldBytes, RequestBytes: cfg.traceRequestBytes,
			Metrics: tracerouter.NewMetrics(reg), Log: slog.Default(),
		})
		if err != nil {
			return err
		}
		traceRouter.Start(bgCtx)
		defer traceRouter.Close()
		safego.GoOrStop(ctx, slog.Default(), "ledger-otlp", stop, func(context.Context) {
			slog.Info("starting the internal OTLP receiver", "addr", cfg.otlpAddr)
			if err := ledger.Serve(bgCtx, cfg.otlpAddr, &ledger.Ingester{DB: pool, Metrics: ledger.NewMetrics(reg), Prices: prices.NewResolver(pricing.Embedded())}, slog.Default(), otlpauth.Middleware(otlpTokens, otlpMetrics), traceRouter.Mount()); err != nil {
				slog.Error("spend ledger OTLP receiver failed", "err", err)
				stop()
			}
		})
	}

	apiMetrics := api.NewMetrics(reg)
	handler, err := api.NewHandler(api.Options{
		Auth:           authSvc,
		Metrics:        apiMetrics,
		TrustedOrigins: splitList(cfg.trustedOrigins),
		Services:       []func(*connect.Server){svc.Register},
		UI:             web.Dist(),
		Mounts:         []func(*http.ServeMux){adminSvc.Mount},
		Ready:          gate.ReadyHandler(),
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{srv}
	toolsErr := make(chan error, 1)
	if cfg.toolsAddr != "" {
		tools, err := githubtools.New(pool, githubtools.Options{
			Hosts: hosts,
			Keys:  opener,
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
		// The ask-tier tools of org MCP servers (plan item MC1), for agentgateway only like /mcp.
		mux.Handle(mcpask.Path, mcpask.New(pool, mcpask.Options{}).Handler())
		// ask_user (plan item TK3), the "koinon" target of the gateway.
		mux.Handle(askuser.Path, askuser.New(pool, askuser.Options{Metrics: apiMetrics}).Handler())
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		toolsSrv := &http.Server{Addr: cfg.toolsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, toolsSrv)
		safego.GoOrStop(ctx, slog.Default(), "tools-server", stop, func(context.Context) {
			slog.Info("serving GitHub tools", "addr", cfg.toolsAddr)
			if err := toolsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				toolsErr <- err
				stop()
			}
		})
	}
	// Saved work from a stuck sandbox (ADR 0054) arrives on its own listener, which only koinon-agentd
	// may reach: koinon-agentd holds no KEK, so it posts the bundle here to be sealed. The tools
	// listener stays agentgateway's alone.
	if cfg.savedWorkAddr != "" && keyring != nil && kube != nil {
		secretName := cfg.savedWorkSecret
		keyFn := func(ctx context.Context) ([]byte, error) {
			return savedWorkKey(ctx, kube, cfg.secretsNamespace, secretName)
		}
		if _, err := keyFn(ctx); err != nil {
			return fmt.Errorf("saved-work upload key: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle(savedwork.UploadPath, savedwork.NewReceiver(pool, keyring, keyFn, slog.Default()))
		// The password of an agent's home repository (ADR 0032): koinon-agentd, the checkpoint
		// writer, asks per checkpoint and holds no key-encryption key.
		// The live check of an agent's MCP tools (issue #488): koinon-api reads the agent's gateway key
		// and lists the tools as the agent; koinon-agentd gets names only, never the key.
		if cfg.gatewayMCPURL != "" {
			mux.Handle(mcpcheck.Path, mcpcheck.Server{Keys: mcpcheck.KubeAgentKeys{Client: kube, Namespace: cfg.secretsNamespace}, Gateway: cfg.gatewayMCPURL, Key: keyFn})
		}
		mux.Handle(checkpoint.RepositoryPath, checkpoint.Server{Passwords: checkpoint.Passwords{Q: store.New(pool), Keys: keyring}, Key: keyFn})
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		swSrv := &http.Server{Addr: cfg.savedWorkAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, swSrv)
		safego.GoOrStop(ctx, slog.Default(), "saved-work-server", stop, func(context.Context) {
			slog.Info("serving saved-work uploads", "addr", cfg.savedWorkAddr)
			if err := swSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				toolsErr <- err
				stop()
			}
		})
	}
	safego.Go(ctx, slog.Default(), "shutdown", func(context.Context) {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	})

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
func migrate(ctx context.Context, dsn string, wait time.Duration, withDurable bool) error {
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

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}
	results, err := provider.Up(ctx)
	for _, r := range results {
		slog.Info("applied migration", "version", r.Source.Version, "duration", r.Duration)
	}
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// DBOS keeps its own tables in its own schema; creating that schema needs CREATE on the
	// database, which an external migrate role may lack. So it runs only when the DBOS runtime is
	// on (the chart passes -durable to the migration Job when api.durable.enabled).
	if !withDurable {
		return nil
	}
	return durable.Migrate(ctx, dsn, slog.Default())
}

// savedWorkKey returns the key that signs saved-work uploads, creating its Secret (32 random bytes
// under "key") when it does not exist and never overwriting one that does.
func savedWorkKey(ctx context.Context, kube kubernetes.Interface, namespace, name string) ([]byte, error) {
	secrets := kube.CoreV1().Secrets(namespace)
	get := func() ([]byte, error) {
		sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if len(sec.Data[savedwork.KeyFile]) < 32 {
			return nil, fmt.Errorf("the Secret %s/%s has no usable %q", namespace, name, savedwork.KeyFile)
		}
		return sec.Data[savedwork.KeyFile], nil
	}
	key, err := get()
	if err == nil || !apierrors.IsNotFound(err) {
		return key, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{project.Domain + "/managed-by": "koinon-api"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{savedwork.KeyFile: k},
	}, metav1.CreateOptions{FieldManager: "koinon-api"})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	return get()
}
