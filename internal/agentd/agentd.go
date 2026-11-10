// SPDX-License-Identifier: Apache-2.0

// Package agentd drives runs (plan item AG1; ADRs 0016 as amended by 0049, 0029, 0030).
//
// A [Pool] of workers claims runs from Postgres, takes ownership under a new epoch, waits
// until koinon-reconciler reports the agent's sandbox ready in agent_sandboxes, execs the flavor's ACP launch command through the execution
// backend (OpenShell's non-TTY exec) and speaks ACP to the harness over the stream's stdin
// and stdout:
//
//   - before the harness starts, the run's instructions are written to the sandbox
//     (/sandbox/.koinon/AGENTS.md), and their hash is recorded with the session;
//   - initialize, then session/new on a task's first run (the session ID is stored on the
//     task) or session/load afterwards;
//   - for flavors that announce it, agentd waits until Koinon's MCP tools are there and
//     selects the agent's model on the session;
//   - the task's new messages are sent as the prompt, after the instruction file is checked
//     and, if it changed, restored;
//   - every session update is written as a run event, in order and at most once;
//   - permission requests for paths outside the session directory are rejected; the others
//     are answered from a tier mapping, and anything unmapped is rejected;
//   - end_turn finishes the run, errors fail it with a reason, a control-plane cancel sends
//     session/cancel and closes the stream.
//
// Every write is a compare-and-set on the run's epoch. A worker whose exec stream drops gives
// its lease up at once; one that dies stops heartbeating and its lease expires. Either way a
// worker takes the run over under a new epoch, reopens exec and resumes with session/load,
// whose replayed history is not recorded again.
//
// agentd holds no credentials. The launch command carries only the gateway's URLs and the
// model name; the harness's key is an OpenShell placeholder in the sandbox.
package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/safego"
)

// Backend is the part of [execution.Backend] agentd uses. agentd only execs: sandbox state
// comes from koinon-reconciler through agent_sandboxes, and only the reconciler starts and
// stops sandboxes.
type Backend interface {
	Exec(ctx context.Context, workspace, sandbox string, req execution.ExecRequest) (execution.Process, error)
	// Upload extracts a tar stream into a directory of the sandbox through the execution layer's
	// flow-controlled upload channel (the home restore, ADR 0032).
	Upload(ctx context.Context, workspace, sandbox, destDir string, tar io.Reader) error
}

var _ Backend = execution.Backend(nil)

// Config configures a [Pool]. Zero durations and counts take the defaults below.
type Config struct {
	// Prices finds the price of each model a run uses; required
	// (prices.NewResolver).
	Prices prices.Resolver
	// OwnerID names this replica, for example the pod name. Workers own runs as
	// "<OwnerID>/<n>".
	OwnerID string
	// Workers is how many runs this replica drives at once. Default 8.
	Workers int
	// Lease is how long a claim lasts without a heartbeat. Default 30s.
	Lease time.Duration
	// Heartbeat is how often a worker renews its lease and notices a cancel. Default Lease/3.
	Heartbeat time.Duration
	// Poll is how often an idle worker looks for runs, and how often a worker checks on a
	// sandbox it waits for. Default 1s.
	Poll time.Duration
	// SandboxTimeout bounds the wait for the agent's sandbox to be ready, for an org that has not
	// set queue_wait_seconds (orgsettings). Default 5m. A run whose sandbox is not ready in time
	// goes back to the queue instead of failing (ADR 0066), so a burst beyond the cluster's
	// capacity waits.
	SandboxTimeout time.Duration
	// SandboxWaitCap is the total time a run waits for its sandbox, across requeues, for an org that
	// has not set its own (ADR 0066 amendment 2026-10-28).
	SandboxWaitCap time.Duration
	// WrapUpNotice is how long before the run's time limit the agent is told to wrap up (ADR 0066
	// amendment 2026-10-28).
	WrapUpNotice time.Duration
	// WrapUpFraction is how far through its model-request or spend limit a run is when the agent is
	// told to wrap up (ADR 0081); runlimits.WrapUpFraction (80%) by default, a negative value turns
	// the notice off for these two limits.
	WrapUpFraction float64
	// UsagePoll is how often a running turn reads the run's ledger usage for that notice.
	UsagePoll time.Duration
	// Now defaults to time.Now; tests move it forward to pass SandboxTimeout.
	Now func() time.Time
	// ModelServeTimeout bounds the wait, before a run starts its harness, for the gateway to serve
	// the run's model (a new provider's key file is still reaching the gateway, old issue 174). A run
	// whose model is not served in time fails with a reason that names the provider. Default
	// DefaultModelServeTimeout.
	ModelServeTimeout time.Duration
	// MCPRetryDelay is how long a run waits, before it starts its harness, between finding an MCP
	// server it may use not serving tools and looking again; a second miss fails the run (issue
	// old issue 326). A miss is what Koinon's health record of the server says (probed every 60 s) or, with
	// MCPTools, what the gateway lists to the agent right now. Default DefaultMCPRetryDelay.
	MCPRetryDelay time.Duration
	// MCPTools is the live check of the agent's MCP tools before the harness starts (old issue 488):
	// it lists what the gateway offers the agent under the agent's own key, which koinon-agentd does
	// not hold. Without it only the health record is checked.
	MCPTools MCPTools
	// ReconcileNudge is how often a worker waiting for a sandbox NOTIFYs the reconciler.
	// Default 5s.
	ReconcileNudge time.Duration
	// CancelGrace is how long the harness gets to end its turn after session/cancel.
	// Default 10s.
	CancelGrace time.Duration
	// ReadyTimeout bounds the wait, after session/new or session/load, for a flavor's harness
	// to announce Koinon's MCP tools (readyCommand). Default 1m.
	ReadyTimeout time.Duration
	// ReadySettle is the pause after that announcement before the first prompt: OpenCode
	// registers the tools after a 100 ms debounce, separately from the command (ADR 0052).
	// Default 1s.
	ReadySettle time.Duration
	// GatewayRecordWait bounds the wait, after a harness reports an authentication failure, for the
	// gateway's record of the request to reach the spend ledger (providerrefusal.go): the access log is
	// exported in batches. A negative value does not wait. Default 20s.
	GatewayRecordWait time.Duration
	// SummaryTimeout bounds the end-of-turn summary prompt (summary.go). Default 2m.
	SummaryTimeout time.Duration
	// StuckAfter is how long a process may outlive the sweep's last SIGKILL before the sandbox
	// counts as stuck, and StuckPoll how often it is checked meanwhile (stuck.go). Defaults 60s
	// and 5s.
	StuckAfter, StuckPoll time.Duration
	// SaveTimeout is the time budget for reading a stuck sandbox's uncommitted work. Default 30s.
	SaveTimeout time.Duration
	// SavedWorkURL is koinon-api's tools listener, where a stuck sandbox's work is posted to be
	// sealed (package savedwork); koinon-agentd holds no key-encryption key. Without it that work
	// is not saved, and the timeline says so.
	SavedWorkURL string
	// SavedWorkKey returns the key that signs those posts: a Kubernetes Secret koinon-api creates
	// and koinon-agentd mounts. It is read on each save, so a rotated Secret applies.
	SavedWorkKey func() ([]byte, error)
	// MaxTakeovers fails a run that has been taken over more often than this, instead of
	// resuming it again. Default 5.
	MaxTakeovers int
	// ModelRetries, RetryBackoff and MaxRetryAfter are Koinon's policy for retrying a model request
	// that failed with a transient or throttled error (ADR 0030 failure classes; ADR 0056
	// amendment 2026-10-31, point 3). Exactly one layer applies it: for a flavor whose harness
	// retries model requests itself (flavor.Descriptor.RetriesModelRequests, OpenCode) the
	// harness gets the policy as launch switches and agentd does not repeat the turn; for any
	// other flavor agentd repeats the turn. Either way one failed step costs at most
	// ModelRetries+1 gateway requests and waits at most ModelRetries × MaxRetryAfter.
	//
	// ModelRetries is how many retries follow the first try before the run fails. Default 2
	// (3 tries).
	ModelRetries int
	// MaxRefusedTurns is how often a run continues a turn the harness cancelled after agentd
	// refused a permission request (refusal.go) before it fails. Default 5.
	MaxRefusedTurns int
	// RetryBackoff is the first wait before a transient retry; it doubles per retry up to a
	// minute (the harness adds jitter). Default 1s.
	RetryBackoff time.Duration
	// MaxRetryAfter is the longest Retry-After Koinon waits out; a provider asking for more
	// fails the run as throttled, so a long Retry-After cannot hold a run. Default 60s.
	MaxRetryAfter time.Duration
	// ownRetryLayer makes agentd repeat failed turns even for a flavor whose harness retries.
	// Only this package's tests set it (export_test.go), to exercise agentd's own layer with the
	// fake harness, which does not retry.
	ownRetryLayer bool
	// GatewayURL is agentgateway's base URL as sandboxes reach it, for example
	// http://koinon-gateway.koinon.svc:8080. Flavors get <GatewayURL>/v1 for models and
	// <GatewayURL>/mcp for tools. Required; it must not carry credentials.
	GatewayURL string
	// DefaultModel is the model of agents whose version names none, or "default", when set;
	// otherwise the org's stored default model is used (old issue 256).
	DefaultModel string
	// SessionCwd is the harness session's working directory in the sandbox. Default
	// /sandbox/work.
	SessionCwd string
	// Permissions answers the harness's permission requests. Default DefaultPermissions.
	Permissions *PermissionPolicy
	// DrainDeadline is how long a stopping pool (its Run context ended: SIGTERM) lets the turns in
	// progress finish before it hands their runs off (ADR 0085 decision 2). From the stop on, no
	// worker claims a run, a run that has not begun its turn is handed off at once, and the
	// heartbeats go on. Default 5m. The pod's terminationGracePeriodSeconds must be this plus 30s.
	DrainDeadline time.Duration
	// KeepLeaseOnShutdown leaves the leases of runs in progress to expire when the pool
	// stops, as if the process had been killed. By default they are released so another
	// replica resumes at once. Tests set it to emulate a killed pod.
	KeepLeaseOnShutdown bool
	// Checkpoints turns on home checkpoints (checkpoint.go); nil leaves them off.
	Checkpoints *Checkpoints
	// Taint is the MCP leak guard's feed of the sandbox's own tool results (taint.go,
	// mcpguard.Feed). koinon-agentd always sets it; nil, in tests only, reports nothing.
	Taint TaintFeed
	// Tracing turns on run spans (ADR 0074); nil leaves them off.
	Tracing *Tracing
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Metrics receives the pool's metrics; nil keeps them unexported.
	Metrics *Metrics
}

func (c *Config) defaults() error {
	if c.OwnerID == "" {
		return errors.New("agentd: OwnerID is required")
	}
	if c.GatewayURL == "" {
		return errors.New("agentd: GatewayURL is required")
	}
	u, err := url.Parse(c.GatewayURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("agentd: GatewayURL must be an http or https URL, got %q", c.GatewayURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		// Arguments are visible in the sandbox: never put credentials there.
		return errors.New("agentd: GatewayURL must not carry user info, a query or a fragment")
	}
	setDefault(&c.Workers, 8)
	setDefault(&c.Lease, 30*time.Second)
	setDefault(&c.DrainDeadline, DefaultDrainDeadline)
	setDefault(&c.Heartbeat, c.Lease/3)
	setDefault(&c.Poll, time.Second)
	setDefault(&c.SandboxTimeout, orgsettings.DefaultQueueWait)
	setDefault(&c.SandboxWaitCap, orgsettings.DefaultSandboxWaitCap)
	setDefault(&c.WrapUpNotice, 5*time.Minute)
	setDefault(&c.WrapUpFraction, runlimits.WrapUpFraction)
	setDefault(&c.UsagePoll, 5*time.Second)
	if c.Now == nil {
		c.Now = time.Now
	}
	setDefault(&c.ReconcileNudge, 5*time.Second)
	setDefault(&c.ModelServeTimeout, DefaultModelServeTimeout)
	setDefault(&c.MCPRetryDelay, DefaultMCPRetryDelay)
	setDefault(&c.CancelGrace, 10*time.Second)
	setDefault(&c.ReadyTimeout, time.Minute)
	setDefault(&c.ReadySettle, time.Second)
	setDefault(&c.MaxTakeovers, 5)
	setDefault(&c.ModelRetries, DefaultModelRetries)
	setDefault(&c.MaxRefusedTurns, 5)
	setDefault(&c.RetryBackoff, DefaultRetryBackoff)
	setDefault(&c.MaxRetryAfter, DefaultMaxRetryAfter)
	setDefault(&c.GatewayRecordWait, defaultGatewayRecordWait)
	setDefault(&c.SummaryTimeout, defaultSummaryWait)
	setDefault(&c.StuckAfter, defaultStuckAfter)
	setDefault(&c.StuckPoll, defaultStuckPoll)
	setDefault(&c.SaveTimeout, defaultSaveTimeout)
	setDefault(&c.SessionCwd, "/sandbox/work")
	if c.Heartbeat >= c.Lease {
		return errors.New("agentd: Heartbeat must be shorter than Lease")
	}
	if c.Lease < time.Second {
		return errors.New("agentd: Lease must be at least 1s")
	}
	if c.Prices.IsZero() {
		return errors.New("agentd: Prices is required (prices.NewResolver)")
	}
	if c.Permissions == nil {
		c.Permissions = DefaultPermissions()
	}
	if c.Checkpoints != nil {
		if c.Checkpoints.Store == nil || c.Checkpoints.Repositories == nil {
			return errors.New("agentd: Checkpoints needs a Store and Repositories")
		}
		c.Checkpoints.defaults()
		if c.Checkpoints.Timeout >= c.Checkpoints.Lease {
			// A writer must end before its lease: the sweeper counts a running checkpoint only
			// within its lease (ListDueHomeRepositories), and another writer takes it over after.
			return errors.New("agentd: Checkpoints.Timeout must be shorter than Checkpoints.Lease")
		}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics(nil)
	}
	if c.Tracing != nil {
		if err := c.Tracing.defaults(); err != nil {
			return err
		}
	}
	return nil
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// Pool claims and drives runs.
type Pool struct {
	db      *pgxpool.Pool
	q       *store.Queries
	backend Backend
	cfg     Config
	log     *slog.Logger
	// saveSem lets one save run at a time: a bundle is up to 50 MB.
	saveSem chan struct{}
	http    *http.Client
	// sink exports run spans; nil when tracing is off.
	sink *traceSink
	// stopping is closed when Run's context ends: the pool drains. Nil (never closed) before Run.
	stopping <-chan struct{}
	// sweepRefused holds the retired repositories (org/agent/epoch) the sweep has refused and
	// logged, so each is logged once per process; the SweepRefused gauge counts them every sweep.
	// An entry goes when its row next passes the check (a fixed row is logged again if it regresses).
	sweepRefused sync.Map
}

// DefaultDrainDeadline is Config.DrainDeadline's default.
const DefaultDrainDeadline = 5 * time.Minute

// The defaults of Koinon's model-retry policy (ADR 0056 amendment 2026-10-31, point 3): up to 3
// tries, waits of 1s then 2s, and a Retry-After of up to 60s honoured.
const (
	DefaultModelRetries  = 2
	DefaultRetryBackoff  = time.Second
	DefaultMaxRetryAfter = time.Minute
)

// New returns a pool. Run starts it.
func New(db *pgxpool.Pool, backend Backend, cfg Config) (*Pool, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	p := &Pool{
		db: db, q: store.New(db), backend: backend, cfg: cfg, log: cfg.Logger,
		saveSem: make(chan struct{}, 1), http: &http.Client{Timeout: cfg.SaveTimeout},
	}
	if cfg.Tracing != nil {
		sink, err := newTraceSink(cfg.Tracing, cfg.Metrics, cfg.Logger)
		if err != nil {
			return nil, fmt.Errorf("agentd: tracing: %w", err)
		}
		p.sink = sink
	}
	return p, nil
}

// Run drives runs until ctx ends, then drains: it claims nothing more, lets the turns in
// progress finish for up to Config.DrainDeadline (the heartbeats go on), hands the runs still
// going off, and waits for the workers to stop (ADR 0085 decision 2).
func (p *Pool) Run(ctx context.Context) error {
	// runCtx drives the runs. It outlives ctx by the drain deadline.
	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()
	p.stopping = ctx.Done()
	go func() {
		select {
		case <-runCtx.Done():
			return
		case <-ctx.Done():
		}
		if p.cfg.KeepLeaseOnShutdown { // a simulated kill: no drain
			cancelRuns()
			return
		}
		p.log.Info("stopping: no new runs are claimed; turns in progress get to finish", "drain_deadline", p.cfg.DrainDeadline.String())
		t := time.NewTimer(p.cfg.DrainDeadline)
		defer t.Stop()
		select {
		case <-runCtx.Done():
		case <-t.C:
			p.log.Warn("the drain deadline passed; handing the runs in progress off", "drain_deadline", p.cfg.DrainDeadline.String())
			cancelRuns()
		}
	}()

	var wg sync.WaitGroup
	if p.cfg.Checkpoints != nil {
		wg.Go(func() { p.checkpointLoop(ctx) })
	}
	for i := range p.cfg.Workers {
		wg.Go(func() { p.work(ctx, runCtx, p.cfg.OwnerID+"/"+strconv.Itoa(i)) })
	}
	wg.Wait()
	if p.sink != nil {
		// Spans of runs that just ended; bounded, so a dead receiver cannot hold the shutdown.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.Tracing.ExportTimeout+time.Second)
		p.sink.shutdown(sctx)
		cancel()
	}
	return nil
}

// work claims and drives runs. ctx ends when the pool stops (no more claims); runCtx ends when the
// drain deadline passes (the runs in progress are handed off).
func (p *Pool) work(ctx, runCtx context.Context, owner string) {
	log := p.log.With("owner", owner)
	for ctx.Err() == nil {
		// One iteration at a time, so a panic in a run is logged and the worker goes on: the run's
		// lease expires and another worker takes it over (bounded by MaxTakeovers).
		panicked := safego.Do(log, "agentd-worker", func() {
			// Claimed with runCtx, so a claim that commits is never abandoned half-way; a run claimed
			// as the pool stops is handed off at once (execute).
			run, ok, err := p.claim(runCtx, owner)
			if err != nil && runCtx.Err() == nil {
				log.Warn("claiming a run failed", "err", err)
			}
			if !ok {
				// Jitter keeps idle workers of all replicas from polling in step.
				sleep(ctx, p.cfg.Poll/2+rand.N(p.cfg.Poll)) //nolint:gosec // jitter, not security
				return
			}
			p.drive(runCtx, run, log)
		})
		if panicked {
			// Back off after a panic, so a bad row does not spin the worker and flood the log.
			sleep(ctx, p.cfg.Poll)
		}
	}
}

// claim takes over a run whose owner's lease expired, or else claims the oldest claimable
// run, in some org. Expired runs come first: they were already running.
func (p *Pool) claim(ctx context.Context, owner string) (store.Run, bool, error) {
	// Only the orgs with a claimable or expired run: the poll must not cost a query per org that
	// ever existed (issue 378: a long test suite's database grows by an org per spec).
	orgs, err := p.q.ListOrgIDsWithRunWork(ctx)
	if err != nil {
		return store.Run{}, false, err
	}
	// Start at a random org so a busy org does not starve the others.
	off := 0
	if len(orgs) > 0 {
		off = rand.N(len(orgs)) //nolint:gosec // fairness, not security
	}
	lease := int32(p.cfg.Lease.Seconds())
	var firstErr error
	for i := range orgs {
		org := orgs[(off+i)%len(orgs)]
		r, err := p.q.TakeOverExpiredRun(ctx, store.TakeOverExpiredRunParams{OrgID: org, OwnerID: owner, LeaseSeconds: lease})
		if err == nil {
			p.cfg.Metrics.Takeovers.Inc()
			return r, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) && firstErr == nil {
			firstErr = err
		}
	}
	for i := range orgs {
		org := orgs[(off+i)%len(orgs)]
		r, err := p.q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: owner, LeaseSeconds: lease})
		if err == nil {
			p.cfg.Metrics.Claimed.Inc()
			return r, true, nil
		}
		// A unique violation is the rare race runs_one_active_per_agent backstops: another
		// worker won the agent's slot. Move on.
		var pgErr *pgconn.PgError
		lostRace := errors.As(err, &pgErr) && pgErr.Code == "23505"
		if !errors.Is(err, pgx.ErrNoRows) && !lostRace && firstErr == nil {
			firstErr = err
		}
	}
	return store.Run{}, false, firstErr
}

// sleep waits for d or until ctx ends, and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
