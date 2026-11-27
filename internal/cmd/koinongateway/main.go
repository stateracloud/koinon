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
	"crypto/x509"
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

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/formatgate"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/gateway/approvalroute"
	"github.com/stateracloud/koinon/internal/gateway/credkind"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/gateway/stateclient"
	"github.com/stateracloud/koinon/internal/gateway/usagewire"
	"github.com/stateracloud/koinon/internal/gatewaykeys"
	"github.com/stateracloud/koinon/internal/installid"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpgateway/pgaudit"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/memlimit"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/modelgateway"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

// mcpPath is where the MCP endpoint is served. It is the path agentgateway served it at, so an
// agent's URL does not change with the driver.
const mcpPath = "/mcp"

// usageFlushLimit bounds the last export at shutdown. The pod's grace period is the shutdown delay,
// the drain timeout and 30 seconds, and the flush runs after the drain, so it must fit in the 30.
const usageFlushLimit = 20 * time.Second

type config struct {
	listenAddr  string
	healthAddr  string
	metricsAddr string

	// modelPath also serves the model path (ADR 0097 step 2, issue 279): the route of each model format,
	// on the same listener. Off, the gateway serves the MCP path only.
	modelPath bool
	// mcpPath serves the MCP path. It is on unless a deployment runs the model path in pods of its own.
	mcpPath bool

	stateURL       string
	stateTokenFile string
	podName        string
	// databaseURLFile holds the connection string of the gateway's own database login (role
	// migrations.GatewayRole). Empty: no guard, so org targets are not served (fail closed).
	databaseURLFile string
	// recipientKeyDir is the mounted Secret with the wrapped gateway recipient keys (ADR 0097 decision 12).
	// Empty: the gateway loads no key. wrapping is the gateway's own wrapping key (never the KEK's).
	recipientKeyDir string
	wrapping        wrapping.Config

	// egressAddr serves the forge and registry routes (#1086, package egress) on a listener of their
	// own. Empty: no egress listener.
	egressAddr string
	// actorTokenVerifier selects how the egress listener checks the actor token: verifierEd25519 (the
	// OpenShell driver) or verifierSubstrate (ADR 0120, amendment for issue #1386, item 4).
	actorTokenVerifier string
	// actorTokenIssuer and actorTokenKeysDir check the Ed25519 actor token: the issuer the minter writes and
	// a directory of Ed25519 public keys, one <kid>.pub file each. The gateway holds no minting key.
	// exit: #1386 PR B, after #1094.
	actorTokenIssuer  string
	actorTokenKeysDir string
	// actorJWTIssuer and actorJWTCAFile check the Substrate actor JWT: the one issuer the gateway trusts and
	// the PEM bundle that verifies the issuer's TLS certificate.
	actorJWTIssuer string
	actorJWTCAFile string
	// egressRoutesFile holds the platform routes (no org, no credential). Empty: no platform route.
	egressRoutesFile string
	// egressCAFile is a PEM bundle of extra roots that verify the egress upstreams, added to the system
	// roots: a forge with a private CA (ADR 0062). Empty: the system roots only.
	egressCAFile string
	// namespace is Koinon's namespace, and egressAllowedHosts the internal hosts the platform allows:
	// the URL policy that the egress dialer applies to every upstream address.
	namespace          string
	egressAllowedHosts []string

	// restoreHold holds the gateway (ADR 0107, issue #1358): it is ready without a first State revision
	// (a held reconciler publishes none) and refuses every call on the MCP and model paths and every egress request.
	restoreHold bool

	drain time.Duration
	// shutdownDelay is how long the gateway keeps serving after SIGTERM, with /readyz at 503.
	shutdownDelay time.Duration
}

func (c *config) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.listenAddr, "listen-addr", ":8080", "address of the MCP listener, where agents connect with their gateway key (path "+mcpPath+")")
	fs.StringVar(&c.healthAddr, "health-addr", ":15021", "address of the health listener (/healthz, /readyz); empty disables it")
	fs.StringVar(&c.metricsAddr, "metrics-addr", ":15020", "address of the Prometheus listener (/metrics); empty disables it")
	fs.BoolVar(&c.mcpPath, "mcp-path", true, "serve the MCP path (path "+mcpPath+")")
	fs.BoolVar(&c.modelPath, "model-path", false,
		"also serve the model path: the chat completions and Messages routes, with the usage tap; usage records go to koinon-api over OTLP")
	fs.StringVar(&c.stateURL, "state-url", "",
		"base URL of koinon-api's internal listener that serves the gateway State stream (for example http://koinon-api.koinon.svc:4318)")
	fs.StringVar(&c.stateTokenFile, "state-token-file", "",
		"file with the gateway's bearer token for that listener (the OTLP token Secret, key token, mounted read-only); read on every request, so a rotation needs no restart")
	fs.StringVar(&c.databaseURLFile, "database-url-file", "",
		"file with the Postgres connection string of the gateway's own login (a member of the role "+migrations.GatewayRole+"). "+
			"With it the leak guard runs in the process and org MCP targets are served; without it the gateway serves platform targets only")
	fs.StringVar(&c.recipientKeyDir, "recipient-key-dir", "",
		"directory of the Secret with the gateway's wrapped recipient keys (one file per key ID). With it the gateway unwraps them, checks them against the gateway_recipient_keys rows and reports each loaded; "+
			"it is not ready while a row needs a key it cannot unwrap. The key-wrapping flags (--kek-backend and the --openbao-* or --awskms-* flags) name the gateway's own wrapping key")
	c.wrapping.Flags(fs)
	restorehold.Flag(fs, &c.restoreHold)
	fs.StringVar(&c.podName, "pod-name", "", "base of this process's copy id on the State stream (the pod name); a random boot nonce is appended")
	fs.StringVar(&c.egressAddr, "egress-addr", "",
		"address of the egress listener that serves the forge and registry routes at "+egress.PathPrefix+"<host>[:port]/<path>; empty disables it")
	fs.StringVar(&c.actorTokenVerifier, "actor-token-verifier", verifierEd25519,
		"how the egress routes check the actor token: "+verifierEd25519+" (the OpenShell driver: --actor-token-issuer and --actor-token-keys-dir) or "+
			verifierSubstrate+" (the Substrate driver: --actor-jwt-issuer and --actor-jwt-ca-file)")
	fs.StringVar(&c.actorTokenIssuer, "actor-token-issuer", "", "issuer of the Ed25519 actor identity tokens that the egress routes accept")
	fs.StringVar(&c.actorTokenKeysDir, "actor-token-keys-dir", "",
		"directory of the Ed25519 public keys that check actor tokens, one <key id>.pub PEM file each (a mounted ConfigMap or Secret)")
	fs.StringVar(&c.actorJWTIssuer, "actor-jwt-issuer", "",
		"the one issuer of Substrate actor JWTs that the egress routes trust (an https URL, for example https://idp.ate-system.svc); never taken from a token")
	fs.StringVar(&c.actorJWTCAFile, "actor-jwt-ca-file", "",
		"PEM file of the CAs that verify the TLS certificate of the Substrate actor JWT issuer; only these roots are used, and the check cannot be skipped")
	fs.StringVar(&c.egressRoutesFile, "egress-routes-file", "",
		"JSON file of the platform egress routes, which every org may use: no org and no credential. Empty: only the routes of Koinon state")
	fs.StringVar(&c.egressCAFile, "egress-ca-file", "",
		"PEM file of extra CA roots, added to the system roots, that verify the egress upstreams (a forge with a private CA). Empty: the system roots only")
	fs.StringVar(&c.namespace, "namespace", "", "Koinon's namespace: the egress routes never reach a Service in it")
	fs.StringSliceVar(&c.egressAllowedHosts, "egress-allowed-hosts", nil,
		"internal hosts (names, IPs or CIDRs, optionally with a port) that the egress routes may reach although they are internal")
	fs.DurationVar(&c.shutdownDelay, "shutdown-delay", 5*time.Second,
		"on SIGTERM /readyz turns 503 at once and the gateway keeps serving for this long, so the Service and clients stop sending requests to it; then it drains")
	fs.DurationVar(&c.drain, "drain-timeout", defaultDrain,
		"on SIGTERM the gateway refuses new requests, then lets calls in flight finish for up to this long; with the MCP path it must be at least "+mcpgateway.MinDrain.String()+" (the call timeout twice)")
}

// defaultDrain is the default --drain-timeout: the MCP call timeout and a margin.
const defaultDrain = mcpgateway.MinDrain + 30*time.Second

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
	if !c.mcpPath && !c.modelPath {
		return errors.New("--mcp-path=false needs --model-path: the gateway would serve nothing")
	}
	// On Agent Substrate the model and MCP listeners check the actor JWT too (issue #1489), so the
	// Substrate verifier needs its flags also in a pod without the egress listener.
	if c.egressAddr == "" && c.actorTokenVerifier == verifierSubstrate {
		if err := c.validateActorToken(); err != nil {
			return err
		}
	}
	if c.egressAddr != "" {
		if err := c.validateActorToken(); err != nil {
			return err
		}
		if c.namespace == "" {
			return errors.New("--egress-addr needs --namespace: the egress dialer refuses Koinon's own Services")
		}
		if c.egressAddr == c.listenAddr {
			return errors.New("--egress-addr must differ from --listen-addr")
		}
		if err := catalog.ValidateAllowedHosts(c.egressAllowedHosts); err != nil {
			return fmt.Errorf("--egress-allowed-hosts: %w", err)
		}
	}
	if c.shutdownDelay < 0 {
		return errors.New("--shutdown-delay must not be negative")
	}
	if c.drain <= 0 {
		return errors.New("--drain-timeout must be positive")
	}
	// A tool call is never retried. It may wait up to the call timeout for a person's approval and then run
	// upstream for up to the call timeout. A shorter drain cuts a call that is still running when a rollout
	// starts (issue 1347).
	if c.mcpPath && c.drain < mcpgateway.MinDrain {
		return fmt.Errorf("--drain-timeout (%s) must be at least %s (the MCP call timeout twice: approval wait and upstream call): a call in flight at SIGTERM would be cut", c.drain, mcpgateway.MinDrain)
	}
	return nil
}

// The values of --actor-token-verifier.
const (
	verifierEd25519   = "ed25519"
	verifierSubstrate = "substrate"
)

// validateActorToken checks the flags of the actor token check that the egress listener needs.
func (c *config) validateActorToken() error {
	switch c.actorTokenVerifier {
	case verifierEd25519:
		if c.actorTokenIssuer == "" || c.actorTokenKeysDir == "" {
			return errors.New("--egress-addr needs --actor-token-issuer and --actor-token-keys-dir: the egress routes check the actor token")
		}
		if c.actorJWTIssuer != "" || c.actorJWTCAFile != "" {
			return errors.New("--actor-jwt-issuer and --actor-jwt-ca-file need --actor-token-verifier=" + verifierSubstrate)
		}
	case verifierSubstrate:
		if c.actorJWTIssuer == "" || c.actorJWTCAFile == "" {
			return errors.New("--actor-token-verifier=" + verifierSubstrate + " needs --actor-jwt-issuer and --actor-jwt-ca-file: the gateway trusts one issuer and checks its TLS with that CA bundle")
		}
		if c.actorTokenIssuer != "" || c.actorTokenKeysDir != "" {
			return errors.New("--actor-token-issuer and --actor-token-keys-dir are for --actor-token-verifier=" + verifierEd25519 + ", not " + verifierSubstrate)
		}
	default:
		return fmt.Errorf("--actor-token-verifier must be %s or %s, not %q", verifierEd25519, verifierSubstrate, c.actorTokenVerifier)
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
	cmd.AddCommand(newKeysCommand())
	cli.EnvAlias(cmd, "pod-name", "POD_NAME")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
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
	// Every log line of the process goes through the scrub of the MCP credentials the gateway opens
	// (#1174): the audit line, the guard, the key loader and the State stream client too.
	credScrub := mcpgateway.NewCredentialScrub()
	log := slog.New(credScrub.Handler(slog.Default().Handler()))
	if _, err := readToken(cfg.stateTokenFile); err != nil {
		return err // fail at start, not on the first request
	}
	reg := metrics.NewRegistry()
	// The egress keys and routes load before any listener starts: a bad key directory or route file
	// stops the start, not a gateway that is half up.
	sub, err := newSubstrateIdentity(ctx, cfg)
	if err != nil {
		return err
	}
	eg, egState, err := newEgress(ctx, cfg, sub, reg, log, credScrub.Record)
	if err != nil {
		return err
	}
	gopts := mcpgateway.Options{
		Metrics: mcpgateway.NewMetrics(reg),
		Tracer:  otel.Tracer("koinon-gateway"),
		Audit:   logAudit{log: log},
		Logger:  log,
		Scrub:   credScrub,
	}
	if sub != nil && cfg.mcpPath {
		if gopts.Actors, err = sub.identifier(actortoken.AudienceMCP); err != nil {
			return err
		}
	}
	// The guard serves the MCP path only: a pod that runs the model path alone opens no database login.
	var keys *gatewaykeys.Loader
	// guardLoad is the guard's database check; nil when the pod runs no guard. /readyz asks it.
	var guardLoad loadChecker
	if cfg.mcpPath && cfg.databaseURLFile != "" {
		// A call the guard holds for a person is parked through koinon-api (#1301): koinon-api records the
		// approval, and the gateway's database login has no right on approvals.
		approvals, err := approvalroute.NewClient(approvalroute.ClientConfig{
			URL: cfg.stateURL, Token: func() (string, error) { return readToken(cfg.stateTokenFile) },
		})
		if err != nil {
			return err
		}
		guard, audit, pool, err := openGuard(ctx, cfg.databaseURLFile, approvals.Approve, log, reg)
		if err != nil {
			return err
		}
		defer pool.Close()
		guardLoad = guard
		gopts.Guard, gopts.Audit = guard, multiAudit{logAudit{log: log}, audit}
		// The pod reports the formats it reads, so koinon-api seals a credential to the gateway key only when
		// every gateway pod opens it (ADR 0097 decision 12.5 step 1). A pod without a key reads none.
		rep := formatgate.NewReporter(pool, formatgate.Gateway)
		if cfg.recipientKeyDir == "" {
			rep.Reads = nil
		}
		rep.Log = func(msg string, err error) { log.Warn(msg, "error", err) }
		go rep.Run(ctx)
		if cfg.recipientKeyDir != "" {
			if keys, err = startKeyLoader(ctx, cfg, pool, reg, log); err != nil {
				return err
			}
			// Sealed MCP credentials (mcpv2- files) open with these keys; a refused one is counted.
			gopts.Credentials = mcpgateway.StaticCredentials{Keys: keys.Keyring, OnOpenFailure: keys.CountFailure}
			// The forge credentials of the egress routes (forgev2- files, #1086 part C) open with the same
			// keys. A pod without them refuses every credentialed route (credential_unavailable).
			if eg != nil {
				eg.SetCredentials(egress.SealedCredentials{Keys: keys.Keyring, OnOpenFailure: keys.CountFailure})
			}
		}
	}
	gw := mcpgateway.New(gopts)
	var (
		mg        *modelgateway.Gateway
		exp       *usagewire.Exporter
		modelHalf gateway.ModelPathGateway
	)
	if cfg.modelPath {
		exp = usagewire.NewExporter(usagewire.ExporterOptions{
			URL: cfg.stateURL, Token: func() (string, error) { return readToken(cfg.stateTokenFile) }, Logger: log,
		})
		mopts := modelgateway.Options{
			Sink: exp.Record, Metrics: modelgateway.NewMetrics(reg), Tracer: credScrub.Tracer(otel.Tracer("koinon-gateway")),
			Logger: log,
			// Each provider key the model path reads goes into the scrub of every log line and span (#1221).
			Scrub: credScrub.Record,
		}
		if sub != nil {
			if mopts.Actors, err = sub.identifier(actortoken.AudienceModel); err != nil {
				return err
			}
		}
		mg = modelgateway.New(mopts)
		modelHalf = mg
		// A dropped record is a lost charge: both numbers are visible to an operator (ADR 0023: no labels).
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "koinon_gateway_usage_records_dropped_total",
			Help: "Usage records the exporter dropped (queue full or refused by the receiver).",
		}, func() float64 { return float64(exp.Dropped()) }))
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "koinon_gateway_usage_records_queued",
			Help: "Usage records that wait for export.",
		}, func() float64 { return float64(exp.Queued()) }))
	}
	var mcpHalf gateway.MCPGateway
	if cfg.mcpPath {
		mcpHalf = gw
	}
	copyID, err := stateclient.NewCopyID(cfg.podName)
	if err != nil {
		return err
	}
	scfg := stateclient.Config{
		URL: cfg.stateURL, Token: func() (string, error) { return readToken(cfg.stateTokenFile) },
		CopyID: copyID, MCP: mcpHalf, Model: modelHalf, Metrics: stateclient.NewMetrics(reg), Log: log,
	}
	switch {
	case egState != nil:
		scfg.Egress = egState
	case sub != nil:
		// No egress listener: each revision still fills the actor table of the model and MCP listeners.
		scfg.Egress = egress.ActorState{Actors: sub.actors}
	}
	sc, err := stateclient.New(scfg)
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
	var keyReady keyReadier
	if keys != nil {
		keyReady = keys
	}
	if err := serveHealth(healthCtx, ctx, cfg.healthAddr, func() bool {
		// A held install has no revision to wait for: the held reconciler publishes none (ADR 0107).
		held := cfg.restoreHold || ((!cfg.mcpPath || gw.Revision() != "") && (mg == nil || mg.Revision() != ""))
		return podReady(guardLoad, keyReady, held)
	}, func() bool { return sc != nil && sc.Stale() != nil }, log); err != nil {
		return err
	}

	mux := http.NewServeMux()
	if cfg.mcpPath {
		mux.Handle(mcpPath, heldRefuse(cfg.restoreHold, gw.Handler()))
	}
	if mg != nil {
		for _, p := range gateway.RoutePaths() {
			mux.Handle(p, heldRefuse(cfg.restoreHold, mg.Handler()))
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("MCP listener: %w", err)
	}
	// net/http writes handler panics and connection errors to ErrorLog; route them through the scrub.
	srv := &http.Server{
		Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	served := make(chan error, 2)
	safego.Go(ctx, log, "mcp-listener", func(context.Context) { served <- srv.Serve(ln) })
	egSrv, err := serveEgress(ctx, cfg, eg, log, served)
	if err != nil {
		_ = srv.Close()
		return err
	}
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
	// The exporter outlives the listener: a call that finishes during the drain still writes its record.
	expCtx, stopExport := context.WithCancel(context.WithoutCancel(ctx))
	defer stopExport()
	if exp != nil {
		safego.Go(expCtx, log, "usage-export", func(ctx context.Context) { exp.Run(ctx, min(cfg.drain, usageFlushLimit)) })
	}
	safego.Go(ctx, log, "state-stream", func(ctx context.Context) { _ = sc.Run(ctx) })
	log.Info("starting koinon-gateway", "version", project.Version, "listen", ln.Addr().String(), "copy", copyID)

	select {
	case err := <-served:
		if egSrv != nil {
			_ = egSrv.Close()
		}
		_ = srv.Close()
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listener: %w", err)
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
	if eg != nil {
		eg.Drain()
	}
	if mg != nil {
		mg.Drain()
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.drain)
	defer cancel()
	egDone := make(chan struct{})
	if egSrv == nil {
		close(egDone)
	} else {
		// The egress listener drains with the MCP listener, under the same deadline.
		safego.Go(sctx, log, "egress-drain", func(ctx context.Context) {
			defer close(egDone)
			if err := egSrv.Shutdown(ctx); err != nil {
				_ = egSrv.Close()
				log.Warn("drain deadline passed: open egress requests were closed", "err", err)
			}
		})
	}
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		log.Warn("drain deadline passed: open requests were closed", "err", err)
	}
	<-egDone
	if exp != nil {
		// Run flushes what is queued once its context ends; wait for it so the last records reach the ledger.
		stopExport()
		<-exp.Done()
	}
	stopHealth()
	log.Info("stopped koinon-gateway")
	return nil
}

// newEgress builds the gateway of the forge and registry routes when cfg names an egress listener, and
// returns nil when it does not. It loads the actor token keys and the platform routes, and returns
// the applier that merges the org routes of each State revision with them.
func newEgress(ctx context.Context, cfg config, sub *substrateIdentity, reg prometheus.Registerer, log *slog.Logger, scrub func(kind credkind.Kind, org string, values ...string)) (*egress.Gateway, *egress.StateRoutes, error) {
	if cfg.egressAddr == "" {
		return nil, nil, nil
	}
	ident, actors, keys, err := newIdentifier(cfg, sub)
	if err != nil {
		return nil, nil, err
	}
	// GatewayDialContext resolves each route host once, as an absolute name, and refuses Koinon's own
	// namespace by name and internal addresses by address.
	roots, err := egressRoots(cfg.egressCAFile)
	if err != nil {
		return nil, nil, err
	}
	policy := catalog.URLPolicy{Namespace: cfg.namespace, AllowedHosts: cfg.egressAllowedHosts}
	eg, err := egress.New(egress.Options{
		Identifier: ident,
		RootCAs:    roots,
		Dial:       policy.GatewayDialContext(&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}),
		Metrics:    egress.NewMetrics(reg),
		Log:        log,
		// Each credential the gateway attaches is masked in its log lines too.
		Scrub: scrub,
		// A held gateway is deny-all on the egress listener too (ADR 0107, issue #1390).
		Held: cfg.restoreHold,
	})
	if err != nil {
		return nil, nil, err
	}
	// The platform file is the table until the first State revision; then each revision merges its org
	// routes with it (egress.StateRoutes). Without a file and before a revision, every request is
	// refused (no_table).
	st := &egress.StateRoutes{Gateway: eg, Actors: actors}
	if cfg.egressRoutesFile != "" {
		t, err := egress.LoadPlatformRoutes(cfg.egressRoutesFile)
		if err != nil {
			return nil, nil, err
		}
		st.Platform = t
		eg.Apply(t)
	}
	// keys is the count of Ed25519 public keys; the Substrate verifier reads its keys from the issuer
	// on the first token, so it has none at start.
	log.Info("egress routes loaded", "actor_token_verifier", cfg.actorTokenVerifier, "keys", keys, "platform_routes", cfg.egressRoutesFile != "")
	return eg, st, nil
}

// newIdentifier returns the egress listener's actor token check that cfg selects. For the Substrate
// verifier it is sub's identifier of the egress audience, with sub's actor table; the table is nil for
// the Ed25519 verifier. keys is the count of Ed25519 public keys it loaded (0 for the Substrate
// verifier).
func newIdentifier(cfg config, sub *substrateIdentity) (_ route.Identifier, _ *route.ActorTable, keys int, _ error) {
	switch cfg.actorTokenVerifier {
	case verifierEd25519:
		// exit: #1386 PR B, after #1094 (the OpenShell driver and the Ed25519 path go away).
		pub, err := actortoken.LoadPublicKeys(cfg.actorTokenKeysDir)
		if err != nil {
			return nil, nil, 0, err
		}
		ver, err := actortoken.NewVerifier(cfg.actorTokenIssuer, pub)
		if err != nil {
			return nil, nil, 0, err
		}
		return ver, nil, len(pub), nil
	case verifierSubstrate:
		if sub == nil {
			return nil, nil, 0, errors.New("the Substrate actor token check is not set up")
		}
		id, err := sub.identifier(actortoken.AudienceEgress)
		if err != nil {
			return nil, nil, 0, err
		}
		return id, sub.actors, 0, nil
	}
	return nil, nil, 0, fmt.Errorf("--actor-token-verifier must be %s or %s, not %q", verifierEd25519, verifierSubstrate, cfg.actorTokenVerifier)
}

// substrateIdentity is the Substrate actor JWT check of the process (issue #1489): one verifier, whose
// issuer key cache every listener shares, and one actor table, which each State revision fills. Each
// listener gets its own audience (identifier).
type substrateIdentity struct {
	verifier *actortoken.SubstrateVerifier
	actors   *route.ActorTable
}

// newSubstrateIdentity returns the Substrate check, or nil when cfg selects another verifier. It does not
// contact the issuer: the verifier reads the issuer's keys on the first token, and ctx bounds those reads.
//
// The key policy (ADR 0120 amendment of 2026-11-27): the verifier refetches the issuer's keys at most once
// a minute (actortoken.KeyRefetchInterval) and uses them for at most actortoken.KeyMaxAge (10 minutes).
// When the issuer is down for longer, every actor JWT is refused with 503 on every listener until it
// is back. That is fail closed by design: a key the issuer has withdrawn must not stay valid.
func newSubstrateIdentity(ctx context.Context, cfg config) (*substrateIdentity, error) {
	if cfg.actorTokenVerifier != verifierSubstrate {
		return nil, nil
	}
	ver, err := actortoken.NewSubstrateVerifier(ctx, actortoken.SubstrateConfig{
		Issuer: cfg.actorJWTIssuer, CAFile: cfg.actorJWTCAFile, Audience: actortoken.AudienceEgress,
	})
	if err != nil {
		return nil, err
	}
	return &substrateIdentity{verifier: ver, actors: &route.ActorTable{}}, nil
}

// identifier is the check of the listener of audience.
func (s *substrateIdentity) identifier(audience string) (*route.SubstrateIdentifier, error) {
	v, err := s.verifier.WithAudience(audience)
	if err != nil {
		return nil, err
	}
	return route.NewSubstrateIdentifier(v, s.actors)
}

// egressRoots is the system roots plus the certificates of the PEM file at path, or nil (the system
// roots) when path is empty. A file without a certificate stops the start: the operator named it for
// a reason.
func egressRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path) //nolint:gosec // an operator's flag
	if err != nil {
		return nil, fmt.Errorf("--egress-ca-file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--egress-ca-file: %s holds no PEM certificate", path)
	}
	return pool, nil
}

// serveEgress starts the egress listener of eg, and returns nil when eg is nil. The listener's end is
// sent to served.
func serveEgress(ctx context.Context, cfg config, eg *egress.Gateway, log *slog.Logger, served chan<- error) (*http.Server, error) {
	if eg == nil {
		return nil, nil
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.egressAddr)
	if err != nil {
		return nil, fmt.Errorf("egress listener: %w", err)
	}
	// The handler is served as it is, never through a ServeMux: a mux cleans the path, and the gate
	// must see the path the guest sent so it can refuse one that is not canonical.
	srv := &http.Server{
		Handler: eg.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	safego.Go(ctx, log, "egress-listener", func(context.Context) { served <- srv.Serve(ln) })
	log.Info("egress listener started", "listen", ln.Addr().String())
	return srv, nil
}

// loadChecker is the guard's view of its database login (mcpguard.GatewayGuard.Loaded).
type loadChecker interface{ Loaded() error }

// keyReadier is the key loader's readiness (gatewaykeys.Loader.Ready).
type keyReadier interface{ Ready() (bool, error) }

// podReady is the /readyz answer of the pod. A pod is ready when it applied a config (configured), its key
// loader holds the keys it needs, and the leak guard reached its database. The guard opens a lazy pool
// and retries every 2 s: a broken login would leave every org target closed behind a ready pod, and a
// rollout would not stop. A nil guard or loader means the pod runs without one. Each later outage is not
// checked here: the guard refuses on its own then (fail closed), and a database blip must not
// remove every endpoint.
func podReady(guard loadChecker, keys keyReadier, configured bool) bool {
	if guard != nil && guard.Loaded() != nil {
		return false
	}
	if keys != nil {
		if ok, _ := keys.Ready(); !ok {
			return false
		}
	}
	return configured
}

// heldRefuse returns next, or, while the install is held (ADR 0107), a handler that refuses every
// request: a held gateway is ready but deny-all, whatever config it holds.
func heldRefuse(held bool, next http.Handler) http.Handler {
	if !held {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, restorehold.Message, http.StatusServiceUnavailable)
	})
}

// readyzHandler answers /readyz. A replica with no config would refuse every key: it is not ready until the
// first revision applied (configured). Once one has, a later State stream outage keeps the config in force,
// so that answer latches. degraded does not latch: it is asked on every probe, and a pod that fails to
// apply a newer revision (stateclient.Client.Stale) leaves the Service although it was ready before. Without
// that, a copy stuck on an old revision would go on to accept keys that a later revision revoked (#1347).
func readyzHandler(term context.Context, configured, degraded func() bool) http.HandlerFunc {
	var applied atomic.Bool
	return func(w http.ResponseWriter, _ *http.Request) {
		if !applied.Load() && configured() {
			applied.Store(true)
		}
		if term.Err() != nil || !applied.Load() || degraded() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// serveHealth serves /healthz (the process runs) and /readyz (the gateway applied a config, is not
// degraded and is not draining) on addr. An empty addr serves nothing.
func serveHealth(life, term context.Context, addr string, configured, degraded func() bool, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", readyzHandler(term, configured, degraded))
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
// writer. approve records the approval of a call that needs a person (koinon-api's approval route).
// The guard's route stays closed (every message and call is refused) until the database has answered once; a later outage refuses on its own, because a decision that cannot be recorded is a
// refusal (ADR 0120 section 2).
func openGuard(ctx context.Context, urlFile string, approve mcpguard.ApproveFunc, log *slog.Logger, reg prometheus.Registerer) (*mcpguard.GatewayGuard, mcpgateway.Audit, *pgxpool.Pool, error) {
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
		// A call that needs a person's decision is parked: koinon-api records the ADR 0042 approval of
		// the exact call. If it cannot, the call is refused (fail closed).
		Approve: approve,
	})
	ga := mcpguard.NewGatewayGuard(g, log)
	safego.Go(ctx, log, "guard-load", func(ctx context.Context) {
		for ctx.Err() == nil {
			err := ga.Load(ctx)
			if err == nil {
				log.Info("the leak guard reached its database")
				return
			}
			log.Warn("the leak guard cannot reach its database; org targets stay closed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	})
	return ga, pgaudit.New(pool), pool, nil
}

// startKeyLoader builds the gateway's key loader on the guard's database login and runs it. The wrapping
// key is the gateway's own; the gateway's identity may decrypt with it and may not encrypt (ADR 0097 12.3).
// The first pass runs in the background: /readyz stays 503 until it passed once.
func startKeyLoader(ctx context.Context, cfg config, pool *pgxpool.Pool, reg prometheus.Registerer, log *slog.Logger) (*gatewaykeys.Loader, error) {
	ws, err := wrapping.New(ctx, cfg.wrapping, wrapping.Service, installid.BoundOnlySource(pool))
	if err != nil {
		return nil, fmt.Errorf("the gateway's key-wrapping key: %w", err)
	}
	copyID, err := stateclient.NewCopyID(cfg.podName)
	if err != nil {
		return nil, err
	}
	l := gatewaykeys.NewLoader(cfg.recipientKeyDir, pool, ws.WithInstallID(installid.Source(pool)), copyID, gatewaykeys.NewMetrics(reg))
	l.Logf = func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }
	safego.Go(ctx, log, "recipient-keys", l.Run)
	return l, nil
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
