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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
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
	// DefaultKeepWarmPerOrg is the default cap of keep-warm sandboxes per org (old issue 315).
	DefaultKeepWarmPerOrg = 2
	DefaultResyncInterval = 30 * time.Second
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

	// Gateway is agentgateway's LLM listener as sandboxes reach it.
	Gateway Endpoint
	// Keys opens the provider keys stored in Postgres (the secret store).
	Keys catalog.Opener
	// ProviderKeys receives the opened keys for the gateway. Required when any served
	// provider has a key.
	ProviderKeys ProviderKeyStore
	// ProviderKeyDir is where the gateway mounts ProviderKeys, for the rendered config.
	ProviderKeyDir string
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
	// Default 2; a negative value grants none. Over the cap the oldest agents win.
	KeepWarmPerOrg int
	// ResyncInterval is the time between passes without a wake-up. Default 30 seconds.
	ResyncInterval time.Duration
	// Checkpoints turns on home checkpoints (checkpoint.go): a sandbox is stopped, recreated or
	// archived only after its home was backed up, and a recreated sandbox keeps its home
	// generation and gets the home restored. koinon-agentd writes the checkpoints.
	Checkpoints bool
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
	cfg Config
	log *slog.Logger
	// lastUnmarked is when the loop last counted unmarked objects; only the leading loop touches it.
	lastUnmarked time.Time
	pool         *pgxpool.Pool
	q            *store.Queries
	backend      execution.Backend
	gw           gateway.ModelGateway
	secrets      SecretStore
	// states stores the revisions of the gateway State (the State stream, ADR 0097).
	states staterev.Publisher

	mu sync.Mutex
	// wsMu guards the per-pass workspace map, which per-sandbox jobs read concurrently.
	wsMu sync.Mutex
	// githubRevs is the token revision last pushed per GitHub provider record, to log refreshes.
	githubRevs map[string]string
	// draftGitHubDropped holds the GitHub providers claimDraft has dropped (under mu), so a draft agent
	// does not cost four gateway calls every pass; reconcileActive clears the entry when it makes the
	// record again.
	draftGitHubDropped map[string]bool
	// harnessProfileSettled holds the harness profiles, by workspace and profile ID (harnessProfileKey),
	// that claimHarnessProfiles found marked or missing (under mu), so an org whose harnesses no active
	// agent keeps current does not cost a gateway call per harness every pass. reconcileActive clears
	// the entry when it imports the profile again; a new workspace is a new key.
	harnessProfileSettled map[string]bool
	// clampMu guards clampLogged: the clamps last logged per agent, so a pass that changes nothing
	// logs nothing.
	clampMu     sync.Mutex
	clampLogged map[string]string
	// plan is the gateway side of a pass and its memory (internal/reconciler/gatewayplan).
	plan *gatewayplan.Planner
	mcp  *mcpservers.Service
	m    *Metrics

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
	probeMu   sync.Mutex
	probeDue  []gatewayplan.Probe
	probeWake chan struct{}
}

// New returns a Reconciler.
func New(pool *pgxpool.Pool, backend execution.Backend, gw gateway.ModelGateway, secrets SecretStore, cfg Config) (*Reconciler, error) {
	if cfg.Gateway.Host == "" || cfg.Gateway.Port == 0 {
		return nil, errors.New("reconciler: the agentgateway endpoint is required")
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
	if cfg.KeepWarmPerOrg == 0 {
		cfg.KeepWarmPerOrg = DefaultKeepWarmPerOrg
	}
	if cfg.MCPProbeInterval == 0 {
		cfg.MCPProbeInterval = DefaultMCPProbeInterval
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
	return &Reconciler{
		cfg: cfg, log: cfg.Logger, pool: pool, q: store.New(pool), states: staterev.Publisher{DB: stateDB(cfg, pool)},
		backend: backend, gw: gw, secrets: secrets, githubRevs: map[string]string{}, draftGitHubDropped: map[string]bool{},
		harnessProfileSettled: map[string]bool{}, mcp: mcpservers.New(pool, nil, cfg.Logger), m: cfg.Metrics,
		plan: gatewayplan.New(gatewayplan.Config{
			KeyDir: cfg.ProviderKeyDir, KeySettle: cfg.ProviderKeySettle, ToolServers: cfg.ToolServers,
			MCPAskURL: cfg.MCPAskURL, Logger: cfg.Logger, Wake: wake,
		}),
	}, nil
}

// stateDB is the database the State stream's revisions go to: the pool, unless the config names one.
func stateDB(cfg Config, pool *pgxpool.Pool) staterev.DB {
	if cfg.StateRevisionDB != nil {
		return cfg.StateRevisionDB
	}
	return pool
}

// statePublishTimeout bounds publishState.
const statePublishTimeout = 5 * time.Second

// publishState stores the state of this pass as a revision of the State stream when it differs from
// the latest one (ADR 0097 decision 2.1), after the Apply, so the stored revision records what was
// applied. Nothing reads the stream yet, so a failure is logged and
// counted and never holds the pass back: the Apply below is what serves the state. The state is the
// one a driver serves, without the keys of agents whose limit is invalid (WithoutInvalidLimits), so a
// gateway that follows the stream serves what the agentgateway drivers serve.
func (r *Reconciler) publishState(ctx context.Context, s gateway.State) {
	// Its own short deadline: a slow database must not hold the pass.
	ctx, cancel := context.WithTimeout(ctx, statePublishTimeout)
	defer cancel()
	served, _ := s.WithoutInvalidLimits()
	p, err := r.states.Publish(ctx, served)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindStateRevision).Inc()
		r.log.Warn("storing the gateway state revision failed; the gateway is applied as before", "err", err)
		return
	}
	if p.Changed {
		r.m.StateRevisions.Inc()
		r.log.Debug("gateway state revision stored", "revision", p.Revision)
	}
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
	// keepWarm is set when the agent asks to stay warm and the org's cap grants it (old issue 315).
	keepWarm bool

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
	n.Sandbox, n.Provider, n.KeySecret = w.sb.SandboxName, w.sb.ProviderName, w.sb.KeySecretName
	return n
}

func (w *work) logAttrs() []any {
	return []any{"org", w.org.Slug, "agent", execution.UUIDString(w.agent.ID.Bytes), "sandbox", w.sb.SandboxName}
}

// ReconcileAll runs one pass over every org and agent. It returns the errors of the agents it
// could not converge; the others are converged regardless.
//
// It also counts the unmarked OpenShell objects (see observeUnmarked). The loop does not call it: it
// runs [Reconciler.reconcilePass] and counts at most once per resync interval, with its own timeout.
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	q := r.newWorkQueue(ctx)
	defer q.Stop()
	b, err := r.reconcilePass(ctx, q)
	// The loop leaves the sandbox work to the queue; a caller of ReconcileAll wants it done.
	err = errors.Join(append([]error{err}, b.wait()...)...)
	r.observeSandboxes(ctx)
	r.observeUnmarked(ctx)
	return err
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

// observeUnmarkedDue counts the unmarked objects when a resync interval has passed since the last
// count, so a burst of NOTIFY wake-ups does not list the gateway every time. The count has its own
// timeout (unmarkedTimeout) and never runs inside the pass's. The gauge keeps its values between counts.
func (r *Reconciler) observeUnmarkedDue(ctx context.Context) {
	if !r.lastUnmarked.IsZero() && time.Since(r.lastUnmarked) < r.cfg.ResyncInterval {
		return
	}
	r.lastUnmarked = time.Now()
	uctx, cancel := context.WithTimeout(ctx, unmarkedTimeout)
	defer cancel()
	r.observeUnmarked(uctx)
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

// observeUnmarked sets koinon_openshell_unmarked_objects and
// koinon_openshell_unmarked_objects_waiting_for_agent from one classified unmarked list (the one
// koinon-reconciler unmarked list prints: the gateway's sandboxes, providers and profiles, whatever
// this pass did or failed to do, issue 461, with each object's reason from the database, issue 511).
// waits-for-agent objects go to the second gauge, every other reason to the first, so the drill gate
// ("0 and stale 0") means no object without a known owner is left. A list that fails, on the gateway
// or in the database, keeps the previous values and sets koinon_openshell_unmarked_objects_stale to
// 1; a complete one sets it to 0. Without an install mark ownership is off, no object can be claimed
// and nothing is safe: the kinds read 0 and stale reads 1, so the drill condition is not met.
func (r *Reconciler) observeUnmarked(ctx context.Context) {
	_, ok := r.backend.(unmarkedLister)
	if !ok || !r.owns() {
		for _, k := range unmarkedKinds {
			r.m.UnmarkedObjects.WithLabelValues(k).Set(0)
			r.m.UnmarkedWaiting.WithLabelValues(k).Set(0)
		}
		r.m.UnmarkedStale.Set(1)
		return
	}
	objs, err := ListUnmarked(ctx, r.q, r.backend, r.cfg.Harnesses)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("listing unmarked OpenShell objects failed; the gauges keep their last values", "err", err)
		}
		r.m.UnmarkedStale.Set(1)
		return
	}
	unowned, waiting := map[string]int{}, map[string]int{}
	for _, o := range objs {
		if o.Reason == ReasonWaitsForAgent {
			waiting[o.Kind]++
		} else {
			unowned[o.Kind]++
		}
	}
	for _, k := range unmarkedKinds {
		r.m.UnmarkedObjects.WithLabelValues(k).Set(float64(unowned[k]))
		r.m.UnmarkedWaiting.WithLabelValues(k).Set(float64(waiting[k]))
	}
	r.m.UnmarkedStale.Set(0)
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
		if err := r.claimHarnessProfiles(ctx, org, agents); err != nil {
			if errors.Is(err, errAbortPass) {
				r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
				return errors.Join(append(errs, err)...)
			}
			// A harness profile is part of what an agent's sandbox converges (reconcileActive and
			// claimDraft ensure it with the provider record), so its claim counts as a sandbox error.
			r.m.PassErrors.WithLabelValues(errKindSandbox).Inc()
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
	agents := make([]gatewayplan.Agent, len(items))
	for i, w := range items {
		agents[i] = gatewayplan.Agent{
			Org: gatewayplan.Org{ID: w.org.ID, Slug: w.org.Slug}, ID: w.agent.ID, Ref: agentRef(w.org, w.agent.ID),
			Active: w.agent.Status == store.AgentStatusActive, Key: w.key, Tools: w.tools,
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
		MCP: mcpServed.servers, MCPFiles: mcpServed.files, LedgerEnforcesBudgets: ledgerEnforces, Allowances: allowances,
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
		if n, err := ledger.StopSpent(ctx, r.pool, w.agent, now); err != nil {
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
	if len(draft.Files) > 0 {
		var err error
		if written, present, err = r.cfg.ProviderKeys.Put(ctx, draft.Files); err != nil {
			r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
			return errors.Join(append(errs, fmt.Errorf("reconciler: deliver provider keys: %w", err))...)
		}
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
	// Sandbox work is a per-agent work queue, outside the sequential pass (plan item LT1, practice B4,
	// issue 573). An agent whose key the gateway already serves does not depend on the Apply below,
	// which may wait for agentgateway's hot reload for about a minute: its sandbox work is queued now,
	// concurrently with it. An agent with a new key is queued after the Apply, as before. The pass
	// never waits for the queue: a slow call for one agent holds only that agent.
	r.submit(q, b, workOf(items, plan.Early), workspaces)

	var res gatewayplan.ApplyResult
	rejectedReason := ""
	applyStart := time.Now()
	_, applyErr := r.gw.Apply(ctx, plan.State)
	r.m.GatewayApply.Observe(time.Since(applyStart).Seconds())
	// Record what was applied. A gateway that took nothing (unreachable, whole config refused) has
	// nothing to record; a partial rejection still left the rest live.
	var applyRej *gateway.RejectedError
	if applyErr == nil || errors.As(applyErr, &applyRej) {
		r.publishState(ctx, plan.State)
	}
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
	r.probeSoon(outcome.Probe)
	if !res.Failed {
		if err := r.recordServed(ctx, outcome.Served); err != nil {
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			errs = append(errs, err)
		}
		if r.cfg.ProviderKeys != nil && len(provErrs) == 0 && len(mcpErrs) == 0 {
			if err := r.cfg.ProviderKeys.Prune(ctx, plan.Keep); err != nil {
				r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
				errs = append(errs, fmt.Errorf("reconciler: prune provider keys: %w", err))
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
	r.submit(q, b, workOf(items, outcome.Run), workspaces)
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

func (l planLookups) IssueKey(ctx context.Context, ref gateway.AgentRef) (gateway.IssuedKey, error) {
	return l.r.gw.IssueKey(ctx, ref)
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
		// Defense in depth: a missing NetworkPolicy is counted and logged there, never fatal here.
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
	ensureKey := func(claim bool) error {
		return r.backend.EnsureProvider(ctx, ws, execution.Provider{
			Name:               names.Provider,
			Profile:            r.profile(w.agent.Harness, harness),
			Credentials:        map[string]string{keyEnvVar: w.material},
			CredentialRevision: w.key.ID,
			Labels:             ownerLabels(w.org.ID, w.agent.ID),
			ClaimLegacy:        claim,
		})
	}
	if err := ensureKey(false); err != nil {
		return err
	}
	// The ensure imported the harness profile: once no active agent keeps it current, the org pass
	// looks at it again.
	r.mu.Lock()
	delete(r.harnessProfileSettled, harnessProfileKey(ws, r.profile(w.agent.Harness, harness).ID))
	r.mu.Unlock()

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
		if err := r.ensureGitHub(ctx, ws, w, names, harness, read, false); err != nil {
			return err
		}
		providers = append(providers, names.GitHubProvider)
		network[githubRule] = GitHubRule(read, r.githubBinaries(harness))
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
	tune, err := orgsettings.Get(ctx, r.q, w.org.ID, orgsettings.Settings{IdleTimeout: r.cfg.IdleTimeout, ParkWarm: r.cfg.ParkWarm,
		SandboxCreationsPerMinute: orgsettings.DefaultSandboxCreationsPerMinute})
	if err != nil {
		return fmt.Errorf("read org settings: %w", err)
	}

	sb, err := r.backend.GetSandbox(ctx, ws, names.Sandbox)
	if errors.Is(err, execution.ErrNotFound) {
		if r.owns() {
			// No sandbox attaches the providers now, so the ones made before the install label
			// existed can be claimed (deleted and made again with the label), issue 461. While the
			// sandbox lived they were taken as this install's, because OpenShell may refuse to delete
			// an attached provider and a delete must not strip a live sandbox of it.
			if err := ensureKey(true); err != nil {
				return err
			}
			if hasRead {
				if err := r.ensureGitHub(ctx, ws, w, names, harness, read, true); err != nil {
					return err
				}
			}
		}
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
		if held, err := r.holdForCreationRate(ctx, w, tune, orgsettings.CreationCreate, nil, hash); err != nil || held {
			return err
		}
		lrReady := r.cfg.LimitRange == nil || r.cfg.LimitRange.Ready(ctx, ws)
		sb, err = r.backend.CreateSandbox(ctx, ws, spec)
		if err != nil {
			return err
		}
		r.log.Info("created sandbox", w.logAttrs()...)
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
		if err := audit.RecordAfter(ctx, r.pool, audit.Event{
			Org: w.org.ID, Actor: audit.System("koinon-reconciler:sandbox-repair"), Action: "sandbox.recreate", TargetKind: "agent", TargetID: w.agent.ID.String(),
			After: map[string]any{"sandbox": names.Sandbox, "reason": w.sb.UnhealthyReason.String, "home_generation": sb.ID},
		}); err != nil {
			return err
		}
		sb.Phase = execution.PhaseDeleting
		return r.record(ctx, w, sb, hash, "recreating a stuck sandbox: "+w.sb.UnhealthyReason.String)
	}
	if (sb.Phase == execution.PhaseError || sb.Phase == execution.PhaseCompleted) && !act.HasRunning && !w.agent.UnhealthyAt.Valid {
		// The sandbox's main process exited or it failed. A running run has lost it anyway and is
		// left to fail; a starting run has not started the harness and waits for the new sandbox
		// (agentd waits on Error and Deleting). With no running run, delete it so the next pass
		// recreates it, as for a stuck one; parked runs resume in the new sandbox (ADR 0051).
		return r.repair(ctx, w, ws, names.Sandbox, sb, hash, homeDirtiedSince(w.sb))
	}
	if sb.Labels[LabelSpecHash] != hash || r.supervisorChanged(sb) || r.unmarked(sb) {
		// Image, providers or filesystem rules changed (or the sandbox was replaced by
		// hand): OpenShell cannot change those in place, so the sandbox is recreated, which
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
			if sb, err = r.backend.StopSandbox(ctx, ws, names.Sandbox); err != nil {
				return err
			}
			r.log.Info("stopped idle sandbox", w.logAttrs()...)
		}
	case execution.PhaseStopped:
		if act.HasActive || act.HasDemand || w.keepWarm || r.woken(w.sb, tune) {
			if held, err := r.holdForCreationRate(ctx, w, tune, orgsettings.CreationStart, &sb, hash); err != nil || held {
				return err
			}
			if sb, err = r.backend.StartSandbox(ctx, ws, names.Sandbox); err != nil {
				return err
			}
			r.log.Info("started sandbox for a run", w.logAttrs()...)
		} else if done, err := r.archive(ctx, w, ws, sb, act, hash); err != nil || done {
			return err
		}
	}
	return r.record(ctx, w, sb, hash, "")
}

// holdForCreationRate takes one of the org's sandbox creations per minute (issue #27, ADR 0066
// amendment 2026-11-12). With none free it records why and reports held: the run stays queued or
// starting, the sandbox is not touched, and the next pass tries again. A burst larger than the rate
// therefore queues behind it and no run fails for it. sb is the stopped sandbox about to start, or
// nil when none exists yet.
func (r *Reconciler) holdForCreationRate(ctx context.Context, w *work, tune orgsettings.Settings, kind string, sb *execution.Sandbox, hash string) (held bool, err error) {
	ok, wait, err := orgsettings.ReserveSandboxCreation(ctx, r.pool, w.org.ID, w.agent.ID, kind, tune.SandboxCreationsPerMinute)
	if err != nil {
		return false, fmt.Errorf("reserve a sandbox creation: %w", err)
	}
	if ok {
		return false, nil
	}
	r.m.SandboxCreationsHeld.Inc()
	msg := fmt.Sprintf("waiting for the org's sandbox creation rate (%d per minute); next slot in %s", tune.SandboxCreationsPerMinute, wait.Round(time.Second))
	r.log.Info("sandbox creation held by the org's rate limit", append(w.logAttrs(), "wait", wait.String())...)
	if sb != nil {
		return true, r.record(ctx, w, *sb, hash, msg)
	}
	return true, r.recordState(ctx, w, store.SandboxStatePending, creationHeldPhase, msg)
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

// unmarked reports a sandbox without the install label while the backend owns what it makes: it
// was made before the label existed (issue 461). Labels are immutable, so the sandbox is recreated
// with the label, like any spec change. A sandbox labeled for another install never gets here:
// GetSandbox refuses it.
func (r *Reconciler) unmarked(sb execution.Sandbox) bool {
	return r.owns() && sb.Labels[execution.LabelInstall] == ""
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
		if err := audit.RecordAfter(ctx, r.pool, audit.Event{
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
	if err := audit.RecordAfter(ctx, r.pool, audit.Event{
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
// for its re-activation. Runs cannot be queued for a draft agent. It also claims the agent's
// provider and harness profile for this install (claimDraft), which no other path does for a draft.
func (r *Reconciler) reconcileDraft(ctx context.Context, w *work) error {
	sb, err := r.backend.GetSandbox(ctx, w.sb.Workspace, w.sb.SandboxName)
	if errors.Is(err, execution.ErrNotFound) {
		if err := r.claimDraft(ctx, w, true); err != nil {
			return err
		}
		return r.recordState(ctx, w, store.SandboxStatePending, "", "")
	}
	if err != nil {
		return err
	}
	if sb.Phase == execution.PhaseReady {
		if sb, err = r.backend.StopSandbox(ctx, w.sb.Workspace, w.sb.SandboxName); err != nil {
			return err
		}
		r.log.Info("stopped sandbox of a draft agent", w.logAttrs()...)
	}
	if err := r.claimDraft(ctx, w, false); err != nil {
		return err
	}
	return r.record(ctx, w, sb, w.sb.SpecHash, "")
}

// claimDraft marks a draft agent's objects with the install mark (issue 511, ADR 0106 decision 5).
// Only reconcileActive claimed them before, so the objects of an agent that went back to draft stayed
// unmarked for good and the unmarked gauge never reached 0.
//
// The provider is ensured with the key the agent kept (a draft agent keeps its key for a quick
// re-activation; without a Secret that still matches it nothing is touched and no key is issued).
// The ensure imports the harness profile with the mark; without it the org pass marks the profile
// (claimHarnessProfiles), which needs no key. The provider record itself is recreated with
// the mark only when noSandbox is set: the stopped sandbox of a draft agent still attaches it, and
// OpenShell may refuse to delete an attached provider (see Provider.ClaimLegacy), so that record is
// claimed when the agent is active again and its sandbox is recreated. With no sandbox the GitHub
// provider and profile are dropped: the agent has no use for them until it is active, and
// reconcileActive makes them again, marked, before it creates a sandbox.
func (r *Reconciler) claimDraft(ctx context.Context, w *work, noSandbox bool) error {
	if !r.owns() || w.key.ID == "" {
		return nil
	}
	harness, ok := r.cfg.Harnesses[w.agent.Harness]
	if !ok || !harness.rendered() {
		return nil
	}
	_, material, ok, err := r.recordedMaterial(ctx, w)
	if err != nil {
		return err
	}
	if !ok {
		r.log.Debug("not claiming the objects of a draft agent: its key Secret does not match the recorded key", w.logAttrs()...)
		return nil
	}
	ws, names := w.sb.Workspace, w.names()
	if err := r.backend.EnsureProvider(ctx, ws, execution.Provider{
		Name:               names.Provider,
		Profile:            r.profile(w.agent.Harness, harness),
		Credentials:        map[string]string{keyEnvVar: material},
		CredentialRevision: w.key.ID,
		Labels:             ownerLabels(w.org.ID, w.agent.ID),
		ClaimLegacy:        noSandbox,
	}); err != nil {
		return err
	}
	if !noSandbox {
		return nil
	}
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
	ws, name := w.sb.Workspace, w.sb.SandboxName
	final := store.SandboxStateDeleted
	sb, err := r.backend.GetSandbox(ctx, ws, name)
	switch {
	case errors.Is(err, execution.ErrNotFound):
	case err != nil:
		return err
	case r.cfg.HomeRetention == RetentionRetain:
		if sb.Phase == execution.PhaseReady {
			if sb, err = r.backend.StopSandbox(ctx, ws, name); err != nil {
				return err
			}
		}
		if sb.Phase != execution.PhaseStopped {
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

// profileClaimer is implemented by a backend that marks an existing provider profile
// (the OpenShell backend with an owner).
type profileClaimer interface {
	ClaimProviderProfile(ctx context.Context, workspace string, p execution.ProviderProfile) (bool, error)
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

// claimHarnessProfiles marks, once per org and pass, the harness profiles (koinon-agentgateway-<harness>)
// that no active agent keeps current (issue 511). A profile belongs to the workspace and the harness, not
// to an agent: every agent of the harness in the org shares it, and OpenShell refuses to delete one a
// provider uses. An active agent re-imports it with the mark on every pass, so what is left are the
// harnesses whose agents are all draft or archived, or that have none. A draft agent does not count as
// keeping its harness: claimDraft imports the profile only when the agent's key Secret still matches
// the recorded key, and a draft agent with no key or no sandbox row is never reconciled at all. An
// archived agent leaves the reconcile list once its cleanup ends, so a claim made only while cleaning
// it up would miss every agent archived before the mark was on; the org pass sees them all. The mark
// is the whole claim: archiving keeps the org's data and must not remove what a sibling agent or an
// un-archive needs. A profile that does not exist is not created, and an org with no recorded
// workspace has none to claim in. Idempotent: a marked profile is left alone, and one found marked or
// missing is not asked for again (harnessProfileSettled) until an active agent imports it.
func (r *Reconciler) claimHarnessProfiles(ctx context.Context, org store.ListOrgsToReconcileRow, agents []store.Agent) error {
	c, ok := r.backend.(profileClaimer)
	if !ok || !r.owns() {
		return nil
	}
	kept := map[string]bool{}
	for _, a := range agents {
		if a.Status == store.AgentStatusActive {
			kept[a.Harness] = true
		}
	}
	row, err := r.q.GetOrgWorkspace(ctx, org.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read workspace of org %s: %w", errAbortPass, org.Slug, err)
	}
	var errs []error
	for _, harness := range slices.Sorted(maps.Keys(r.cfg.Harnesses)) {
		f := r.cfg.Harnesses[harness]
		if kept[harness] || !f.rendered() {
			continue
		}
		p := r.profile(harness, f)
		k := harnessProfileKey(row.Workspace, p.ID)
		r.mu.Lock()
		settled := r.harnessProfileSettled[k]
		r.mu.Unlock()
		if settled {
			continue
		}
		claimed, err := c.ClaimProviderProfile(ctx, row.Workspace, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconciler: claim harness profile %s of org %s: %w", harness, org.Slug, err))
			continue
		}
		if claimed {
			r.log.Info("marked a harness profile no active agent keeps current", "org", org.Slug, "harness", harness)
		}
		r.mu.Lock()
		r.harnessProfileSettled[k] = true
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

// harnessProfileKey is the key of a harness profile in harnessProfileSettled.
func harnessProfileKey(workspace, profileID string) string { return workspace + "/" + profileID }

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
	return r.recordRow(ctx, w, sandboxState(sb.Phase), string(sb.Phase), msg, reason, hash, sb.ID)
}

// recordState stores a state without an observed sandbox; the recorded home generation is kept.
func (r *Reconciler) recordState(ctx context.Context, w *work, state store.SandboxState, phase, msg string, hash ...string) error {
	h := w.sb.SpecHash
	if len(hash) > 0 {
		h = hash[0]
	}
	return r.recordRow(ctx, w, state, phase, msg, "", h, "")
}

func (r *Reconciler) recordRow(ctx context.Context, w *work, state store.SandboxState, phase, msg, reason, hash, generation string) error {
	sb, err := r.q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: w.org.ID, AgentID: w.agent.ID,
		State: state, Phase: phase, Message: msg, Reason: reason, SpecHash: hash, HomeGeneration: generation,
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
func (r *Reconciler) recordForeign(ctx context.Context, w *work, err error) {
	_ = r.recordRow(ctx, w, store.SandboxStateError, "Error", err.Error(), ForeignReason, w.sb.SpecHash, "")
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
