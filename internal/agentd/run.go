// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/agentd/loopguard"
	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/agentmodel"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/orcatalog"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providertype"
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/safego"
	"github.com/stateracloud/koinon/internal/thinking"
)

// How a driven run ends, besides success (nil).
var (
	// errFenced: the worker may no longer write (another owner, a cancel, or it gave up).
	errFenced = runoutcome.ErrFenced
	// errDropped: the harness connection ended mid-run; the run is resumed by a takeover.
	errDropped = errors.New("harness connection lost")
	// errShutdown: the pool is stopping.
	errShutdown = errors.New("shutting down")
)

// failure fails the run with a reason.
type failure struct {
	reason string
	// class is the failure class (classify.go); empty means fatal.
	class store.RunFailureClass
	// retryAfter is the provider's Retry-After of a throttled request.
	retryAfter time.Duration
	// gateway: the gateway answered the harness's model request with 401 or 429 (classify.go).
	gateway bool
	// authLike: the harness reported an authentication failure; the gateway's record says who
	// refused it (providerRefusal, providerrefusal.go).
	authLike bool
	// modelNotFound: the gateway answered 404 or model_not_found for the run's model.
	modelNotFound bool
	// sandbox is set when the sandbox never became ready within the org's cap (sandbox.go).
	sandbox *sandboxUnavailable
}

// sandboxUnavailable is why and how long a sandbox never became ready.
type sandboxUnavailable struct {
	cause  execution.SandboxWaitCause
	detail string
	waited time.Duration
	// cap is the org's sandbox wait cap the wait applied, read once; every text about the failure
	// uses it, so they all agree with the run's failure reason.
	cap time.Duration
	// causeSeen is how long after the start of the wait the sandbox row first named cause, when
	// causeWasSeen.
	causeSeen    time.Duration
	causeWasSeen bool
}

func (f *failure) Error() string { return f.reason }

// requeued sends the run back to the queue: the sandbox did not become ready in time (ADR 0066).
type requeued struct{ reason string }

func (q *requeued) Error() string { return q.reason }

// supersededRun ends the run as succeeded without a turn: its messages were read already (#21).
type supersededRun struct{ reason string }

func (e *supersededRun) Error() string { return e.reason }

// canceled cancels the run as its owner, for example at the wall-clock cap, or on a spent
// budget (budgetstop.go).
type canceled struct {
	reason string
	// class is the failure class recorded with the cancel; empty for none.
	class store.RunFailureClass
	// extra is merged into the payload of the run's closing events.
	extra map[string]any
}

func (c *canceled) Error() string { return c.reason }

// fence says why a worker stopped being allowed to write (internal/runoutcome).
type fence = runoutcome.Fence

const (
	fenceLost     = runoutcome.FenceLost     // a newer epoch owns the run, or it finished
	fenceCanceled = runoutcome.FenceCanceled // the control plane canceled the run
	fenceAbandon  = runoutcome.FenceAbandon  // the worker cannot reach the database and gives up
	fenceFailed   = runoutcome.FenceFailed   // a permanent error: the run fails with runner.feedFailure
)

// resumeNote opens the prompt of a run resumed after the harness connection was lost.
const resumeNote = "[Koinon] The connection to you was interrupted during your last turn, and " +
	"your session has been reloaded. Continue the task from where you stopped. Do not repeat " +
	"actions that already succeeded."

// Exit codes of a launch command that will not get better on retry: usage error (the entry
// script's), not executable, not found.
var permanentExit = map[int]bool{64: true, 126: true, 127: true}

// runner drives one run under one epoch.
type runner struct {
	p   *Pool
	q   *store.Queries
	log *slog.Logger
	run store.Run

	// own writes the run's events and ends it (internal/runoutcome): the source seq, the write
	// mutex and the fence state live there.
	own *runoutcome.Owner
	// res is how the turns ended, when execute got as far as the turn and the policy ended it with
	// anything but end_turn. The run's ending is then res, not an error.
	res *turnResult
	// window is the model's context window from the catalog (0 unknown), and overflow the inputs of
	// the prompt over the cap (overflow.go).
	window   int64
	overflow *inputOverflow
	// turnState is the run's phase for a draining pool (turnSetup, turnRunning, turnHandedOff).
	turnState atomic.Int32

	replaying atomic.Bool

	// loop reads the harness's stream for a stuck agent (internal/agentd/loopguard); the turn driver
	// acts on what it finds.
	loop *loopguard.Detector

	toolsMu sync.Mutex
	tools   map[string]chan struct{} // tool call IDs; closed once the tool call is recorded

	// taint follows tool calls for the leak guard's feed (taint.go).
	taint taintCalls
	// feedCounts are the current harness's counts for the leak guard (taint.go); nil without a feed.
	feedCounts atomic.Pointer[feedCounts]
	// feedFailure is why the run fails when the feed met a permanent error (feedGiveUp).
	feedFailure atomic.Pointer[failure]

	// delivered are the approval requests whose outcomes this turn's prompt carries; they are
	// marked delivered when the turn ends.
	delivered []pgtype.UUID
	// deliveredQ are the resolved questions whose outcomes this turn's prompt carries.
	deliveredQ []pgtype.UUID

	// refusals are the permission requests agentd rejected in the turn in progress (refusal.go).
	refMu    sync.Mutex
	refusals []refusal

	// cwd is the session directory, set before the harness starts; permission requests for
	// paths outside it are rejected.
	cwd string
	// sbWorkspace and sbName are the sandbox the harness runs in, set with cwd; permission
	// requests resolve their paths in it.
	sbWorkspace, sbName string

	// model and modelSource are the model the run uses and the layer it came from
	// (internal/agentmodel).
	model       string
	modelSource agentmodel.Source
	// harness is the agent's harness, set with the launch command.
	harness string

	// vcfg and home are the run's version config and the sandbox's home generation, for
	// handling a stuck sandbox (stuck.go).
	vcfg agentconfig.Config
	home pgtype.Text
	// sandboxID is the OpenShell ID of the sandbox the run uses (the home generation is the
	// home's lineage and outlives sandboxes restored from checkpoints).
	sandboxID string

	// sum is the summary prompt in progress (summary.go), nil otherwise.
	sumMu sync.Mutex
	sum   *summaryState

	// tr is the run's tracing state; nil when the run is not traced (runtrace.go).
	tr *runTrace

	cmdMu      sync.Mutex
	commands   []string      // the harness's last available_commands_update
	cmdChanged chan struct{} // closed and replaced on every available_commands_update
}

// setCommands records the commands the harness announced.
func (r *runner) setCommands(names []string) {
	r.cmdMu.Lock()
	defer r.cmdMu.Unlock()
	r.commands = names
	close(r.cmdChanged)
	r.cmdChanged = make(chan struct{})
}

// waitCommand waits until the harness announces command, the connection ends or the
// timeout passes.
func (r *runner) waitCommand(ctx context.Context, conn *acp.Conn, command string, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		r.cmdMu.Lock()
		ok, changed := slices.Contains(r.commands, command), r.cmdChanged
		r.cmdMu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-conn.Done():
			return errors.New("the connection ended")
		case <-t.C:
			return &failure{reason: fmt.Sprintf("Koinon's MCP tools did not become available: the harness did not "+
				"announce %q within %s of starting the session (is agentgateway's MCP route reachable from the sandbox?)",
				command, timeout)}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// toolSeen returns a channel that is closed once the harness's update for the tool call has
// been recorded.
func (r *runner) toolSeen(id string) chan struct{} {
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	if r.tools == nil {
		r.tools = map[string]chan struct{}{}
	}
	ch, ok := r.tools[id]
	if !ok {
		ch = make(chan struct{})
		r.tools[id] = ch
	}
	return ch
}

func (r *runner) markTool(id string) {
	ch := r.toolSeen(id)
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// Phases of a run's turn, for a draining pool: set up (a stop hands the run off), running (a stop
// waits for the turn, up to the drain deadline), or handed off during the setup.
const (
	turnSetup int32 = iota
	turnRunning
	turnHandedOff
)

func (p *Pool) drive(ctx context.Context, run store.Run, log *slog.Logger) {
	r := &runner{
		p: p, q: p.q, run: run, cmdChanged: make(chan struct{}), loop: loopguard.New(p.cfg.LoopThresholds),
		log: log.With("org", run.OrgID.String(), "run", run.ID.String(), "epoch", run.Epoch),
	}
	// The text the agent types reaches watchers live, batched; only the completed items are stored (old issue 344).
	live := runwatch.NewChunkPublisher(p.db, run.OrgID, run.ID, 0, r.log)
	defer live.Close()
	r.own = runoutcome.New(runoutcome.Config{
		DB: p.db, Queries: p.q, Run: run, Log: r.log, Now: p.cfg.Now,
		Metrics: outcomeMetrics{p.cfg.Metrics}, Stop: r.stopped, Live: live.Add, LiveFlush: live.Flush,
	})
	r.log.Info("driving run", "state", run.State, "takeovers", run.Takeovers)
	r.tr = p.traceStart(ctx, run, r.own.Fenced(), r.log)

	hctx, stop := context.WithCancel(ctx)
	var hb sync.WaitGroup
	hb.Go(func() { r.heartbeat(hctx) })
	// A panic in the run becomes a failure: the heartbeat stops and the run concludes as failed on
	// its own path, instead of keeping a live lease no other worker would ever take over.
	var err error
	if safego.Do(r.log, "run", func() { err = r.execute(ctx) }) {
		err = &failure{reason: "internal error: the run panicked"}
	}
	if r.turnState.Load() == turnHandedOff && !errors.Is(err, errFenced) {
		// The pool stopped before the turn began and cut the run's setup short: whatever error that
		// caused, the run goes back to the queue.
		err = errShutdown
	}
	stop()
	hb.Wait()
	// The turn's row ends with the run's ending, inside the transaction of the verb that ends it
	// (internal/runoutcome), so whoever sees the run finished sees the turn ended. The trace's spans
	// end first only so that they are exported promptly.
	end := ending{err: err, res: r.res}
	turn := r.tr.turnRow(end, r.own.FenceKind())
	r.tr.finish(ctx, end, r.own.FenceKind())
	r.conclude(ctx, end, turn)
}

// heartbeat renews the lease until ctx ends. A heartbeat that matches no rows means the run
// was canceled or taken over; one that cannot reach the database for a whole lease means
// another worker may already own the run.
func (r *runner) heartbeat(ctx context.Context) {
	t := time.NewTicker(r.p.cfg.Heartbeat)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.own.Fenced():
			return
		case <-t.C:
		}
		_, err := r.q.HeartbeatRun(ctx, store.HeartbeatRunParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, LeaseSeconds: r.p.leaseSeconds(),
		})
		switch {
		case err == nil:
			last = time.Now()
		case errors.Is(err, pgx.ErrNoRows):
			r.own.FenceFromDB(ctx)
			return
		case ctx.Err() != nil:
			return
		case time.Since(last) >= r.p.cfg.Lease:
			r.log.Warn("lease lost: heartbeats failed", "err", err)
			r.own.Fence(fenceAbandon)
			return
		default:
			r.log.Warn("heartbeat failed", "err", err)
		}
	}
}

func (p *Pool) leaseSeconds() int32 {
	return int32(p.cfg.Lease.Seconds())
}

// stopped returns errFenced or errShutdown when either applies.
func (r *runner) stopped(ctx context.Context) error {
	switch {
	case r.own.IsFenced():
		return errFenced
	case r.turnState.Load() == turnHandedOff:
		// The pool stopped while the run was still setting up and cut it short.
		return errShutdown
	case ctx.Err() != nil:
		return errShutdown
	}
	return nil
}

// modelGone turns the gateway's 404 for the agent's own model into the reason a run that began with
// the model gone gets: the check at the start passed, and the provider or its price was removed
// before the request. Any other error, and a model that still checks out, stay as they are.
func (r *runner) modelGone(ctx context.Context, err error) error {
	var f *failure
	if r.modelSource != agentmodel.SourceAgent || !errors.As(err, &f) || !f.modelNotFound {
		return err
	}
	var unusable *agentmodel.UnusableError
	if cerr := agentmodel.Check(ctx, r.p.cfg.Prices, r.q, r.run.OrgID, r.model, time.Now()); errors.As(cerr, &unusable) {
		return &failure{reason: unusable.RunReason()}
	}
	return err
}

// dbErr classifies a database error of the run's own writes and reads.
func (r *runner) dbErr(ctx context.Context, what string, err error) error {
	return r.own.Err(ctx, what, err)
}

// execute drives the run until its turn ends.
func (r *runner) execute(ctx context.Context) error {
	cfg := r.p.cfg
	if int(r.run.Takeovers) > cfg.MaxTakeovers {
		return &failure{reason: fmt.Sprintf("the harness connection was lost %d times", r.run.Takeovers)}
	}

	// fctx ends when the worker is fenced, which aborts whatever it waits for. The prompt
	// turn uses ctx instead, so that a cancel can still be sent over ACP.
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		stopping := r.p.stopping
		for {
			select {
			case <-r.own.Fenced():
				cancel()
				return
			case <-stopping:
				// The pool is draining (ADR 0085 decision 2): a run still setting up has no turn to
				// finish, so it is handed off at once. A turn that began finishes up to the deadline,
				// and keeps watching for a fence meanwhile.
				stopping = nil
				if r.turnState.CompareAndSwap(turnSetup, turnHandedOff) {
					r.log.Info("the pool is stopping before the turn began; handing the run off")
					cancel()
					return
				}
			case <-fctx.Done():
				return
			}
		}
	}()

	agent, err := r.q.GetAgent(fctx, store.GetAgentParams{OrgID: r.run.OrgID, ID: r.run.AgentID})
	if err != nil {
		return r.dbErr(ctx, "load agent", err)
	}
	// ClaimNextRun skips unhealthy agents, so only a run claimed just before the mark gets here; a
	// resumed (parked) run is not failed for it: its sandbox work and approvals are intact.
	if agent.UnhealthyAt.Valid && !r.run.PromptThroughSeq.Valid {
		return &failure{reason: "the agent is unhealthy and will not run until an admin resumes it: " + agent.UnhealthyReason.String}
	}
	version, err := r.q.GetAgentVersion(fctx, store.GetAgentVersionParams{OrgID: r.run.OrgID, ID: r.run.AgentVersionID})
	if err != nil {
		return r.dbErr(ctx, "load agent version", err)
	}
	task, err := r.q.GetTask(fctx, store.GetTaskParams{OrgID: r.run.OrgID, ID: r.run.TaskID})
	if err != nil {
		return r.dbErr(ctx, "load task", err)
	}
	if reason, err := r.superseded(fctx, task); err != nil {
		return err
	} else if reason != "" {
		return &supersededRun{reason: reason}
	}
	// An unreadable config would leave the run without its instructions (and model): it fails
	// before anything starts.
	vc, err := versionConfig(version.Config)
	if err != nil {
		return &failure{reason: "the agent version's config is unreadable: " + err.Error()}
	}
	// The agent's own setting wins over the version's model, which wins over the org default
	// (internal/agentmodel). A set model that is no longer served or priced fails the run: falling
	// back would run the agent on a model nobody chose.
	model, source := agentmodel.Choose(agent.Model.String, versionModel(vc), cfg.DefaultModel)
	if source == agentmodel.SourceAgent {
		var unusable *agentmodel.UnusableError
		switch err := agentmodel.Check(fctx, r.p.cfg.Prices, r.q, r.run.OrgID, model, time.Now()); {
		case errors.As(err, &unusable):
			return &failure{reason: unusable.RunReason()}
		case err != nil:
			return r.dbErr(ctx, "check the agent's model", err)
		}
	}
	if model == "" {
		// The org's stored default model (old issue 256); it is never derived from what the org serves.
		var err error
		if model, err = agentmodel.OrgDefault(fctx, r.q, r.run.OrgID); err != nil {
			return r.dbErr(ctx, "load the org's default model", err)
		}
		source = agentmodel.SourceOrgDefault
		if model == "" {
			return &failure{reason: agentmodel.NoDefaultMessage}
		}
		var unusable *agentmodel.UnusableError
		switch err := agentmodel.Check(fctx, r.p.cfg.Prices, r.q, r.run.OrgID, model, time.Now()); {
		case errors.As(err, &unusable):
			return &failure{reason: "the org's default model is not usable: " + unusable.Error() + "; set another default model"}
		case err != nil:
			return r.dbErr(ctx, "check the org's default model", err)
		}
	}
	r.model, r.modelSource = model, source
	r.tr.identify(agent, version, task, model)
	gw := strings.TrimSuffix(cfg.GatewayURL, "/")
	facts, err := r.modelFacts(fctx, model)
	if err != nil {
		// Intended: a database error here fails the run start (and is retried by dbErr's rules)
		// instead of running without the model's format and prompt-cache mode, which would quietly
		// turn prompt caching off (ADR 0068, #398). A missing or unusable price is not an error here.
		return r.dbErr(ctx, "look up the model", err)
	}
	r.log.Info("the run's model", "model", model, "source", string(source), "format", string(facts.Format))
	ptype, format := facts.ProviderType, facts.Format
	r.window = facts.ContextWindow
	marker, unknown := cacheMarker(facts.Cache)
	if unknown {
		r.log.Warn("the model has a cache price but its provider format cannot be cached by Koinon; it runs without prompt caching", "model", model)
	}
	// OpenRouter keeps a session on one upstream, so the run's cache stays warm. Only the run ID
	// is sent, and only to OpenRouter.
	sessionID := ""
	if ptype == "openrouter" {
		sessionID = r.run.ID.String()
	}
	// Koinon decides whether the model thinks, from the catalog and the agent's setting; the harness
	// renders that and never guesses from the model ID (ADR 0069 amendment 2026-10-29).
	thinkMode := thinking.Of(ptype, model, format).Effective(agentmodel.ChooseThinking(agent.Thinking.String, vc.Thinking))
	r.log.Info("the run's thinking", "model", model, "mode", string(thinkMode))
	cmd, err := launchCommand(agent.Harness, endpoints{
		ModelBaseURL: gw + "/v1", Model: model, MCPURL: gw + "/mcp", Format: format, CacheMarker: marker,
		Thinking: thinkMode, SessionID: sessionID, Retry: retryPolicyOf(r.p.cfg), CompactionTokens: facts.CompactionTokens,
	})
	if err != nil {
		return &failure{reason: err.Error()}
	}
	cmd = append(cmd, harnessArgs(agent.Harness, vc)...)
	r.vcfg = vc
	r.harness = agent.Harness
	r.taint.setMCPPrefix(mcpToolPrefix(agent.Harness))
	// A code-host connection whose credential was lost fails the run at once, before the waits for
	// the model and the sandbox: nothing it would wait for can bring the credential back.
	if err := r.checkCodeHostCredential(fctx); err != nil {
		return r.execErr(ctx, "code-host connection", err)
	}
	// The gateway may not serve the model yet: a new provider's key file is still reaching the
	// gateway pod (old issue 174). The reconciler starts the sandbox meanwhile, because this run is
	// demand for it, so the two waits overlap.
	if err := r.waitModel(fctx, model); err != nil {
		return r.execErr(ctx, "wait for the model", err)
	}
	sb, err := r.waitSandbox(fctx)
	if err != nil {
		return r.execErr(ctx, "sandbox", err)
	}
	// An allowed MCP server that is not serving tools is recorded, retried once and then fails the
	// run (mcpwait.go, old issues 326 and 488). It runs once the sandbox is ready: the live check
	// uses the agent's key, which exists by then.
	if err := r.waitMCPServers(fctx); err != nil {
		return r.execErr(ctx, "check the agent's MCP servers", err)
	}
	// Nothing of an earlier run may still be running when this one starts (sweep.go); a
	// sweep that cannot end everything fails the run, unless processes that will not die make
	// the sandbox stuck: it is then recreated (stuck.go) and the run waits for the new one.
	var main string
	var ended []string
	for attempt := 0; ; attempt++ {
		r.home = sb.HomeGeneration
		r.sandboxID = sb.SandboxID.String
		main, ended, err = r.sweep(fctx, sb.Workspace, sb.SandboxName)
		var sv *survivorsError
		if !errors.As(err, &sv) {
			break
		}
		out, herr := r.handleSurvivors(ctx, sb.Workspace, sb.SandboxName, sv)
		if herr != nil {
			return r.execErr(ctx, "handle the stuck sandbox", herr)
		}
		switch {
		case out == stuckFenced:
			return errFenced
		case out == stuckUnhealthy:
			return &failure{reason: "the agent is unhealthy: its sandbox got stuck again and was not recreated; an admin must resume the agent"}
		case attempt >= 2, out == stuckUnknown:
			return &failure{reason: sv.Error()}
		}
		if out == stuckRecreate {
			if sb, err = r.waitSandbox(fctx); err != nil {
				return r.execErr(ctx, "sandbox", err)
			}
		}
	}
	if err != nil {
		return r.execErr(ctx, "end the processes left in the sandbox", err)
	}
	workspace, sandbox := sb.Workspace, sb.SandboxName
	log := r.log.With("sandbox", sandbox)
	if unknown {
		// Visible on the task timeline: the run is costlier than the price list suggests.
		if err := r.own.Record(fctx, "run.uncached", map[string]any{"model": model, "reason": prices.UncachedReason(ptype, model)}); err != nil {
			return r.dbErr(ctx, "record the uncached run", err)
		}
	}
	if len(ended) > 0 {
		log.Warn("ended processes left in the sandbox", "pids", ended, "main", main)
		if err := r.own.Record(fctx, "run.sandbox_swept", map[string]any{"pids": ended, "main": main}); err != nil {
			return r.dbErr(ctx, "record the sweep", err)
		}
	}
	// A sandbox the reconciler recreated from a home checkpoint gets the home back before the
	// harness starts (checkpoint.go), so the task's session is there for session/load.
	if err := r.restoreHome(fctx, sb); err != nil {
		return r.execErr(ctx, "restore the home", err)
	}
	// The harness keeps its sessions in the sandbox's home volume, which OpenShell deletes with
	// the sandbox. The task's session is loaded only in the home it was created in; after a
	// recreated sandbox (or for a session recorded before generations existed) the run starts a
	// new session instead of failing on session/load.
	//
	// A task whose agent version was activated with "apply to open tasks" starts a new session
	// too, with a handoff (handoff.go); that takes precedence over a reset, whose prompt carries no
	// summary.
	restart := task.HarnessSessionID.Valid && task.SessionRestartPending
	loaded := task.HarnessSessionID.Valid && !restart && sameHome(task.HarnessSessionGeneration, sb.HomeGeneration)
	reset := task.HarnessSessionID.Valid && !loaded && !restart
	cwd := cfg.SessionCwd
	if loaded {
		cwd = task.HarnessSessionCwd.String
	}
	if err := r.mkdir(fctx, workspace, sandbox, cwd); err != nil {
		return r.execErr(ctx, "prepare the session directory", err)
	}
	r.cwd = cwd
	r.sbWorkspace, r.sbName = workspace, sandbox
	// The run's own version decides the instructions, so a version bump applies at the next
	// run without recreating the sandbox.
	repos, err := r.repositories(fctx)
	if err != nil {
		return r.execErr(ctx, "read the code host of the agent's repositories", err)
	}
	instructions := instructionsFile(agent.Harness, vc.Instructions, cwd, repos, vc.WebEgress())
	if err := r.writeInstructions(fctx, workspace, sandbox, instructions); err != nil {
		return r.execErr(ctx, "write the instructions", err)
	}
	if err := r.writeGitConfig(fctx, workspace, sandbox); err != nil {
		return r.execErr(ctx, "write the git configuration", err)
	}

	var stderr tail
	proc, err := r.p.backend.Exec(ctx, workspace, sandbox, execution.ExecRequest{
		Command: cmd, Workdir: cwd, Stderr: &stderr,
	})
	if err != nil {
		return r.execErr(ctx, "exec the harness", err)
	}
	// The sweep runs after the harness is closed and ends whatever the harness or the agent
	// left running. It must end before the run gives up the agent's active slot
	// (runs_one_active_per_agent), or the agent's next run could start its harness first and
	// have it ended: on end_turn below, before the run is finished; otherwise deferred, before
	// conclude finishes or releases the run.
	// conn is set once the harness is started; closeHarness first frees a reader held back by a
	// lagging handler, which closing the process alone does not reach.
	var conn *acp.Conn
	closeHarness := sync.OnceFunc(func() {
		if conn != nil {
			conn.Close()
		}
		closeProcess(proc)
	})
	sweepOnce := sync.OnceFunc(func() { r.sweepAfter(ctx, workspace, sandbox) })
	defer sweepOnce()
	defer closeHarness()
	if err := r.feedStart(fctx); err != nil {
		if errors.Is(err, mcpguard.ErrFeedFenced) {
			// A newer epoch already started the run's feed: another worker owns the run, as when the
			// feed's writer finds it fenced (taint.go). Nothing is recorded; that worker drives it.
			r.log.Warn("the leak guard's feed belongs to a newer worker of the run; this worker stops")
			r.own.Fence(fenceLost)
			return errDropped
		}
		if code, ok := runoutcome.PermanentSQLState(err); ok {
			// Another worker would meet the same refusal: fail once instead of one takeover per worker.
			return &failure{reason: feedFailReason(code), class: classFatal}
		}
		// The leak guard could not be told this worker's epoch: resume later rather than run unwatched.
		r.interrupted(ctx, "start the leak guard's feed: "+err.Error(), nil, "")
		return errDropped
	}
	conn = acp.NewConn(fctx, handler{r: r}, proc.Stdin(), proc.Stdout())
	conn.SetLogger(log)
	harnessErr := func(what string, err error) error {
		return r.harnessErr(ctx, conn, proc, &stderr, what, err)
	}

	init, err := conn.Initialize(fctx)
	if err != nil {
		return harnessErr("initialize", err)
	}
	var session acp.SessionID
	if loaded {
		if !init.AgentCapabilities.LoadSession {
			return &failure{reason: "the harness cannot load the task's session (no loadSession capability)"}
		}
		session = acp.SessionID(task.HarnessSessionID.String)
		// The harness replays the session's history as updates before it answers. They are
		// already recorded (or belong to earlier runs), so they are not written again.
		r.replaying.Store(true)
		err = conn.LoadSession(fctx, session, cwd)
		r.replaying.Store(false)
		if err != nil {
			return harnessErr("session/load", err)
		}
	} else {
		if reset {
			log.Info("the sandbox home was replaced; starting a new session",
				"previous_session", task.HarnessSessionID.String,
				"session_generation", task.HarnessSessionGeneration.String, "home_generation", sb.HomeGeneration.String)
		}
		if session, err = conn.NewSession(fctx, cwd); err != nil {
			return harnessErr("session/new", err)
		}
		if err := r.storeSession(fctx, task, session, cwd, sb.HomeGeneration, reset, restart); err != nil {
			return r.dbErr(ctx, "store the session", err)
		}
	}
	if command := readyCommand(agent.Harness); command != "" {
		if err := r.waitCommand(fctx, conn, command, cfg.ReadyTimeout); err != nil {
			var f *failure
			if errors.As(err, &f) {
				return f
			}
			return harnessErr("wait for the MCP tools", err)
		}
		if !sleep(fctx, cfg.ReadySettle) {
			return cmp.Or(r.stopped(ctx), errShutdown)
		}
	}
	// The harness may report a model of its own choosing; agentd selects the agent's model
	// and never relies on what the session reports.
	if value := modelOption(agent.Harness, format, model); value != "" {
		if err := conn.SetModel(fctx, session, value); err != nil {
			return harnessErr("select the model "+value, err)
		}
	}
	if r.run.State == store.RunStateStarting {
		if _, err := r.q.MarkRunRunning(fctx, store.MarkRunRunningParams{OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch}); err != nil {
			return r.dbErr(ctx, "mark the run running", err)
		}
	}
	if err := r.own.Record(fctx, "run.session", map[string]any{
		"sessionId": session, "loaded": loaded, "reset": reset, "restart": restart, "takeovers": r.run.Takeovers,
		"agent": init.AgentInfo, "model": model,
		"instructions": map[string]any{"path": instructionsPath, "sha256": sha256Hex(instructions)},
	}); err != nil {
		return r.dbErr(ctx, "record the session", err)
	}

	text, through, err := r.prompt(fctx, task, loaded, reset, restart)
	if err != nil {
		return r.dbErr(ctx, "build the prompt", err)
	}
	if r.overflow != nil {
		// An input is over the cap: keep it, ask a person and park; the prompt is not sent.
		if err := r.parkOnOverflow(fctx, task, vc, closeHarness, sweepOnce); err != nil {
			var f *failure
			if errors.As(err, &f) {
				return err
			}
			return r.dbErr(ctx, "park the run on an input over the context cap", err)
		}
		return nil
	}
	if _, err := r.q.SetRunPromptThrough(fctx, store.SetRunPromptThroughParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, PromptThroughSeq: through,
	}); err != nil {
		return r.dbErr(ctx, "record the prompt", err)
	}
	if err := r.own.Record(fctx, "run.prompt", map[string]any{
		"text": text, "throughSeq": through, "resumed": r.run.PromptThroughSeq.Valid,
	}); err != nil {
		return r.dbErr(ctx, "record the prompt", err)
	}
	// The agent can write its home, instruction file included; the prompt goes out only with
	// the run's instructions in place.
	if err := r.checkInstructions(fctx, workspace, sandbox, agent.Harness, instructions); err != nil {
		return r.execErr(ctx, "check the instructions", err)
	}
	if !r.turnState.CompareAndSwap(turnSetup, turnRunning) {
		return errShutdown // the pool began to stop first
	}
	res, err := r.turnWithRetry(ctx, fctx, conn, proc, &stderr, session, text)
	if err != nil {
		return err
	}
	if res.out.Kind != turnpolicy.OutEndTurn {
		// The run ends with the policy's outcome, which conclude carries out.
		r.res = res
		return nil
	}
	// The agent's note for the task, before the harness is let go.
	r.summarize(ctx, conn, session)
	closeHarness()
	sweepOnce()
	// Finishes the run, or parks it on its approvals.
	return r.endTurn(ctx)
}

// kindSessionReset is the task event of a run that started a new session because the task's
// session was stored in a home volume the sandbox no longer has.
const kindSessionReset = "run.session_reset"

// sessionResetMessage is the session reset event's message, for the task's timeline.
const sessionResetMessage = "The agent's workspace was reset, so this run starts a new session."

// sameHome reports whether a session stored with generation lives in the sandbox's current home.
// An unknown generation on either side never matches.
func sameHome(generation, home pgtype.Text) bool {
	return generation.Valid && home.Valid && generation.String == home.String
}

// storeSession records a new session on the task with the home generation it lives in and,
// when it replaces a session the home lost, a task event that says so.
func (r *runner) storeSession(ctx context.Context, task store.Task, session acp.SessionID, cwd string, home pgtype.Text, reset, restart bool) error {
	return pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if _, err := q.SetTaskHarnessSession(ctx, store.SetTaskHarnessSessionParams{
			OrgID: r.run.OrgID, ID: task.ID, RunID: r.run.ID, Epoch: r.run.Epoch,
			HarnessSessionID: string(session), HarnessSessionCwd: cwd, HarnessSessionGeneration: home,
		}); err != nil {
			return err
		}
		kind, message := kindSessionReset, sessionResetMessage
		switch {
		case restart:
			kind, message = kindSessionRestarted, sessionRestartedMessage
		case !reset:
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"message": message, "previousSessionId": task.HarnessSessionID.String, "sessionId": string(session),
		})
		if err != nil {
			return err
		}
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: r.run.OrgID, TaskID: task.ID, RunID: r.run.ID,
			Kind: kind, ActorKind: store.TaskActorKindSystem, Payload: payload,
		})
		return err
	})
}

// execErr classifies an error of the execution backend.
func (r *runner) execErr(ctx context.Context, what string, err error) error {
	if stop := r.stopped(ctx); stop != nil {
		return stop
	}
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	var rq *requeued
	if errors.As(err, &rq) {
		return rq
	}
	if errors.Is(err, execution.ErrUnavailable) || errors.Is(err, execution.ErrFailedPrecondition) ||
		errors.Is(err, execution.ErrStreamClosed) {
		// The gateway or the sandbox is not reachable right now: resume later.
		r.interrupted(ctx, what+": "+err.Error(), nil, "")
		return errDropped
	}
	return &failure{reason: what + ": " + err.Error()}
}

// harnessErr classifies an error of an ACP call. If the connection ended, the harness exited
// or the stream dropped: the run is resumed, unless the launch command itself is broken.
func (r *runner) harnessErr(ctx context.Context, conn *acp.Conn, proc execution.Process, stderr *tail, what string, err error) error {
	if stop := r.stopped(ctx); stop != nil {
		return stop
	}
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		// The connection is alive: the harness answered with an error.
		return classFailure(what, err)
	}
	// Record what the harness sent before the drop first, so the interruption comes last.
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if !conn.WaitUpdates(wctx) {
		r.log.Warn("not every session update was delivered before the connection ended")
	}
	cancel()
	if errors.Is(conn.ReadErr(), acp.ErrLineTooLong) {
		// The harness wrote a line the protocol reader refuses. It is still blocked writing to a
		// pipe nobody reads, and a resumed harness would write the same line: end it, and fail.
		closeProcess(proc)
		r.own.FlushItems(ctx, true)
		return &failure{
			reason: lineTooLongReason(stderr.String()),
			class:  classFatal,
		}
	}
	code, werr := waitProcess(proc, harnessExitWait)
	if werr == nil && permanentExit[code] {
		return &failure{reason: fmt.Sprintf("the harness exited with code %d: %s", code, strings.TrimSpace(stderr.String()))}
	}
	reason := what + ": " + err.Error()
	var exit *int
	if werr != nil {
		reason = what + ": " + werr.Error()
	} else {
		exit = &code
	}
	r.interrupted(ctx, reason, exit, stderr.String())
	return errDropped
}

// lineTooLongReason is the failure text of a harness that wrote a line over the limit, with the
// tail of its stderr when it has one.
func lineTooLongReason(stderr string) string {
	reason := "harness output line too long: the harness wrote a line of more than 10 MiB without a newline"
	if t := strings.TrimSpace(stderr); t != "" {
		reason += ": " + t
	}
	return reason
}

// harnessExitWait is how long agentd waits for a harness to exit after its connection ended
// before it closes the exec stream.
const harnessExitWait = 10 * time.Second

// waitProcess waits for proc to exit, at most d. A harness that does not exit (it can be blocked
// writing to a pipe nobody reads) has its stream closed, which ends the command; then the wait
// is bounded once more. The error says so when the process never reported an exit.
func waitProcess(proc execution.Process, d time.Duration) (int, error) {
	type result struct {
		code int
		err  error
	}
	res := make(chan result, 1)
	go func() {
		code, err := proc.Wait()
		res <- result{code, err}
	}()
	select {
	case r := <-res:
		return r.code, r.err
	case <-time.After(d):
	}
	// Close can block on the same signal as Wait (an OpenShell exec), so it must not hold the bound.
	go func() { _ = proc.Close() }()
	select {
	case r := <-res:
		if r.err == nil {
			r.err = errors.New("the harness did not exit after its connection ended; its stream was closed")
		}
		return r.code, r.err
	case <-time.After(d):
		return 0, errors.New("the harness did not exit after its connection ended, and closing its stream did not end it")
	}
}

// interrupted records why the harness connection was lost, for the run's timeline.
func (r *runner) interrupted(ctx context.Context, reason string, exit *int, stderr string) {
	r.log.Warn("harness connection lost", "reason", reason, "exit", exit)
	payload := map[string]any{"reason": reason}
	if exit != nil {
		payload["exitCode"] = *exit
	}
	if stderr != "" {
		payload["stderrTail"] = stderr
	}
	r.own.FlushItems(ctx, true) // a message the harness was in the middle of is cut off
	if err := r.own.Record(ctx, "run.interrupted", payload); err != nil {
		r.log.Warn("recording the interruption failed", "err", err)
	}
}

// prompt builds the turn's prompt from the task's messages that no earlier run sent, and
// returns it with the timeline seq it covers. A run resumed after a takeover was already
// prompted: it gets the resume note plus anything that arrived since. A new session that
// replaces one the sandbox home lost (reset) knows nothing of the task, so it gets the
// task's title and every message, plus the run's approval outcomes not sent yet.
func (r *runner) prompt(ctx context.Context, task store.Task, loaded, reset, restart bool) (string, int64, error) {
	resumed := r.run.PromptThroughSeq.Valid
	after := r.run.PromptThroughSeq.Int64
	switch {
	case reset:
		after = 0
	case !resumed:
		var err error
		after, err = r.q.GetTaskPromptedThrough(ctx, store.GetTaskPromptedThroughParams{
			OrgID: r.run.OrgID, TaskID: task.ID, RunID: r.run.ID,
		})
		if err != nil {
			return "", 0, err
		}
	}
	msgs, err := r.q.ListTaskMessagesAfter(ctx, store.ListTaskMessagesAfterParams{
		OrgID: r.run.OrgID, TaskID: task.ID, AfterSeq: after,
	})
	if err != nil {
		return "", 0, err
	}
	outcomes, err := r.outcomes(ctx)
	if err != nil {
		return "", 0, err
	}
	var notice string
	if reset || !loaded {
		if notice, err = r.mcpNotice(ctx); err != nil {
			return "", 0, err
		}
	}
	if restart {
		return r.handoffPrompt(ctx, task, outcomes, notice)
	}
	handled, err := r.q.ListTaskInputArtifacts(ctx, store.ListTaskInputArtifactsParams{OrgID: r.run.OrgID, TaskID: task.ID})
	if err != nil {
		return "", 0, err
	}
	kept := make(map[int64]bool, len(handled))
	ownKept := false
	for _, a := range handled {
		kept[a.MessageSeq] = true
		ownKept = ownKept || a.RunID == r.run.ID
	}
	var texts []string
	var seqs []int64
	through := after
	for _, m := range msgs {
		through = m.Seq
		if kept[m.Seq] {
			continue // the context guard kept this one whole and asked; it is not sent
		}
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Payload, &body) == nil && strings.TrimSpace(body.Text) != "" {
			texts = append(texts, body.Text)
			seqs = append(seqs, m.Seq)
		}
	}
	mode := promptNew
	switch {
	case reset:
		mode = promptReset
	// A claimed parked run is starting with its prompt already recorded: it resumes after
	// its approvals were decided. A takeover keeps the state it had.
	case resumed && r.run.State == store.RunStateStarting:
		mode = promptParkResume
		if ownKept {
			// This run parked on an input over the cap before anything was sent: the agent has not
			// seen the task yet.
			mode = promptParkResumeFresh
		}
	case resumed:
		mode = promptTakeover
	case loaded:
		mode = promptContinue
	}
	if len(handled) > 0 && (reset || mode == promptParkResume || mode == promptParkResumeFresh) {
		r.rehydrate(ctx, handled)
	}
	// A reset reads from the start; the seq it covers never goes back.
	through = max(through, r.run.PromptThroughSeq.Int64)
	r.guardInputs(texts, seqs, max(after, r.run.PromptThroughSeq.Int64))
	return assemblePrompt(mode, task.Title, notice, texts, outcomes), through, nil
}

// supersededReason is the outcome reason of a run that ended without a turn because the task's
// earlier run had read its messages already.
const supersededReason = "its messages were already read by the task's earlier run"

// superseded says whether a run has nothing new to say (#21): it returns the outcome reason, or "" when the run goes on. A message sent
// while a run is parked queues a run behind it; the parked run resumes, reads that message, and the
// queued run then starts with no unread message, which would cost a model turn that only says
// "Continue with the task." execute returns supersededRun and conclude drops the run instead (Owner.Supersede), and only when all hold:
// it is a fresh run (a resumed or taken-over one has work), the task has been prompted before
// (a first run with an empty message is a scheduled task), no session restart is pending (its
// handoff prompt is not made of messages), nothing is unread, and the task's previous run
// succeeded. After a failed or canceled run, a run with no new message is a person's retry and
// still runs; ending it as a success would also complete a task whose last run failed.
func (r *runner) superseded(ctx context.Context, task store.Task) (string, error) {
	if r.run.PromptThroughSeq.Valid || (task.HarnessSessionID.Valid && task.SessionRestartPending) {
		return "", nil
	}
	after, err := r.q.GetTaskPromptedThrough(ctx, store.GetTaskPromptedThroughParams{
		OrgID: r.run.OrgID, TaskID: task.ID, RunID: r.run.ID,
	})
	if err != nil {
		return "", r.dbErr(ctx, "check for unread messages", err)
	}
	if after == 0 {
		return "", nil
	}
	msgs, err := r.q.ListTaskMessagesAfter(ctx, store.ListTaskMessagesAfterParams{
		OrgID: r.run.OrgID, TaskID: task.ID, AfterSeq: after,
	})
	if err != nil {
		return "", r.dbErr(ctx, "check for unread messages", err)
	}
	if len(msgs) > 0 {
		return "", nil
	}
	runs, err := r.q.ListRunsForTask(ctx, store.ListRunsForTaskParams{OrgID: r.run.OrgID, TaskID: task.ID})
	if err != nil {
		return "", r.dbErr(ctx, "list the task's runs", err)
	}
	var prev *store.Run
	for i := range runs {
		if runs[i].ID != r.run.ID && !runs[i].CreatedAt.Time.After(r.run.CreatedAt.Time) {
			prev = &runs[i]
		}
	}
	if prev == nil || prev.State != store.RunStateSucceeded {
		return "", nil
	}
	r.log.Info("the run has no unread message; ending it without a turn", "previous", prev.ID.String())
	return supersededReason, nil
}

// promptMode says which of the turn's prompt shapes assemblePrompt builds.
type promptMode int

const (
	// promptNew is the first prompt of a session that has not loaded the task yet.
	promptNew promptMode = iota
	// promptContinue is a follow-up message in a session that knows the task.
	promptContinue
	// promptTakeover resumes a run another worker lost.
	promptTakeover
	// promptParkResume resumes a parked run after its approvals and questions were decided.
	promptParkResume
	// promptParkResumeFresh resumes a run that parked on an input over the context cap before it sent
	// any prompt: the task's title comes first.
	promptParkResumeFresh
	// promptReset is a new session that replaces one the sandbox home lost.
	promptReset
)

// assemblePrompt joins the parts of a turn's prompt: the task header and MCP notice (new and
// reset sessions), the resume note with the outcomes, the task messages, and for a reset the
// outcomes after the messages. With nothing to say it falls back to a nudge.
func assemblePrompt(mode promptMode, title, notice string, msgs []string, outcomes string) string {
	var parts []string
	header := func() {
		parts = append(parts, "Task: "+title)
		if notice != "" {
			parts = append(parts, notice)
		}
	}
	switch mode {
	case promptReset, promptNew:
		header()
	case promptParkResume:
		parts = append(parts, parkResumeNote+outcomes)
	case promptParkResumeFresh:
		header()
		parts = append(parts, parkResumeNote+outcomes)
	case promptTakeover:
		parts = append(parts, resumeNote+outcomes)
	case promptContinue:
	}
	parts = append(parts, msgs...)
	if mode == promptReset && outcomes != "" {
		parts = append(parts, parkResumeNote+outcomes)
	}
	if len(parts) == 0 {
		parts = append(parts, "Continue with the task.")
	}
	return strings.Join(parts, "\n\n")
}

// ending is how execute ended: res when the turn controller ended the turns with anything but
// end_turn, otherwise err (nil: the run succeeded or parked). A setup-phase failure, a dropped
// connection and the like are errors; the turn's outcome is never turned back into one.
type ending struct {
	err error
	res *turnResult
}

// conclude maps how execute ended onto the verbs of the run's owner (internal/runoutcome). turn is
// the run's open turn row, which the verbs end with the run.
func (r *runner) conclude(ctx context.Context, e ending, turn runoutcome.Turn) {
	err := e.err
	if t := e.res; t != nil {
		switch t.out.Kind {
		case turnpolicy.OutCapReached:
			r.cancelAtTimeLimit(ctx, turn, t.capNote)
			return
		case turnpolicy.OutHarnessCancelled, turnpolicy.OutRetriesExhausted, turnpolicy.OutRefusalsExceeded, turnpolicy.OutLoop:
			reason, class, _ := t.fails()
			r.fail(ctx, turn, reason, class)
			return
		case turnpolicy.OutFenced:
			r.concludeFenced(ctx, turn)
			return
		case turnpolicy.OutShutdown:
			r.concludeShutdown(ctx, turn)
			return
		}
		err = t.err // OutFailed, OutPassthrough: the driver's classified error of the last prompt
	}
	var f *failure
	var c *canceled
	var rq *requeued
	var sr *supersededRun
	switch {
	case err == nil:
	case errors.As(err, &sr):
		if serr := r.own.Supersede(ctx, turn, sr.reason); serr != nil && !errors.Is(serr, errFenced) {
			r.log.Warn("ending the superseded run failed", "reason", sr.reason, "err", serr)
		}
	case errors.As(err, &rq):
		if rerr := r.own.Requeue(ctx, turn, rq.reason); errors.Is(rerr, errFenced) {
			r.log.Warn("the run could not be requeued: it was taken over, canceled or is no longer starting", "state", r.run.State, "reason", rq.reason)
		} else if rerr != nil {
			r.log.Warn("requeueing the run failed; its lease expires and a worker takes it over", "reason", rq.reason, "err", rerr)
		}
	case errors.As(err, &f) && f.sandbox != nil:
		su := f.sandbox
		if ferr := r.own.SandboxUnavailable(ctx, turn, runoutcome.Unavailable{
			Reason: f.reason, Cause: su.cause, Detail: su.detail, Waited: su.waited, Cap: su.cap,
			CauseSeen: su.causeSeen, CauseWasSeen: su.causeWasSeen, Class: classSandboxUnavailable,
		}); ferr != nil {
			r.log.Warn("failing the run failed", "reason", f.reason, "err", ferr)
		}
	case errors.As(err, &f):
		r.fail(ctx, turn, f.reason, f.class)
	case errors.As(err, &c):
		r.cancel(ctx, turn, c.reason, c.class, c.extra)
	case errors.Is(err, errFenced):
		r.concludeFenced(ctx, turn)
	case errors.Is(err, errDropped):
		r.own.Release(ctx, turn)
	case errors.Is(err, errShutdown):
		r.concludeShutdown(ctx, turn)
	default:
		r.log.Warn("driving the run failed", "err", err)
		r.own.Release(ctx, turn)
	}
}

// fail fails the run; an empty class is fatal.
func (r *runner) fail(ctx context.Context, turn runoutcome.Turn, reason string, class store.RunFailureClass) {
	if ferr := r.own.Fail(ctx, turn, reason, cmp.Or(class, classFatal)); ferr != nil {
		r.log.Warn("failing the run failed", "reason", reason, "err", ferr)
	}
}

// cancel cancels the run as its owner.
func (r *runner) cancel(ctx context.Context, turn runoutcome.Turn, reason string, class store.RunFailureClass, extra map[string]any) {
	if ferr := r.own.Cancel(ctx, turn, reason, class, extra); ferr != nil {
		r.log.Warn("canceling the run failed", "reason", reason, "err", ferr)
	}
}

// cancelAtTimeLimit cancels the run as its owner at its wall-clock cap, with the time-limit note.
func (r *runner) cancelAtTimeLimit(ctx context.Context, turn runoutcome.Turn, reason string) {
	if ferr := r.own.CancelAtTimeLimit(ctx, turn, reason); ferr != nil {
		r.log.Warn("canceling the run failed", "reason", reason, "err", ferr)
		return
	}
	r.p.cfg.Metrics.TimeLimitStops.Inc()
}

// concludeFenced ends a worker that may no longer write the run.
func (r *runner) concludeFenced(ctx context.Context, turn runoutcome.Turn) {
	switch r.own.FenceKind() {
	case fenceCanceled:
		r.log.Info("run canceled")
		r.own.FlushItems(ctx, true) // what the agent was saying when it was canceled
		r.own.CloseTurn(ctx, turn)
	case fenceAbandon:
		r.log.Warn("giving the run up")
		r.own.Release(ctx, turn)
	case fenceFailed:
		f := r.feedFailure.Load()
		if f == nil { // set before the fence; defensive
			f = &failure{reason: "the leak guard's feed failed for good"}
		}
		r.fail(ctx, turn, f.reason, f.class)
	default:
		r.log.Info("run taken over or finished elsewhere")
	}
}

// concludeShutdown ends a worker whose pool is stopping.
func (r *runner) concludeShutdown(ctx context.Context, turn runoutcome.Turn) {
	if r.p.cfg.KeepLeaseOnShutdown {
		r.own.FlushItems(ctx, true)
		r.own.CloseTurn(ctx, turn)
	} else {
		r.own.Release(context.WithoutCancel(ctx), turn)
	}
}

// handler records the harness's session updates and answers its permission requests.
type handler struct{ r *runner }

func (h handler) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	at := time.Now() // for the trace, before the event write
	var b []byte
	if n.Update.ToolCall != nil || n.Update.ToolCallUpdate != nil {
		var err error
		if b, err = json.Marshal(n.Update); err != nil {
			return err
		}
		// The leak guard's feed (taint.go) comes before every path below that returns early: a tool
		// can finish during the end-of-turn summary too, and its result must set its legs. It reads
		// the replay of a resumed session as well: a worker that gave the run up may have failed to
		// report a result.
		if err := h.r.feedTaint(ctx, b); err != nil {
			return err
		}
		// The leak guard waits for this count: it is added only once the update has been fed.
		if c := h.r.feedCounts.Load(); c != nil && finishesTool(b) {
			c.add(0, 1)
		}
	}
	if h.r.summaryUpdate(n) {
		return nil // the summary prompt's reply is the task's note, not a run event
	}
	if n.Update.AvailableCommandsUpdate != nil {
		// Harness state, not part of the run's timeline: kept for waitCommand only.
		h.r.setCommands(acp.Flatten(n).Commands)
		return nil
	}
	if b == nil {
		var err error
		if b, err = json.Marshal(n.Update); err != nil {
			return err
		}
	}
	if h.r.replaying.Load() {
		return nil
	}
	h.r.loop.Observe(n)
	if err := h.r.own.RecordUpdate(ctx, "acp."+updateKind(b), b); err != nil {
		return err
	}
	h.r.tr.update(n, at)
	if n.Update.ToolCall != nil {
		h.r.markTool(string(n.Update.ToolCall.ToolCallId))
	}
	// A run's tokens are reported by the spend ledger (package ledger, plan item L1), which adds
	// them to the run for every request agentgateway logs (tokens are reported, not enforced:
	// ADR 0081). ACP cannot report them: the
	// only usage update (usage_update, unstable in acp-go-sdk v0.13.5) reports the context
	// window's occupancy (used/size) and a cost, which rise and fall with compaction and cannot
	// be summed into tokens used, and the unstable PromptResponse.usage arrives only when the turn,
	// and so the run, has ended. usage_update is still recorded as a run event.
	return nil
}

// toolCallWait bounds how long a permission request waits for its tool call's update. The
// SDK hands notifications to one queue and requests to their own goroutines, so a request
// can overtake the tool_call update the harness sent just before it; waiting keeps the run
// events in the harness's order.
const toolCallWait = 250 * time.Millisecond

func (h handler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (acp.Decision, error) {
	if h.r.summarizingNow() {
		// The summary prompt does nothing: every permission request is refused.
		h.r.log.Warn("permission refused during the end-of-turn summary")
		return acp.Choose(req, acp.RejectOnce), nil
	}
	if h.r.replaying.Load() {
		return acp.Decision{}, nil
	}
	d := h.r.p.cfg.Permissions.decide(req, h.r.cwd, h.r.pathResolver(ctx))
	if d.Verdict == Reject {
		h.r.noteRefusal(req, d)
	}
	select {
	case <-h.r.toolSeen(string(req.ToolCall.ToolCallId)):
	case <-time.After(toolCallWait):
	case <-ctx.Done():
	}
	if err := h.r.own.Record(ctx, "acp.permission", map[string]any{"toolCall": req.ToolCall, "decision": d}); err != nil {
		return acp.Decision{}, err
	}
	h.r.log.Info("permission answered", "kind", d.Kind, "tier", d.Tier, "verdict", d.Verdict, "outside", d.Outside)
	return acp.Decision{OptionID: d.OptionID}, nil
}

// updateKind is the update's discriminator as a run event kind suffix.
func updateKind(update []byte) string {
	var u struct {
		Kind string `json:"sessionUpdate"`
	}
	_ = json.Unmarshal(update, &u)
	var b strings.Builder
	for _, c := range strings.ToLower(u.Kind) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			b.WriteRune(c)
		}
		if b.Len() == 56 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// modelFacts is what the run needs to know about the model it uses.
type modelFacts struct {
	ProviderType string
	// Format is the model's wire format; the harness uses the provider entry for it (ADR 0069).
	Format modelformat.Format
	// Cache is the model's prompt-cache mode (ADR 0068, 0069).
	Cache prices.CacheMode
	// CompactionTokens is where the harness compacts the conversation (compactionTokens).
	CompactionTokens int
	// ContextWindow is the model's context window in tokens from the catalog, or 0 when the catalog
	// has none (contextguard then assumes its default).
	ContextWindow int64
}

// modelFacts looks the model up by its plain name. A model that is not there (or has no price) runs
// as before, with chat completions and no cache: the gateway refuses it anyway. Only a database
// error is returned.
func (r *runner) modelFacts(ctx context.Context, name string) (modelFacts, error) {
	f, err := r.priceFacts(ctx, name)
	if err != nil {
		return modelFacts{}, err
	}
	f.ContextWindow, err = r.contextWindow(ctx, f.ProviderType, name)
	return f, err
}

// contextWindow is the model's accepted context length in the catalog (ADR 0070): today only the
// accepted OpenRouter limits (internal/orcatalog) are stored; any other model has none, and 0 is
// returned. Only a database error is returned.
func (r *runner) contextWindow(ctx context.Context, providerType, name string) (int64, error) {
	if providerType != providertype.OpenRouter {
		return 0, nil
	}
	v, err := r.q.GetOpenRouterLimitValue(ctx, store.GetOpenRouterLimitValueParams{Model: name, Field: orcatalog.FieldContextLength})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return int64(v.Value), nil
}

func (r *runner) priceFacts(ctx context.Context, name string) (modelFacts, error) {
	qt, err := r.p.cfg.Prices.Find(ctx, r.q, r.run.OrgID, name, time.Now())
	m := qt.Model
	if _, ok := prices.Unpriceable(err); ok {
		if m.Name == "" {
			return modelFacts{Format: modelformat.OpenAI, Cache: prices.CacheNone, CompactionTokens: defaultCompactionTokens}, nil
		}
		// The model exists but has no usable price: its format and provider type still apply.
		return modelFacts{ProviderType: m.ProviderType, Format: m.Format, Cache: prices.CacheNone, CompactionTokens: defaultCompactionTokens}, nil
	} else if err != nil {
		return modelFacts{}, err
	}
	return modelFacts{ProviderType: m.ProviderType, Format: m.Format, Cache: prices.CacheModeFor(m.ProviderType, m.Name, m.Format, qt.Resolution.Price), CompactionTokens: compactionTokens(qt.Resolution.Price)}, nil
}
