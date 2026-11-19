// SPDX-License-Identifier: Apache-2.0

// Package koinonapi is the command tree of koinon-api, which serves the Koinon API and web UI.
//
// Every flag can also be set by an environment variable: KOINON_ plus the
// flag name in upper case with dashes as underscores (--database-url is
// KOINON_DATABASE_URL). Secrets are read from files, never from flags.
//
// "koinon-api migrate" applies the database migrations and exits; the Helm
// chart runs it as a hook Job with the migration role. The command tree (commands.go) follows
// ADR 0088; without a subcommand koinon-api serves.
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
// "koinon-api audit verify [--org ID | --install]" walks the install audit chain (ADR 0100) and the
// org chains (ADR 0023) and exits non-zero at the first broken link.
//
// "koinon-api preflight [flags]" checks the cluster for Koinon's install
// prerequisites and exits non-zero when one is missing (plan item H2); the chart
// runs it as a pre-install and pre-upgrade hook Job. It needs no database.
package koinonapi

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/apiassembly"
	"github.com/stateracloud/koinon/internal/askuser"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/auth/ratelimit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/declarative"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/stateserver"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/kekhealth"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpcheck"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/orcatalog"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/internal/savedwork"
	"github.com/stateracloud/koinon/internal/scheduler"
	"github.com/stateracloud/koinon/internal/schemagate"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/tracing/export"
	tracerouter "github.com/stateracloud/koinon/internal/tracing/router"
	"github.com/stateracloud/koinon/internal/triggers/poll"
	"github.com/stateracloud/koinon/internal/triggers/schedule"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
	"github.com/stateracloud/koinon/web"
)

type config struct {
	pricesRefresh         string
	pricesRefreshInterval time.Duration
	pricesMaxChange       int
	pricesMaxAutoPerDay   int

	addr        string
	databaseURL string
	migrate     bool
	durable     bool
	// restoreHold starts the process without the DBOS runtime, the sweepers, the price refresh and the
	// code-host scope refresh (issue #462).
	restoreHold bool
	// restoreAcceptImporterDeletions accepts the declarative removals a restore brought in (ADR 0110
	// decision 1): the restore's release step records them as accepted instead of waiting.
	restoreAcceptImporterDeletions bool

	// sandboxResources is the platform's sandbox sizing policy, from the config file's sandboxResources
	// (ADR 0083); a per-agent override must stay inside its bounds.
	sandboxResources sandboxres.Policy
	// installMark is this install's random mark (the chart's koinon-install-mark Secret). Its hash
	// binds the database to the install: tokens and sessions another install issued are refused
	// (ADR 0110). Empty turns the binding off.
	installMark    string
	executorID     string
	wait           time.Duration
	trustedOrigins string
	// trustedProxies lists the proxy networks whose X-Forwarded-For / Forwarded the login and admin API limiters believe.
	trustedProxies string
	// trustedProxyHeader is the header those proxies write: x-forwarded-for or forwarded.
	trustedProxyHeader string

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
	// anchorInterval is how often the org audit chains are anchored in the install chain (ADR 0100
	// decision 4.1).
	anchorInterval time.Duration
	watchPoll      time.Duration

	otlpAddr     string
	otlpTokenDir string
	// otlpTracesTokenDir holds the token only koinon-agentd sends (/v1/traces), apart from the gateway's (issue 55).
	otlpTracesTokenDir string
	// The trace router's byte budgets; 0 derives each from the container memory limit (its cgroup's,
	// or KOINON_MEMORY_LIMIT_BYTES when that is lower; 20% queues, 5% intake, 3% held roots, 8% requests).
	traceQueueBytes, traceIntakeBytes, traceHeldBytes, traceRequestBytes int
	metricsAddr                                                          string
	toolsAddr                                                            string
	savedWorkAddr                                                        string
	guardAddr                                                            string
	guardByteCap                                                         int
	savedWorkSecret                                                      string
	// auditSigningSecret names the Secret with the Ed25519 seed that signs audit checkpoints (ADR 0100
	// decision 4.2); koinon-api creates it on first use and never overwrites it. Empty turns checkpoints off.
	auditSigningSecret string
	secretsNamespace   string
	kubeconfig         string
	githubAPIURL       string
	// chartVersion is the Helm chart's version, which the chart passes; it is only recorded
	// (platform.settings.observed).
	chartVersion string
	// release is the Helm release name, which the chart passes. The start-up check scopes its
	// generated-Secret list with it (#375); it is also recorded in platform.settings.observed.
	release string
	// flags is the flag set serve was started with, for the platform settings record.
	flags *pflag.FlagSet

	kekDir string
	// kekSecret names the Secret with the key-encryption keys. koinon-api creates it on its first
	// start when kekCreate is set and it is missing, and never overwrites it.
	kekSecret string
	kekCreate bool
	// kekWrapping is how the KEKs are wrapped (wrapping.New); zero is the local Secret-held KEK.
	kekWrapping wrapping.Config

	// egressAllowedHosts and its deprecated alias providerAllowedHosts are joined: internal hosts that
	// providers and MCP servers may name.
	// gatewayMCPURL is agentgateway's MCP endpoint as koinon-api reaches it, for approved MCP calls.
	gatewayMCPURL string
	// gatewayMCPCheckURL is the MCP endpoint of the live check of an agent's MCP tools. An agent reaches MCP
	// through agentgateway until the follow-up that moves it (ADR 0097), so the check must use the URL the agent
	// uses, which is not always the URL of approved calls. Empty: the check uses gatewayMCPURL.
	gatewayMCPCheckURL string
	// gatewayURL is agentgateway's LLM listener, for the provider echo check.
	gatewayURL string
	// gatewayMode is the chart's gateway.mode (standalone or kubernetes).
	gatewayMode project.GatewayMode
	// sealMCPv2 is set by the chart when koinon-gateway serves every MCP path (gateway.mcp.driver=koinon): then
	// a stored MCP credential is sealed to the gateway's key when one is active (ADR 0097 decision 12.5 step 0).
	sealMCPv2            bool
	egressAllowedHosts   string
	providerAllowedHosts string
}

// serveFlags registers the flags of "koinon-api serve" (and of a bare "koinon-api", which serves).
func (c *config) serveFlags(fs *pflag.FlagSet) {
	fs.StringVar(&c.addr, "addr", ":8080", "listen address")
	c.databaseFlag(fs)
	fs.BoolVar(&c.migrate, "migrate", false, "apply database migrations before starting")
	c.durableFlag(fs, "run the DBOS runtime (schedules, durable timers; ADR 0066); needs the DBOS tables that \"koinon-api migrate --durable\" creates")
	fs.StringVar(&c.executorID, "executor-id", "",
		"this process's DBOS executor ID, unique among the replicas and never reused (the chart passes the pod name); "+
			"the executors heartbeat into a table and one of them recovers the workflows of a dead one (ADR 0085). "+
			"Empty is DBOS's default, \"local\", which is right for one process only")
	c.waitFlag(fs)
	c.restoreHoldFlag(fs)
	fs.BoolVar(&c.restoreAcceptImporterDeletions, "restore-accept-importer-deletions", false,
		"accept the declarative removals a restore brought in (providers, MCP servers and connections whose Secret is missing); without it the importer keeps them until you accept (ADR 0110 decision 1)")
	fs.StringVar(&c.installMark, "install-mark", "",
		"this install's random mark (the chart's koinon-install-mark Secret). The database records its hash, and API tokens and sessions that another install issued are refused (a restored copy, ADR 0110); empty turns that off")
	fs.StringVar(&c.pricesRefresh, "prices-refresh", string(prices.RefreshAuto),
		"the scheduled price refresh (ADR 0098): auto fetches OpenRouter's public price list only while some org has an openrouter provider, "+
			"on always fetches it, off never makes a request to any price source; a fetched change applies by itself only within the bounds below")
	fs.DurationVar(&c.pricesRefreshInterval, "prices-refresh-interval", 24*time.Hour, "how often the scheduled price refresh fetches")
	fs.IntVar(&c.pricesMaxChange, "prices-refresh-max-change-percent", pricefetch.DefaultBounds.MaxChangePercent,
		"largest change, in percent of the last reviewed rate, that applies without a person; it can only be lowered")
	fs.IntVar(&c.pricesMaxAutoPerDay, "prices-refresh-max-auto-per-day", pricefetch.DefaultBounds.MaxAutoPerDay,
		"most automatic price changes in 24 hours; it can only be lowered")
	fs.StringVar(&c.trustedOrigins, "trusted-origins", "",
		"comma-separated extra origins allowed to make state-changing requests (CSRF)")

	fs.StringVar(&c.trustedProxies, "trusted-proxies", "",
		"comma-separated CIDRs of the proxies and load balancers in front of koinon-api; from a peer in one of them the "+
			"login and admin API limiters read the client address from X-Forwarded-For / Forwarded (rightmost untrusted hop); "+
			"empty never trusts those headers")

	fs.StringVar(&c.trustedProxyHeader, "trusted-proxy-header", ratelimit.HeaderXForwardedFor,
		"the header the --trusted-proxies write the client address to: x-forwarded-for or forwarded; the other is ignored")

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
	fs.DurationVar(&c.anchorInterval, "audit-anchor-interval", time.Hour,
		"how often koinon-api anchors every org audit chain in the install audit chain (one anchor per interval across replicas; at least "+minAnchorInterval.String()+"; every anchor stores one row per org, so a short interval grows the install database fast)")
	fs.DurationVar(&c.watchPoll, "watch-poll-interval", services.DefaultWatchPollInterval,
		"how often a WatchRun stream re-reads its run without a notification (a backstop for missed NOTIFYs)")
	fs.StringVar(&c.otlpAddr, "otlp-addr", ":4318",
		"listen address of the internal OTLP/HTTP receiver (the spend ledger's logs from agentgateway); "+
			"every request needs the bearer token in --otlp-token-dir; empty disables it")
	fs.StringVar(&c.otlpTokenDir, "otlp-token-dir", "",
		"directory with the bearer token of the OTLP receiver's /v1/logs, the gateway's (file token) and, during a rotation, the one it replaced (file previous); "+
			"required with --otlp-addr")
	fs.StringVar(&c.otlpTracesTokenDir, "otlp-traces-token-dir", "",
		"directory with the bearer token of the OTLP receiver's /v1/traces, held only by koinon-agentd (same files as --otlp-token-dir); "+
			"the two tokens never admit each other's path; required with --otlp-addr")

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
	c.kekWrapping.Flags(fs)
	fs.StringVar(&c.auditSigningSecret, "audit-signing-secret", "",
		"Secret with the Ed25519 key that signs audit checkpoints (ADR 0100 decision 4.2); created on first use, never overwritten, read by koinon-api alone; empty turns checkpoints off")
	fs.StringVar(&c.savedWorkAddr, "saved-work-addr", "",
		"listen address of the saved-work upload endpoint, for koinon-agentd only; empty disables it")
	fs.StringVar(&c.savedWorkSecret, "saved-work-secret", "koinon-saved-work",
		"Secret with the key that signs saved-work uploads; created on first use, never overwritten")
	fs.StringVar(&c.guardAddr, "guard-addr", ":8083",
		"listen address of the MCP leak guard (agentgateway's mcpGuardrails, ADR 0091); reachable only from agentgateway. Empty disables it, and the gateway then refuses every org MCP call")
	fs.IntVar(&c.guardByteCap, "leak-guard-byte-cap", mcpguard.DefaultByteCap,
		"payload bytes a run that holds both legs may send through external-low tools before the guard records one over-threshold event; no call is refused for it (ADR 0091 decision 5, amendment 2026-11-17)")
	fs.StringVar(&c.toolsAddr, "tools-addr", ":8081",
		"listen address of the GitHub tools MCP server, for agentgateway only; empty disables it")
	fs.StringVar(&c.secretsNamespace, "secrets-namespace", "",
		"namespace of connection Secrets (default $POD_NAMESPACE); without one, connections cannot be created")
	fs.StringVar(&c.chartVersion, "chart-version", "",
		"version of the Helm chart that deployed this pod (the chart passes it); only recorded in the install audit log")
	fs.StringVar(&c.release, "release", "",
		"Helm release name that deployed this pod (the chart passes it); scopes the generated-Secret list to Secrets labelled koinon.dev/release=<name>, and is recorded in the install audit log")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config when in a cluster")
	fs.StringVar(&c.githubAPIURL, "github-api-url", github.DefaultBaseURL, "GitHub REST API base URL")
	fs.StringVar(&c.kekSecret, "kek-secret", "",
		"Secret with the key-encryption keys, read through the Kubernetes API; takes precedence over --kek-dir")
	fs.StringVar(&c.gatewayURL, "gateway-url", "", "agentgateway's LLM listener as koinon-api reaches it (for example http://koinon-gateway.koinon.svc:8080), for the provider echo check; empty disables the check")
	c.gatewayMode = project.GatewayStandalone
	fs.Var(&c.gatewayMode, "gateway-mode", "gateway.mode: standalone or kubernetes; kubernetes cannot render a server's own CA, so the API refuses caBundle and insecureSkipVerify")
	fs.BoolVar(&c.sealMCPv2, "gateway-mcp-seal-v2", false,
		"seal a stored MCP credential to the gateway's public key (envelope v2) when a key is active, and as v1 otherwise; set only when koinon-gateway serves every MCP path, because agentgateway reads the credential as plaintext")
	fs.StringVar(&c.gatewayMCPURL, "gateway-mcp-url", "", "agentgateway's MCP endpoint, for the calls humans approved on remote MCP servers; empty disables them")
	fs.StringVar(&c.gatewayMCPCheckURL, "gateway-mcp-check-url", "", "the MCP endpoint agents use, for the live check of an agent's MCP tools; empty: --gateway-mcp-url")
	fs.StringVar(&c.egressAllowedHosts, "egress-allowed-hosts", "",
		"comma-separated hosts (optionally host:port) that providers and MCP servers may name although they are internal "+
			"(loopback, link-local, metadata and Koinon's own namespace are denied otherwise); platform-level")
	fs.StringVar(&c.providerAllowedHosts, "provider-allowed-hosts", "",
		"deprecated alias of --egress-allowed-hosts, kept for one release")
	fs.BoolVar(&c.kekCreate, "kek-create", true, "create the --kek-secret Secret with a new key when it does not exist")
	fs.StringVar(&c.kekDir, "kek-dir", "",
		"directory with the mounted key-encryption keys Secret (files named by KEK ID plus \"active\"); "+
			"without it, provider keys and connection secrets cannot be stored")
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

// Main runs koinon-api with args and returns the process exit code.
func Main(ctx context.Context, args []string) int {
	if limit := memlimit.Apply(); limit > 0 {
		slog.Info("memory soft limit set", "bytes", limit)
	}
	return cli.Execute(ctx, NewRootCommand(), args)
}

// serve is "koinon-api serve": the API, the web UI and the listeners beside them.
func serve(ctx context.Context, cfg config) error {
	// stop ends the process's work, as a signal does: a background goroutine that panics calls it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if cfg.migrate {
		if err := migrate(ctx, cfg.databaseURL, cfg.wait, migrateOptions{durable: cfg.durable}); err != nil {
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
	// On a fresh install the database and its schema come up after koinon-api starts (the chart's
	// migration Job waits for CNPG), so bootstrap is retried for up to --wait while the database
	// is unreachable or not migrated. Any other error fails at once.
	runBootstrap := func(ctx context.Context) (auth.BootstrapResult, error) {
		return auth.Bootstrap(ctx, pool, auth.BootstrapConfig{
			Username:               cfg.bootstrapUsername,
			PasswordFile:           cfg.bootstrapPasswordFile,
			LoginEnabled:           cfg.bootstrapLogin,
			DefaultOrgIdentityMode: mode,
			OIDCIssuer:             cfg.oidcIssuer,
			GroupMappings:          mappings,
		}, time.Now())
	}
	res, err := retryBootstrap(ctx, cfg.wait, 2*time.Second, func() (auth.BootstrapResult, error) { return runBootstrap(ctx) })
	// Bootstrap writes install audit events in the transaction of its change (ADR 0100 decision 3).
	// A database that does not yet let this login write the chain (the CloudNativePG upgrade window,
	// decision 1.1a) must not stop the start: the transaction rolled back, so bootstrap is idempotently
	// retried in the background until the grant arrives.
	// The transaction is deferred until the grant arrives (retried below, once the background context
	// exists); bootstrap sessions are revoked meanwhile when bootstrap login is off.
	bootstrapDenied := errors.Is(err, audit.ErrInstallWriteDenied)
	bootstrapErr := err
	if bootstrapDenied {
		slog.Warn("bootstrap is deferred", "revoked_bootstrap_sessions", res.RevokedSessions)
	} else if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if !bootstrapDenied {
		reportBootstrap(res, cfg.bootstrapLogin)
	}

	// The install binding (ADR 0110): which install this database belongs to. A held start writes an
	// unbound row, so a restored copy refuses the source's tokens and sessions from its first request.
	bindingOwner := ""
	if cfg.installMark != "" {
		bindingOwner = execution.OwnerID(cfg.installMark)
	}
	binding := installbinding.New(pool, bindingOwner, cfg.restoreHold, slog.Default()).WithScope(installbinding.ScopeEdge)
	if st, err := binding.Claim(ctx, installbinding.Targets{
		PublicURL: installbinding.Origin(cfg.publicURL), OIDCRedirectURL: installbinding.Origin(cfg.oidcRedirectURL),
	}); err != nil {
		// The schema may not be migrated yet; Run retries the claim and credentials are refused until it succeeds.
		slog.Warn("install binding: first claim failed, retrying in the background", "err", err)
	} else if binding.Enabled() {
		slog.Info("install binding", "state", st, "held", cfg.restoreHold)
	}
	safego.Go(ctx, slog.Default(), "install-binding", binding.Run)

	authCfg := auth.Config{
		Binding:            binding,
		SessionIdleTimeout: cfg.sessionIdle,
		SessionMaxLifetime: cfg.sessionMax,
		BootstrapLogin:     cfg.bootstrapLogin,
		PublicURL:          cfg.publicURL,
		DeviceCookieSecret: []byte(cfg.installMark),
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
	// Run content past its org's retention is blanked (issue #69). Idempotent and batched with
	// SKIP LOCKED, so every replica may run it.
	contentPurger := orgsettings.NewPurger(pool, slog.Default(), time.Now)
	safego.GoOrStop(purgeCtx, slog.Default(), "run-content-purger", stop, func(ctx context.Context) { contentPurger.Run(ctx, time.Hour) })

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
	// A platform entry the new default list caught up with is retired (ADR 0098 decision 1.5). It
	// writes an install audit event for what it retired (ADR 0100), so a fresh install with no admin is covered. It is
	// bookkeeping: a failure (a stored entry this binary refuses, a database hiccup) is a warning,
	// never a reason not to serve; the next start tries again.
	// A retirement the install audit chain refused (the writer role is not granted yet, ADR 0100
	// decision 1.1a) is retried in the background once the background context exists, below.
	_, retireErr := priceSvc.RetireCaughtUp(ctx)
	if retireErr != nil {
		slog.Warn("platform price retirement skipped; the platform entries stay as they are", "error", retireErr)
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
	if bootstrapDenied {
		_ = retryInstallWrite(bgCtx, stop, installRetry{Goroutine: "bootstrap-retry", What: "bootstrap"}, installWriteRetry,
			bootstrapErr, bootstrapRetryWrite(runBootstrap, cfg.bootstrapLogin))
	}
	if errors.Is(retireErr, audit.ErrInstallWriteDenied) {
		_ = retryInstallWrite(bgCtx, stop, installRetry{Goroutine: "price-retire-caught-up", What: "platform price retirement"}, installWriteRetry,
			retireErr, func(ctx context.Context) error {
				_, err := priceSvc.RetireCaughtUp(ctx)
				return err
			})
	}
	hub := runwatch.New(pool.Config().ConnConfig, slog.Default())
	safego.GoOrStop(bgCtx, slog.Default(), "runwatch-hub", stop, func(ctx context.Context) { hub.Run(ctx) })
	if cfg.restoreHold {
		// Restore-hold (issue #462): nothing acts until the operator releases it. The sweepers cancel runs
		// past their wall clock and expire approvals and questions by time, and a restored backlog is older
		// than its clock; the DBOS runtime would fire schedules and recover pending workflows.
		slog.Warn("restore-hold: schedules, the DBOS runtime, the sweepers, the price refresh and the code-host scope refresh are off until the hold is released (restoreHold=false in the chart)")
	} else {
		safego.GoOrStop(bgCtx, slog.Default(), "scheduler-sweeper", stop, func(ctx context.Context) {
			scheduler.Sweeper(ctx, pool, cfg.sweepInterval, slog.Default())
		})
	}
	// Notifications that follow a commit (pre-wakes) are DBOS steps with --durable, and inline
	// best effort without it (ADR 0066, practice B2).
	var notifier *durable.Notifier
	var durableRT *durable.Runtime // for its metrics, registered once the registry exists
	var sched *schedule.Scheduler
	var pollers *poll.Poller
	runDurable := cfg.durable && !cfg.restoreHold
	if !cfg.durable {
		slog.Warn("schedule triggers do not fire: they run on the DBOS runtime (api.durable.enabled, --durable)")
	}
	var directMessages *services.DirectMessageQueue
	var runNow *services.RunNowQueue
	if runDurable {
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
		// The shared code-host pollers (#25): one poller per (connection, resource), run by the
		// replica that holds their leader lock. No trigger subscribes yet (the code_host_event
		// filter is not defined), so they have no rows; they start below with the schedule.
		pollers = poll.New(poll.Config{Logger: slog.Default()}, pool, orgWork, poll.NoSubscriptions)
		if err := rt.Register(pollers.Registrar()); err != nil {
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
	// The gateway's public key (ADR 0097 decision 12): koinon-api holds the active public key only, read
	// from the table every reload interval. No caller seals a v2 envelope yet (slice 3); the key is held
	// ready, and a row that fails its check is refused and logged.
	gatewayKeys := &gatewaykeys.PublicKeys{}
	safego.GoOrStop(bgCtx, slog.Default(), "gateway-public-key", stop, func(ctx context.Context) {
		gatewayKeys.Run(ctx, pool, func(f string, a ...any) { slog.Warn(fmt.Sprintf(f, a...)) })
	})
	// The secret store (ADR 0056): provider keys and connection secrets are envelopes in
	// Postgres, wrapped by the KEKs of a mounted Secret, which are wrapped in turn as
	// cfg.kekWrapping says (ADR 0085 decision 12). koinon-api is decrypt-only: with a KMS, new KEKs
	// come from the key-admin Job.
	var keyring *secretstore.Keyring
	// Once the install is bound-only (old issue 615), KEKs stored unbound are refused; the marker is read on
	// every load until it is set.
	wrappers, err := wrapping.New(ctx, cfg.kekWrapping, wrapping.Service, installid.BoundOnlySource(pool))
	if err != nil {
		return err
	}
	// KMS-wrapped KEKs are bound to the install ID the database holds (old issue 615).
	wrappers = wrappers.WithInstallID(installid.Source(pool))
	// On every load and reload: probe the active KEK's wrapping key (koinon_kek_active_reachable,
	// registered below with the other metrics) and record the KEK fingerprints (old issue 591).
	kekMetrics := kekhealth.NewMetrics(nil)
	kekMonitor := &kekhealth.Monitor{Wrappers: wrappers, Metrics: kekMetrics, Recorder: &kekhealth.Recorder{Q: store.New(pool)}, Log: slog.Default()}
	// The key events of the install audit chain (ADR 0100 decision 3): the keys this process generates,
	// and the check on start that compares the whole key-set state with the last recorded one.
	keyAudits := newKeyAudit(pool, kube, cfg.secretsNamespace, cfg.release, cfg.kekSecret, cfg.savedWorkSecret, cfg.auditSigningSecret)
	switch {
	case cfg.kekSecret != "" && kube != nil:
		// The chart renders no key material (a render would make new keys every time, and a new
		// KEK loses every stored secret): koinon-api creates the Secret once, and only reads it after.
		kekStore := keyscli.KubeStore{
			Client: kube, Namespace: cfg.secretsNamespace, Name: cfg.kekSecret, Wrappers: wrappers,
			UsedID: func(ctx context.Context, id string) (bool, error) { return kekIDUsedYet(ctx, pool, id) },
		}
		keyAudits.keySet = kekStore
		if cfg.kekCreate {
			// The first KEK of an install makes it bound-only (old issue 615): no KEK stored unbound belongs here.
			created, err := keyAudits.ensureGenerated(ctx, "secret", cfg.kekSecret, func(ctx context.Context) (bool, error) {
				return kekStore.EnsureWith(ctx, time.Now(),
					func(ctx context.Context) (bool, error) { return keyscli.AnyEnvelope(ctx, pool) },
					func(ctx context.Context) error {
						_, err := installid.SetBoundOnly(ctx, pool, secretstore.BoundOnlyFirstInstall)
						return err
					})
			})
			if err != nil {
				return err
			}
			if created {
				slog.Warn("created the key-encryption key Secret; back it up now ("+project.SecretKeysDocURL+")", "secret", cfg.kekSecret)
			}
		}
		if keyring, err = loadKeyring(ctx, kekStore, kekMonitor.Observer()); err != nil {
			return err
		}
		safego.GoOrStop(bgCtx, slog.Default(), "keyring-watch", stop, func(ctx context.Context) {
			watchKeyring(ctx, keyring, kekStore, time.Minute, kekMonitor.Observer(), keyAudits.watchChanged)
		})
	case cfg.kekDir != "":
		keyAudits.keySet = keyscli.DirStore{Dir: cfg.kekDir, Wrappers: wrappers}
		if keyring, err = secretstore.LoadDir(ctx, cfg.kekDir, wrappers, kekMonitor.Observer()); err != nil {
			return err
		}
		safego.GoOrStop(bgCtx, slog.Default(), "keyring-watch-dir", stop, func(ctx context.Context) {
			keyring.WatchDir(ctx, cfg.kekDir, wrappers, time.Minute, slog.Default(), kekMonitor.Observer())
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
	// The driver gate (ADR 0097 decision 12.5 step 0): without it koinon-api seals v1 only.
	var mcpRecipient mcpservers.RecipientSource
	if cfg.sealMCPv2 {
		mcpRecipient = gatewayKeys
	}
	reg := metrics.NewRegistry()
	apiMetrics := api.NewMetrics(reg)
	asm, err := apiassembly.Assemble(ctx, apiassembly.Outside{
		Pool: pool, Kube: kube, Keyring: keyring, Binding: binding, BindingOwner: bindingOwner, Ready: gate.ReadyHandler(),
	}, apiassembly.Settings{
		SandboxResources: cfg.sandboxResources, EgressAllowedHosts: splitList(cfg.egressAllowedHosts + "," + cfg.providerAllowedHosts),
		Namespace: cfg.secretsNamespace, GatewayMCPURL: cfg.gatewayMCPURL, GatewayURL: cfg.gatewayURL, GatewayMode: cfg.gatewayMode, GitHubAPIURL: cfg.githubAPIURL,
		WatchPoll: cfg.watchPoll, TrustedOrigins: splitList(cfg.trustedOrigins), RestoreHold: cfg.restoreHold,
		GuardByteCap: cfg.guardByteCap, PublicURL: cfg.publicURL, OIDCIssuer: cfg.oidcIssuer, OIDCRedirectURL: cfg.oidcRedirectURL,
		AcceptImporterDeletions: cfg.restoreAcceptImporterDeletions,
		MCPRecipient:            mcpRecipient,
	}, apiassembly.Prebuilt{
		Auth: authSvc, Hub: hub, Metrics: apiMetrics, Notifier: notifier, DirectMessages: directMessages, RunNow: runNow,
	})
	if err != nil {
		return err
	}
	svc := asm.Server
	if !cfg.restoreHold {
		safego.GoOrStop(bgCtx, slog.Default(), "approval-sweeper", stop, func(ctx context.Context) { asm.Approvals.Sweeper(ctx, cfg.sweepInterval) })
		// The periodic refresh and the services' on-change readings share the scope module's queue (Syncer.Request); svc.Drain drains it at shutdown.
		safego.GoOrStop(bgCtx, slog.Default(), "code-host-scope", stop, func(ctx context.Context) { asm.Scope.Run(ctx, scopesync.DefaultInterval) })
		// The notification of an opened question (the park that opens it commits in koinon-agentd).
		safego.GoOrStop(bgCtx, slog.Default(), "question-sweeper", stop, func(ctx context.Context) { svc.QuestionSweeper(ctx, cfg.sweepInterval) })
	}
	if sched != nil {
		sched.Bind(svc)
		safego.GoOrStop(bgCtx, slog.Default(), "schedule-triggers", stop, func(ctx context.Context) { sched.Run(ctx) })
		slog.Info("schedule triggers started")
	}
	if !cfg.restoreHold {
		// The release step (revoke every session, record restore.release) runs here, before the
		// listeners start, and is retried until it succeeds; it does not wait for an importer pass.
		rel := newReleaseAtStart(binding, pool, kube, cfg.secretsNamespace, cfg.restoreAcceptImporterDeletions)
		if !rel.Try(ctx, 15*time.Second) {
			safego.GoOrStop(bgCtx, slog.Default(), "restore-release", stop, func(ctx context.Context) { rel.Retry(ctx) })
		}
	}
	if pollers != nil {
		pollers.Bind(poll.ConnectionOpener{Hosts: asm.Hosts, Keys: asm.Opener}, poll.LogDeliverer{Logger: slog.Default()})
		safego.GoOrStop(bgCtx, slog.Default(), "code-host-pollers", stop, func(ctx context.Context) { pollers.Run(ctx) })
	}
	if kube != nil {
		var sealer connections.Sealer
		if keyring != nil {
			sealer = keyring
		}
		imp := &declarative.Importer{Client: kube, Namespace: cfg.secretsNamespace, Pool: pool, Catalog: asm.Catalog, MCP: asm.MCP, Outward: outward.New(pool).WithRestoreHold(asm.Hold), Held: asm.Hold, Release: binding, AcceptRemovals: cfg.restoreAcceptImporterDeletions, URLPolicy: asm.URLPolicy, Sealer: sealer, Log: slog.Default()}
		safego.GoOrStop(bgCtx, slog.Default(), "declarative-importer", stop, func(ctx context.Context) { imp.Run(ctx) })
	} else {
		slog.Warn("no Kubernetes access: declarative Secrets are not imported")
	}

	audit.RegisterMetrics(reg)
	auth.RegisterMetrics(reg)
	// koinon-api checks its own login against the install audit writer role, at startup and every
	// installWriteRetry: only it knows the login it connects as (the migrate Job may be another
	// identity). A missing grant is logged with the GRANT to run and shows as
	// koinon_install_audit_writer_ok 0; it never fails the start (ADR 0100 decision 1.1a).
	audit.RegisterInstallWriterMetric(reg)
	// The anchor job (ADR 0100 decision 4.1): every replica polls, the first to find an anchor due writes
	// it. A refused write (the writer role not granted yet) is logged and retried at the next poll.
	if err := validateAnchorInterval(cfg.anchorInterval); err != nil {
		return err
	}
	audit.RegisterAnchorMetric(reg)
	anchorJob := &audit.AnchorJob{DB: pool, Interval: cfg.anchorInterval, Log: slog.Default()}
	// Signed checkpoints (ADR 0100 decision 4.2) need the Kubernetes API for the signing Secret; without
	// it, or without --audit-signing-secret, none is signed and the checkpoint age gauge only grows.
	switch {
	case cfg.auditSigningSecret != "" && kube != nil:
		anchorJob.Keys = &kubeSigningKeys{kube: kube, namespace: cfg.secretsNamespace, name: cfg.auditSigningSecret, audits: keyAudits}
	default:
		slog.Warn("audit checkpoints are off: they need --audit-signing-secret and access to the Kubernetes API")
	}
	safego.GoOrStop(bgCtx, slog.Default(), "audit-anchor", stop, func(ctx context.Context) { anchorJob.Run(ctx) })
	writerCheck := &audit.InstallWriterCheck{DB: pool, Log: slog.Default()}
	writerCheck.Check(ctx)
	safego.GoOrStop(bgCtx, slog.Default(), "install-audit-writer-check", stop, func(ctx context.Context) {
		writerCheck.Run(ctx, installWriteRetry)
	})
	reg.MustRegister(kekMetrics.Collectors()...)
	reg.MustRegister(asm.GuardMetrics.Collectors()...)
	restorehold.Metric(reg, cfg.restoreHold)
	binding.Metrics(reg)
	binding.ReleasePendingMetric(reg)
	declarative.RegisterMetrics(reg)
	if durableRT != nil {
		// Every replica reports the same database-derived counts; alerts aggregate with max (ADR 0085).
		reg.MustRegister(durableRT.Collector())
	}
	if pollers != nil {
		reg.MustRegister(pollers.Collector())
	}
	// The price refresh (ADR 0098, amended 2026-11-07). Off, koinon-api makes no request to any price
	// source; auto fetches a source only while a provider of a type it prices exists.
	priceMetrics := prices.NewMetrics(reg)
	refreshMode, err := prices.ParseRefreshMode(cfg.pricesRefresh)
	if err != nil {
		return fmt.Errorf("--prices-refresh: %w", err)
	}
	if refreshMode != prices.RefreshOff && cfg.pricesRefreshInterval < minPricesRefreshInterval {
		return fmt.Errorf("--prices-refresh-interval %s is below the minimum %s", cfg.pricesRefreshInterval, minPricesRefreshInterval)
	}
	refresher, err := prices.NewRefresher(priceSvc, prices.RefreshConfig{
		Adapters: pricefetch.NewAdapters(httpClient()),
		Bounds:   pricefetch.Bounds{MaxChangePercent: cfg.pricesMaxChange, MaxAutoPerDay: cfg.pricesMaxAutoPerDay},
		Metrics:  priceMetrics,
		Catalog:  orcatalog.New(pool),
	})
	if err != nil {
		return fmt.Errorf("price refresh: %w", err)
	}
	// The platform price RPCs (PriceService, platform admin) accept, dismiss and fetch through the
	// same refresher as the loop below and the prices commands.
	svc.BindPriceRefresher(refresher)
	// Startup bookkeeping never depends on a platform admin existing (a fresh install has none until
	// the first login): without one, the change is recorded and logged, not audited in an org. Nor
	// does it wait for the install audit writer's grant (ADR 0100 decision 1.1a): on an upgrade of a
	// CloudNativePG cluster that grant is the chart's post-upgrade hook, which Helm runs only once these
	// pods are ready, so a refused audit write is retried in the background instead of failing the start.
	syncSettings := func(ctx context.Context) error {
		return refresher.SyncSettings(ctx, refreshMode, cfg.pricesRefreshInterval)
	}
	if err := retryInstallWrite(bgCtx, stop, installRetry{Goroutine: "price-refresh-settings", What: "price refresh setting"},
		installWriteRetry, syncSettings(ctx), syncSettings); err != nil {
		return err
	}
	if _, err := refresher.ReportUnpriced(ctx); err != nil {
		slog.Warn("counting unpriced models", "err", err)
	}
	slog.Info("price refresh", "mode", refreshMode, "interval", cfg.pricesRefreshInterval,
		"autoApply", refreshMode != prices.RefreshOff, "maxChangePercent", cfg.pricesMaxChange, "maxAutoPerDay", cfg.pricesMaxAutoPerDay)
	// The loop gets the configured setting but runs each tick in the recorded one where that fetches
	// less (Refresher.Effective): while the retry above has not recorded the setting and its install
	// event, mode on does not fetch on the strength of an unrecorded change.
	if !cfg.restoreHold {
		safego.GoOrStop(bgCtx, slog.Default(), "price-refresh", stop, func(ctx context.Context) {
			refresher.Loop(ctx, refreshMode, cfg.pricesRefreshInterval)
		})
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
		// /v1/traces has its own token, held only by koinon-agentd (issue 55); every other path,
		// /v1/logs included, takes the gateway's.
		otlpTraceTokens, err := otlpauth.NewTokens(cfg.otlpTracesTokenDir, slog.Default())
		if err != nil {
			return fmt.Errorf("--otlp-traces-token-dir: %w", err)
		}
		if otlpTokens.SharesToken(otlpTraceTokens) {
			return errors.New("--otlp-token-dir and --otlp-traces-token-dir hold the same token: /v1/traces needs a token of its own, held only by koinon-agentd (issue 55)")
		}
		otlpMetrics := otlpauth.NewMetrics(reg)
		// The trace router (ADR 0074): agentd's spans arrive at /v1/traces, inside the token check.
		exportClient, err := export.NewClient(asm.URLPolicy.DialContext(&net.Dialer{Timeout: 5 * time.Second}), 0)
		if err != nil {
			return err
		}
		traceRouter, err := tracerouter.New(tracerouter.Config{
			Store: tracerouter.NewPGStore(pool), Routes: asm.Traces, Secrets: tracerouter.NewPGSecrets(pool, asm.Opener, slog.Default()),
			Health: tracerouter.NewPGHealth(pool), Sender: exportClient, MemoryLimit: memlimit.Limit(),
			QueueBytes: cfg.traceQueueBytes, IntakeBytes: cfg.traceIntakeBytes, MaxHeldBytes: cfg.traceHeldBytes, RequestBytes: cfg.traceRequestBytes,
			Metrics: tracerouter.NewMetrics(reg), Log: slog.Default(),
		})
		if err != nil {
			return err
		}
		traceRouter.Start(bgCtx)
		defer traceRouter.Close()
		// The gateway State stream (ADR 0097, issue 279 slice 3) shares this listener and the gateway's
		// token: only a holder of it can open the stream or ack a revision, and agentd's trace token
		// never admits it. The server also sits behind the same check on its own (it refuses to start
		// without one). Acks are recorded and gate nothing yet.
		gwAuth := otlpauth.ScopedMiddleware(otlpTokens, map[string]*otlpauth.Tokens{tracerouter.TracesPath: otlpTraceTokens}, otlpMetrics)
		stateSrv, err := stateserver.New(stateserver.Config{
			Store: stateserver.NewPGStore(pool), Auth: gwAuth, Notify: stateserver.PoolNotify(pool),
			Metrics: stateserver.NewMetrics(reg), Log: slog.Default(),
		})
		if err != nil {
			return err
		}
		safego.Go(bgCtx, slog.Default(), "gateway-state-stream", stateSrv.Run)
		safego.GoOrStop(ctx, slog.Default(), "ledger-otlp", stop, func(context.Context) {
			slog.Info("starting the internal OTLP receiver", "addr", cfg.otlpAddr)
			if err := agentgateway.Serve(bgCtx, cfg.otlpAddr, &ledger.Ingester{DB: pool, Metrics: ledger.NewMetrics(reg), Prices: prices.NewResolver(pricing.Embedded())}, slog.Default(), gwAuth, stateSrv.Mount(), traceRouter.Mount()); err != nil {
				slog.Error("spend ledger OTLP receiver failed", "err", err)
				stop()
			}
		})
	}

	proxies, err := ratelimit.ParseTrustedProxies(splitList(cfg.trustedProxies))
	if err != nil {
		return fmt.Errorf("--trusted-proxies: %w", err)
	}
	if err := proxies.UseHeader(cfg.trustedProxyHeader); err != nil {
		return fmt.Errorf("--trusted-proxy-header: %w", err)
	}
	srv := &http.Server{Addr: cfg.addr, Handler: proxies.Middleware(asm.Handler), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{srv}
	toolsErr := make(chan error, 1)
	if cfg.toolsAddr != "" {
		tools, err := githubtools.New(pool, githubtools.Options{
			Hosts: asm.Hosts,
			Keys:  asm.Opener,
			// Trusted-preset calls run at once instead of on the next sweep.
			Execute: asm.ExecuteAllowed,
			// A call no person decides (a Trusted preset, an approval scope) runs only when the
			// leak guard finds it clean; otherwise it goes to a person (ADR 0091, #687).
			LeakCheck: asm.Guard.CheckCodeHost,
		})
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/mcp", tools.Handler())
		// The ask-tier tools of org MCP servers (plan item MC1), for agentgateway only like /mcp.
		mux.Handle(mcpask.Path, asm.Ask.Handler())
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
		_, err := keyAudits.ensureGenerated(ctx, "secret", secretName, func(ctx context.Context) (bool, error) {
			_, created, err := ensureSavedWorkKey(ctx, kube, cfg.secretsNamespace, secretName)
			return created, err
		})
		if err != nil {
			return fmt.Errorf("saved-work upload key: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle(savedwork.UploadPath, savedwork.NewReceiver(pool, keyring, keyFn, slog.Default()))
		// The live check of an agent's MCP tools (old issue 488): koinon-api reads the agent's gateway key
		// and lists the tools as the agent; koinon-agentd gets names only, never the key.
		if checkURL := cmp.Or(cfg.gatewayMCPCheckURL, cfg.gatewayMCPURL); checkURL != "" {
			mux.Handle(mcpcheck.Path, mcpcheck.Server{Keys: mcpcheck.KubeAgentKeys{Client: kube, Namespace: cfg.secretsNamespace}, Gateway: checkURL, Key: keyFn})
		}
		// The password of an agent's home repository (ADR 0032): koinon-agentd, the checkpoint
		// writer, asks per checkpoint and holds no key-encryption key.
		mux.Handle(checkpoint.RepositoryPath, checkpoint.Server{Passwords: checkpoint.Passwords{Q: store.New(pool), Keys: keyring, Begin: pool.Begin}, Key: keyFn})
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
	// Every key this process could have generated exists now. Compare the whole key-set state with the last
	// recorded key event and record the difference (ADR 0100 decision 3): a KEK made outside Koinon, a
	// Secret the ensure-secrets Job generated, an activate or a retire whose event was lost.
	keyAudits.startObserving(bgCtx, stop)
	// Record the Helm-set platform settings that differ from the last recorded ones (ADR 0100 decision 3).
	if cfg.flags != nil {
		settings, err := platformSettings(cfg.flags)
		if err != nil {
			return err
		}
		startObservingSettings(bgCtx, stop, audit.NewRunner(pool), settings)
	}
	var guardSrv *grpc.Server
	if cfg.guardAddr != "" {
		lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.guardAddr)
		if err != nil {
			return fmt.Errorf("--guard-addr: %w", err)
		}
		guardSrv = grpc.NewServer()
		extmcp.RegisterExtMcpServer(guardSrv, mcpguard.NewServer(asm.Guard))
		safego.GoOrStop(ctx, slog.Default(), "mcp-guard", stop, func(context.Context) {
			slog.Info("serving the MCP leak guard", "addr", cfg.guardAddr)
			if err := guardSrv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				toolsErr <- fmt.Errorf("mcp leak guard: %w", err)
				stop()
			}
		})
	} else {
		slog.Warn("the MCP leak guard is off (--guard-addr empty): the gateway refuses every org MCP call")
	}
	shutdownDone := make(chan struct{})
	safego.Go(ctx, slog.Default(), "shutdown", func(context.Context) {
		defer close(shutdownDone)
		<-ctx.Done()
		if guardSrv != nil {
			guardSrv.GracefulStop()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
		// Then the background code-host scope readings: the requests that start them have ended, so
		// they finish, or are canceled, inside the same deadline.
		svc.Drain(shutdownCtx)
	})

	slog.Info("starting koinon-api", "version", project.Version, "addr", cfg.addr,
		"oidc", cfg.oidcIssuer != "", "web_ui", web.Dist() != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown starts; the process must not exit before the drain.
	<-shutdownDone
	select {
	case err := <-toolsErr:
		return fmt.Errorf("github tools server: %w", err)
	default:
		return nil
	}
}

// migrateOptions are the flags of "koinon-api migrate".
type migrateOptions struct {
	// durable also creates the DBOS runtime's tables.
	durable bool
	// grantsOnly applies no migration: it waits, within the same wait, for the install audit writer
	// role and grants it (grantInstallAuditWriter).
	grantsOnly bool
	// requireWriter fails the migration when the install audit writer role does not exist, instead
	// of warning. The chart passes it on an external database, where no hook will create the role
	// later: the Job fails with the remedy rather than koinon-api logging a refused write forever.
	requireWriter bool
	// waitGateway makes --grants-only wait for the gateway group role too (gateway.mcp.driver=koinon).
	waitGateway bool
}

// migrate applies pending migrations, first waiting up to wait for the database: on a fresh install
// it may still be starting.
func migrate(ctx context.Context, dsn string, wait time.Duration, o migrateOptions) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := waitForDatabase(waitCtx, db); err != nil {
		return err
	}
	if o.grantsOnly {
		// The wait ends once the role and the tables exist and the grant is made. It does not wait for
		// koinon_api's membership in the role: CNPG adds the managed role's inRoles on its own schedule,
		// possibly after this hook succeeds. That is harmless: koinon-api retries every install write
		// the database refuses (retryInstallWrite) until the membership lands (ADR 0100 decision 1.1a).
		if err := grantInstallAuditWriter(waitCtx, wait, 2*time.Second, func(ctx context.Context) (bool, error) {
			return migrations.GrantInstallAuditWriter(ctx, db)
		}); err != nil {
			return err
		}
		if o.waitGateway {
			return waitGrantGateway(waitCtx, wait, 2*time.Second, db)
		}
		return grantGateway(waitCtx, db)
	}

	// On an external database nothing creates the install audit writer role later, so the chart
	// passes --require-install-writer. The check needs no table and runs before any migration: a
	// failed Job leaves the schema as it was.
	if o.requireWriter {
		if err := migrations.CheckInstallAuditWriter(ctx, db); err != nil {
			return fmt.Errorf("%w: create the role and add koinon-api's login role to it "+
				"("+project.InstallAuditDocURL+"), then run the migration again; no migration was applied; "+
				"without it koinon-api records no install audit event", err)
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
	// The install audit writer role is the chart's (CloudNativePG managed role, or the operator's on an
	// external database). On a Helm upgrade of a CloudNativePG cluster it may appear only after this
	// pre-upgrade hook, so a missing role is a warning, not a failure: the chart's post-upgrade hook
	// (Argo CD: PostSync) runs "migrate --grants-only", which waits for it (ADR 0100 decision 1.1a).
	granted, err := migrations.GrantInstallAuditWriter(ctx, db)
	if err != nil {
		return fmt.Errorf("grant the install audit writer: %w", err)
	}
	if !granted && o.requireWriter {
		// The role was there before the migrations (checked above): it was dropped meanwhile.
		return fmt.Errorf("the role %s or the install audit tables disappeared during the migration: "+
			"create the role ("+project.InstallAuditDocURL+"), then run the migration again", migrations.InstallAuditWriterRole)
	}
	if !granted {
		slog.Warn("the role " + migrations.InstallAuditWriterRole + " does not exist yet, so it was granted nothing: " +
			"with the chart's CloudNativePG cluster its post-upgrade grant hook grants it once the Cluster has created it; " +
			"on an external database create it (" + project.InstallAuditDocURL + ") and run koinon-api migrate again. " +
			"Until then koinon-api writes no install audit event")
	}
	if err := grantGateway(ctx, db); err != nil {
		return err
	}
	// DBOS keeps its own tables in its own schema; creating that schema needs CREATE on the
	// database, which an external migrate role may lack. So it runs only when the DBOS runtime is
	// on (the chart passes --durable to the migration Job when api.durable.enabled).
	if !o.durable {
		return nil
	}
	return durable.Migrate(ctx, dsn, slog.Default())
}

// grantGateway gives koinon-gateway's database role its privileges (ADR 0097 decision 5). The role
// exists only when the chart's gateway switch is on, so a missing role is not an error: nothing
// connects as it, and the next migrate or --grants-only run grants it once it exists.
func grantGateway(ctx context.Context, db migrations.Execer) error {
	granted, err := migrations.GrantGateway(ctx, db)
	if err != nil {
		return fmt.Errorf("grant the gateway role: %w", err)
	}
	if !granted {
		slog.Info("the role " + migrations.GatewayRole + " does not exist, so it was granted nothing (the Koinon MCP gateway is off, or the role comes later)")
	}
	// The roles of the gateway key lifecycle (ADR 0097 decision 12). They exist only with the chart's
	// gateway key switch; a missing one is skipped.
	roles, err := migrations.GrantGatewayKeyRoles(ctx, db)
	if err != nil {
		return fmt.Errorf("grant the gateway key roles: %w", err)
	}
	if len(roles) > 0 {
		slog.Info("granted the gateway key roles their privileges", "roles", roles)
	}
	return nil
}

// waitGrantGateway calls GrantGateway every poll until the role exists and is granted, for as long as ctx
// lasts. A new CloudNativePG managed role appears only after the Cluster update, so one call can find
// nothing to grant on an upgrade.
func waitGrantGateway(ctx context.Context, wait, poll time.Duration, db migrations.Execer) error {
	for {
		granted, err := migrations.GrantGateway(ctx, db)
		switch {
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("grant the gateway role: %w", err)
		case granted:
			slog.Info("granted the gateway role its privileges", "role", migrations.GatewayRole)
			return nil
		}
		slog.Info("waiting for the gateway role", "role", migrations.GatewayRole)
		select {
		case <-ctx.Done():
			return fmt.Errorf("the role %s did not appear within %s: the CloudNativePG Cluster has not reconciled its managed roles", migrations.GatewayRole, wait)
		case <-time.After(poll):
		}
	}
}

// grantInstallAuditWriter calls grant every poll until it reports that it granted, for as long as
// ctx lasts (the migrate Job's --wait). On an upgrade of a CloudNativePG cluster the Cluster creates
// the writer role (a managed role) only after the pre-upgrade migration, on the operator's own
// schedule; the post-upgrade hook waits for it here. Running out of time fails the hook, so the
// release reports what the pods would otherwise only log.
func grantInstallAuditWriter(ctx context.Context, wait, poll time.Duration, grant func(context.Context) (bool, error)) error {
	for {
		granted, err := grant(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("grant the install audit writer: %w", err)
		case granted:
			slog.Info("granted the install audit writer its privileges", "role", migrations.InstallAuditWriterRole)
			return nil
		}
		slog.Info("waiting for the install audit writer role and tables", "role", migrations.InstallAuditWriterRole)
		select {
		case <-ctx.Done():
			return fmt.Errorf("the role %s or the install audit tables did not appear within %s: the CloudNativePG Cluster has not reconciled its managed roles, or the migration did not run", migrations.InstallAuditWriterRole, wait)
		case <-time.After(poll):
		}
	}
}

// waitForDatabase pings db until it answers or ctx ends.
func waitForDatabase(ctx context.Context, db *sql.DB) error {
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		slog.Info("waiting for the database", "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("database not reachable: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// savedWorkKey returns the key that signs saved-work uploads, creating its Secret (32 random bytes
// under "key") when it does not exist and never overwriting one that does.
func savedWorkKey(ctx context.Context, kube kubernetes.Interface, namespace, name string) ([]byte, error) {
	key, _, err := ensureSavedWorkKey(ctx, kube, namespace, name)
	return key, err
}

// ensureSavedWorkKey is savedWorkKey that also reports whether this call created the Secret (not a
// concurrent replica, not an earlier start), so the start records keys.generated once.
func ensureSavedWorkKey(ctx context.Context, kube kubernetes.Interface, namespace, name string) ([]byte, bool, error) {
	return ensureRandomKeySecret(ctx, kube, namespace, name, savedwork.KeyFile)
}

// ensureRandomKeySecret returns the 32 bytes under dataKey of the Secret, creating the Secret with 32
// random bytes when it does not exist and never overwriting one that does. created is true only for the
// call that made the Secret. Both the saved-work key and the audit signing seed are made this way.
func ensureRandomKeySecret(ctx context.Context, kube kubernetes.Interface, namespace, name, dataKey string) ([]byte, bool, error) {
	secrets := kube.CoreV1().Secrets(namespace)
	get := func() ([]byte, error) {
		sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if len(sec.Data[dataKey]) < 32 {
			return nil, fmt.Errorf("the Secret %s/%s has no usable %q", namespace, name, dataKey)
		}
		return sec.Data[dataKey], nil
	}
	key, err := get()
	if err == nil || !apierrors.IsNotFound(err) {
		return key, false, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, false, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{project.Domain + "/managed-by": "koinon-api"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{dataKey: k},
	}, metav1.CreateOptions{FieldManager: "koinon-api"})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, false, err
	}
	created := err == nil // not AlreadyExists: another replica made it
	key, err = get()
	return key, created && err == nil, err
}

// minAnchorInterval is the shortest --audit-anchor-interval: every anchor is an install chain event and
// one row per org in install_audit_anchor_heads (100 orgs every 5 minutes is about 10 million rows a year),
// and every anchor is also a line in the install export's header.
const minAnchorInterval = 5 * time.Minute

// validateAnchorInterval rejects an --audit-anchor-interval below minAnchorInterval.
func validateAnchorInterval(d time.Duration) error {
	if d < minAnchorInterval {
		return fmt.Errorf("--audit-anchor-interval %s is below the minimum %s", d, minAnchorInterval)
	}
	return nil
}

// minPricesRefreshInterval keeps a misconfigured interval (0, negative, minutes) from polling the public
// source in a tight loop; the 24h stability rule gains nothing from faster polling.
const minPricesRefreshInterval = time.Hour
