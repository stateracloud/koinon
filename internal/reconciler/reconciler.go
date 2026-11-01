// SPDX-License-Identifier: Apache-2.0

// Package reconciler drives each agent toward its desired execution state (plan item R2):
//
//   - an active agent gets its org's OpenShell workspace (one per org, ADR 0002), an
//     agentgateway key issued through the ModelGateway (material in a Kubernetes Secret in
//     Koinon's namespace, only the hash in gateway config and Postgres), a provider record
//     holding that key behind a stable placeholder, and a sandbox on its flavor image with an
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
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
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
	// DefaultKeepWarmPerOrg is the default cap of keep-warm sandboxes per org (#315).
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
	// GitBinaries may use the GitHub endpoints of a read grant, besides the flavor's binaries.
	// Default DefaultGitBinaries.
	GitBinaries []string
	// Flavors maps each flavor to its image and binaries. Agents of a flavor without an image
	// are reported as errors.
	Flavors map[store.AgentFlavor]Flavor
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
	// KeepWarmPerOrg caps the sandboxes an org's admins can keep warm (agents.keep_warm, #315).
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
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Metrics receives the pass metrics; nil keeps them unexported.
	Metrics *Metrics
	// Supervisor says which supervisor image the OpenShell gateway starts sandboxes with. It is part
	// of the sandbox spec hash, so an OpenShell upgrade recreates each sandbox through the usual path
	// (ADR 0085 decision 9). Nil leaves it out of the hash.
	Supervisor SupervisorSource
	// MaxRecreatesPerPass caps the sandboxes one pass deletes to recreate them with a changed spec
	// (a new supervisor, image or resources); the others wait for a later pass. Zero is
	// DefaultMaxRecreatesPerPass; negative is no cap.
	MaxRecreatesPerPass int
	// Now defaults to time.Now; tests move it forward to pass the idle timeout.
	Now func() time.Time
}

// Reconciler reconciles agents into OpenShell sandboxes.
type Reconciler struct {
	cfg     Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	q       *store.Queries
	backend execution.Backend
	gw      gateway.ModelGateway
	secrets SecretStore

	mu sync.Mutex
	// wsMu guards the per-pass workspace map, which per-sandbox jobs read concurrently.
	wsMu sync.Mutex
	// githubRevs is the token revision last pushed per GitHub provider record, to log refreshes.
	githubRevs map[string]string
	// unpricedLogged is the unpriced model set last logged.
	unpricedLogged string
	// keyWrittenAt is when each provider key file written by this process was written, until it
	// has had ProviderKeySettle to reach the gateway.
	keyWrittenAt map[string]time.Time
	// clampMu guards clampLogged: the clamps last logged per agent, so a pass that changes nothing
	// logs nothing.
	clampMu     sync.Mutex
	clampLogged map[string]string
	// lastLimit is the budget last pushed per agent key, so one agent whose budget cannot be
	// computed keeps it (and never gets more) instead of stopping the pass for everyone.
	lastLimit map[gateway.AgentRef]gateway.Limit
	// notLive holds the IDs of keys issued by this process that no applied gateway state has
	// carried yet. While the gateway refuses state, an agent with such a key gets no sandbox
	// work: its provider record would name a key the gateway does not know.
	notLive map[string]bool
	// appliedSinceStart is set once a gateway state has been applied by this process. Before
	// that, notLive is empty after a restart whatever the gateway serves, so a failing Apply holds
	// back every active agent.
	appliedSinceStart bool
	// appliedFile is the key file (data key) each provider ID was last applied with.
	appliedFile map[string]string
	// appliedMCPFile is the same for org MCP servers, by server ID.
	appliedMCPFile map[string]string
	// probeKeys are the prober's gateway keys, in memory only; probes are the servers the last
	// applied state lets the prober list.
	probeKeys map[gateway.AgentRef]gateway.IssuedKey
	probes    []mcpProbe
	mcp       *mcpservers.Service
	m         *Metrics

	// supMu guards the supervisor image read from the gateway (supervisor.go).
	supMu    sync.Mutex
	supImage string
	supKnown bool
	supErr   error
	// recreates counts this pass's spec-change recreates against MaxRecreatesPerPass.
	recreates atomic.Int32
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
	return &Reconciler{
		cfg: cfg, log: cfg.Logger, pool: pool, q: store.New(pool),
		backend: backend, gw: gw, secrets: secrets, githubRevs: map[string]string{},
		mcp: mcpservers.New(pool, nil, cfg.Logger), m: cfg.Metrics,
	}, nil
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
	// keepWarm is set when the agent asks to stay warm and the org's cap grants it (#315).
	keepWarm bool
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
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	start := time.Now()
	err := r.reconcileAll(ctx)
	r.m.PassDuration.Observe(time.Since(start).Seconds())
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.m.Passes.WithLabelValues(result).Inc()
	r.observeSandboxes(ctx)
	return err
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

func (r *Reconciler) reconcileAll(ctx context.Context) error {
	r.recreates.Store(0)
	r.refreshSupervisor(ctx)
	orgs, err := r.q.ListOrgsToReconcile(ctx)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
		return fmt.Errorf("reconciler: list orgs: %w", err)
	}
	workspaces := map[string]string{} // org ID -> workspace, ensured in this pass
	var (
		items []*work
		keys  []gateway.Key
		errs  []error
		tools = map[gateway.AgentRef][]gateway.ToolGrant{}
	)
	for _, org := range orgs {
		agents, err := r.q.ListAgentsToReconcile(ctx, org.ID)
		if err != nil {
			// Without the full list, an Apply would revoke this org's keys.
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			return fmt.Errorf("reconciler: list agents of org %s: %w", org.Slug, err)
		}
		warm := 0
		for _, a := range agents {
			w, err := r.prepare(ctx, org, a, workspaces)
			if errors.Is(err, errAbortPass) {
				// prepare aborts only on a database read.
				r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
				return err
			}
			if err != nil {
				r.m.PassErrors.WithLabelValues(errKindAgentPrepare).Inc()
				errs = append(errs, err)
				continue
			}
			if w == nil {
				continue
			}
			if a.KeepWarm && a.Status == store.AgentStatusActive && warm < r.cfg.KeepWarmPerOrg {
				// The list is oldest first, so the cap keeps the same agents from pass to pass.
				w.keepWarm = true
				warm++
			}
			items = append(items, w)
			if w.key.ID != "" {
				keys = append(keys, w.key)
			}
			if len(w.tools) > 0 {
				tools[agentRef(org, a.ID)] = w.tools
			}
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

	// One gateway state for every agent: keys of archived agents are revoked by omission.
	state := gateway.State{Keys: keys, Providers: served.providers, MCP: r.mcpTargets(), Tools: tools}
	r.warnUnpriced(state)
	// Spend limits net of recorded spend (plan item L1), recomputed every pass, so a key
	// rotation or a gateway that lost its budget counters gets the remainder, not the full
	// limit. An agent whose budget cannot be computed fails closed (its last pushed limit, or
	// zero) and is reported; it never stops the pass for the other agents and orgs.
	now := r.cfg.Now()
	for _, w := range items {
		if w.key.ID == "" {
			continue
		}
		limitFor := r.cfg.AgentLimit
		if limitFor == nil {
			limitFor = ledger.AgentLimit
		}
		l, err := limitFor(ctx, r.q, w.agent, now)
		if err != nil {
			r.m.PassErrors.WithLabelValues(errKindSpendLimit).Inc()
			failed := r.failClosedLimit(w.key.Agent, now)
			r.log.Error("agent budget not computed; its key keeps its last pushed limit, or zero if it had none",
				"agent", execution.UUIDString(w.agent.ID.Bytes), "org", w.key.Agent.Org, "err", err)
			errs = append(errs, fmt.Errorf("spend limit of agent %s not computed (key fails closed): %w", execution.UUIDString(w.agent.ID.Bytes), err))
			state.SetLimit(w.key.Agent, failed)
			continue
		}
		r.rememberLimit(w.key.Agent, l)
		state.SetLimit(w.key.Agent, l)
		// A budget with nothing left stops the agent's runs (or the org's) with the budget reason
		// before the Apply below blocks or revokes the key, so a run in flight records the budget
		// stop and not the gateway's 401 or 429 (issue #188). A limit that could not be computed
		// fails closed above but stops nothing: it is not known to be spent.
		if l != nil && l.Amount <= 0 {
			if n, err := ledger.StopSpent(ctx, r.pool, w.agent, now); err != nil {
				r.m.PassErrors.WithLabelValues(errKindSpendLimit).Inc()
				errs = append(errs, fmt.Errorf("stop the runs of spent agent %s: %w", execution.UUIDString(w.agent.ID.Bytes), err))
			} else if n > 0 {
				r.log.Info("budget spent: runs stopped before the key is blocked", append(w.logAttrs(), "runs", n)...)
			}
		}
	}
	// Provider key files first, then the config that names them; files no provider uses go last.
	// One provider's new key file still travelling to the gateway pod never holds back the others:
	// that provider keeps serving with its previous file, or is left out until the file has arrived.
	var written []string
	var present map[string]bool
	files := map[string][]byte{}
	maps.Copy(files, served.files)
	maps.Copy(files, mcpServed.files)
	if len(files) > 0 {
		var err error
		if written, present, err = r.cfg.ProviderKeys.Put(ctx, files); err != nil {
			r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
			return errors.Join(append(errs, fmt.Errorf("reconciler: deliver provider keys: %w", err))...)
		}
	}
	settling := r.keysSettling(written, now)
	keep := r.settleProviders(&state, served, settling, r.recordedFiles(ctx, served, settling, present))
	maps.Copy(keep, r.settleMCP(&mcpServed, settling))
	probes, err := r.applyMCP(ctx, &state, &mcpServed, items)
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindMCP).Inc()
		return errors.Join(append(errs, err)...)
	}
	errs = append(errs, r.applyModelCheck(ctx, &state, orgs, now)...)
	// Sandbox work is per-sandbox jobs, outside the sequential pass (plan item LT1, practice B4).
	// An agent whose key the gateway already serves does not depend on the Apply below, which may
	// wait for agentgateway's hot reload for about a minute: its sandbox starts and stops now,
	// concurrently with it. An agent with a new key waits for the Apply, as before.
	var early, late []*work
	for _, w := range items {
		if w.agent.Status == store.AgentStatusActive && w.key.ID != "" && r.keyNotLive(w.key.ID) {
			late = append(late, w)
			continue
		}
		early = append(early, w)
	}
	// One semaphore for the whole pass, so the early and the late jobs together stay within
	// SandboxJobs.
	sem := make(chan struct{}, r.cfg.SandboxJobs)
	earlyJobs := r.startJobs(ctx, early, workspaces, sem)

	var rejections []gateway.Rejection
	applyFailed := false
	rejectedReason := ""
	applyStart := time.Now()
	_, applyErr := r.gw.Apply(ctx, state)
	r.m.GatewayApply.Observe(time.Since(applyStart).Seconds())
	if err := applyErr; err != nil {
		// A Kubernetes-mode gateway judges objects one by one: the revision is live for everything
		// but the objects named, which are reported on their org and do not hold back the others.
		var rej *gateway.RejectedError
		if !errors.As(err, &rej) {
			// The gateway is unreachable or the reload did not complete. Sandbox work does not
			// depend on it (start on demand, idle stop), so it goes on, except for agents whose
			// new key is not live yet. Nothing is recorded as applied and nothing is pruned.
			applyFailed = true
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
			rejections = rej.Rejected
			r.m.PassErrors.WithLabelValues(errKindGatewayRejected).Inc()
			for _, x := range rej.Rejected {
				r.log.Error("the gateway rejected an object; the rest is live", "org", x.Org, "kind", x.Kind, "object", x.Name, "reason", x.Reason)
			}
			errs = append(errs, fmt.Errorf("reconciler: %w", rej))
		}
	}
	r.m.GatewayConfigRejected.Set(b2f(rejectedReason != ""))
	if !applyFailed {
		r.keysLive(state, rejections)
		r.recordApplied(served, state, rejections)
		if err := r.recordServed(ctx, orgs, served, state, rejections); err != nil {
			r.m.PassErrors.WithLabelValues(errKindDatabase).Inc()
			errs = append(errs, err)
		}
		r.recordAppliedMCP(mcpServed, probes)
		if r.cfg.ProviderKeys != nil && len(provErrs) == 0 && len(mcpErrs) == 0 {
			if err := r.cfg.ProviderKeys.Prune(ctx, keep); err != nil {
				r.m.PassErrors.WithLabelValues(errKindProviderKeys).Inc()
				errs = append(errs, fmt.Errorf("reconciler: prune provider keys: %w", err))
			}
		}
	}

	heldBack := 0
	defer func() { r.m.AgentsHeldBack.Set(float64(heldBack)) }()
	var lateRun []*work
	for _, w := range late {
		if applyFailed || r.keyNotLive(w.key.ID) {
			r.log.Warn("the gateway does not serve the agent's new key yet; its sandbox work waits", w.logAttrs()...)
			msg := "waiting for the gateway to serve the agent's new key"
			if rejectedReason != "" {
				msg += ": agentgateway rejected the config: " + rejectedReason
			}
			r.recordMessage(ctx, w, errors.New(msg))
			heldBack++
			continue
		}
		lateRun = append(lateRun, w)
	}
	errs = append(errs, r.startJobs(ctx, lateRun, workspaces, sem).wait()...)
	errs = append(errs, earlyJobs.wait()...)
	// Each rejected object is reported on its org's agents, after their own pass has recorded its state.
	// Several rejections of one org are joined into one message, not overwritten.
	byOrg := map[string][]string{}
	for _, x := range rejections {
		byOrg[x.Org] = append(byOrg[x.Org], fmt.Sprintf("the gateway rejected %s %s: %s", x.Kind, x.Name, x.Reason))
	}
	for _, w := range items {
		if msgs := byOrg[w.org.Slug]; len(msgs) > 0 {
			r.recordMessage(ctx, w, errors.New(strings.Join(msgs, "; ")))
		}
	}
	return errors.Join(errs...)
}

// jobs is a set of per-sandbox jobs started by startJobs.
type jobs struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

// wait blocks until every job has ended and returns their errors.
func (j *jobs) wait() []error {
	j.wg.Wait()
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.errs
}

// startJobs starts one job per agent, holding a slot of sem (sized SandboxJobs, shared by every call of a pass), and returns at once. A job
// is everything the reconciler does for one agent's sandbox: creating, starting, stopping,
// recreating and recording it. A slow start (an image pull, a stuck OpenShell call) holds up
// only its own agent, not the rest of the pass and not the gateway update.
func (r *Reconciler) startJobs(ctx context.Context, ws []*work, workspaces map[string]string, sem chan struct{}) *jobs {
	j := &jobs{}
	j.wg.Add(1)
	safego.Go(ctx, r.log, "reconcile-pass", func(context.Context) {
		defer j.wg.Done()
		for _, w := range ws {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			j.wg.Add(1)
			go func() {
				defer j.wg.Done()
				defer func() { <-sem }()
				var err error
				if safego.Do(r.log, "reconcile-agent", func() { err = r.reconcileAgent(ctx, w, workspaces) }) {
					err = errors.New("internal error: reconciling the agent panicked")
				}
				if err != nil {
					r.m.PassErrors.WithLabelValues(errKindSandbox).Inc()
					r.log.Warn("agent not converged", append(w.logAttrs(), "err", err)...)
					r.recordMessage(ctx, w, err)
					j.mu.Lock()
					j.errs = append(j.errs, fmt.Errorf("agent %s: %w", execution.UUIDString(w.agent.ID.Bytes), err))
					j.mu.Unlock()
				}
			}()
		}
	})
	return j
}

// keysLive records that the applied state carries its keys.
func (r *Reconciler) keysLive(s gateway.State, rejected []gateway.Rejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appliedSinceStart = true
	refused := map[string]bool{}
	for _, x := range rejected {
		if x.Kind == "Key" {
			refused[x.Name] = true
		}
	}
	for _, k := range s.Keys {
		if !refused[k.ID] {
			delete(r.notLive, k.ID)
		}
	}
}

// keyNotLive reports whether the gateway may not serve key id yet. Until this process has applied
// a state, it cannot tell, so every key counts as not live.
func (r *Reconciler) keyNotLive(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.appliedSinceStart || r.notLive[id]
}

// recordedFiles returns, for each provider whose key file is settling and that this process has not
// applied, the key file gateway_served_models says it was last served with, when that file is still in
// the provider keys Secret: a key rotated while the reconciler was down (or before a new leader took
// over) keeps being served with the previous file instead of the provider being dropped. A failed
// read only loses the fallback.
func (r *Reconciler) recordedFiles(ctx context.Context, served servedProviders, settling, present map[string]bool) map[string]string {
	out := map[string]string{}
	if len(settling) == 0 {
		return out
	}
	for i, id := range served.ids {
		name := served.fileNames[i]
		if name == "" || !settling[name] {
			continue
		}
		r.mu.Lock()
		applied := r.appliedFile[id]
		r.mu.Unlock()
		if applied != "" {
			continue
		}
		file, err := r.q.GetGatewayServedKeyFile(ctx, store.GetGatewayServedKeyFileParams{OrgID: served.orgIDs[i], ProviderID: served.pids[i]})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			r.log.Warn("reading the key file a provider was last served with failed", "provider", id, "err", err)
		case file != name && present[file]:
			out[id] = file
		}
	}
	return out
}

// settleProviders handles providers whose key file is still settling: with a previously applied
// file (this process's, else the recorded one) the provider keeps using it, without one it is left
// out of state. It returns the data keys to keep in the provider keys Secret.
func (r *Reconciler) settleProviders(state *gateway.State, served servedProviders, settling map[string]bool, recorded map[string]string) map[string]bool {
	keep := map[string]bool{}
	for name := range served.files {
		keep[name] = true
	}
	if len(settling) == 0 {
		return keep
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []gateway.Provider
	for i, p := range served.providers {
		name := served.fileNames[i]
		if name == "" || !settling[name] {
			out = append(out, p)
			continue
		}
		if prev := cmp.Or(r.appliedFile[served.ids[i]], recorded[served.ids[i]]); prev != "" {
			p.APIKeyFile = path.Join(r.cfg.ProviderKeyDir, prev)
			keep[prev] = true
			out = append(out, p)
			r.log.Info("a new provider key is still reaching the gateway; serving with the previous one", "org", p.Org, "provider", p.Name)
			continue
		}
		r.log.Info("a new provider key is still reaching the gateway; the provider is not served until it has", "org", p.Org, "provider", p.Name)
	}
	state.Providers = out
	return keep
}

// recordApplied remembers which key file each served provider was applied with. A provider of an org
// whose model object the gateway rejected was not applied, so its file is not recorded.
func (r *Reconciler) recordApplied(served servedProviders, state gateway.State, rejections []gateway.Rejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rejected := map[string]bool{}
	for _, x := range rejections {
		if x.Kind == "AgentgatewayModel" {
			rejected[x.Org] = true
		}
	}
	next := map[string]string{}
	for i, id := range served.ids {
		if rejected[served.providers[i].Org] {
			continue
		}
		for _, p := range state.Providers {
			if p.Org == served.providers[i].Org && p.Name == served.providers[i].Name && p.APIKeyFile != "" {
				next[id] = path.Base(p.APIKeyFile)
			}
		}
	}
	r.appliedFile = next
}

// recordServed writes, for every org, the models the applied state serves to gateway_served_models,
// which koinon-agentd waits on before a run starts its harness (issue #174). A provider whose new key
// file is still reaching the gateway pod and that has no previous file is not in state, so its models
// are not recorded; one that keeps serving with its previous file stays recorded. An org whose model
// objects the gateway rejected gets nothing new recorded (as in recordApplied), but models no longer
// in state are removed. Only the leader reconciles, so only the leader writes.
func (r *Reconciler) recordServed(ctx context.Context, orgs []store.ListOrgsToReconcileRow, served servedProviders, state gateway.State, rejections []gateway.Rejection) error {
	rejected := map[string]bool{}
	for _, x := range rejections {
		if x.Kind == "AgentgatewayModel" {
			rejected[x.Org] = true
		}
	}
	type entries struct {
		ids   []pgtype.UUID
		files []string
	}
	byOrg := map[string]*entries{}
	for _, p := range state.Providers {
		e := byOrg[p.Org]
		if e == nil {
			e = &entries{}
			byOrg[p.Org] = e
		}
		file := ""
		if p.APIKeyFile != "" {
			file = path.Base(p.APIKeyFile)
		}
		for _, m := range p.Models {
			if m.Price == nil {
				continue // not served (ADR 0041)
			}
			if id, ok := served.modelIDs[gateway.ModelRef(p.Org, m.Name)]; ok {
				e.ids = append(e.ids, id)
				e.files = append(e.files, file)
			}
		}
	}
	var errs []error
	for _, org := range orgs {
		// Never nil: pgx sends a nil slice as NULL, and "<> ALL (NULL)" would remove nothing.
		e := entries{ids: []pgtype.UUID{}, files: []string{}}
		if got := byOrg[org.Slug]; got != nil {
			e.ids, e.files = append(e.ids, got.ids...), append(e.files, got.files...)
		}
		var err error
		if rejected[org.Slug] {
			err = r.q.PruneGatewayServedModels(ctx, store.PruneGatewayServedModelsParams{OrgID: org.ID, ModelIds: e.ids})
		} else {
			err = r.q.SyncGatewayServedModels(ctx, store.SyncGatewayServedModelsParams{OrgID: org.ID, ModelIds: e.ids, KeyFiles: e.files})
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("reconciler: record the models the gateway serves for org %s: %w", org.Slug, err))
		}
	}
	return errors.Join(errs...)
}

// keysSettling records the files written in this pass and returns the data keys that have not yet
// had ProviderKeySettle to reach the gateway pod.
func (r *Reconciler) keysSettling(written []string, now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keyWrittenAt == nil {
		r.keyWrittenAt = map[string]time.Time{}
	}
	for _, name := range written {
		r.keyWrittenAt[name] = now
	}
	settling := map[string]bool{}
	for name, at := range r.keyWrittenAt {
		if r.cfg.ProviderKeySettle-now.Sub(at) > 0 {
			settling[name] = true
		} else {
			delete(r.keyWrittenAt, name)
		}
	}
	return settling
}

// warnUnpriced logs the models the gateway does not serve for lack of a price, once per set.
func (r *Reconciler) warnUnpriced(s gateway.State) {
	unpriced := s.Unpriced()
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprint(unpriced)
	if len(unpriced) > 0 && key != r.unpricedLogged {
		r.log.Warn("models without a price are not served; give free models a zero price", "models", unpriced)
	}
	r.unpricedLogged = key
}

// prepare loads or creates the agent's row and, for an active agent, its key. It returns nil
// for agents with nothing rendered and nothing to render.
func (r *Reconciler) prepare(ctx context.Context, org store.ListOrgsToReconcileRow, a store.Agent, workspaces map[string]string) (*work, error) {
	w := &work{org: org, agent: a}
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

// ensureKey returns the agent's key and its material from the key Secret, issuing a new key
// when there is none or the Secret no longer matches the recorded hash (a hand edit or a
// deleted Secret: the old material is gone, so the key is rotated).
func (r *Reconciler) ensureKey(ctx context.Context, w *work) (gateway.Key, string, error) {
	sec, found, err := r.secrets.Get(ctx, w.sb.KeySecretName)
	if err != nil {
		return gateway.Key{}, "", err
	}
	if recorded, ok := recordedKey(w.org, w.sb); ok && found &&
		sec.KeyID == recorded.ID && gateway.HashKey(sec.Material) == recorded.Hash {
		return recorded, sec.Material, nil
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
	r.mu.Lock()
	if r.notLive == nil {
		r.notLive = map[string]bool{}
	}
	r.notLive[issued.ID] = true
	r.mu.Unlock()
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
	done[id] = name
	if r.cfg.Observer != nil && ws.Active {
		// The observer only adds detail to a wait: a failure is counted and logged there, never fatal here.
		_ = r.cfg.Observer.EnsureAccess(ctx, name)
	}
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
	flavor, ok := r.cfg.Flavors[w.agent.Flavor]
	if !ok || flavor.Image == "" || len(flavor.Binaries) == 0 {
		return fmt.Errorf("flavor %q has no image or binaries configured", w.agent.Flavor)
	}
	ws, err := r.ensureWorkspace(ctx, w.org, workspaces)
	if err != nil {
		return err
	}
	names := w.names()

	// The key goes only into the provider record; the sandbox sees a placeholder.
	if err := r.backend.EnsureProvider(ctx, ws, execution.Provider{
		Name:               names.Provider,
		Profile:            r.profile(w.agent.Flavor, flavor),
		Credentials:        map[string]string{keyEnvVar: w.material},
		CredentialRevision: w.key.ID,
		Labels:             ownerLabels(w.org.ID, w.agent.ID),
	}); err != nil {
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
		if err := r.ensureGitHub(ctx, ws, w, names, flavor, read); err != nil {
			return err
		}
		providers = append(providers, names.GitHubProvider)
		network[githubRule] = GitHubRule(read, r.githubBinaries(flavor))
	}
	if rule, ok := PluginInstallRule(w.cfg, w.agent.Flavor, flavor); ok {
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

	spec, hash := r.sandboxSpec(w.org.ID, w.agent.ID, names, flavor, providers, network, r.sandboxResources(w))
	tune, err := orgsettings.Get(ctx, r.q, w.org.ID, orgsettings.Settings{IdleTimeout: r.cfg.IdleTimeout, ParkWarm: r.cfg.ParkWarm})
	if err != nil {
		return fmt.Errorf("read org settings: %w", err)
	}

	sb, err := r.backend.GetSandbox(ctx, ws, names.Sandbox)
	if errors.Is(err, execution.ErrNotFound) {
		if !act.HasActive && !act.HasDemand && !r.woken(w.sb, tune) && !w.keepWarm {
			// Nothing wants the agent, so no sandbox is created: it starts on demand (#315). A
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
		sb, err = r.backend.CreateSandbox(ctx, ws, spec)
		if err != nil {
			return err
		}
		r.log.Info("created sandbox", w.logAttrs()...)
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
		if err := r.backend.DeleteSandbox(ctx, ws, names.Sandbox); err != nil {
			return err
		}
		r.m.SandboxRepairs.Inc()
		r.log.Warn("deleting a stuck sandbox to recreate it", append(w.logAttrs(), "reason", w.sb.UnhealthyReason.String)...)
		if err := audit.RecordErr(ctx, r.q, audit.Event{
			Org: w.org.ID, Actor: audit.System(), Action: "sandbox.recreate", TargetKind: "agent", TargetID: w.agent.ID.String(),
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
	if sb.Labels[LabelSpecHash] != hash && !r.supervisorKnown() {
		// The gateway's supervisor image was never read, so the hash lacks it and a mismatch
		// proves nothing. Recreating now would recreate every sandbox once the image is known.
		r.log.Debug("not comparing the sandbox spec hash: the OpenShell supervisor image is not known yet", w.logAttrs()...)
	} else if sb.Labels[LabelSpecHash] != hash {
		// Image, providers or filesystem rules changed (or the sandbox was replaced by
		// hand): OpenShell cannot change those in place, so the sandbox is recreated, which
		// also replaces its home volume. Never while a run holds the agent, parked runs
		// included: a parked run resumes its harness session, which lives in that volume.
		// (agentd starts a new session when the volume was replaced anyway, see
		// agent_sandboxes.home_generation.)
		if act.HasHeld {
			return r.record(ctx, w, sb, hash, "sandbox spec changed; recreating it after the active and parked runs finish")
		}
		if ok, note, err := r.homeGate(ctx, w, ws, sb, homeDirtiedSince(w.sb)); err != nil || !ok {
			if err != nil {
				return err
			}
			return r.record(ctx, w, sb, hash, note)
		}
		if !r.takeRecreate() {
			// The cap keeps an OpenShell upgrade from recreating the whole fleet in one pass.
			return r.record(ctx, w, sb, hash, "sandbox spec changed; waiting for a free recreate slot of this pass")
		}
		if err := r.backend.DeleteSandbox(ctx, ws, names.Sandbox); err != nil {
			return err
		}
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
		if err := audit.RecordErr(ctx, r.q, audit.Event{
			Org: w.org.ID, Actor: audit.System(), Action: "agent.mark_unhealthy", TargetKind: "agent", TargetID: w.agent.ID.String(),
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
	if err := r.backend.DeleteSandbox(ctx, ws, name); err != nil {
		return err
	}
	r.m.SandboxRepairs.Inc()
	r.log.Warn("deleting a failed sandbox to recreate it", append(w.logAttrs(), "detail", detail)...)
	if err := audit.RecordErr(ctx, r.q, audit.Event{
		Org: w.org.ID, Actor: audit.System(), Action: "sandbox.recreate", TargetKind: "agent", TargetID: w.agent.ID.String(),
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
// for its re-activation. Runs cannot be queued for a draft agent.
func (r *Reconciler) reconcileDraft(ctx context.Context, w *work) error {
	sb, err := r.backend.GetSandbox(ctx, w.sb.Workspace, w.sb.SandboxName)
	if errors.Is(err, execution.ErrNotFound) {
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
	return r.record(ctx, w, sb, w.sb.SpecHash, "")
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
			if err := r.backend.DeleteSandbox(ctx, ws, name); err != nil {
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

// recordMessage stores a reconcile error on the row for operators, keeping the state.
func (r *Reconciler) recordMessage(ctx context.Context, w *work, err error) {
	if w.sb.SandboxName == "" {
		return
	}
	// The message is for operators; the reason of the latest condition stays as observed.
	_ = r.recordRow(ctx, w, w.sb.State, w.sb.Phase, err.Error(), w.sb.Reason, w.sb.SpecHash, "")
}

// rememberLimit records the budget pushed for agent.
func (r *Reconciler) rememberLimit(agent gateway.AgentRef, l *gateway.Limit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastLimit == nil {
		r.lastLimit = map[gateway.AgentRef]gateway.Limit{}
	}
	if l == nil {
		delete(r.lastLimit, agent)
		return
	}
	r.lastLimit[agent] = *l
}

// failClosedLimit is the budget for an agent whose budget could not be computed: the last one
// pushed while it is still from the current UTC day, else zero. A pushed limit only ever shrinks
// as the gateway charges against it, but it must not be regranted every day by a computation that
// keeps failing; zero blocks the key until a pass computes it.
//
// A gateway that does not enforce limits itself (Kubernetes mode: the ledger revokes the key when
// the amount is zero) cannot make a positive last limit bind, so there the key fails closed to zero.
func (r *Reconciler) failClosedLimit(agent gateway.AgentRef, now time.Time) *gateway.Limit {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gw.(gateway.LedgerBudgets); ok && g.LedgerEnforcesBudgets() {
		return &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: ledger.Window, Generation: "failed"}
	}
	if l, ok := r.lastLimit[agent]; ok {
		day, _, _ := strings.Cut(l.Generation, "-")
		if day == strconv.FormatInt(budget.DayStart(now).Unix(), 10) {
			return &l
		}
	}
	return &gateway.Limit{Unit: gateway.USD, Amount: 0, Window: ledger.Window, Generation: "failed"}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
