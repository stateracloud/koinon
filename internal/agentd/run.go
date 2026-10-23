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

	"github.com/procoduck/koinon/internal/acp"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
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
type failure struct{ reason string }

func (f *failure) Error() string { return f.reason }

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

	// cwd is the session directory, set before the harness starts; permission requests for
	// paths outside it are rejected.
	cwd string

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
	err := r.execute(ctx)
	stop()
	hb.Wait()
	r.conclude(ctx, err)
}

func (r *runner) fence(k fence) {
	r.fenceOnce.Do(func() {
		r.fenceKind.Store(int32(k)) //nolint:gosec // fence is an int32
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
	return int32(p.cfg.Lease.Seconds()) //nolint:gosec // bounded by configuration
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
	version, err := r.q.GetAgentVersion(fctx, store.GetAgentVersionParams{OrgID: r.run.OrgID, ID: r.run.AgentVersionID})
	if err != nil {
		return r.dbErr(ctx, "load agent version", err)
	}
	ticket, err := r.q.GetTicket(fctx, store.GetTicketParams{OrgID: r.run.OrgID, ID: r.run.TicketID})
	if err != nil {
		return r.dbErr(ctx, "load ticket", err)
	}
	vc, err := versionConfig(version.Config)
	if err != nil {
		r.log.Warn("agent version config unreadable: no model, harness settings or instructions of its own", "err", err)
	}
	model := versionModel(vc)
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		return &failure{reason: "the agent names no model and no default model is configured"}
	}
	gw := strings.TrimSuffix(cfg.GatewayURL, "/")
	cmd, err := launchCommand(agent.Flavor, endpoints{ModelBaseURL: gw + "/v1", Model: model, MCPURL: gw + "/mcp"})
	if err != nil {
		return &failure{reason: err.Error()}
	}
	cmd = append(cmd, harnessArgs(agent.Flavor, vc)...)
	workspace, sandbox, err := r.waitSandbox(fctx)
	if err != nil {
		return r.execErr(ctx, "sandbox", err)
	}
	log := r.log.With("sandbox", sandbox)
	// Nothing of an earlier run may still be running when this one starts (sweep.go); a
	// sweep that cannot end everything fails the run.
	main, ended, err := r.sweep(fctx, workspace, sandbox)
	if err != nil {
		return r.execErr(ctx, "end the processes left in the sandbox", err)
	}
	if len(ended) > 0 {
		log.Warn("ended processes left in the sandbox", "pids", ended, "main", main)
		if err := r.record(fctx, "run.sandbox_swept", map[string]any{"pids": ended, "main": main}); err != nil {
			return r.dbErr(ctx, "record the sweep", err)
		}
	}
	cwd := cfg.SessionCwd
	if ticket.HarnessSessionID.Valid {
		cwd = ticket.HarnessSessionCwd.String
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
	loaded := ticket.HarnessSessionID.Valid
	if loaded {
		if !init.AgentCapabilities.LoadSession {
			return &failure{reason: "the harness cannot load the ticket's session (no loadSession capability)"}
		}
		session = acp.SessionID(ticket.HarnessSessionID.String)
		// The harness replays the session's history as updates before it answers. They are
		// already recorded (or belong to earlier runs), so they are not written again.
		r.replaying.Store(true)
		err = conn.LoadSession(fctx, session, cwd)
		r.replaying.Store(false)
		if err != nil {
			return harnessErr("session/load", err)
		}
	} else {
		if session, err = conn.NewSession(fctx, cwd); err != nil {
			return harnessErr("session/new", err)
		}
		if _, err := r.q.SetTicketHarnessSession(fctx, store.SetTicketHarnessSessionParams{
			OrgID: r.run.OrgID, ID: ticket.ID, RunID: r.run.ID, Epoch: r.run.Epoch,
			HarnessSessionID: string(session), HarnessSessionCwd: cwd,
		}); err != nil {
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
		"sessionId": session, "loaded": loaded, "takeovers": r.run.Takeovers,
		"agent": init.AgentInfo, "model": model,
		"instructions": map[string]any{"path": instructionsPath, "sha256": sha256Hex(instructions)},
	}); err != nil {
		return r.dbErr(ctx, "record the session", err)
	}

	text, through, err := r.prompt(fctx, ticket, loaded)
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
	err = r.turn(ctx, conn, proc, &stderr, session, text)
	if !errors.Is(err, errEndTurn) {
		return err
	}
	closeHarness()
	sweepOnce()
	return r.finish(ctx, store.RunStateSucceeded, "")
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
	deadline := time.Until(r.run.StartedAt.Time.Add(time.Duration(r.run.MaxWallClockSeconds) * time.Second))
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
			return &failure{reason: "the turn stopped: " + string(res.stop)}
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
		return &failure{reason: what + ": " + err.Error()}
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

// prompt builds the turn's prompt from the ticket's messages that no earlier run sent, and
// returns it with the timeline seq it covers. A run resumed after a takeover was already
// prompted: it gets the resume note plus anything that arrived since.
func (r *runner) prompt(ctx context.Context, ticket store.Ticket, loaded bool) (string, int64, error) {
	resumed := r.run.PromptThroughSeq.Valid
	after := r.run.PromptThroughSeq.Int64
	if !resumed {
		var err error
		after, err = r.q.GetTicketPromptedThrough(ctx, store.GetTicketPromptedThroughParams{
			OrgID: r.run.OrgID, TicketID: ticket.ID, RunID: r.run.ID,
		})
		if err != nil {
			return "", 0, err
		}
	}
	msgs, err := r.q.ListTicketMessagesAfter(ctx, store.ListTicketMessagesAfterParams{
		OrgID: r.run.OrgID, TicketID: ticket.ID, AfterSeq: after,
	})
	if err != nil {
		return "", 0, err
	}
	var parts []string
	switch {
	case resumed:
		parts = append(parts, resumeNote)
	case !loaded:
		parts = append(parts, "Ticket: "+ticket.Title)
	}
	through := after
	for _, m := range msgs {
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Payload, &body) == nil && strings.TrimSpace(body.Text) != "" {
			parts = append(parts, body.Text)
		}
		through = m.Seq
	}
	if len(parts) == 0 {
		parts = append(parts, "Continue with the ticket.")
	}
	return strings.Join(parts, "\n\n"), through, nil
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

// finish ends the run as its owner, with a closing run event and a ticket event, in one
// transaction. On success the ticket event is the agent's reply.
func (r *runner) finish(ctx context.Context, outcome store.RunState, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srcSeq++
	src := r.srcSeq
	err := pgx.BeginFunc(ctx, r.p.db, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		payload, _ := json.Marshal(map[string]string{"reason": reason})
		if _, err := q.AppendRunEventOnce(ctx, store.AppendRunEventOnceParams{
			OrgID: r.run.OrgID, RunID: r.run.ID, Epoch: r.run.Epoch,
			Kind: "run." + string(outcome), Payload: payload, SourceSeq: src,
		}); err != nil {
			return err
		}
		if _, err := q.FinishRun(ctx, store.FinishRunParams{
			OrgID: r.run.OrgID, ID: r.run.ID, Epoch: r.run.Epoch, Outcome: outcome,
			OutcomeReason: pgtype.Text{String: reason, Valid: reason != ""},
		}); err != nil {
			return err
		}
		ev := store.AppendTicketEventParams{
			OrgID: r.run.OrgID, TicketID: r.run.TicketID, RunID: r.run.ID,
		}
		if outcome == store.RunStateSucceeded {
			reply, err := r.reply(ctx, q)
			if err != nil {
				return err
			}
			if reply == "" {
				return nil
			}
			ev.Kind, ev.ActorKind, ev.ActorAgentID = "message", store.TicketActorKindAgent, r.run.AgentID
			ev.Payload, _ = json.Marshal(map[string]string{"text": reply})
		} else {
			ev.Kind, ev.ActorKind = "run."+string(outcome), store.TicketActorKindSystem
			ev.Payload = payload
		}
		_, err := q.AppendTicketEvent(ctx, ev)
		return err
	})
	if err != nil {
		return r.dbErr(ctx, "finish the run", err)
	}
	r.log.Info("run finished", "outcome", outcome, "reason", reason)
	return nil
}

// reply is the agent's message text of the run, across every epoch.
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
	switch {
	case err == nil:
	case errors.As(err, &f):
		if ferr := r.finish(ctx, store.RunStateFailed, f.reason); ferr != nil {
			r.log.Warn("failing the run failed", "reason", f.reason, "err", ferr)
		}
	case errors.As(err, &c):
		if ferr := r.finish(ctx, store.RunStateCanceled, c.reason); ferr != nil {
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
	// TODO(token cap, #27): call scheduler.RecordTokenUsage once ACP reports tokens consumed.
	// ACP session updates carry no consumption today: the only usage update (usage_update,
	// unstable in acp-go-sdk v0.13.5) reports the context window's occupancy (used/size) and
	// a cost, which rise and fall with compaction and cannot be summed into tokens used. The
	// unstable PromptResponse.usage arrives only when the turn, and so the run, has ended, too
	// late to cap it. Until then only the spend ledger (plan item L1, not built yet), the
	// hook's other caller, can enforce the cap; usage_update is still recorded as a run event.
	return nil
}

// toolCallWait bounds how long a permission request waits for its tool call's update. The
// SDK hands notifications to one queue and requests to their own goroutines, so a request
// can overtake the tool_call update the harness sent just before it; waiting keeps the run
// events in the harness's order.
const toolCallWait = 250 * time.Millisecond

func (h handler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (acp.Decision, error) {
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
