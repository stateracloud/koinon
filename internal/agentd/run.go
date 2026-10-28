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
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/safego"
)

// How a driven run ends, besides success (nil).
var (
	// errFenced: the worker may no longer write (another owner, a cancel, or it gave up).
	errFenced = errors.New("no longer the run's owner")
	// errDropped: the harness connection ended mid-run; the run is resumed by a takeover.
	errDropped = errors.New("harness connection lost")
	// errShutdown: the pool is stopping.
	errShutdown = errors.New("shutting down")
	// errEndTurn: the turn ended with end_turn; execute closes the harness and sweeps before
	// it ends the run.
	errEndTurn = errors.New("the turn ended")
)

// failure fails the run with a reason.
type failure struct {
	reason string
	// class is the failure class (classify.go); empty means fatal.
	class store.RunFailureClass
	// retryAfter is the provider's Retry-After of a throttled request.
	retryAfter time.Duration
}

func (f *failure) Error() string { return f.reason }

// requeued sends the run back to the queue: the sandbox did not become ready in time (ADR 0066).
type requeued struct{ reason string }

func (q *requeued) Error() string { return q.reason }

// canceled cancels the run as its owner, for example at the wall-clock cap.
type canceled struct{ reason string }

func (c *canceled) Error() string { return c.reason }

// fence says why a worker stopped being allowed to write.
type fence int32

const (
	fenceLost     fence = iota + 1 // a newer epoch owns the run, or it finished
	fenceCanceled                  // the control plane canceled the run
	fenceAbandon                   // the worker cannot reach the database and gives up
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

	fenceOnce sync.Once
	fenced    chan struct{}
	fenceKind atomic.Int32

	mu        sync.Mutex // serialises appends and srcSeq
	srcSeq    int64
	replaying atomic.Bool

	toolsMu sync.Mutex
	tools   map[string]chan struct{} // tool call IDs; closed once the tool call is recorded

	// delivered are the approval requests whose outcomes this turn's prompt carries; they are
	// marked delivered when the turn ends.
	delivered []pgtype.UUID
	// deliveredQ are the resolved questions whose outcomes this turn's prompt carries.
	deliveredQ []pgtype.UUID

	// cwd is the session directory, set before the harness starts; permission requests for
	// paths outside it are rejected.
	cwd string

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

func (p *Pool) drive(ctx context.Context, run store.Run, log *slog.Logger) {
	r := &runner{
		p: p, q: p.q, run: run, fenced: make(chan struct{}), cmdChanged: make(chan struct{}),
		log: log.With("org", run.OrgID.String(), "run", run.ID.String(), "epoch", run.Epoch),
	}
	r.log.Info("driving run", "state", run.State, "takeovers", run.Takeovers)

	hctx, stop := context.WithCancel(ctx)
	var hb sync.WaitGroup
	hb.Go(func() { r.heartbeat(hctx) })
	// A panic in the run becomes a failure: the heartbeat stops and the run concludes as failed on
	// its own path, instead of keeping a live lease no other worker would ever take over.
	var err error
	if safego.Do(r.log, "run", func() { err = r.execute(ctx) }) {
		err = &failure{reason: "internal error: the run panicked"}
	}
	stop()
	hb.Wait()
	r.conclude(ctx, err)
}

func (r *runner) fence(k fence) {
	r.fenceOnce.Do(func() {
		r.fenceKind.Store(int32(k))
		close(r.fenced)
	})
}

func (r *runner) isFenced() bool {
	select {
	case <-r.fenced:
		return true
	default:
		return false
	}
}

// fenceFromDB fences after a write matched no rows, telling a cancel from a lost epoch.
func (r *runner) fenceFromDB(ctx context.Context) {
	cur, err := r.q.GetRun(ctx, store.GetRunParams{OrgID: r.run.OrgID, ID: r.run.ID})
	switch {
	case err != nil:
		r.fence(fenceAbandon)
	case cur.State == store.RunStateCanceled && cur.Epoch == r.run.Epoch:
		r.fence(fenceCanceled)
	default:
		r.fence(fenceLost)
	}
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
		case <-r.fenced:
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
			r.fenceFromDB(ctx)
			return
		case ctx.Err() != nil:
			return
		case time.Since(last) >= r.p.cfg.Lease:
			r.log.Warn("lease lost: heartbeats failed", "err", err)
			r.fence(fenceAbandon)
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
	case r.isFenced():
		return errFenced
	case ctx.Err() != nil:
		return errShutdown
	}
	return nil
}

// dbErr classifies a database error of the run's own writes and reads.
func (r *runner) dbErr(ctx context.Context, what string, err error) error {
	if stop := r.stopped(ctx); stop != nil {
		return stop
	}
	if errors.Is(err, pgx.ErrNoRows) {
		r.fenceFromDB(ctx)
		return errFenced
	}
	return fmt.Errorf("%s: %w", what, err)
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
		select {
		case <-r.fenced:
			cancel()
		case <-fctx.Done():
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
	// An unreadable config would leave the run without its instructions (and model): it fails
	// before anything starts.
	vc, err := versionConfig(version.Config)
	if err != nil {
		return &failure{reason: "the agent version's config is unreadable: " + err.Error()}
	}
	model := versionModel(vc)
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		// The org's first served model: providers by name, then models by name (PV1).
		served, err := r.q.ListServedCatalog(fctx, r.run.OrgID)
		if err != nil {
			return r.dbErr(ctx, "load the org's models", err)
		}
		for _, m := range served {
			// Only a priced model is served (the gateway answers 404 for the rest).
			pm := prices.Model{ID: m.ModelID.String(), Name: m.ModelName, ProviderType: m.ProviderType}
			if _, err := prices.Resolve(fctx, r.q, r.run.OrgID, pm, time.Now()); err == nil {
				model = m.ModelName
				break
			}
		}
	}
	if model == "" {
		return &failure{reason: "the agent names no model and its org serves none; add a provider and a model with a price"}
	}
	gw := strings.TrimSuffix(cfg.GatewayURL, "/")
	mode, ptype := r.cacheMode(fctx, model)
	marker, unknown := cacheMarker(mode)
	if unknown {
		r.log.Warn("the model has a cache price but its provider format cannot be cached by Koinon; it runs without prompt caching", "model", model)
	}
	// OpenRouter keeps a session on one upstream, so the run's cache stays warm. Only the run ID
	// is sent, and only to OpenRouter.
	sessionID := ""
	if ptype == "openrouter" {
		sessionID = r.run.ID.String()
	}
	cmd, err := launchCommand(agent.Flavor, endpoints{ModelBaseURL: gw + "/v1", Model: model, MCPURL: gw + "/mcp", CacheMarker: marker, SessionID: sessionID})
	if err != nil {
		return &failure{reason: err.Error()}
	}
	cmd = append(cmd, harnessArgs(agent.Flavor, vc)...)
	r.vcfg = vc
	// The gateway may not serve the model yet: a new provider's key file is still reaching the
	// gateway pod (issue #174). The reconciler starts the sandbox meanwhile, because this run is
	// demand for it, so the two waits overlap.
	if err := r.waitModel(fctx, model); err != nil {
		return r.execErr(ctx, "wait for the model", err)
	}
	sb, err := r.waitSandbox(fctx)
	if err != nil {
		return r.execErr(ctx, "sandbox", err)
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
		if err := r.record(fctx, "run.uncached", map[string]any{"model": model, "reason": prices.UncachedReason(ptype, model)}); err != nil {
			return r.dbErr(ctx, "record the uncached run", err)
		}
	}
	if len(ended) > 0 {
		log.Warn("ended processes left in the sandbox", "pids", ended, "main", main)
		if err := r.record(fctx, "run.sandbox_swept", map[string]any{"pids": ended, "main": main}); err != nil {
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
	// The run's own version decides the instructions, so a version bump applies at the next
	// run without recreating the sandbox.
	instructions := instructionsFile(vc.Instructions)
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
	closeHarness := sync.OnceFunc(func() { closeProcess(proc) })
	sweepOnce := sync.OnceFunc(func() { r.sweepAfter(ctx, workspace, sandbox) })
	defer sweepOnce()
	defer closeHarness()
	conn := acp.NewConn(handler{r: r}, proc.Stdin(), proc.Stdout())
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
	if command := readyCommand(agent.Flavor); command != "" {
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
	if value := modelOption(agent.Flavor, model); value != "" {
		if err := conn.SetModel(fctx, session, value); err != nil {
			return harnessErr("select the model "+value, err)
		}
	}
	if r.run.State == store.RunStateStarting {
		if _, err := r.q.MarkRunRunning(fctx, store.MarkRunRunningParams{OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch}); err != nil {
			return r.dbErr(ctx, "mark the run running", err)
		}
	}
	if err := r.record(fctx, "run.session", map[string]any{
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
	if _, err := r.q.SetRunPromptThrough(fctx, store.SetRunPromptThroughParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, PromptThroughSeq: through,
	}); err != nil {
		return r.dbErr(ctx, "record the prompt", err)
	}
	if err := r.record(fctx, "run.prompt", map[string]any{
		"text": text, "throughSeq": through, "resumed": r.run.PromptThroughSeq.Valid,
	}); err != nil {
		return r.dbErr(ctx, "record the prompt", err)
	}
	// The agent can write its home, instruction file included; the prompt goes out only with
	// the run's instructions in place.
	if err := r.checkInstructions(fctx, workspace, sandbox, agent.Flavor, instructions); err != nil {
		return r.execErr(ctx, "check the instructions", err)
	}
	err = r.turnWithRetry(ctx, fctx, conn, proc, &stderr, session, text)
	if !errors.Is(err, errEndTurn) {
		return err
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

type promptResult struct {
	stop acp.StopReason
	err  error
}

// turn sends the prompt and waits for the turn to end, a cancel, the wall-clock cap or the
// pool to stop.
func (r *runner) turn(ctx context.Context, conn *acp.Conn, proc execution.Process, stderr *tail, session acp.SessionID, text string) error {
	done := make(chan promptResult, 1)
	go func() {
		stop, err := conn.Prompt(ctx, session, text)
		done <- promptResult{stop, err}
	}()
	// Parked time does not count against the cap (ClaimNextRun adds it to parked_seconds).
	capd := time.Duration(r.run.MaxWallClockSeconds)*time.Second + time.Duration(r.run.ParkedSeconds)*time.Second
	deadline := time.Until(r.run.StartedAt.Time.Add(capd))
	capTimer := time.NewTimer(max(deadline, 0))
	defer capTimer.Stop()

	select {
	case res := <-done:
		if res.err != nil {
			return r.harnessErr(ctx, conn, proc, stderr, "session/prompt", res.err)
		}
		switch res.stop {
		case acp.StopEndTurn:
			return errEndTurn
		case acp.StopCancelled:
			return &failure{reason: "the harness cancelled the turn"}
		default:
			return stopFailure(res.stop)
		}
	case <-r.fenced:
		if fence(r.fenceKind.Load()) == fenceCanceled {
			r.log.Info("run canceled; cancelling the turn")
			r.cancelTurn(ctx, conn, session, done)
		}
		return errFenced
	case <-capTimer.C:
		r.log.Info("wall-clock cap reached; cancelling the turn")
		r.cancelTurn(ctx, conn, session, done)
		return &canceled{reason: fmt.Sprintf("wall-clock cap of %s reached", time.Duration(r.run.MaxWallClockSeconds)*time.Second)}
	case <-ctx.Done():
		return errShutdown
	}
}

// turnWithRetry runs the turn and repeats it, in the same session, when the model request
// failed with a transient or throttled error (classify.go): after the backoff, or after the
// provider's Retry-After. Every other failure ends the turn at once. The wait ends when the worker
// is fenced or stops.
func (r *runner) turnWithRetry(ctx, fctx context.Context, conn *acp.Conn, proc execution.Process, stderr *tail, session acp.SessionID, text string) error {
	cfg := r.p.cfg
	for attempt := 0; ; attempt++ {
		err := r.turn(ctx, conn, proc, stderr, session, text)
		var f *failure
		if !errors.As(err, &f) || !f.retryable() {
			return err
		}
		delay := retryDelay(f, attempt, cfg.RetryBackoff)
		if attempt >= cfg.ModelRetries || delay > cfg.MaxRetryAfter {
			return exhausted(f, attempt, cfg.MaxRetryAfter)
		}
		r.log.Info("retrying the turn", "class", f.class, "attempt", attempt+1, "delay", delay)
		if rerr := r.record(fctx, "run.retry", map[string]any{
			"failureClass": f.class, "attempt": attempt + 1, "delaySeconds": delay.Seconds(), "reason": f.reason,
		}); rerr != nil {
			return r.dbErr(ctx, "record the retry", rerr)
		}
		if !sleep(fctx, delay) {
			return cmp.Or(r.stopped(ctx), errShutdown)
		}
		text = fmt.Sprintf(retryNote, f.class)
	}
}

// cancelTurn sends session/cancel and gives the harness CancelGrace to end the turn, so it
// can save its session. The stream is closed afterwards either way.
func (r *runner) cancelTurn(ctx context.Context, conn *acp.Conn, session acp.SessionID, done <-chan promptResult) {
	if err := conn.Cancel(ctx, session); err != nil {
		r.log.Warn("session/cancel failed", "err", err)
		return
	}
	select {
	case res := <-done:
		r.log.Info("turn ended after cancel", "stop", res.stop, "err", res.err)
	case <-time.After(r.p.cfg.CancelGrace):
		r.log.Warn("the harness did not end its turn after session/cancel")
	case <-ctx.Done():
	}
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
	code, werr := proc.Wait()
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
	if err := r.record(ctx, "run.interrupted", payload); err != nil {
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
	var texts []string
	through := after
	for _, m := range msgs {
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Payload, &body) == nil && strings.TrimSpace(body.Text) != "" {
			texts = append(texts, body.Text)
		}
		through = m.Seq
	}
	mode := promptNew
	switch {
	case reset:
		mode = promptReset
	// A claimed parked run is starting with its prompt already recorded: it resumes after
	// its approvals were decided. A takeover keeps the state it had.
	case resumed && r.run.State == store.RunStateStarting:
		mode = promptParkResume
	case resumed:
		mode = promptTakeover
	case loaded:
		mode = promptContinue
	}
	// A reset reads from the start; the seq it covers never goes back.
	through = max(through, r.run.PromptThroughSeq.Int64)
	return assemblePrompt(mode, task.Title, notice, texts, outcomes), through, nil
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

// record appends a run event under the next source seq, retrying transient failures. The
// source seq makes a retry after an ambiguous failure (the insert committed, the reply was
// lost) a no-op instead of a duplicate.
func (r *runner) record(ctx context.Context, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.recordRaw(ctx, kind, b)
}

func (r *runner) recordRaw(ctx context.Context, kind string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isFenced() {
		return errFenced
	}
	r.srcSeq++
	src := r.srcSeq
	backoff := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		_, err := r.q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch,
			Kind: kind, Payload: payload, SourceSeq: src,
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := r.q.GetRunEventBySource(ctx, store.GetRunEventBySourceParams{
				OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch, SourceSeq: src,
			}); gerr == nil {
				return nil // an earlier attempt committed
			}
			r.fenceFromDB(ctx)
			return errFenced
		}
		if attempt == 4 || !sleep(ctx, backoff) {
			// The event cannot be recorded: stop rather than drop it. The lease expires and
			// another worker resumes the session.
			r.log.Warn("recording a run event failed", "kind", kind, "err", err)
			r.fence(fenceAbandon)
			return err
		}
		backoff *= 2
	}
}

// finish ends the run as its owner, with a closing run event and a task event, in one
// transaction. On success the task event is the agent's reply.
func (r *runner) finish(ctx context.Context, outcome store.RunState, reason string, class store.RunFailureClass) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcSeq++
	src := r.srcSeq
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		return r.finishIn(ctx, r.q.WithTx(tx), src, outcome, reason, class)
	})
	if err != nil {
		return r.dbErr(ctx, "finish the run", err)
	}
	r.p.cfg.Metrics.Finished.WithLabelValues(string(outcome)).Inc()
	r.log.Info("run finished", "outcome", outcome, "reason", reason)
	return nil
}

// finishIn ends the run in the caller's transaction.
func (r *runner) finishIn(ctx context.Context, q *store.Queries, src int64, outcome store.RunState, reason string, class store.RunFailureClass) error {
	body := map[string]string{"reason": reason}
	if class != "" {
		body["failureClass"] = string(class)
	}
	payload, _ := json.Marshal(body)
	if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
		OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch,
		Kind: "run." + string(outcome), Payload: payload, SourceSeq: src,
	}); err != nil {
		return err
	}
	if _, err := q.FinishRun(ctx, store.FinishRunParams{
		OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, Outcome: outcome,
		OutcomeReason: pgtype.Text{String: reason, Valid: reason != ""},
		FailureClass:  store.NullRunFailureClass{RunFailureClass: class, Valid: class != ""},
	}); err != nil {
		return err
	}
	if outcome == store.RunStateSucceeded {
		return r.postReply(ctx, q)
	}
	_, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: r.run.OrgID, TaskID: r.run.TaskID, RunID: r.run.ID,
		Kind: "run." + string(outcome), ActorKind: store.TaskActorKindSystem, Payload: payload,
	})
	return err
}

// postReply puts the agent's reply since the run's last park on the task, if it said
// anything.
func (r *runner) postReply(ctx context.Context, q *store.Queries) error {
	reply, err := r.reply(ctx, q)
	if err != nil || reply == "" {
		return err
	}
	text, _ := json.Marshal(map[string]string{"text": reply})
	_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
		OrgID: r.run.OrgID, TaskID: r.run.TaskID, RunID: r.run.ID,
		Kind: "message", ActorKind: store.TaskActorKindAgent, ActorAgentID: r.run.AgentID, Payload: text,
	})
	return err
}

// reply is the agent's message text of the run since its last park, across every epoch.
func (r *runner) reply(ctx context.Context, q *store.Queries) (string, error) {
	var b strings.Builder
	var after int64
	for {
		evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, AfterSeq: after, MaxEvents: 500,
		})
		if err != nil {
			return "", err
		}
		for _, e := range evs {
			after = e.Seq
			if e.Kind == kindParked {
				// Posted when the run parked.
				b.Reset()
				continue
			}
			if e.Kind != "acp.agent_message_chunk" {
				continue
			}
			var u struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(e.Payload, &u) == nil {
				b.WriteString(u.Content.Text)
			}
		}
		if len(evs) < 500 {
			return strings.TrimSpace(b.String()), nil
		}
	}
}

// conclude acts on how execute ended.
func (r *runner) conclude(ctx context.Context, err error) {
	var f *failure
	var c *canceled
	var rq *requeued
	switch {
	case err == nil:
	case errors.As(err, &rq):
		if rerr := r.requeue(ctx, rq.reason); errors.Is(rerr, errFenced) {
			r.log.Warn("the run could not be requeued: it was taken over, canceled or is no longer starting", "state", r.run.State, "reason", rq.reason)
			r.fenceFromDB(ctx)
		} else if rerr != nil {
			r.log.Warn("requeueing the run failed; its lease expires and a worker takes it over", "reason", rq.reason, "err", rerr)
		}
	case errors.As(err, &f):
		if ferr := r.finish(ctx, store.RunStateFailed, f.reason, cmp.Or(f.class, classFatal)); ferr != nil {
			r.log.Warn("failing the run failed", "reason", f.reason, "err", ferr)
		}
	case errors.As(err, &c):
		if ferr := r.finish(ctx, store.RunStateCanceled, c.reason, ""); ferr != nil {
			r.log.Warn("canceling the run failed", "reason", c.reason, "err", ferr)
		}
	case errors.Is(err, errFenced):
		switch fence(r.fenceKind.Load()) {
		case fenceCanceled:
			r.log.Info("run canceled")
		case fenceAbandon:
			r.log.Warn("giving the run up")
			r.release(ctx)
		default:
			r.log.Info("run taken over or finished elsewhere")
		}
	case errors.Is(err, errDropped):
		r.release(ctx)
	case errors.Is(err, errShutdown):
		if !r.p.cfg.KeepLeaseOnShutdown {
			r.release(context.WithoutCancel(ctx))
		}
	default:
		r.log.Warn("driving the run failed", "err", err)
		r.release(ctx)
	}
}

// release gives the lease up so a worker takes the run over at once.
func (r *runner) release(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := r.q.ReleaseRunLease(ctx, store.ReleaseRunLeaseParams{OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.log.Warn("releasing the lease failed; it expires on its own", "err", err)
		return
	}
	r.log.Info("lease released for a takeover")
}

// handler records the harness's session updates and answers its permission requests.
type handler struct{ r *runner }

func (h handler) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	if h.r.summaryUpdate(n) {
		return nil // the summary prompt's reply is the task's note, not a run event
	}
	if n.Update.AvailableCommandsUpdate != nil {
		// Harness state, not part of the run's timeline: kept for waitCommand only.
		h.r.setCommands(acp.Flatten(n).Commands)
		return nil
	}
	if h.r.replaying.Load() {
		return nil
	}
	b, err := json.Marshal(n.Update)
	if err != nil {
		return err
	}
	if err := h.r.recordRaw(ctx, "acp."+updateKind(b), b); err != nil {
		return err
	}
	if n.Update.ToolCall != nil {
		h.r.markTool(string(n.Update.ToolCall.ToolCallId))
	}
	// The token cap (#27) is fed by the spend ledger (package ledger, plan item L1), which calls
	// scheduler.RecordTokenUsage for every request agentgateway logs. ACP cannot feed it: the
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
		return acp.Choose(req, acp.RejectOnce, acp.RejectAlways), nil
	}
	d := h.r.p.cfg.Permissions.decide(req, h.r.cwd)
	if h.r.replaying.Load() {
		return acp.Decision{}, nil
	}
	select {
	case <-h.r.toolSeen(string(req.ToolCall.ToolCallId)):
	case <-time.After(toolCallWait):
	case <-ctx.Done():
	}
	if err := h.r.record(ctx, "acp.permission", map[string]any{"toolCall": req.ToolCall, "decision": d}); err != nil {
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

// cacheMode is the prompt-cache mode of the model the run uses (ADR 0068). A model without a
// price runs as before: the gateway refuses it anyway.
func (r *runner) cacheMode(ctx context.Context, name string) (mode prices.CacheMode, providerType string) {
	m, err := prices.Lookup(ctx, r.q, r.run.OrgID, name)
	if err != nil {
		return prices.CacheNone, ""
	}
	res, err := prices.Resolve(ctx, r.q, r.run.OrgID, m, time.Now())
	if err != nil {
		return prices.CacheNone, ""
	}
	return prices.CacheModeFor(m.ProviderType, m.Name, res.Price), m.ProviderType
}
