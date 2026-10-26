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
//   - initialize, then session/new on a ticket's first run (the session ID is stored on the
//     ticket) or session/load afterwards;
//   - for flavors that announce it, agentd waits until Koinon's MCP tools are there and
//     selects the agent's model on the session;
//   - the ticket's new messages are sent as the prompt, after the instruction file is checked
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
)

// Backend is the part of [execution.Backend] agentd uses. agentd only execs: sandbox state
// comes from koinon-reconciler through agent_sandboxes, and only the reconciler starts and
// stops sandboxes.
type Backend interface {
	Exec(ctx context.Context, workspace, sandbox string, req execution.ExecRequest) (execution.Process, error)
}

var _ Backend = execution.Backend(nil)

// Config configures a [Pool]. Zero durations and counts take the defaults below.
type Config struct {
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
	// SandboxTimeout bounds the wait for the agent's sandbox to be ready. Default 5m.
	SandboxTimeout time.Duration
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
	// GatewayURL is agentgateway's base URL as sandboxes reach it, for example
	// http://koinon-gateway.koinon.svc:8080. Flavors get <GatewayURL>/v1 for models and
	// <GatewayURL>/mcp for tools. Required; it must not carry credentials.
	GatewayURL string
	// DefaultModel is the model of agents whose version names none, or "default", when set;
	// otherwise the org's first served model is used (plan item PV1).
	DefaultModel string
	// SessionCwd is the harness session's working directory in the sandbox. Default
	// /sandbox/work.
	SessionCwd string
	// Permissions answers the harness's permission requests. Default DefaultPermissions.
	Permissions *PermissionPolicy
	// KeepLeaseOnShutdown leaves the leases of runs in progress to expire when the pool
	// stops, as if the process had been killed. By default they are released so another
	// replica resumes at once. Tests set it to emulate a killed pod.
	KeepLeaseOnShutdown bool
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
	setDefault(&c.Heartbeat, c.Lease/3)
	setDefault(&c.Poll, time.Second)
	setDefault(&c.SandboxTimeout, 5*time.Minute)
	setDefault(&c.ReconcileNudge, 5*time.Second)
	setDefault(&c.CancelGrace, 10*time.Second)
	setDefault(&c.ReadyTimeout, time.Minute)
	setDefault(&c.ReadySettle, time.Second)
	setDefault(&c.MaxTakeovers, 5)
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
	if c.Permissions == nil {
		c.Permissions = DefaultPermissions()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics(nil)
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
}

// New returns a pool. Run starts it.
func New(db *pgxpool.Pool, backend Backend, cfg Config) (*Pool, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	return &Pool{
		db: db, q: store.New(db), backend: backend, cfg: cfg, log: cfg.Logger,
		saveSem: make(chan struct{}, 1), http: &http.Client{Timeout: cfg.SaveTimeout},
	}, nil
}

// Run drives runs until ctx ends, then waits for the workers to stop.
func (p *Pool) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range p.cfg.Workers {
		wg.Go(func() { p.work(ctx, p.cfg.OwnerID+"/"+strconv.Itoa(i)) })
	}
	wg.Wait()
	return nil
}

func (p *Pool) work(ctx context.Context, owner string) {
	log := p.log.With("owner", owner)
	for ctx.Err() == nil {
		run, ok, err := p.claim(ctx, owner)
		if err != nil && ctx.Err() == nil {
			log.Warn("claiming a run failed", "err", err)
		}
		if !ok {
			// Jitter keeps idle workers of all replicas from polling in step.
			sleep(ctx, p.cfg.Poll/2+rand.N(p.cfg.Poll)) //nolint:gosec // jitter, not security
			continue
		}
		p.drive(ctx, run, log)
	}
}

// claim takes over a run whose owner's lease expired, or else claims the oldest claimable
// run, in some org. Expired runs come first: they were already running.
func (p *Pool) claim(ctx context.Context, owner string) (store.Run, bool, error) {
	orgs, err := p.q.ListOrgIDs(ctx)
	if err != nil {
		return store.Run{}, false, err
	}
	// Start at a random org so a busy org does not starve the others.
	off := 0
	if len(orgs) > 0 {
		off = rand.N(len(orgs)) //nolint:gosec // fairness, not security
	}
	lease := int32(p.cfg.Lease.Seconds()) //nolint:gosec // bounded by configuration
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
