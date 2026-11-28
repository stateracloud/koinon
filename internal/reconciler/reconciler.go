// SPDX-License-Identifier: Apache-2.0

// Package reconciler drives each agent toward its desired execution state (plan item R2):
//
//   - an active agent gets its org's OpenShell workspace (one per org, ADR 0002), an
//     agentgateway key issued through the ModelGateway (material in a Kubernetes Secret in
//     Koinon's namespace, only the hash in gateway config and Postgres), a provider record
//     holding that key behind a stable placeholder, and a sandbox on its harness image with an
//     enforced policy (ADR 0045) and a persistent home;
//   - a sandbox stops after IdleTimeout without an active run (parked runs included) and starts
//     again when a run is queued or a park is resolved (ADR 0049);
//   - archiving deletes the sandbox, the provider record and the key; the home volume follows
//     HomeRetention;
//   - every pass re-applies everything, so hand edits are reverted, and every rendered object
//     carries owner labels.
//
// The observed sandbox state is written to agent_sandboxes, which koinon-agentd reads before it
// execs into a sandbox.
//
// # Loop
//
// The reconciler is level-triggered: each pass reads the whole desired state from Postgres
// (ADR 0003) and converges every object. Passes run on a timer (ResyncInterval) and on
// Postgres NOTIFY wake-ups from triggers on agents and runs, so a queued run starts its
// sandbox within a second rather than a resync interval. NOTIFY is only a hint (it is lost
// while no listener is connected); the timer covers missed hints, idle timeouts and drift. A
// work queue table was not used: a full pass is cheap at v0.1's scale, the gateway config is
// one document rendered from all agents anyway, and a queue would add claim and retry state
// without removing the need for a periodic drift pass. One replica reconciles at a time,
// holding a Postgres advisory lock on its listening connection.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
	"github.com/stateracloud/koinon/internal/reconciler/leaderterm"
	"github.com/stateracloud/koinon/internal/remoteagents"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/sandboxres"
)

// Retention decides what happens to an archived agent's home volume.
type Retention string

// Home retention values.
const (
	// RetentionDelete deletes the sandbox and with it the home volume (the default).
	RetentionDelete Retention = "delete"
	// RetentionRetain keeps the stopped sandbox, and so its home volume, after archive. Its
	// provider record and key are deleted; it never starts again. OpenShell 0.1.5 deletes the
	// workspace PVC together with its sandbox and offers no way to detach it, so keeping the
	// stopped sandbox is the only way to keep the volume through the gateway API.
	RetentionRetain Retention = "retain"
)

// Defaults.
const (
	DefaultIdleTimeout = 10 * time.Minute
	// DefaultKeepWarmPerOrg is the default cap of keep-warm sandboxes per org: none. A warm agent is
	// a running actor that holds its full CPU and memory limits in the worker pool, and a suspended
	// agent resumes in about a second (measured, #1090).
	DefaultKeepWarmPerOrg = 0
	DefaultResyncInterval = 30 * time.Second
	// DefaultLeaderHeartbeat is how long the leader's lock connection may be silent before the
	// leader checks it with a query (issue 635).
	DefaultLeaderHeartbeat = 4 * time.Second
	// DefaultLeaderPingTimeout bounds that check. A leader that cannot confirm its lock connection
	// within heartbeat + ping timeout (8 s by default) stops its work.
	DefaultLeaderPingTimeout = 4 * time.Second
	// DefaultMCPProbeInterval is how often each org MCP server is probed (plan item MC1).
	DefaultMCPProbeInterval = 60 * time.Second
)

// Endpoint is a host and port.
type Endpoint struct {
	Host string
	Port uint32
}

// Config configures a Reconciler.
type Config struct {
	// StateRevisionDB, when set, replaces the pool for storing State stream revisions. For tests of
	// a slow or failing store.
	StateRevisionDB staterev.DB
	// MCPAckWait is how long the first probe of a new or changed MCP server waits for every live
	// koinon-gateway copy to ack the State revision (#1260); zero means DefaultMCPAckWait. MCPAckPoll
	// is how often it reads the acks; zero means DefaultMCPAckPoll.
	MCPAckWait time.Duration
	MCPAckPoll time.Duration

	// Gateway is agentgateway's LLM listener as sandboxes reach it.
	Gateway Endpoint
	// Egress is koinon-gateway's egress listener as sandboxes reach it (#1086 part C2). With it, the
	// sandbox of an agent with a code-host grant gets a rule for the forge routes there (EgressRule).
	// Nil: no such rule, and git reaches the forge only through the driver's own rule.
	Egress *Endpoint
	// MCPGateway is koinon-gateway's MCP listener as sandboxes reach it, when koinon-gateway serves the
	// agents' MCP path (gateway.mcp.driver=koinon, ADR 0097 decision 5). The sandbox profile then allows
	// /mcp on it and not on Gateway, and the MCP prober lists tools through it. Zero: agentgateway serves /mcp
	// on Gateway.
	MCPGateway Endpoint
	// Keys opens the provider keys stored in Postgres (the secret store).
	Keys catalog.Opener
	// ProviderKeys receives the opened keys for the gateway. Required when any served
	// provider has a key.
	ProviderKeys ProviderKeyStore
	// ProviderKeyDir is where the gateway mounts ProviderKeys, for the rendered config.
	ProviderKeyDir string
	// MCPKeys receives the credential files and CA bundles of MCP servers (names mcp-..., mcpca-...), apart
	// from the provider keys, so koinon-gateway mounts no provider key (ADR 0097 decision 5). Nil: they go to
	// ProviderKeys, as in kubernetes mode.
	MCPKeys ProviderKeyStore
	// MCPKeyDir is where the gateway pods mount MCPKeys, for the rendered config. Empty: ProviderKeyDir.
	MCPKeyDir string
	// MCPSealed is true when koinon-gateway serves every MCP path (gateway.mcp.driver=koinon). Only then does the
	// reconciler serve a sealed (v2) credential: agentgateway would send the envelope upstream as the key.
	MCPSealed bool
	// ForgeRecipient is the gateway key the reconciler seals an agent's code-host token to, for the
	// agent's forge routes at koinon-gateway (#1086 part C, ADR 0097 decision 12). Nil delivers no forge
	// credential: those routes fail closed (503 credential_unavailable).
	ForgeRecipient RecipientKeys
	// Prices finds the price of each served model; required (prices.NewResolver).
	Prices prices.Resolver
	// AgentLimit computes an agent's budget; nil is ledger.AgentLimit. It is a seam for tests.
	AgentLimit func(ctx context.Context, q *store.Queries, agent store.Agent, now time.Time) (*gateway.Limit, error)
	// URLPolicy is checked again, with a fresh lookup, for every provider URL before it is served.
	URLPolicy catalog.URLPolicy
	// ProviderKeySettle is how long a pass waits, after writing a new provider key file, before it
	// applies a config that names the file: kubelet needs about a minute to update a mounted Secret
	// and agentgateway rejects a config whose key file is missing. Zero applies at once.
	ProviderKeySettle time.Duration
	// ToolServers maps a connector type ID to the URL of its MCP tool server
	// (for github: koinon-api's tools listener, plan item C3). Each becomes an
	// agentgateway MCP target named after the connector, and every active
	// agent is allowed the tools its approved grants expose. Empty serves no
	// tools.
	ToolServers map[string]string
	// MCPAskURL is the base URL, as agentgateway reaches it, of koinon-api's approval-gated MCP tool
	// proxy (internal/mcpask); an org server "wiki" has its ask-tier tools at <MCPAskURL>/wiki. Empty
	// offers no ask-tier tools of org MCP servers.
	MCPAskURL string
	// MCPProbeInterval is the time between health probes of each org MCP server. Default
	// DefaultMCPProbeInterval.
	MCPProbeInterval time.Duration
	// GitHub mints the installation tokens of repo.read grants (plan item C2). Nil leaves
	// agents with such a grant unconverged, with an error.
	GitHub GitHubTokens
	// GitBinaries may use the GitHub endpoints of a read grant, besides the harness's binaries.
	// Default DefaultGitBinaries.
	GitBinaries []string
	// Harnesses maps each harness to its image and binaries. Agents of a harness without an image
	// are reported as errors.
	Harnesses map[string]Harness
	// RuntimeClassName is the sandbox runtime class (ADR 0045: kata). Empty uses the
	// gateway's default.
	RuntimeClassName string
	// SandboxResources is the platform's sandbox CPU and memory sizing: the default, the bounds
	// and the Kata memory rule (ADR 0083). Nil is sandboxres.DefaultPolicy.
	SandboxResources *sandboxres.Policy
	// IdleTimeout stops a sandbox after this long without activity (see package wake), for an org
	// that has not set idle_timeout_seconds. Default 10 minutes.
	IdleTimeout time.Duration
	// ParkWarm keeps a sandbox up this long after its run parked on a2a or input_required, for an
	// org that has not set park_warm_seconds. Default 15 minutes (ADR 0066).
	ParkWarm time.Duration
	// SandboxJobs bounds the per-sandbox jobs running at once. Default 8.
	SandboxJobs int
	// KeepWarmPerOrg caps the sandboxes an org's admins can keep warm (agents.keep_warm, old issue 315).
	// Default 0, which grants none, as does a negative value. Over the cap the oldest agents win.
	KeepWarmPerOrg int
	// ResyncInterval is the time between passes without a wake-up. Default 30 seconds.
	ResyncInterval time.Duration
	// LeaderHeartbeat and LeaderPingTimeout bound the time a leader works after its lock connection
	// died without a TCP reset (hard node loss, partition): LeaderHeartbeat + LeaderPingTimeout.
	// Defaults 4 s and 4 s. TCP keepalive alone would take about 2.5 minutes, and a TCP proxy or
	// pooler in between answers keepalive probes for a dead peer.
	LeaderHeartbeat   time.Duration
	LeaderPingTimeout time.Duration
	// Checkpoints turns on home checkpoints (checkpoint.go): a sandbox is stopped, recreated or
	// archived only after its home was backed up, and a recreated sandbox keeps its home
	// generation and gets the home restored. koinon-agentd writes the checkpoints.
	Checkpoints bool
	// CheckpointsReadOnly is the restore-only mode (checkpoints.readOnly, ADR 0110 decision 4):
	// koinon-agentd writes no backup, so nothing waits for one. A recreation uses the newest complete
	// snapshot when it was taken since the home was last written, and otherwise is blocked with the
	// reason at once; an idle stop does not wait (it keeps the volume).
	CheckpointsReadOnly bool
	// CheckpointStopWait is how long an idle stop waits for its checkpoint. Default
	// DefaultCheckpointStopWait.
	CheckpointStopWait time.Duration
	// ArchiveAfter is how long a sandbox stays stopped before it and its volume are deleted, once
	// its home's latest checkpoint is complete (plan item AR1, ADR 0032). Zero means
	// DefaultArchiveAfter; negative turns the archive tier off. Needs Checkpoints.
	ArchiveAfter time.Duration
	// HomeRetention decides the home volume's fate on archive. Default RetentionDelete.
	HomeRetention Retention
	// Observer explains sandboxes that never become ready by reading their pods (ADR 0078). Nil turns
	// it off (sandboxObserver.enabled=false): the recorded reason is then OpenShell's.
	Observer *SandboxObserver
	// NetworkPolicy applies the chart's default-deny NetworkPolicy in each workspace namespace
	// (spike 8.15, ADR 0045). Nil turns it off (sandboxNetworkPolicy.enabled=false).
	NetworkPolicy *SandboxNetworkPolicy
	// LimitRange applies the chart's LimitRange in each workspace namespace (#76, ADR 0083 follow-up).
	// Nil turns it off (sandboxLimitRange.enabled=false).
	LimitRange *SandboxLimitRange
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Metrics receives the pass metrics; nil keeps them unexported.
	Metrics *Metrics
	// Supervisor says which supervisor image the OpenShell gateway starts sandboxes with. It is part
	// of the sandbox spec hash, so an OpenShell upgrade recreates each sandbox through the usual path
	// (ADR 0085 decision 9). Nil leaves it out of the hash.
	Supervisor SupervisorSource
	// GatewayID is openshell.gatewayId, the ID the sandbox admission policies select namespaces by, and
	// GatewayIDs says which gateway_id the OpenShell gateway runs with. Every pass compares them and
	// sets koinon_reconciler_openshell_gateway_id_mismatch (old issue 475). Either nil/empty skips it.
	GatewayID  string
	GatewayIDs GatewayIDSource
	// MaxRecreatesPerPass caps the sandboxes deleted to recreate them with a changed spec (a new
	// supervisor, image or resources); the others wait for a later pass. It is a token bucket: this
	// many at once, refilled at this many per ResyncInterval (the rate of one pass per interval).
	// Zero is DefaultMaxRecreatesPerPass; negative is no cap.
	MaxRecreatesPerPass int
	// WorkTimeout bounds one agent's sandbox work item (the creating, starting, stopping,
	// recreating and recording of one sandbox). A stuck item ends after it and holds only its own
	// agent until then. Zero is DefaultWorkTimeout.
	WorkTimeout time.Duration
	// Now defaults to time.Now; tests move it forward to pass the idle timeout.
	Now func() time.Time
}

// Reconciler reconciles agents into OpenShell sandboxes.
type Reconciler struct {
	cfg  Config
	log  *slog.Logger
	pool *pgxpool.Pool
	// db is the pool behind the leader term: the statements of a lead carry its term, and the database
	// refuses them once a newer leader took the term (issue 635). q runs on it.
	db      leaderterm.DB
	q       *store.Queries
	backend execution.Backend
	// crash holds the counters of the crash and liveness path (crash.go).
	crash   crashTracker
	gw      gateway.ModelGateway
	secrets SecretStore
	// states stores the revisions of the gateway State (the State stream, ADR 0097).
	states staterev.Publisher

	mu sync.Mutex
	// wsMu guards the per-pass workspace map, which per-sandbox jobs read concurrently.
	wsMu sync.Mutex
	// githubRevs is the token revision last pushed per GitHub provider record, to log refreshes.
	githubRevs map[string]string
	// forgeSeals is the sealed code-host token of each agent (under mu), so an unchanged token keeps its
	// file name between passes.
	forgeSeals map[agentKey]forgeSeal
	// draftGitHubDropped holds the GitHub providers dropDraftGitHub has dropped (under mu), so a draft agent
	// does not cost four gateway calls every pass; reconcileActive clears the entry when it makes the
	// record again.
	draftGitHubDropped map[string]bool
	// clampMu guards clampLogged: the clamps last logged per agent, so a pass that changes nothing
	// logs nothing.
	clampMu     sync.Mutex
	clampLogged map[string]string
	// plan is the gateway side of a pass and its memory (internal/reconciler/gatewayplan).
	plan *gatewayplan.Planner
	mcp  *mcpservers.Service
	// remoteAgents reads the cards of remote agents for the health check; it never seals a credential.
	remoteAgents *remoteagents.Service
	m            *Metrics

	// supMu guards the supervisor image read from the gateway (supervisor.go).
	supMu    sync.Mutex
	supImage string
	supKnown bool
	supErr   error
	// gwMu guards gwMismatch, the last result of the gateway ID check (gatewayid.go).
	gwMu       sync.Mutex
	gwMismatch bool
	// bucketMu guards the recreate token bucket (takeRecreate).
	bucketMu     sync.Mutex
	bucketTokens float64
	bucketAt     time.Time
	recreating   map[agentKey]bool // agents holding a token for a recreate not yet started

	// probeMu guards probeDue: the MCP servers a pass newly applied, waiting for the prober (issue
	// old issue 226). probeWake wakes the prober to take them.
	probeMu sync.Mutex
	// keyRevs maps each served key ID to the first State revision that held it, and pendingKeys holds
	// the keys issued since the start that no stored revision holds yet (keyGate).
	keyRevMu    sync.Mutex
	keyRevs     map[string]int64
	pendingKeys map[string]bool
	// forgeRevs maps each forge credential name in the stored State to the first revision that held it
	// (noteForge), under keyRevMu. forgeRows is what the database holds for each agent (recordForge,
	// pass goroutine only).
	forgeRevs map[string]int64
	forgeRows map[agentKey]forgeRow
	// actorRevs maps each Substrate actor in the stored State to the first revision that held it
	// (noteActors, issue #1386), under keyRevMu.
	actorRevs map[gatewayplan.Actor]int64
	probeDue  []dueProbe
	probeWake chan struct{}
	// probeParked holds, under probeMu, the servers whose probe was held because no stored revision
	// held the probe key (the publish failed). The next pass that stores a revision queues them again
	// (#1260), so they do not wait for the next probe interval.
	probeParked map[pgtype.UUID]bool
	// probeAsked holds, per server, the last time a gateway wake (#1173) queued its probe, and
	// probeHeld the servers with a wake held back until mcpProbeWakeEvery has passed. Both under probeMu.
	probeAsked map[pgtype.UUID]time.Time
	probeHeld  map[pgtype.UUID]bool
}

// New returns a Reconciler.
func New(pool *pgxpool.Pool, backend execution.Backend, gw gateway.ModelGateway, secrets SecretStore, cfg Config) (*Reconciler, error) {
	if cfg.Gateway.Host == "" || cfg.Gateway.Port == 0 {
		return nil, errors.New("reconciler: the agentgateway endpoint is required")
	}
	if (cfg.MCPGateway.Host == "") != (cfg.MCPGateway.Port == 0) {
		return nil, errors.New("reconciler: the MCP gateway endpoint needs both a host and a port")
	}
	if cfg.Prices.IsZero() {
		return nil, errors.New("reconciler: Prices is required (prices.NewResolver)")
	}
	if cfg.ArchiveAfter == 0 {
		cfg.ArchiveAfter = DefaultArchiveAfter
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.ParkWarm == 0 {
		cfg.ParkWarm = orgsettings.DefaultParkWarm
	}
	if cfg.SandboxJobs <= 0 {
		cfg.SandboxJobs = 8
	}
	if cfg.MaxRecreatesPerPass == 0 {
		cfg.MaxRecreatesPerPass = DefaultMaxRecreatesPerPass
	}
	if cfg.WorkTimeout <= 0 {
		cfg.WorkTimeout = DefaultWorkTimeout
	}
	if cfg.MCPProbeInterval == 0 {
		cfg.MCPProbeInterval = DefaultMCPProbeInterval
	}
	if cfg.LeaderHeartbeat <= 0 {
		cfg.LeaderHeartbeat = DefaultLeaderHeartbeat
	}
	if cfg.LeaderPingTimeout <= 0 {
		cfg.LeaderPingTimeout = DefaultLeaderPingTimeout
	}
	if cfg.ResyncInterval == 0 {
		cfg.ResyncInterval = DefaultResyncInterval
	}
	if cfg.SandboxResources == nil {
		p := sandboxres.DefaultPolicy()
		cfg.SandboxResources = &p
	} else if err := cfg.SandboxResources.Validate(); err != nil {
		return nil, fmt.Errorf("reconciler: sandbox resources: %w", err)
	}
	switch cfg.HomeRetention {
	case "":
		cfg.HomeRetention = RetentionDelete
	case RetentionDelete, RetentionRetain:
	default:
		return nil, fmt.Errorf("reconciler: home retention must be %q or %q, not %q", RetentionDelete, RetentionRetain, cfg.HomeRetention)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics(nil)
	}
	if cfg.GitBinaries == nil {
		cfg.GitBinaries = DefaultGitBinaries
	}
	if cfg.Observer != nil {
		if err := cfg.Observer.validate(); err != nil {
			return nil, err
		}
		cfg.Observer.Errors = cfg.Metrics.ObserverErrors
		if cfg.Observer.Log == nil {
			cfg.Observer.Log = cfg.Logger
		}
	}
	if cfg.NetworkPolicy != nil {
		if err := cfg.NetworkPolicy.validate(); err != nil {
			return nil, err
		}
		cfg.NetworkPolicy.Errors = cfg.Metrics.NetworkPolicyErrors
		if cfg.NetworkPolicy.Log == nil {
			cfg.NetworkPolicy.Log = cfg.Logger
		}
	}
	if cfg.LimitRange != nil {
		if err := cfg.LimitRange.validate(); err != nil {
			return nil, err
		}
		cfg.LimitRange.Errors = cfg.Metrics.LimitRangeErrors
		if cfg.LimitRange.Log == nil {
			cfg.LimitRange.Log = cfg.Logger
		}
	}
	wake := func() { notifySelf(pool, cfg.Logger) }
	db := leaderterm.DB{Pool: pool}
	return &Reconciler{
		cfg: cfg, log: cfg.Logger, pool: pool, db: db, q: store.New(db), states: staterev.Publisher{DB: stateDB(cfg, db)},
		backend: backend, gw: gw, secrets: secrets, githubRevs: map[string]string{}, forgeSeals: map[agentKey]forgeSeal{}, draftGitHubDropped: map[string]bool{},
		mcp: mcpservers.New(pool, nil, cfg.Logger), m: cfg.Metrics,
		remoteAgents: remoteagents.New(pool, nil, cfg.Logger).WithOpener(cfg.Keys).WithURLPolicy(cfg.URLPolicy),
		plan: gatewayplan.New(gatewayplan.Config{
			KeyDir: cfg.ProviderKeyDir, MCPKeyDir: cfg.MCPKeyDir, KeySettle: cfg.ProviderKeySettle, ToolServers: cfg.ToolServers,
			MCPAskURL: cfg.MCPAskURL, Logger: cfg.Logger, Wake: wake,
		}),
	}, nil
}

// stateDB is the database the State stream's revisions go to: the pool, unless the config names one.
// Both carry the leader term, so a stale leader cannot publish (issue 635).
func stateDB(cfg Config, db leaderterm.DB) staterev.DB {
	if cfg.StateRevisionDB != nil {
		return leaderterm.TxDB{DB: cfg.StateRevisionDB}
	}
	return db
}

// statePublishTimeout bounds one attempt of publishState; statePublishAttempts and
// statePublishBackoff are its retries inside the pass. On a stuck database a pass waits up to about
// 16.5 s (three attempts and the backoff) before the store is a pass error.
const (
	statePublishTimeout  = 5 * time.Second
	statePublishAttempts = 3
	statePublishBackoff  = 500 * time.Millisecond
)

// publishState stores the state of this pass as a revision of the State stream when it differs from
// the latest one (ADR 0097 decision 2.1). koinon-gateway serves the MCP half from the stream, so the
// revision is the State itself for it: it is stored on its own path, before and whatever the
// agentgateway Apply does, so an agentgateway outage or a rejected config does not freeze key revokes
// and grants on koinon-gateway (#1239). The state is the one a driver serves, without the keys of
// agents whose limit is invalid (WithoutInvalidLimits), so a gateway that follows the stream serves
// what the agentgateway drivers serve.
//
// It returns the stored revision that holds the state (the new one, or the latest when nothing
// changed). A failure is retried a few times in the pass; then it is a pass error, and the sandbox
// work of an agent whose key no stored revision holds waits (keyGate). The next pass
// stores the state again. A probe of a new MCP server waits for the ack of this revision (#1260).
func (r *Reconciler) publishState(ctx context.Context, s gateway.State) (int64, error) {
	served, _ := s.WithoutInvalidLimits()
	var err error
	for attempt := range statePublishAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, fmt.Errorf("reconciler: store the gateway state revision: %w", errors.Join(err, ctx.Err()))
			case <-time.After(statePublishBackoff * time.Duration(attempt)):
			}
		}
		// Its own short deadline: a slow database must not hold the pass.
		pctx, cancel := context.WithTimeout(ctx, statePublishTimeout)
		var p staterev.Published
		p, err = r.states.Publish(pctx, served)
		cancel()
		if err == nil {
			if p.Changed {
				r.m.StateRevisions.Inc()
				r.log.Debug("gateway state revision stored", "revision", p.Revision)
			}
			r.noteKeys(served.Keys, p.Revision, s.Keys)
			r.noteForge(served.EgressCredentials, p.Revision)
			r.noteActors(served.EgressActors, p.Revision)
			return p.Revision, nil
		}
		r.log.Warn("storing the gateway state revision failed", "attempt", attempt+1, "err", err)
	}
	r.m.PassErrors.WithLabelValues(errKindStateRevision).Inc()
	return 0, fmt.Errorf("reconciler: store the gateway state revision: %w", err)
}

// notifySelf wakes the leading reconciler through the same channel the triggers use (#249). The
// payload is empty: the loop ignores it.
func notifySelf(pool *pgxpool.Pool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "SELECT pg_notify($1, '')", NotifyChannel); err != nil {
		log.Warn("waking the reconciler at the end of the key settle failed; the next resync picks it up", "err", err)
	}
}

// errAbortPass marks errors after which applying the gateway state could revoke working keys.
var errAbortPass = errors.New("reconciler: pass aborted")

// work is one agent in a pass.
type work struct {
	org   store.ListOrgsToReconcileRow
	agent store.Agent
	sb    store.AgentSandbox
	// key and material are set for active agents whose key is in hand.
	key      gateway.Key
	material string
	keyErr   error
	// cfg is the active version's config, for active agents.
	cfg agentconfig.Config
	// tools are the MCP tools the active version's grants allow.
	tools []gateway.ToolGrant
	// egress are the org egress routes of an active agent (egressroutes.go).
	egress []route.Route
	// forge is the sealed code-host credential that egress names, and forgeFile its envelope; nil
	// when the agent has none.
	forge     *gatewayplan.ForgeCredential
	forgeFile []byte
	// forgeErr says why an agent with a forge read grant has no credential (recordForge).
	forgeErr error
	// forgeExpects says the agent's run start waits for its forge credential (agentEgress).
	forgeExpects bool
	// actor is the agent's Substrate actor with its recorded UID, nil when it has none (actorOf). The
	// pass sets it before it submits the sandbox work, which may change sb meanwhile.
	actor *gatewayplan.Actor
	// keepWarm is set when the agent asks to stay warm and the org's cap grants it (old issue 315).
	keepWarm bool
	// snapFrom is the database clock read just before the last backend call of the pass that can take a
	// snapshot (snapshotClock). record stores it as the snapshot time when the call took one.
	snapFrom pgtype.Timestamptz

	// submitted is set by the pass when the item went to the work queue (pass goroutine only).
	submitted bool
	// wantsRecreate is set when the item asked for a recreate slot; an item that ends without it
	// releases the agent's hold (runJob).
	wantsRecreate bool
	// rej holds the gateway's rejection message for this agent's org, which is recorded after the
	// agent's own work item, by whichever side comes last. A pointer, so a copy of the work (a
	// temporary variation of it) shares it.
	rej *rejectionNote
}

// rejectionNote is the shared state of w.rej; mu guards it.
type rejectionNote struct {
	mu   sync.Mutex
	done bool
	msg  string
}

// markDone says the agent's work item for this pass has ended or will not run, and returns the
// rejection message set by then, which the caller records.
func (w *work) markDone() string {
	w.rej.mu.Lock()
	defer w.rej.mu.Unlock()
	w.rej.done = true
	return w.rej.msg
}

// setRejection stores the rejection message. It returns true when the work item is already done:
// the caller records the message itself. Otherwise the item records it when it ends.
func (w *work) setRejection(msg string) bool {
	w.rej.mu.Lock()
	defer w.rej.mu.Unlock()
	w.rej.msg = msg
	return w.rej.done
}

// names are the agent's object names: the recorded ones, and the GitHub ones derived from the
// IDs (they are not recorded: agentd never needs them).
func (w *work) names() execution.AgentNames {
	n := execution.AgentObjectNames(w.org.ID.Bytes, w.agent.ID.Bytes)
	// Every driver call names the actor: on Agent Substrate a sandbox gets a new actor name for each new
	// actor (issue #1386, ActorRef); elsewhere it is the sandbox name.
	n.Sandbox, n.Provider, n.KeySecret = w.sb.ActorRef(), w.sb.ProviderName, w.sb.KeySecretName
	return n
}

func (w *work) logAttrs() []any {
	attrs := []any{"org", w.org.Slug, "agent", execution.UUIDString(w.agent.ID.Bytes), "sandbox", w.sb.SandboxName}
	if w.sb.ActorName.Valid {
		attrs = append(attrs, "actor", w.sb.ActorName.String)
	}
	return attrs
}

// ReconcileAll runs one pass over every org and agent. It returns the errors of the agents it
// could not converge; the others are converged regardless.
//
// The loop does not call it: it runs [Reconciler.reconcilePass].
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	q := r.newWorkQueue(ctx)
	defer q.Stop()
	b, err := r.reconcilePass(ctx, q)
	// The loop leaves the sandbox work to the queue; a caller of ReconcileAll wants it done.
	err = errors.Join(append([]error{err}, b.wait()...)...)
	r.observeSandboxes(ctx)
	return err
}

// Close stops the pending key-settle wake-ups (notifySelf, #249) and keeps later passes from
// scheduling more. Each one is a timer that sends a NOTIFY on the shared reconcile channel
// ProviderKeySettle after a pass wrote a key file, so a reconciler that is dropped without Close
// wakes whichever leader holds the lock then, long after its own work ended (#1074). Run calls it
// when it returns; a caller that only uses ReconcileAll calls it when it is done. Close is safe to
// call more than once, and a pass after it still converges: the next wake-up or resync picks up
// the end of a settle.
func (r *Reconciler) Close() {
	r.plan.Close()
}

// reconcilePass is one pass over every org and agent, with its metrics, without the gateway count.
// A pass loads the state, runs the gateway step (plan, Apply, recordServed) and hands each agent's
// sandbox work to q; it does not wait for that work. The returned batch holds the work of this pass.
func (r *Reconciler) reconcilePass(ctx context.Context, q *workQueue) (*jobs, error) {
	start := time.Now()
	b := &jobs{}
	r.log.Info("reconcile pass started")
	err := r.reconcileAll(ctx, q, b)
	took := time.Since(start)
	r.m.PassDuration.Observe(took.Seconds())
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.m.Passes.WithLabelValues(result).Inc()
	r.log.Info("reconcile pass ended", "duration", took.Round(time.Millisecond), "jobs", b.count(), "result", result)
	// The jobs end later. Then the recorded states are current: count them, and say how long the
	// slowest job took, so a slow backend call is in the log.
	safego.Go(ctx, r.log, "reconcile-jobs-report", func(ctx context.Context) {
		b.wg.Wait()
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		r.observeSandboxes(octx)
		name, d := b.slowest()
		r.log.Info("reconcile jobs finished", "since_pass_start", time.Since(start).Round(time.Millisecond),
			"jobs", b.count(), "slowest", name, "slowest_took", d.Round(time.Millisecond))
	})
	return b, err
}

// observeSandboxes sets the sandboxes gauge from the recorded states. A failed read keeps the
// previous values.
func (r *Reconciler) observeSandboxes(ctx context.Context) {
	rows, err := r.q.CountAgentSandboxesByState(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Debug("counting sandboxes for metrics failed", "err", err)
		}
		return
	}
	counts := map[store.SandboxState]int64{}
	for _, row := range rows {
		counts[row.State] = row.N
	}
	for _, s := range sandboxStates {
		r.m.Sandboxes.WithLabelValues(string(s)).Set(float64(counts[s]))
	}
}

func (r *Reconciler) reconcileAll(ctx context.Context, q *workQueue, b *jobs) error {
	r.refreshSupervisor(ctx)
	r.checkGatewayID(ctx)
	orgs, err := r.q.ListOrgsToReconcile(ctx)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		return fmt.Errorf("reconciler: list orgs: %w", err)
	}
	workspaces := map[string]string{} // org ID -> workspace, ensured in this pass
	var (
		items []*work
		errs  []error
	)
	for _, org := range orgs {
		agents, err := r.q.ListAgentsToReconcile(ctx, org.ID)
		if err != nil {
			// Without the full list, an Apply would revoke this org's keys.
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			return fmt.Errorf("reconciler: list agents of org %s: %w", org.Slug, err)
		}
		warm := 0
		// granted holds the agents decided in this pass: true when granted, false when refused. An
		// agent without an entry (prepare failed, or nothing to do) keeps its stored value.
		granted := map[pgtype.UUID]bool{}
		for _, a := range agents {
			w, err := r.prepare(ctx, org, a, workspaces)
			if errors.Is(err, errAbortPass) {
				// prepare aborts only on a database read.
				r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
				return err
			}
			if err != nil || w == nil {
				if err != nil {
					r.m.PassErrors.WithLabelValues(errKindAgentPrepare).Inc()
					errs = append(errs, err)
				}
				if a.KeepWarm && a.KeepWarmGranted && a.Status == store.AgentStatusActive {
					// Its slot stays held, so a passing error does not hand it to the next agent.
					warm++
				}
				continue
			}
			if a.KeepWarm && a.Status == store.AgentStatusActive && warm < r.cfg.KeepWarmPerOrg {
				// The list is oldest first, so the cap keeps the same agents from pass to pass.
				w.keepWarm = true
				granted[a.ID] = true
				warm++
			} else {
				granted[a.ID] = false
			}
			items = append(items, w)
		}
		if err := r.recordKeepWarmGrants(ctx, org.ID, agents, granted); err != nil {
			// The grant itself holds; only the API's view of it is late. The next pass retries.
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			errs = append(errs, err)
		}
	}

	// The org's providers and models (PV1). Without the list an Apply would stop serving every
	// model, so a database error aborts the pass.
	served, provErrs, err := r.loadProviders(ctx, orgs)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		return errors.Join(append(errs, fmt.Errorf("%w: %w", errAbortPass, err))...)
	}
	r.m.PassErrors.WithLabelValues(errKindProvider).Add(float64(len(provErrs)))
	errs = append(errs, provErrs...)
	// The orgs' remote MCP servers (MC1), likewise.
	mcpServed, mcpErrs, err := r.loadMCP(ctx, orgs)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		return errors.Join(append(errs, fmt.Errorf("%w: %w", errAbortPass, err))...)
	}
	r.m.PassErrors.WithLabelValues(errKindMCP).Add(float64(len(mcpErrs)))
	errs = append(errs, mcpErrs...)

	// The gateway side is decided by the plan (internal/reconciler/gatewayplan): one state for every
	// agent, keys of archived agents revoked by omission. The reconciler loads its inputs and runs its
	// steps in the order it returns them.
	//
	// Spend limits net of recorded spend (plan item L1) are recomputed every pass, so a key rotation or
	// a gateway that lost its budget counters gets the remainder, not the full limit.
	now := r.cfg.Now()
	limitFor := r.cfg.AgentLimit
	if limitFor == nil {
		limitFor = ledger.AgentLimit
	}
	live := make(map[agentKey]bool, len(items))
	for _, w := range items {
		live[agentKey{w.org.ID, w.agent.ID}] = true
	}
	r.releaseRecreatesExcept(live, q.Busy)
	r.pruneForgeSeals(live)
	agents := make([]gatewayplan.Agent, len(items))
	egressFiles := map[string][]byte{}
	for i, w := range items {
		if w.forge != nil {
			egressFiles[w.forge.File] = w.forgeFile
		}
		w.actor = r.actorOf(w)
		agents[i] = gatewayplan.Agent{
			Org: gatewayplan.Org{ID: w.org.ID, Slug: w.org.Slug}, ID: w.agent.ID, Ref: agentRef(w.org, w.agent.ID),
			Active: w.agent.Status == store.AgentStatusActive, Key: w.key, Tools: w.tools, Egress: w.egress,
			Forge: w.forge, Actor: w.actor,
		}
		if w.key.ID != "" {
			agents[i].Limit, agents[i].LimitErr = limitFor(ctx, r.q, w.agent, now)
		}
	}
	orgRefs := make([]gatewayplan.Org, len(orgs))
	for i, o := range orgs {
		orgRefs[i] = gatewayplan.Org{ID: o.ID, Slug: o.Slug}
	}
	ledgerEnforces := false
	if g, ok := r.gw.(gateway.LedgerBudgets); ok {
		ledgerEnforces = g.LedgerEnforcesBudgets()
	}
	// The MCP allowlists are read before any key file is written: a failure after the Put would leave
	// the written files unrecorded as settling (the next pass would see them unchanged).
	var allowances map[string]map[pgtype.UUID][]gatewayplan.Allowed
	if len(mcpServed.servers) > 0 {
		var err error
		if allowances, err = r.mcpAllowancesOf(ctx, items); err != nil {
			r.m.PassErrors.WithLabelValues(errKindMCP).Inc()
			return errors.Join(append(errs, err)...)
		}
	}
	draft := r.plan.Begin(gatewayplan.Input{
		Now: now, Orgs: orgRefs, Agents: agents,
		Providers: served.providers, ProviderFiles: served.files, ModelIDs: served.modelIDs,
		MCP: mcpServed.servers, MCPFiles: mcpServed.files, EgressFiles: egressFiles, LedgerEnforcesBudgets: ledgerEnforces, Allowances: allowances,
	})
	// An agent whose budget cannot be computed fails closed (its last pushed limit, or zero) and is
	// reported; it never stops the pass for the other agents and orgs.
	for _, lf := range draft.LimitFailures {
		w := items[lf.Agent]
		r.m.PassErrors.WithLabelValues(errKindSpendLimit).Inc()
		r.log.Error("agent budget not computed; its key keeps its last pushed limit, or zero if it had none",
			"agent", execution.UUIDString(w.agent.ID.Bytes), "org", w.key.Agent.Org, "err", lf.Err)
		errs = append(errs, fmt.Errorf("spend limit of agent %s not computed (key fails closed): %w", execution.UUIDString(w.agent.ID.Bytes), lf.Err))
	}
	// A budget with nothing left stops the agent's runs (or the org's) with the budget reason before
	// the Apply below blocks or revokes the key, so a run in flight records the budget stop and not the
	// gateway's 401 or 429 (old issue 188).
	for _, i := range draft.Stops {
		w := items[i]
		if n, err := ledger.StopSpent(ctx, r.db, w.agent, now); err != nil {
			r.m.PassErrors.WithLabelValues(errKindSpendLimit).Inc()
			errs = append(errs, fmt.Errorf("stop the runs of spent agent %s: %w", execution.UUIDString(w.agent.ID.Bytes), err))
		} else if n > 0 {
			r.log.Info("budget spent: runs stopped before the key is blocked", append(w.logAttrs(), "runs", n)...)
		}
	}
	// Provider key files first, then the config that names them; files no provider uses go last.
	// One provider's new key file still travelling to the gateway pod never holds back the others:
	// that provider keeps serving with its previous file, or is left out until the file has arrived.
	var written []string
	var present map[string]bool
	for _, part := range r.splitKeyFiles(draft.Files) {
		w, pr, err := part.store.Put(ctx, part.files)
		if err != nil {
			r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
			return errors.Join(append(errs, fmt.Errorf("reconciler: deliver %s: %w", part.what, err))...)
		}
		written = append(written, w...)
		if present == nil {
			present = map[string]bool{}
		}
		maps.Copy(present, pr)
	}
	plan, err := draft.Settle(ctx, written, present, planLookups{r})
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindMCP).Inc()
		return errors.Join(append(errs, err)...)
	}
	for _, pr := range plan.Problems {
		kind := errKindProvider
		if pr.Kind == gatewayplan.ProblemSpendLimit {
			kind = errKindSpendLimit
		}
		r.m.PassErrors.WithLabelValues(kind).Inc()
		errs = append(errs, pr.Err)
	}
	// The State stream first, on its own path (D3 of #1239): koinon-gateway serves the MCP half from
	// it, whatever the agentgateway Apply below does.
	// servedKeys are the keys koinon-gateway is to serve (keyGate).
	servedKeys := map[string]bool{}
	servedState, _ := plan.State.WithoutInvalidLimits()
	for _, k := range servedState.Keys {
		servedKeys[k.ID] = true
	}
	stateRev, pubErr := r.publishState(ctx, plan.State)
	if pubErr != nil {
		errs = append(errs, pubErr)
	}
	// Sandbox work is a per-agent work queue, outside the sequential pass (plan item LT1, practice B4,
	// issue 573). An agent whose key the gateway already serves does not depend on the Apply below,
	// which may wait for agentgateway's hot reload for about a minute: its sandbox work is queued now,
	// concurrently with it. An agent with a new key is queued after the Apply, as before. The pass
	// never waits for the queue: a slow call for one agent holds only that agent. koinon-gateway must
	// serve the key too: an early agent whose key the live copies have not acked waits with the others.
	early, deferred, _ := r.newKeyGate(ctx, servedKeys).split(ctx, items, plan.Early)
	// What the run start of each agent waits for on its forge credential (#1086 part C2), before any
	// sandbox work of this pass: a run must never find no row for an agent that expects one, because
	// the pass has not written it yet. The rows are marked served at the end of the pass.
	if err := r.recordForge(ctx, items, false); err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		errs = append(errs, err)
	}
	r.submit(q, b, workOf(items, early), workspaces)

	var res gatewayplan.ApplyResult
	rejectedReason := ""
	applyStart := time.Now()
	_, applyErr := r.gw.Apply(ctx, plan.State)
	r.m.GatewayApply.Observe(time.Since(applyStart).Seconds())
	if err := applyErr; err != nil {
		// A Kubernetes-mode gateway judges objects one by one: the revision is live for everything
		// but the objects named, which are reported on their org and do not hold back the others.
		var rej *gateway.RejectedError
		if !errors.As(err, &rej) {
			// The gateway is unreachable or the reload did not complete. Sandbox work does not
			// depend on it (start on demand, idle stop), so it goes on, except for agents whose
			// new key is not live yet. Nothing is recorded as applied and nothing is pruned.
			res.Failed = true
			var cfgRej *gateway.ConfigRejectedError
			if errors.As(err, &cfgRej) {
				// The whole file was refused and the gateway keeps its previous config, so no key,
				// revocation, model or limit of this pass is live. Its reason is only in the gateway's
				// log; say it here, on the agents waiting for it and in the metrics (ADR 0050).
				rejectedReason = cfgRej.Reason
				if rejectedReason == "" {
					rejectedReason = "the reason could not be read from the gateway's log"
				}
				r.m.PassErrors.WithLabelValues(errKindGatewayConfig).Inc()
				r.log.Error("agentgateway rejected the config; it keeps serving the previous one, so no change of this pass is live",
					"revision", string(cfgRej.Revision), "reason", rejectedReason)
			} else {
				r.m.PassErrors.WithLabelValues(errKindGatewayApply).Inc()
			}
			errs = append(errs, fmt.Errorf("reconciler: apply gateway state: %w", err))
		} else {
			res.Rejections = rej.Rejected
			r.m.PassErrors.WithLabelValues(errKindGatewayRejected).Inc()
			for _, x := range rej.Rejected {
				r.log.Error("the gateway rejected an object; the rest is live", "org", x.Org, "kind", x.Kind, "object", x.Name, "reason", x.Reason)
			}
			errs = append(errs, fmt.Errorf("reconciler: %w", rej))
		}
	}
	r.m.GatewayConfigRejected.Set(b2f(rejectedReason != ""))
	outcome := plan.Applied(res)
	r.probeSoon(outcome.Probe, stateRev)
	if pubErr == nil {
		r.probeSoon(r.takeParked(), stateRev)
	}
	if !res.Failed {
		if err := r.recordServed(ctx, outcome.Served); err != nil {
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			errs = append(errs, err)
		}
		if r.cfg.ProviderKeys != nil && len(provErrs) == 0 && len(mcpErrs) == 0 {
			// With a store of its own for MCP files, the provider keys Secret keeps no MCP file: an
			// earlier version wrote them there, and the prune removes them once the new Secret has them.
			keepProviders, keepMCP := plan.Keep, map[string]bool(nil)
			if r.cfg.MCPKeys != nil {
				keepProviders, keepMCP = splitKeep(plan.Keep)
			}
			if err := r.cfg.ProviderKeys.Prune(ctx, keepProviders); err != nil {
				r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
				errs = append(errs, fmt.Errorf("reconciler: prune provider keys: %w", err))
			}
			if r.cfg.MCPKeys != nil {
				if err := r.cfg.MCPKeys.Prune(ctx, keepMCP); err != nil {
					r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
					errs = append(errs, fmt.Errorf("reconciler: prune MCP credentials: %w", err))
				}
			}
		}
	}

	if err := r.recordProviderStatus(ctx, orgRefs, res, applyErr, rejectedReason, outcome.Served, served.skipped); err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		errs = append(errs, err)
	}

	heldBack := 0
	defer func() { r.m.AgentsHeldBack.Set(float64(heldBack)) }()
	for _, w := range workOf(items, outcome.Held) {
		r.log.Warn("the gateway does not serve the agent's new key yet; its sandbox work waits", w.logAttrs()...)
		msg := "waiting for the gateway to serve the agent's new key"
		if rejectedReason != "" {
			msg += ": agentgateway rejected the config: " + rejectedReason
		}
		r.recordMessage(ctx, w, errors.New(msg))
		heldBack++
	}
	// Run start waits until koinon-gateway serves the agent's key too (D1 of #1239): agentd's first
	// MCP call carries it. The wait is bounded (MCPAckWait) and happens only when an agent waits; after
	// it those agents are held, and the next pass looks again.
	run := append(append([]int(nil), outcome.Run...), deferred...)
	if _, wait, maxRev := r.newKeyGate(ctx, servedKeys).split(ctx, items, run); len(wait) > 0 {
		if maxRev > 0 {
			r.awaitGatewayAcked(ctx, maxRev)
		}
		var held []int
		run, held, _ = r.newKeyGate(ctx, servedKeys).split(ctx, items, run)
		for _, w := range workOf(items, held) {
			r.log.Warn("koinon-gateway does not serve the agent's key yet; its sandbox work waits", w.logAttrs()...)
			r.recordMessage(ctx, w, errors.New("waiting for koinon-gateway to serve the agent's key"))
			heldBack++
		}
	}
	r.submit(q, b, workOf(items, run), workspaces)
	// Mark the forge rows whose revision every live koinon-gateway copy acked. It comes after the submit:
	// the sandbox work does not wait for it, only the harness start in koinon-agentd does.
	if err := r.recordForge(ctx, items, true); err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		errs = append(errs, err)
	}
	// Each rejected object is reported on its org's agents, after their own work item has recorded its
	// state (recordRejection). Several rejections of one org are joined into one message, not overwritten.
	byOrg := map[string][]string{}
	for _, x := range res.Rejections {
		byOrg[x.Org] = append(byOrg[x.Org], fmt.Sprintf("the gateway rejected %s %s: %s", x.Kind, x.Name, x.Reason))
	}
	for _, w := range items {
		if !w.submitted {
			w.markDone()
		}
		if msgs := byOrg[w.org.Slug]; len(msgs) > 0 && w.setRejection(strings.Join(msgs, "; ")) {
			r.recordMessage(ctx, w, errors.New(strings.Join(msgs, "; ")))
		}
	}
	return errors.Join(errs...)
}

// agentKey identifies an agent in the work queue.
type agentKey struct{ org, agent pgtype.UUID }

// sandboxJob is one agent's sandbox work of one pass.
type sandboxJob struct {
	w          *work
	workspaces map[string]string
	batch      *jobs
}

// workQueue is the per-agent queue of sandbox jobs (workqueue.go).
type workQueue = keyedQueue[agentKey, *sandboxJob]

// newWorkQueue returns the queue the leader's passes feed. Its workers stop with ctx or with Stop,
// and SandboxJobs bounds how many agents are worked on at once.
func (r *Reconciler) newWorkQueue(ctx context.Context) *workQueue {
	return newKeyedQueue(ctx, r.cfg.SandboxJobs, r.runJob, func(j *sandboxJob) {
		// Replaced by a newer item of the same agent, or left when the leader ended: it never runs.
		j.w.markDone()
		j.batch.wg.Done()
	})
}

// DefaultWorkTimeout bounds one agent's sandbox work item.
const DefaultWorkTimeout = 5 * time.Minute

// jobs is the set of sandbox jobs one pass handed to the queue.
type jobs struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
	n    int
	// slowestName and slowestTook are the longest job that ended.
	slowestName string
	slowestTook time.Duration
}

// wait blocks until every job has ended (or was dropped) and returns their errors.
func (j *jobs) wait() []error {
	j.wg.Wait()
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.errs
}

func (j *jobs) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.n
}

func (j *jobs) slowest() (string, time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.slowestName, j.slowestTook
}

// submit queues one job per agent. A job is everything the reconciler does for one agent's sandbox:
// creating, starting, stopping, recreating and recording it. Queued behind another job of the same
// agent it replaces any older queued one; it never runs in parallel with it.
func (r *Reconciler) submit(q *workQueue, b *jobs, ws []*work, workspaces map[string]string) {
	for _, w := range ws {
		w.submitted = true
		b.wg.Add(1)
		b.mu.Lock()
		b.n++
		b.mu.Unlock()
		q.Add(agentKey{w.org.ID, w.agent.ID}, &sandboxJob{w: w, workspaces: workspaces, batch: b})
	}
}

// runJob is the queue's worker function: one agent's sandbox work, with its own timeout.
func (r *Reconciler) runJob(ctx context.Context, _ agentKey, j *sandboxJob) {
	defer j.batch.wg.Done()
	w := j.w
	start := time.Now()
	jctx, cancel := context.WithTimeout(ctx, r.cfg.WorkTimeout)
	defer cancel()
	// An item that waited behind a running one of the same agent starts from the row as that one left
	// it, not from the row its pass loaded.
	if sb, gerr := r.q.GetAgentSandbox(jctx, store.GetAgentSandboxParams{OrgID: w.org.ID, AgentID: w.agent.ID}); gerr == nil {
		w.sb = sb
	}
	var err error
	if safego.Do(r.log, "reconcile-agent", func() { err = r.reconcileAgent(jctx, w, j.workspaces) }) {
		err = errors.New("internal error: reconciling the agent panicked")
	}
	if err != nil {
		// Recorded under the queue's context: the job's own may have run out.
		r.m.PassErrors.WithLabelValues(errKindSandbox).Inc()
		r.log.Warn("agent not converged", append(w.logAttrs(), "err", err)...)
		if errors.Is(err, execution.ErrForeign) {
			r.recordForeign(ctx, w, err)
		} else {
			r.recordMessage(ctx, w, err)
		}
	}
	if msg := w.markDone(); msg != "" {
		r.recordMessage(ctx, w, errors.New(msg))
	}
	if !w.wantsRecreate {
		r.recreateReleased(agentKey{w.org.ID, w.agent.ID})
	}
	took := time.Since(start)
	j.batch.mu.Lock()
	if err != nil {
		j.batch.errs = append(j.batch.errs, fmt.Errorf("agent %s: %w", execution.UUIDString(w.agent.ID.Bytes), err))
	}
	if took > j.batch.slowestTook {
		j.batch.slowestName, j.batch.slowestTook = execution.UUIDString(w.agent.ID.Bytes), took
	}
	j.batch.mu.Unlock()
}

// workOf returns the items at the given indexes.
func workOf(items []*work, idx []int) []*work {
	out := make([]*work, 0, len(idx))
	for _, i := range idx {
		out = append(out, items[i])
	}
	return out
}

// mcpAllowancesOf reads the MCP allowlists of the orgs that have an active agent with a key, once per org.
func (r *Reconciler) mcpAllowancesOf(ctx context.Context, items []*work) (map[string]map[pgtype.UUID][]gatewayplan.Allowed, error) {
	out := map[string]map[pgtype.UUID][]gatewayplan.Allowed{}
	for _, w := range items {
		if w.agent.Status != store.AgentStatusActive || w.key.ID == "" {
			continue
		}
		if _, ok := out[w.org.Slug]; ok {
			continue
		}
		a, err := r.mcpAllowances(ctx, w.org.ID)
		if err != nil {
			return nil, err
		}
		out[w.org.Slug] = a
	}
	return out, nil
}

// recordServed writes, for every org, the models the applied state serves to gateway_served_models,
// which koinon-agentd waits on before a run starts its harness (old issue 174). An org whose model objects
// the gateway rejected gets nothing new recorded, but models no longer in state are removed. Only the
// leader reconciles, so only the leader writes.
func (r *Reconciler) recordServed(ctx context.Context, served []gatewayplan.ServedModels) error {
	var errs []error
	for _, s := range served {
		var err error
		if s.PruneOnly {
			err = r.q.PruneGatewayServedModels(ctx, store.PruneGatewayServedModelsParams{OrgID: s.Org.ID, ModelIds: s.IDs})
		} else {
			err = r.q.SyncGatewayServedModels(ctx, store.SyncGatewayServedModelsParams{OrgID: s.Org.ID, ModelIds: s.IDs, KeyFiles: s.Files})
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("reconciler: record the models the gateway serves for org %s: %w", s.Org.Slug, err))
		}
	}
	return errors.Join(errs...)
}

// planLookups are the reads the gateway plan makes while it settles a pass.
type planLookups struct{ r *Reconciler }

func (l planLookups) SystemKey(ctx context.Context, secret string, org gatewayplan.Org, ref gateway.AgentRef) (gateway.Key, error) {
	return l.r.systemKey(ctx, secret, org.ID, ref)
}

// IssueKey issues a probe key. Until a stored revision holds it, the prober does not use it (#1260).
func (l planLookups) IssueKey(ctx context.Context, ref gateway.AgentRef) (gateway.IssuedKey, error) {
	k, err := l.r.gw.IssueKey(ctx, ref)
	if err == nil {
		l.r.keyIssued(k.ID)
	}
	return k, err
}

func (l planLookups) SystemLimit(ctx context.Context, org gatewayplan.Org, now time.Time) (*gateway.Limit, error) {
	return ledger.SystemLimit(ctx, l.r.q, org.ID, now)
}

func (l planLookups) ServedKeyFile(ctx context.Context, org, provider pgtype.UUID) (string, bool, error) {
	file, err := l.r.q.GetGatewayServedKeyFile(ctx, store.GetGatewayServedKeyFileParams{OrgID: org, ProviderID: provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return file, err == nil, err
}

// prepare loads or creates the agent's row and, for an active agent, its key. It returns nil
// for agents with nothing rendered and nothing to render.
func (r *Reconciler) prepare(ctx context.Context, org store.ListOrgsToReconcileRow, a store.Agent, workspaces map[string]string) (*work, error) {
	w := &work{org: org, agent: a, rej: &rejectionNote{}}
	sb, err := r.q.GetAgentSandbox(ctx, store.GetAgentSandboxParams{OrgID: org.ID, AgentID: a.ID})
	switch {
	case err == nil:
		w.sb = sb
	case errors.Is(err, pgx.ErrNoRows):
		if a.Status != store.AgentStatusActive {
			return nil, nil
		}
		ws, err := r.ensureWorkspace(ctx, org, workspaces)
		if err != nil {
			return nil, err
		}
		names := execution.AgentObjectNames(org.ID.Bytes, a.ID.Bytes)
		w.sb, err = r.q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
			OrgID: org.ID, AgentID: a.ID, Workspace: ws,
			SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
		})
		if err != nil {
			return nil, fmt.Errorf("reconciler: record sandbox of agent %s: %w", execution.UUIDString(a.ID.Bytes), err)
		}
	default:
		// Without the row, the agent's key would be left out of the gateway state and so
		// revoked: abort the pass instead.
		return nil, fmt.Errorf("%w: read sandbox of agent %s: %w", errAbortPass, execution.UUIDString(a.ID.Bytes), err)
	}

	if recorded, ok := recordedKey(org, w.sb); ok {
		// Keep the recorded key in the gateway state unless a newer one replaces it, so a
		// failure below never revokes a working key. Draft agents keep theirs for a quick
		// re-activation; archived agents' keys are left out and so revoked.
		if a.Status != store.AgentStatusArchived {
			w.key = recorded
		}
	}
	if a.Status == store.AgentStatusActive {
		cfg, err := r.activeConfig(ctx, a)
		if err != nil {
			// Without it the agent's tools would be revoked by omission.
			return nil, fmt.Errorf("%w: %w", errAbortPass, err)
		}
		w.cfg = cfg
		if len(r.cfg.ToolServers) > 0 {
			w.tools = ToolGrants(cfg, r.cfg.ToolServers)
		}
		w.egress, w.forge, w.forgeFile, w.forgeErr = r.agentEgress(ctx, w)
		key, material, err := r.ensureKey(ctx, w)
		if err != nil {
			w.keyErr = err
		} else {
			w.key, w.material = key, material
		}
	}
	return w, nil
}

func agentRef(org store.ListOrgsToReconcileRow, agent pgtype.UUID) gateway.AgentRef {
	return gateway.AgentRef{Org: org.Slug, Name: execution.UUIDString(agent.Bytes)}
}

func recordedKey(org store.ListOrgsToReconcileRow, sb store.AgentSandbox) (gateway.Key, bool) {
	if !sb.GatewayKeyID.Valid || !sb.GatewayKeyHash.Valid {
		return gateway.Key{}, false
	}
	return gateway.Key{ID: sb.GatewayKeyID.String, Agent: agentRef(org, sb.AgentID), Hash: sb.GatewayKeyHash.String}, true
}

// recordedMaterial returns the agent's recorded key and its material when the key Secret holds
// that key (same ID, same hash); ok is false when there is no recorded key or the Secret does not
// match it.
func (r *Reconciler) recordedMaterial(ctx context.Context, w *work) (gateway.Key, string, bool, error) {
	sec, found, err := r.secrets.Get(ctx, w.sb.KeySecretName)
	if err != nil {
		return gateway.Key{}, "", false, err
	}
	if recorded, ok := recordedKey(w.org, w.sb); ok && found &&
		sec.KeyID == recorded.ID && gateway.HashKey(sec.Material) == recorded.Hash {
		return recorded, sec.Material, true, nil
	}
	return gateway.Key{}, "", false, nil
}

// ensureKey returns the agent's key and its material from the key Secret, issuing a new key
// when there is none or the Secret no longer matches the recorded hash (a hand edit or a
// deleted Secret: the old material is gone, so the key is rotated).
func (r *Reconciler) ensureKey(ctx context.Context, w *work) (gateway.Key, string, error) {
	recorded, material, ok, err := r.recordedMaterial(ctx, w)
	if err != nil {
		return gateway.Key{}, "", err
	}
	if ok {
		return recorded, material, nil
	}
	reason := "no key yet"
	if w.sb.GatewayKeyID.Valid {
		reason = "key Secret missing or not matching the recorded hash"
	}

	issued, err := r.gw.IssueKey(ctx, agentRef(w.org, w.agent.ID))
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("issue gateway key: %w", err)
	}
	if err := r.secrets.Put(ctx, KeySecret{
		Name: w.sb.KeySecretName, KeyID: issued.ID, KeyHash: issued.Hash, Material: issued.Material,
		Labels: ownerLabels(w.org.ID, w.agent.ID),
	}); err != nil {
		return gateway.Key{}, "", err
	}
	sb, err := r.q.SetAgentSandboxKey(ctx, store.SetAgentSandboxKeyParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		GatewayKeyID:   pgtype.Text{String: issued.ID, Valid: true},
		GatewayKeyHash: pgtype.Text{String: issued.Hash, Valid: true},
	})
	if err != nil {
		return gateway.Key{}, "", fmt.Errorf("record gateway key: %w", err)
	}
	w.sb = sb
	r.plan.KeyIssued(issued.ID)
	r.keyIssued(issued.ID)
	r.log.Info("issued agentgateway key", append(w.logAttrs(), "key_id", issued.ID, "reason", reason)...)
	return issued.Key, issued.Material, nil
}

// ensureWorkspace makes sure the org's workspace exists, once per pass.
func (r *Reconciler) ensureWorkspace(ctx context.Context, org store.ListOrgsToReconcileRow, done map[string]string) (string, error) {
	r.wsMu.Lock()
	defer r.wsMu.Unlock()
	id := execution.UUIDString(org.ID.Bytes)
	if ws, ok := done[id]; ok {
		return ws, nil
	}
	name := execution.WorkspaceName(org.ID.Bytes)
	if row, err := r.q.GetOrgWorkspace(ctx, org.ID); err == nil {
		name = row.Workspace
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("reconciler: read workspace of org %s: %w", org.Slug, err)
	}
	ws, err := r.backend.EnsureWorkspace(ctx, name, ownerLabels(org.ID, pgtype.UUID{}))
	if err != nil {
		return "", fmt.Errorf("reconciler: ensure workspace of org %s: %w", org.Slug, err)
	}
	if _, err := r.q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{
		OrgID: org.ID, Workspace: name, WorkspaceID: ws.ID,
	}); err != nil {
		return "", fmt.Errorf("reconciler: record workspace of org %s: %w", org.Slug, err)
	}
	if r.cfg.Observer != nil && ws.Active {
		// The observer only adds detail to a wait: a failure is counted and logged there, never fatal here.
		_ = r.cfg.Observer.EnsureAccess(ctx, name)
	}
	if r.cfg.NetworkPolicy != nil && ws.Active {
		// Defense in depth, never fatal here. A missing workspace namespace is expected before the
		// first sandbox: the policy is written after CreateSandbox. The writer counts and logs other failures.
		_ = r.cfg.NetworkPolicy.Ensure(ctx, name)
	}
	if r.cfg.LimitRange != nil && ws.Active {
		// LimitRange sizes pods only at admission (ADR 0083 amendment). A missing workspace
		// namespace is expected until the first CreateSandbox (OpenShell creates it then); other
		// Ensure failures hold this org so CreateSandbox cannot run without a writable LimitRange.
		// Mark done only after a definitive outcome so a failed pass does not let another agent skip it.
		if err := r.cfg.LimitRange.Ensure(ctx, name); err != nil && !limitRangeNamespaceMissing(err) {
			return "", fmt.Errorf("reconciler: sandbox LimitRange of org %s: %w", org.Slug, err)
		}
	}
	done[id] = name
	return name, nil
}

func (r *Reconciler) reconcileAgent(ctx context.Context, w *work, workspaces map[string]string) error {
	switch w.agent.Status {
	case store.AgentStatusActive:
		return r.reconcileActive(ctx, w, workspaces)
	case store.AgentStatusArchived:
		return r.reconcileArchived(ctx, w)
	default:
		return r.reconcileDraft(ctx, w)
	}
}

func (r *Reconciler) reconcileActive(ctx context.Context, w *work, workspaces map[string]string) error {
	if w.keyErr != nil {
		return w.keyErr
	}
	harness, ok := r.cfg.Harnesses[w.agent.Harness]
	if !ok || !harness.rendered() {
		return fmt.Errorf("harness %q has no image or binaries configured", w.agent.Harness)
	}
	ws, err := r.ensureWorkspace(ctx, w.org, workspaces)
	if err != nil {
		return err
	}
	names := w.names()

	// The key goes only into the provider record; the sandbox sees a placeholder.
	ensureKey := func() error {
		return r.backend.EnsureProvider(ctx, ws, execution.Provider{
			Name:               names.Provider,
			Profile:            r.profile(w.agent.Harness, harness),
			Credentials:        map[string]string{keyEnvVar: w.material},
			CredentialRevision: w.key.ID,
			Labels:             ownerLabels(w.org.ID, w.agent.ID),
		})
	}
	if err := ensureKey(); err != nil {
		return err
	}

	// The GitHub read grant: its provider record before the sandbox that attaches it.
	baseURL, err := r.baseURLOf(ctx, w)
	if err != nil {
		return err
	}
	read, hasRead, err := GitHubReadAccessAt(w.cfg, baseURL)
	if err != nil {
		return err
	}
	providers := []string{names.Provider}
	network := map[string]execution.NetworkRule{}
	if hasRead {
		if err := r.ensureGitHub(ctx, ws, w, names, harness, read); err != nil {
			return err
		}
		providers = append(providers, names.GitHubProvider)
		network[githubRule] = GitHubRule(read, r.githubBinaries(harness))
		if r.cfg.Egress != nil {
			network[egressRule] = EgressRule(*r.cfg.Egress, read, r.githubBinaries(harness))
		}
	}
	if rule, ok := PluginInstallRule(w.cfg, w.agent.Harness, harness); ok {
		network[npmRule] = rule
	}

	// An ephemeral agent's sandbox belongs to a task (ephemeral.go); the row moves between them.
	act, err := r.q.GetAgentRunActivity(ctx, store.GetAgentRunActivityParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	if err != nil {
		return fmt.Errorf("read run activity: %w", err)
	}
	if done, err := r.ephemeralTurn(ctx, w, ws, act); err != nil || done {
		return err
	}
	names = w.names()

	spec, hash := r.sandboxSpec(w.org.ID, w.agent.ID, names, harness, providers, network, r.sandboxResources(w))
	// The sandbox's gateway access (issue #1444), for a driver with a per-sandbox allow list (Substrate):
	// the egress listener under the same condition as the egressRule entry above. It is set on every pass,
	// before this pass creates or starts the sandbox, and the driver applies it in place: a changed grant
	// never replaces the sandbox, and a removed one takes the listener away at once (within the data plane's
	// policy cache TTL). It is not in spec or
	// hash for that reason.
	access := execution.GatewayAccess{CodeHostEgress: hasRead && r.cfg.Egress != nil}
	if s := execution.GatewayAccessSetterOf(r.backend); s != nil {
		if err := s.SetGatewayAccess(ctx, ws, names.Sandbox, access); err != nil {
			return err
		}
	}
	tune, err := orgsettings.Get(ctx, r.q, w.org.ID, orgsettings.Settings{
		IdleTimeout: r.cfg.IdleTimeout, ParkWarm: r.cfg.ParkWarm,
		SandboxCreationsPerMinute: orgsettings.DefaultSandboxCreationsPerMinute,
	})
	if err != nil {
		return fmt.Errorf("read org settings: %w", err)
	}

	sb, err := r.backend.GetSandbox(ctx, ws, names.Sandbox)
	if errors.Is(err, execution.ErrNotFound) {
		if !act.HasActive && !act.HasDemand && !r.woken(w.sb, tune) && !w.keepWarm {
			// Nothing wants the agent, so no sandbox is created: it starts on demand (old issue 315). A
			// run, a claimable park, a pre-wake signal or a keep-warm setting creates one. An
			// archived agent (archive.go) restores its home on the first run.
			if w.sb.ArchivedAt.Valid {
				return r.recordState(ctx, w, store.SandboxStateDeleting, archivedPhase, archivedMessage)
			}
			return r.recordState(ctx, w, store.SandboxStateDeleting, idlePhase, idleMessage)
		}
		if !hasRead {
			// No sandbox references a GitHub record now: drop one left by a removed grant.
			if err := r.deleteGitHub(ctx, ws, names); err != nil {
				return err
			}
		}
		release, held, err := r.holdForCreationRate(ctx, w, tune, orgsettings.CreationCreate, nil, hash)
		if err != nil || held {
			return err
		}
		// A new Substrate actor gets a new name, recorded before the create and given its gateway access
		// first (issue #1386): a name never comes back for another actor.
		if renamed, err := r.nameNewActor(ctx, w, ws, access); err != nil {
			release()
			return err
		} else if renamed {
			names = w.names()
			spec, hash = r.sandboxSpec(w.org.ID, w.agent.ID, names, harness, providers, network, r.sandboxResources(w))
		}
		lrReady := r.cfg.LimitRange == nil || r.cfg.LimitRange.Ready(ctx, ws)
		if err := r.snapshotClock(ctx, w); err != nil { // the first-boot seal is a snapshot
			release()
			return err
		}
		sb, err = r.backend.CreateSandbox(ctx, ws, spec)
		if err != nil {
			if definiteFailure(err) {
				release()
			}
			return err
		}
		r.log.Info("created sandbox", w.logAttrs()...)
		var npErr error
		if r.cfg.NetworkPolicy != nil {
			// OpenShell creates the workspace namespace with the first sandbox, so the pass-start Ensure
			// could not write the policy there. Write it now, in this pass (#993). A NetworkPolicy
			// selects pods live, so the sandbox just created is covered without a recreate.
			// A failure must not skip the LimitRange handling or the record below: it is returned
			// after them.
			if err := r.cfg.NetworkPolicy.Ensure(ctx, ws); err != nil {
				if limitRangeNamespaceMissing(err) {
					r.cfg.NetworkPolicy.CountError(err)
				}
				npErr = fmt.Errorf("reconciler: sandbox NetworkPolicy after create: %w", err)
			}
		}
		afterCreate := func() error {
			if r.cfg.LimitRange != nil {
				// The namespace exists with the sandbox: apply the LimitRange now. If it was not ready
				// before CreateSandbox, supervisors may be BestEffort; delete once so the next pass
				// admits them sized (#76). Delete even when Ensure fails: otherwise the sandbox stays
				// BestEffort forever on the sandbox-exists branch, and a missing-namespace Ensure
				// error on this path would never be counted.
				ensureErr := r.cfg.LimitRange.Ensure(ctx, ws)
				if !lrReady {
					if err := r.deleteSandbox(ctx, w, ws, names.Sandbox, "the LimitRange was applied after the create"); err != nil {
						return err
					}
					if ensureErr != nil {
						return fmt.Errorf("reconciler: sandbox LimitRange after create: %w", ensureErr)
					}
					r.log.Info("recreating sandbox after the LimitRange was applied", w.logAttrs()...)
					sb.Phase = execution.PhaseDeleting
					return r.record(ctx, w, sb, hash, "recreating sandbox after the LimitRange was applied")
				}
				if ensureErr != nil {
					return fmt.Errorf("reconciler: sandbox LimitRange after create: %w", ensureErr)
				}
			}
			return r.record(ctx, w, sb, hash, "")
		}
		return errors.Join(npErr, afterCreate())
	}
	if err != nil {
		return err
	}

	if sb.Phase == execution.PhaseDeleting {
		return r.record(ctx, w, sb, hash, "")
	}
	if w.sb.UnhealthyAt.Valid && (w.sb.UnhealthyGeneration.String == "" || w.sb.UnhealthyGeneration.String == sb.ID) {
		// koinon-agentd found this sandbox stuck: a process in it survives SIGKILL (ADR 0054). It is
		// recreated at once, parked runs included: they stay parked in Postgres and resume in the new
		// sandbox through the reset path (ADR 0051). The new sandbox has a new ID, which clears the
		// mark (RecordAgentSandbox).
		if ok, note, err := r.homeGate(ctx, w, ws, sb, homeDirtiedSince(w.sb)); err != nil || !ok {
			if err != nil {
				return err
			}
			return r.record(ctx, w, sb, hash, note)
		}
		if err := r.deleteSandbox(ctx, w, ws, names.Sandbox, "the sandbox is stuck"); err != nil {
			return err
		}
		r.m.SandboxRepairs.Inc()
		r.log.Warn("deleting a stuck sandbox to recreate it", append(w.logAttrs(), "reason", w.sb.UnhealthyReason.String)...)
		if err := audit.RecordAfter(ctx, r.db, audit.Event{
			Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-repair"), Action: "sandbox.recreate", TargetKind: "agent", TargetID: w.agent.ID.String(),
			After: map[string]any{"sandbox": names.Sandbox, "reason": w.sb.UnhealthyReason.String, "home_generation": sb.ID},
		}); err != nil {
			return err
		}
		sb.Phase = execution.PhaseDeleting
		return r.record(ctx, w, sb, hash, "recreating a stuck sandbox: "+w.sb.UnhealthyReason.String)
	}
	// A driver that can revert keeps the home of a crashed sandbox, or finds a silent front (issue #1481).
	if done, err := r.sealRestored(ctx, w, ws, names.Sandbox, sb, hash, act, tune); done || err != nil {
		return err
	}
	if handled, err := r.revertCrashed(ctx, w, ws, names.Sandbox, sb, hash); handled || err != nil {
		return err
	}
	if forced, err := r.checkFront(ctx, w, ws, names.Sandbox, sb, hash); forced || err != nil {
		return err
	}
	if (sb.Phase == execution.PhaseError || sb.Phase == execution.PhaseCompleted) && !act.HasRunning && !w.agent.UnhealthyAt.Valid {
		// The sandbox's main process exited or it failed. A running run has lost it anyway and is
		// left to fail; a starting run has not started the harness and waits for the new sandbox
		// (agentd waits on Error and Deleting). With no running run, delete it so the next pass
		// recreates it, as for a stuck one; parked runs resume in the new sandbox (ADR 0051).
		return r.repair(ctx, w, ws, names.Sandbox, sb, hash, homeDirtiedSince(w.sb))
	}
	if r.specChanged(ws, spec, sb, hash) || r.unnamedActor(w) {
		// Image, providers or filesystem rules changed (or the sandbox was replaced by
		// hand, or it is a Substrate actor made before actors had their own names): OpenShell
		// and Substrate cannot change those in place, so the sandbox is recreated, which
		// also replaces its home volume. Never while a run holds the agent, parked runs
		// included: a parked run resumes its harness session, which lives in that volume.
		// (agentd starts a new session when the volume was replaced anyway, see
		// agent_sandboxes.home_generation.)
		if act.HasHeld {
			return r.record(ctx, w, sb, hash, "sandbox spec changed; recreating it after the active and parked runs finish")
		}
		w.wantsRecreate = true
		if !r.takeRecreate(agentKey{w.org.ID, w.agent.ID}) {
			// The cap keeps an OpenShell upgrade from recreating the whole fleet at once. It is taken
			// before the home gate, which requests checkpoints and wakes stopped sandboxes: a sandbox
			// whose gate is still waiting keeps its slot, so at most the cap are in the gate at a time.
			return r.record(ctx, w, sb, hash, "sandbox spec changed; waiting for a free recreate slot")
		}
		// A driver that can repoint keeps the home: no delete, no restore (issue #1482). A refusal falls through.
		if done, err := r.repoint(ctx, w, ws, spec, sb, hash); done || err != nil {
			return err
		}
		if ok, note, err := r.homeGate(ctx, w, ws, sb, homeDirtiedSince(w.sb)); err != nil || !ok {
			if err != nil {
				return err
			}
			return r.record(ctx, w, sb, hash, note)
		}
		if err := r.deleteSandbox(ctx, w, ws, names.Sandbox, "the spec changed"); err != nil {
			return err
		}
		r.recreateStarted(agentKey{w.org.ID, w.agent.ID})
		r.log.Info("deleting sandbox to recreate it with a changed spec", w.logAttrs()...)
		sb.Phase = execution.PhaseDeleting
		return r.record(ctx, w, sb, hash, "recreating with a changed spec")
	}

	switch sb.Phase {
	case execution.PhaseReady:
		if _, err := r.backend.EnsurePolicy(ctx, ws, names.Sandbox, spec.Policy); err != nil {
			return err
		}
		if !act.HasActive && !act.HasDemand && !w.keepWarm && r.idle(w.sb, act, tune) {
			// The home is backed up before the stop (practice B3).
			if ok, err := r.stopGate(ctx, w, sb); err != nil {
				return err
			} else if !ok {
				return r.record(ctx, w, sb, hash, "waiting for the home backup before stopping")
			}
			if sb, err = r.stopSnapshot(ctx, w, ws, names.Sandbox); err != nil {
				return err
			}
			r.log.Info("stopped idle sandbox", w.logAttrs()...)
		}
	case execution.PhaseStopped, execution.PhaseSuspended:
		if act.HasActive || act.HasDemand || w.keepWarm || r.woken(w.sb, tune) {
			release, held, err := r.holdForCreationRate(ctx, w, tune, orgsettings.CreationStart, &sb, hash)
			if err != nil || held {
				return err
			}
			if err := r.snapshotClock(ctx, w); err != nil { // a first boot gets its seal
				release()
				return err
			}
			if sb, err = r.backend.StartSandbox(ctx, ws, names.Sandbox); err != nil {
				if definiteFailure(err) {
					release()
				}
				return err
			}
			r.log.Info("started sandbox for a run", w.logAttrs()...)
		} else if done, err := r.archive(ctx, w, ws, sb, act, hash); err != nil || done {
			return err
		}
	}
	return r.record(ctx, w, sb, hash, "")
}

// stopSandbox stops a sandbox and returns the phase the backend reports afterwards. The reply of
// the stop call is not the last word: on Agent Substrate any request through the router resumes a
// suspended actor, and one that was already on the wire when the stop began can land after it (ADR
// 0119, "Router resume": SuspendSandbox guarantees the actor was suspended when it returned, and the
// caller re-observes the phase). So the phase is read again, and a sandbox that was woken is recorded
// as the running one it is. The next pass sees a running sandbox with nothing to do and stops it
// again: one stop per pass, never a loop inside one.
// A failed re-read is not an error: the stop worked and the next pass observes.
func (r *Reconciler) stopSandbox(ctx context.Context, ws, name string) (execution.Sandbox, error) {
	sb, err := r.backend.StopSandbox(ctx, ws, name)
	if err != nil || !sb.Phase.IsStopped() {
		return sb, err
	}
	seen, err := r.backend.GetSandbox(ctx, ws, name)
	if err != nil || seen.Phase.IsStopped() {
		return sb, nil
	}
	r.log.Info("a stopped sandbox is running again (a late request woke it); the next pass stops it", "workspace", ws, "sandbox", name, "phase", string(seen.Phase))
	return seen, nil
}

// snapshotClock reads the database clock into w.snapFrom just before a backend call that can take a
// snapshot (a stop, or the first-boot seal of a create or a start). The snapshot holds every write before
// that time, so it is a lower bound of the snapshot time, on the clock of restored_at and of the
// checkpoint times (issue #1481). The driver's own clock is never compared with them.
func (r *Reconciler) snapshotClock(ctx context.Context, w *work) error {
	at, err := r.q.DatabaseClock(ctx)
	if err != nil {
		return fmt.Errorf("read the database clock before the snapshot: %w", err)
	}
	w.snapFrom = at
	return nil
}

// stopSnapshot is stopSandbox with the snapshot time read first.
func (r *Reconciler) stopSnapshot(ctx context.Context, w *work, ws, name string) (execution.Sandbox, error) {
	if err := r.snapshotClock(ctx, w); err != nil {
		return execution.Sandbox{}, err
	}
	return r.stopSandbox(ctx, ws, name)
}

// holdForCreationRate takes one of the org's sandbox creations per minute (issue #27, ADR 0066
// amendment 2026-11-12). With none free it records why and reports held: the run stays queued or
// starting, the sandbox is not touched, and the next pass tries again. A burst larger than the rate
// therefore queues behind it and no run fails for it. sb is the stopped sandbox about to start, or
// nil when none exists yet.
//
// release gives the slot back; call it when the backend call that followed failed.
func (r *Reconciler) holdForCreationRate(ctx context.Context, w *work, tune orgsettings.Settings, kind string, sb *execution.Sandbox, hash string) (release func(), held bool, err error) {
	res, ok, wait, err := orgsettings.ReserveSandboxCreation(ctx, r.db, w.org.ID, w.agent.ID, kind, tune.SandboxCreationsPerMinute)
	if err != nil {
		return nil, false, fmt.Errorf("reserve a sandbox creation: %w", err)
	}
	if ok {
		return func() {
			if err := res.Release(context.WithoutCancel(ctx), r.db); err != nil {
				r.log.Warn("release a sandbox creation slot", append(w.logAttrs(), "error", err)...)
			}
		}, false, nil
	}
	r.m.SandboxCreationsHeld.Inc()
	// The message is stable between passes (the wait is only in the log), so a held agent's message
	// does not churn.
	msg := fmt.Sprintf("waiting for the org's sandbox creation rate (%d per minute)", tune.SandboxCreationsPerMinute)
	r.log.Info("sandbox creation held by the org's rate limit", append(w.logAttrs(), "wait", wait.String())...)
	if sb != nil {
		return nil, true, r.record(ctx, w, *sb, hash, msg)
	}
	return nil, true, r.recordState(ctx, w, store.SandboxStatePending, creationHeldPhase, msg)
}

// definiteFailure reports whether a backend call certainly did not make the sandbox, so its slot in
// the org's creation rate can be refunded. A timeout, a cancellation or an unavailable or unknown
// outcome may have created it; the slot stays spent then, which errs toward fewer creations.
func definiteFailure(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Aborted, codes.Internal, codes.DataLoss:
		return false
	}
	return true
}

// creationHeldPhase is the phase recorded while a sandbox waits for the org's creation rate.
const creationHeldPhase = "WaitingForCreationRate"

// deleteSandbox requests the deletion of a sandbox. The OpenShell DeleteSandbox RPC can still block
// until the pods end; the per-agent queue makes that cost one agent and one worker slot, not the
// pass. The next pass observes the end (GetSandbox reports PhaseDeleting until it is gone). It logs before the call and
// after it, with the call's duration and OpenShell's outcome, so a slow backend call shows in the log
// with the agent it held (issue 573).
func (r *Reconciler) deleteSandbox(ctx context.Context, w *work, ws, name, why string) error {
	r.log.Info("requesting the deletion of a sandbox", append(w.logAttrs(), "name", name, "reason", why)...)
	start := time.Now()
	outcome, err := r.backend.DeleteSandbox(ctx, ws, name)
	r.log.Info("sandbox deletion requested", append(w.logAttrs(), "name", name, "outcome", string(outcome),
		"took", time.Since(start).Round(time.Millisecond), "err", err)...)
	return err
}

// specChanged says whether the sandbox must be recreated because it was made from another spec.
// A backend with labels compares the spec hash label, the supervisor image and the install mark. A
// backend without labels (Substrate) gives the sandbox the identity it derived from the spec
// (execution.SpecIdentifier), which the backend keeps as long as the sandbox; a failure to derive
// it leaves the sandbox as it is, the spec error shows when the sandbox is created.
func (r *Reconciler) specChanged(ws string, spec execution.SandboxSpec, sb execution.Sandbox, hash string) bool {
	if execution.IdentifiesSpecs(r.backend) {
		want, err := r.backend.(execution.SpecIdentifier).SpecID(ws, spec)
		return err == nil && want != sb.SpecID
	}
	return sb.Labels[LabelSpecHash] != hash || r.supervisorChanged(sb) || r.unmarked(sb)
}

// unmarked reports a sandbox without the install label while the backend owns what it makes: it
// was made before the label existed (issue 461). Labels are immutable, so the sandbox is recreated
// with the label, like any spec change. A sandbox labeled for another install never gets here:
// GetSandbox refuses it.
func (r *Reconciler) unmarked(sb execution.Sandbox) bool {
	return r.owns() && sb.Labels[execution.LabelInstall] == ""
}

// unnamedActor reports a sandbox that runs as an actor under its sandbox name, on a backend that names
// actors: it was made before issue #1386 gave each actor its own name, so it has no recorded UID and is
// never in the gateway State, and every token for it is refused. It is recreated like a spec change
// (issue 461 does the same for an unmarked sandbox), and the create after it names a new actor.
func (r *Reconciler) unnamedActor(w *work) bool {
	_, ok := execution.ActorNamerOf(r.backend)
	return ok && !w.sb.ActorName.Valid
}

// owns reports whether the backend owns what it makes (an install mark is set).
func (r *Reconciler) owns() bool {
	o, ok := r.backend.(interface{ Owner() string })
	return ok && o.Owner() != ""
}

// maxRepairs is the loop guard of ADR 0054: recreations per agent per 24 hours, stuck and failed
// sandboxes together (agentd's MaxRecreations; both use CountRecentSandboxRecreations).
const maxRepairs = 3

// repair recreates a sandbox in Error or Completed. After maxRepairs recreations in 24 hours it
// marks the agent unhealthy instead, so a sandbox that keeps failing is not recreated in a loop.
func (r *Reconciler) repair(ctx context.Context, w *work, ws, name string, sb execution.Sandbox, hash string, since time.Time) error {
	detail := "phase " + string(sb.Phase)
	if m := truncate(sb.Message, 300); m != "" {
		detail += ": " + m
	}
	n, err := r.q.CountRecentSandboxRecreations(ctx, store.CountRecentSandboxRecreationsParams{OrgID: w.org.ID, AgentID: w.agent.ID})
	if err != nil {
		return fmt.Errorf("count recent recreations: %w", err)
	}
	if n >= maxRepairs {
		reason := fmt.Sprintf("the sandbox kept failing: %d recreations in 24 hours; the last failure: %s", n, detail)
		if _, err := r.q.MarkAgentUnhealthy(ctx, store.MarkAgentUnhealthyParams{OrgID: w.org.ID, ID: w.agent.ID, Reason: reason}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return r.record(ctx, w, sb, hash, "")
			}
			return err
		}
		r.m.AgentsUnhealthy.Inc()
		r.log.Error("the sandbox keeps failing; the agent is marked unhealthy", append(w.logAttrs(), "recreations_in_24h", n, "last", detail)...)
		if err := audit.RecordAfter(ctx, r.db, audit.Event{
			Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-repair"), Action: "agent.mark_unhealthy", TargetKind: "agent", TargetID: w.agent.ID.String(),
			After: map[string]any{"sandbox": name, "reason": reason, "recreations_in_24h": n},
		}); err != nil {
			return err
		}
		return r.record(ctx, w, sb, hash, "")
	}
	if ok, note, err := r.homeGate(ctx, w, ws, sb, since); err != nil || !ok {
		if err != nil {
			return err
		}
		return r.record(ctx, w, sb, hash, note)
	}
	if err := r.deleteSandbox(ctx, w, ws, name, "the sandbox failed"); err != nil {
		return err
	}
	r.m.SandboxRepairs.Inc()
	r.log.Warn("deleting a failed sandbox to recreate it", append(w.logAttrs(), "detail", detail)...)
	if err := audit.RecordAfter(ctx, r.db, audit.Event{
		Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-repair"), Action: "sandbox.recreate", TargetKind: "agent", TargetID: w.agent.ID.String(),
		After: map[string]any{"sandbox": name, "reason": "sandbox " + detail, "phase": string(sb.Phase), "message": truncate(sb.Message, 300), "home_generation": sb.ID},
	}); err != nil {
		return err
	}
	sb.Phase = execution.PhaseDeleting
	return r.record(ctx, w, sb, hash, "recreating a failed sandbox ("+detail+")")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// activitySince is the latest activity of the agent's sandbox, the clock the idle stop and the
// archive count from. Activity is the latest of the sandbox's last start, the last run that
// finished or parked, and the last pre-wake signal (package wake). A run that is active or
// claimable keeps the sandbox up regardless (the caller checks HasActive and HasDemand). It is an
// idle clock, not "the home was written": the checkpoint gates use homeDirtiedSince.
func activitySince(sb store.AgentSandbox, act store.GetAgentRunActivityRow) time.Time {
	var since time.Time
	for _, t := range []pgtype.Timestamptz{sb.StartedAt, act.LastActivity, sb.WokenAt} {
		if t.Valid && t.Time.After(since) {
			since = t.Time
		}
	}
	return since
}

// idle reports whether the sandbox has had no activity for the org's idle timeout. A sandbox
// whose run parked on a2a or input_required (a person or another agent will answer soon) stays
// up for the longer park window counted from that park (plan item LT1, ADR 0066): the later of
// the two deadlines applies. Approval and budget parks use the idle timeout.
func (r *Reconciler) idle(sb store.AgentSandbox, act store.GetAgentRunActivityRow, s orgsettings.Settings) bool {
	since := activitySince(sb, act)
	if since.IsZero() {
		// Ready but never recorded as ready: start counting now.
		return false
	}
	deadline := since.Add(s.IdleTimeout)
	if act.WarmSince.Valid {
		if warm := act.WarmSince.Time.Add(s.ParkWarm); warm.After(deadline) {
			deadline = warm
		}
	}
	return !r.cfg.Now().Before(deadline)
}

// woken reports whether a pre-wake signal came within the idle timeout, so a stopped sandbox
// should start before any run exists. The window equals the idle timeout: a sandbox started for
// a signal that no run follows stops again once it has passed.
func (r *Reconciler) woken(sb store.AgentSandbox, s orgsettings.Settings) bool {
	return sb.WokenAt.Valid && r.cfg.Now().Sub(sb.WokenAt.Time) < s.IdleTimeout
}

// reconcileDraft stops the sandbox of an agent returned to draft and keeps everything else
// for its re-activation. Runs cannot be queued for a draft agent. With no sandbox it drops the
// agent's GitHub provider and profile (dropDraftGitHub).
func (r *Reconciler) reconcileDraft(ctx context.Context, w *work) error {
	sb, err := r.backend.GetSandbox(ctx, w.sb.Workspace, w.sb.ActorRef())
	if errors.Is(err, execution.ErrNotFound) {
		if err := r.dropDraftGitHub(ctx, w); err != nil {
			return err
		}
		return r.recordState(ctx, w, store.SandboxStatePending, "", "")
	}
	if err != nil {
		return err
	}
	if sb.Phase == execution.PhaseReady {
		if sb, err = r.stopSnapshot(ctx, w, w.sb.Workspace, w.sb.ActorRef()); err != nil {
			return err
		}
		r.log.Info("stopped sandbox of a draft agent", w.logAttrs()...)
	}
	return r.record(ctx, w, sb, w.sb.SpecHash, "")
}

// dropDraftGitHub deletes the GitHub provider and profile of a draft agent with no sandbox: the agent
// has no use for them until it is active, and reconcileActive makes them again before it creates a
// sandbox. Once per provider (draftGitHubDropped), so a draft agent does not cost gateway calls every pass.
func (r *Reconciler) dropDraftGitHub(ctx context.Context, w *work) error {
	ws, names := w.sb.Workspace, w.names()
	r.mu.Lock()
	done := r.draftGitHubDropped[names.GitHubProvider]
	r.mu.Unlock()
	if done {
		return nil
	}
	if err := r.deleteGitHub(ctx, ws, names); err != nil {
		return err
	}
	r.mu.Lock()
	r.draftGitHubDropped[names.GitHubProvider] = true
	r.mu.Unlock()
	return nil
}

// reconcileArchived deletes the agent's sandbox (or, with RetentionRetain, stops it), then its
// provider record and key Secret. The key itself was already revoked: the gateway state of
// this pass left it out.
func (r *Reconciler) reconcileArchived(ctx context.Context, w *work) error {
	ws, name := w.sb.Workspace, w.sb.ActorRef()
	final := store.SandboxStateDeleted
	sb, err := r.backend.GetSandbox(ctx, ws, name)
	switch {
	case errors.Is(err, execution.ErrNotFound):
	case err != nil:
		return err
	case r.cfg.HomeRetention == RetentionRetain:
		if sb.Phase == execution.PhaseReady {
			if sb, err = r.stopSnapshot(ctx, w, ws, name); err != nil {
				return err
			}
		}
		if !sb.Phase.IsStopped() {
			return r.record(ctx, w, sb, w.sb.SpecHash, "archived; waiting for the sandbox to stop")
		}
		final = store.SandboxStateRetained
	default:
		if sb.Phase != execution.PhaseDeleting {
			if err := r.deleteSandbox(ctx, w, ws, name, "the agent is archived"); err != nil {
				return err
			}
			r.log.Info("deleting sandbox of an archived agent", w.logAttrs()...)
		}
		sb, err = r.backend.GetSandbox(ctx, ws, name)
		if err == nil {
			return r.recordState(ctx, w, store.SandboxStateDeleting, string(sb.Phase), "archived; waiting for the sandbox to be deleted")
		}
		if !errors.Is(err, execution.ErrNotFound) {
			return err
		}
	}

	if err := r.backend.DeleteProvider(ctx, ws, w.sb.ProviderName); err != nil {
		return err
	}
	if err := r.deleteGitHub(ctx, ws, w.names()); err != nil {
		return err
	}
	if err := r.secrets.Delete(ctx, w.sb.KeySecretName); err != nil {
		return err
	}
	if _, err := r.q.ClearAgentSandboxKey(ctx, store.ClearAgentSandboxKeyParams{OrgID: w.org.ID, AgentID: w.agent.ID}); err != nil {
		return fmt.Errorf("clear gateway key: %w", err)
	}
	r.log.Info("cleaned up archived agent", append(w.logAttrs(), "home", string(r.cfg.HomeRetention))...)
	phase := ""
	if final == store.SandboxStateRetained {
		phase = string(execution.PhaseStopped)
	}
	return r.recordState(ctx, w, final, phase, "")
}

// recordKeepWarmGrants writes the pass's keep-warm decision to agents.keep_warm_granted, so the API and
// the agent page can say whether the operator's cap granted an admin's request (#100). Only the agents
// whose recorded decision differs are written.
func (r *Reconciler) recordKeepWarmGrants(ctx context.Context, org pgtype.UUID, agents []store.Agent, granted map[pgtype.UUID]bool) error {
	var grant, revoke []pgtype.UUID
	for _, a := range agents {
		g, decided := granted[a.ID]
		switch {
		case !decided:
			// No decision in this pass: the stored value stays.
		case g && !a.KeepWarmGranted:
			grant = append(grant, a.ID)
		case !g && a.KeepWarmGranted:
			revoke = append(revoke, a.ID)
		}
	}
	for _, c := range []struct {
		ids     []pgtype.UUID
		granted bool
	}{{grant, true}, {revoke, false}} {
		if len(c.ids) == 0 {
			continue
		}
		if err := r.q.SetAgentsKeepWarmGranted(ctx, store.SetAgentsKeepWarmGrantedParams{OrgID: org, Ids: c.ids, Granted: c.granted}); err != nil {
			return fmt.Errorf("reconciler: record keep-warm grants of org %s: %w", org.String(), err)
		}
	}
	return nil
}

// record stores the observed sandbox. Its OpenShell ID is the home generation: OpenShell
// deletes the home volume with the sandbox, so a new sandbox (a new ID) means a new, empty home,
// while stop and start keep both.
func (r *Reconciler) record(ctx context.Context, w *work, sb execution.Sandbox, hash, note string) error {
	msg, reason := sb.Message, sb.Reason
	if note != "" {
		msg = note
	} else if r.cfg.Observer != nil && sb.Phase == execution.PhaseProvisioning && execution.OpaqueWaitReason(sb.Reason) && sb.Workspace != "" {
		// OpenShell hides the pod's pull and crash errors behind ConfigurationPending (ADR 0078): read the
		// pod, only for such a reason, and keep OpenShell's words when the pod says nothing.
		reason, msg = execution.ExplainWait(sb.Reason, sb.Message, r.cfg.Observer.Explain(ctx, sb.Workspace, sb.Name, sb.ID))
	}
	// A snapshot that the call took gets the database clock read before the call. A snapshot without
	// that read (snapFrom not set) gets no time: the recorded one stays, which is still a lower bound.
	var snapshotAt pgtype.Timestamptz
	if sb.NewSnapshot {
		snapshotAt = w.snapFrom
	}
	if err := r.recordRow(ctx, w, sandboxState(sb.Phase), string(sb.Phase), msg, reason, hash, sb.ID, snapshotAt); err != nil {
		return err
	}
	if err := r.recordActor(ctx, w, sb); err != nil {
		return err
	}
	return r.recordEnforcement(ctx, w, sb.Enforcement)
}

// recordEnforcement stores the enforcement mode the observed sandbox reports (ADR 0045), the
// field a run's evidence shows. A backend that reports none leaves the recorded value.
func (r *Reconciler) recordEnforcement(ctx context.Context, w *work, mode string) error {
	if mode == "" || w.sb.Enforcement == mode {
		return nil
	}
	if err := r.q.SetAgentSandboxEnforcement(ctx, store.SetAgentSandboxEnforcementParams{OrgID: w.org.ID, AgentID: w.agent.ID, Enforcement: mode}); err != nil {
		return fmt.Errorf("record sandbox enforcement: %w", err)
	}
	w.sb.Enforcement = mode
	return nil
}

// recordState stores a state without an observed sandbox; the recorded home generation is kept.
func (r *Reconciler) recordState(ctx context.Context, w *work, state store.SandboxState, phase, msg string, hash ...string) error {
	h := w.sb.SpecHash
	if len(hash) > 0 {
		h = hash[0]
	}
	return r.recordRow(ctx, w, state, phase, msg, "", h, "", pgtype.Timestamptz{})
}

// recordRow stores the observed sandbox. snapshotAt is the database clock before the call that took the
// newest snapshot, or NULL when this observation took none (the recorded time then stays).
func (r *Reconciler) recordRow(ctx context.Context, w *work, state store.SandboxState, phase, msg, reason, hash, generation string, snapshotAt pgtype.Timestamptz) error {
	sb, err := r.q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		State: state, Phase: phase, Message: msg, Reason: reason, SpecHash: hash, HomeGeneration: generation,
		SnapshotAt: snapshotAt,
	})
	if err != nil {
		return fmt.Errorf("record sandbox state: %w", err)
	}
	w.sb = sb
	return nil
}

// ForeignReason is the reason recorded on an agent whose sandbox, workspace or provider another install
// owns (issue 461).
const ForeignReason = "OwnedByAnotherInstall"

// recordForeign moves the row out of ready and into error when an object of the agent belongs to
// another install. A database restored next to the live install still holds the live install's
// "ready" rows; koinon-agentd trusts a ready row, so without this it would run in the other
// install's sandbox. In error, agentd fails the run with sandbox-unavailable and does not exec.
// This row is a courtesy to the operator and to agentd's early failure; the real guard is
// guardSandbox in the OpenShell backend, which refuses every call on a sandbox another install owns.
// So a failed write is logged and the pass goes on.
func (r *Reconciler) recordForeign(ctx context.Context, w *work, err error) {
	if rerr := r.recordRow(ctx, w, store.SandboxStateError, "Error", err.Error(), ForeignReason, w.sb.SpecHash, "", pgtype.Timestamptz{}); rerr != nil {
		r.log.Warn("recording the foreign-owned sandbox failed; guardSandbox still refuses it",
			append(w.logAttrs(), "err", rerr)...)
	}
}

// recordMessage stores a reconcile error on the row for operators, keeping the state.
func (r *Reconciler) recordMessage(ctx context.Context, w *work, err error) {
	if w.sb.SandboxName == "" {
		return
	}
	// The message is for operators. Only the message column changes: the state, phase and reason
	// stay as the agent's own work item recorded them, even when this call comes from a pass that
	// ran while that item was still working (issue 573).
	_, _ = r.q.SetAgentSandboxMessage(ctx, store.SetAgentSandboxMessageParams{
		OrgID: w.org.ID, AgentID: w.agent.ID, Message: err.Error(),
	})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
