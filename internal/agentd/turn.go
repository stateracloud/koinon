// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/runlimits"
)

// The turn driver (ADR 0087). internal/agentd/turnpolicy decides what happens to a run's turn; this
// file owns what the policy must not: the goroutine of the prompt, the ACP connection, the ledger
// reads, the database writes and the timer. It feeds the policy events with the time they happened
// at and carries out the action that comes back. It turns a prompt error into facts (classify.go,
// budgetstop.go, providerrefusal.go read the ledger and the gateway's record) and the policy's
// outcome to execute as a turnResult.

type promptResult struct {
	stop acp.StopReason
	err  error
}

// turnDriver runs the turns of one run: the first prompt, and every prompt the policy sends after
// it (a wrap-up, a refusal, a retry), in the same session.
type turnDriver struct {
	r         *runner
	ctx, fctx context.Context
	conn      *acp.Conn
	proc      execution.Process
	stderr    *tail
	session   acp.SessionID
	c         *turnpolicy.Controller

	// text is the prompt in progress; done delivers its result.
	text    string
	done    chan promptResult
	started time.Time

	// What the policy does not interpret and the outcome returns: the classified error of the last
	// result, its failure, and what agentd refused in the last turn.
	err     error
	failure *failure
	refs    []refusal
}

// turnWithRetry runs the turn and, as the policy says, the turns after it: the wrap-up notice, the
// continuation of a refused turn and the retry of a failed model request. The wait ends when the
// worker is fenced or stops.
func (r *runner) turnWithRetry(ctx, fctx context.Context, conn *acp.Conn, proc execution.Process, stderr *tail, session acp.SessionID, text string) (*turnResult, error) {
	d := &turnDriver{
		r: r, ctx: ctx, fctx: fctx, conn: conn, proc: proc, stderr: stderr, session: session, text: text,
		c: turnpolicy.New(r.turnConfig()),
	}
	return d.run()
}

// turnConfig is what the policy is given about the run and the platform.
func (r *runner) turnConfig() turnpolicy.Config {
	cfg := r.p.cfg
	limits, err := runlimits.Of(r.run)
	if err != nil {
		// The notice is a courtesy, and the limits themselves are enforced elsewhere.
		r.log.Warn("reading the run's limits for the wrap-up notice failed", "err", err)
		limits.MaxModelRequests, limits.MaxSpend = 0, 0
	}
	// One layer retries a failed model request (ADR 0056 amendment 2026-10-31, point 3). When the
	// harness does, with the policy agentd gave it (launchCommand), a retryable failure that reaches
	// agentd has already used every try, so the turn is not repeated.
	retries := cfg.ModelRetries
	if r.harnessRetries() {
		retries = 0
	}
	return turnpolicy.Config{
		CapEnd:          r.capEnd(),
		WallClock:       time.Duration(r.run.MaxWallClockSeconds) * time.Second,
		WrapUpNotice:    cfg.WrapUpNotice,
		WrapUpFraction:  cfg.WrapUpFraction,
		MaxRequests:     limits.MaxModelRequests,
		MaxSpend:        limits.MaxSpend,
		NoticeRecorded:  r.run.WrapUpNoticeAt.Valid,
		CancelGrace:     cfg.CancelGrace,
		MaxRefusedTurns: cfg.MaxRefusedTurns,
		ModelRetries:    retries,
		RetryBackoff:    cfg.RetryBackoff,
		MaxRetryAfter:   cfg.MaxRetryAfter,
	}
}

// harnessRetries reports whether the run's harness is the layer that retries model requests.
func (r *runner) harnessRetries() bool {
	return harnessRetries(r.flavor) && !r.p.cfg.ownRetryLayer
}

// run returns the policy's outcome, or the error of a write the turns needed.
func (d *turnDriver) run() (*turnResult, error) {
	a := d.c.Begin(time.Now())
	for {
		if a.Ended != turnpolicy.EndedNone {
			d.r.tr.noteTurnReason(d.reason(a.Ended))
		}
		var ev turnpolicy.Event
		var err error
		switch a.Kind {
		case turnpolicy.ActReadUsage:
			ev = d.readUsage()
		case turnpolicy.ActMarkNotice:
			ev, err = d.markNotice(a)
		case turnpolicy.ActSendPrompt:
			d.send(a)
			ev = d.await(a)
		case turnpolicy.ActWait, turnpolicy.ActSleep:
			ev = d.await(a)
		case turnpolicy.ActCancel:
			ev = d.cancel(a)
		case turnpolicy.ActRecord:
			err = d.record(a)
			ev = turnpolicy.Event{Kind: turnpolicy.EvRecorded}
		case turnpolicy.ActEnd:
			return d.result(a.Outcome), nil
		}
		if err != nil {
			return nil, err
		}
		a = d.c.Step(ev, time.Now())
	}
}

// reason is how a turn ended, for the run's trace.
func (d *turnDriver) reason(e turnpolicy.Ended) string {
	switch e {
	case turnpolicy.EndedTurn:
		return "end_turn"
	case turnpolicy.EndedWrapUp:
		return "wrap_up"
	case turnpolicy.EndedRefused:
		return "refused"
	case turnpolicy.EndedFailed:
		return "failed"
	case turnpolicy.EndedCanceled:
		return "canceled"
	case turnpolicy.EndedFenced:
		return "fenced"
	case turnpolicy.EndedShutdown:
		return "shutdown"
	}
	return turnReason(d.err)
}

func (d *turnDriver) readUsage() turnpolicy.Event {
	if u, ok := d.r.readUsage(d.fctx); ok {
		return turnpolicy.Event{Kind: turnpolicy.EvUsage, Usage: u}
	}
	return turnpolicy.Event{Kind: turnpolicy.EvUsageUnavailable}
}

// markNotice writes the notice row. A row that was not changed (another owner holds the run) is
// reported to the policy; a failed write ends the turns.
func (d *turnDriver) markNotice(a turnpolicy.Action) (turnpolicy.Event, error) {
	if a.Suppress {
		if err := d.r.suppressWrapUp(d.ctx); err != nil {
			return turnpolicy.Event{}, err
		}
		d.r.log.Warn("the harness did not end its turn for the wrap-up notice; no notice is sent")
		return turnpolicy.Event{Kind: turnpolicy.EvNoticeResult}, nil
	}
	recorded, err := d.r.markWrapUp(d.fctx, a.Cause)
	if err != nil {
		return turnpolicy.Event{}, err
	}
	return turnpolicy.Event{Kind: turnpolicy.EvNoticeResult, Recorded: recorded}, nil
}

// record writes a run event of the policy.
func (d *turnDriver) record(a turnpolicy.Action) error {
	r := d.r
	switch a.Record {
	case turnpolicy.RecordRefusal:
		if err := r.own.Record(d.fctx, "run.refusal", map[string]any{"refusals": d.refs, "count": a.Count, "cwd": r.cwd}); err != nil {
			return r.dbErr(d.ctx, "record the refusal", err)
		}
	case turnpolicy.RecordRetry:
		f := d.failure
		r.log.Info("retrying the turn", "class", f.class, "attempt", a.Attempt, "delay", a.Delay)
		if err := r.own.Record(d.fctx, "run.retry", map[string]any{
			"failureClass": f.class, "attempt": a.Attempt, "delaySeconds": a.Delay.Seconds(), "reason": f.reason,
		}); err != nil {
			return r.dbErr(d.ctx, "record the retry", err)
		}
	}
	return nil
}

// send starts a prompt in the session.
func (d *turnDriver) send(a turnpolicy.Action) {
	r := d.r
	limit := time.Duration(r.run.MaxWallClockSeconds) * time.Second
	switch a.Lead {
	case turnpolicy.LeadWrapUp:
		d.text = wrapUpNote(a.Cause, limit)
	case turnpolicy.LeadRefusal:
		d.text = refusalNote(d.refs, r.cwd)
	case turnpolicy.LeadRetry:
		d.text = fmt.Sprintf(retryNote, a.Class)
	}
	if a.Notice != nil {
		d.text = wrapUpNote(*a.Notice, limit) + "\n\n" + d.text
	}
	text := d.text
	done := make(chan promptResult, 1)
	d.done = done
	d.started = time.Now()
	r.takeRefusals()
	meta := r.tr.beginTurn(d.ctx, text)
	go func() {
		stop, err := d.conn.PromptMeta(d.ctx, d.session, text, meta)
		done <- promptResult{stop, err}
	}()
}

// await waits for the next event: the end of the prompt, the worker being fenced, the deadline of
// the action, a ledger snapshot (when the action asks for polling) or the pool stopping.
func (d *turnDriver) await(a turnpolicy.Action) turnpolicy.Event {
	r := d.r
	var deadline <-chan time.Time
	if !a.Deadline.IsZero() {
		t := time.NewTimer(time.Until(a.Deadline))
		defer t.Stop()
		deadline = t.C
	}
	var poll <-chan time.Time
	if a.PollUsage {
		t := time.NewTicker(r.p.cfg.UsagePoll)
		defer t.Stop()
		poll = t.C
	}
	for {
		select {
		case res := <-d.done:
			return d.promptDone(res)
		case <-r.own.Fenced():
			return turnpolicy.Event{Kind: turnpolicy.EvFenced, FenceCanceled: r.own.FenceKind() == fenceCanceled}
		case <-deadline:
			return turnpolicy.Event{Kind: turnpolicy.EvTick}
		case <-poll:
			if u, ok := r.readUsage(d.ctx); ok {
				return turnpolicy.Event{Kind: turnpolicy.EvUsage, Usage: u}
			}
		case <-d.ctx.Done():
			return turnpolicy.Event{Kind: turnpolicy.EvShutdown}
		}
	}
}

// cancel sends session/cancel and gives the harness the grace to end the turn, so it can save its
// session. Nothing else is watched meanwhile.
func (d *turnDriver) cancel(a turnpolicy.Action) turnpolicy.Event {
	r := d.r
	switch a.Why {
	case turnpolicy.CancelCap:
		r.log.Info("wall-clock cap reached; cancelling the turn")
	case turnpolicy.CancelFence:
		r.log.Info("run canceled; cancelling the turn")
	default:
		r.log.Info("run limit near; interrupting the turn to ask the agent to wrap up", "limit", a.Cause.Kind)
	}
	if err := d.conn.Cancel(d.ctx, d.session); err != nil {
		r.log.Warn("session/cancel failed", "err", err)
		return turnpolicy.Event{Kind: turnpolicy.EvCancelFailed}
	}
	select {
	case res := <-d.done:
		r.log.Info("turn ended after cancel", "stop", res.stop, "err", res.err)
		return d.promptDone(res)
	case <-time.After(a.Grace):
		r.log.Warn("the harness did not end its turn after session/cancel")
		return turnpolicy.Event{Kind: turnpolicy.EvCancelTimeout}
	case <-d.ctx.Done():
		return turnpolicy.Event{Kind: turnpolicy.EvShutdown}
	}
}

// promptDone reads how the prompt ended into the facts the policy decides on. An error is classified
// (harness exit, gateway refusal, budget stop, model gone) only when the policy asks for it: a turn
// that ends because it was cancelled is not a failed turn.
func (d *turnDriver) promptDone(res promptResult) turnpolicy.Event {
	r := d.r
	// The prompt is over: the message the agent was writing is complete after an end_turn and cut
	// off after anything else (old issue 344).
	r.own.FlushItems(d.ctx, res.err != nil || res.stop != acp.StopEndTurn)
	out := turnpolicy.Result{}
	switch {
	case res.err != nil && d.c.NeedsClassification():
		err := r.modelGone(d.ctx, r.providerRefusal(d.ctx, r.budgetStop(d.ctx, r.harnessErr(d.ctx, d.conn, d.proc, d.stderr, "session/prompt", res.err)), d.started))
		var f *failure
		if errors.As(err, &f) {
			d.err, d.failure = err, f
			out = failed(f)
		} else {
			d.err = err
			out.Kind = turnpolicy.ResultPassthrough
		}
	case res.err != nil:
		out.Kind = turnpolicy.ResultErrored
		d.takeRefusals(&out)
	case res.stop == acp.StopEndTurn:
		out.Kind = turnpolicy.ResultEndTurn
	case res.stop == acp.StopCancelled:
		out.Kind = turnpolicy.ResultCancelled
		d.takeRefusals(&out)
	default:
		f := stopFailure(res.stop)
		d.err, d.failure = f, f
		out = failed(f)
	}
	return turnpolicy.Event{Kind: turnpolicy.EvPromptDone, Result: out}
}

func failed(f *failure) turnpolicy.Result {
	return turnpolicy.Result{
		Kind:    turnpolicy.ResultFailed,
		Failure: turnpolicy.Failure{Class: turnpolicy.Class(f.class), RetryAfter: f.retryAfter},
	}
}

// takeRefusals reads what agentd refused in the turn, and whether the worker was stopped meanwhile.
func (d *turnDriver) takeRefusals(out *turnpolicy.Result) {
	d.refs = d.r.takeRefusals()
	out.Refusals = len(d.refs) > 0
	if stop := d.r.stopped(d.ctx); errors.Is(stop, errFenced) {
		out.Stop = turnpolicy.StopFenced
	} else if stop != nil {
		out.Stop = turnpolicy.StopShutdown
	}
}

// turnResult is how the turns of a run ended: the policy's typed outcome with the facts the driver
// holds for it. conclude and the trace switch on Outcome directly. Only an outcome the policy does not
// interpret (OutFailed, OutPassthrough) carries the driver's classified error.
type turnResult struct {
	out turnpolicy.Outcome
	// err is the classified error of the last prompt (OutFailed, OutPassthrough).
	err error
	// failure is the last failed prompt's failure (OutRetriesExhausted).
	failure *failure
	// refs is what agentd refused in the last turn (OutRefusalsExceeded).
	refs []refusal
	// maxRetryAfter is the longest provider wait Koinon accepts (OutRetriesExhausted).
	maxRetryAfter time.Duration
	// harnessTries is how many tries the harness was allowed when it is the retrying layer, else 0
	// (OutRetriesExhausted).
	harnessTries int
	// capNote is the reason of a wall-clock cap (OutCapReached).
	capNote string
}

// fails reports whether the outcome fails the run for a reason the driver holds, with the reason
// and class. OutFailed and OutPassthrough are the classified error of the last prompt instead.
func (t *turnResult) fails() (reason string, class store.RunFailureClass, ok bool) {
	switch t.out.Kind {
	case turnpolicy.OutHarnessCancelled:
		return "the harness cancelled the turn", "", true
	case turnpolicy.OutRetriesExhausted:
		f := exhausted(t.failure, t.out.Retries, t.harnessTries, t.maxRetryAfter)
		return f.reason, f.class, true
	case turnpolicy.OutRefusalsExceeded:
		ref := &refused{refusals: t.refs}
		return fmt.Sprintf("%s (the agent kept asking after %d refusals)", ref.Error(), t.out.Refused), classPolicy, true
	}
	return "", "", false
}

// result carries the policy's outcome to execute.
func (d *turnDriver) result(o turnpolicy.Outcome) *turnResult {
	r := d.r
	tries := 0
	if r.harnessRetries() {
		tries = retryPolicyOf(r.p.cfg).Tries
	}
	return &turnResult{
		out: o, err: d.err, failure: d.failure, refs: d.refs, maxRetryAfter: r.p.cfg.MaxRetryAfter, harnessTries: tries,
		capNote: fmt.Sprintf("wall-clock cap of %s reached", time.Duration(r.run.MaxWallClockSeconds)*time.Second),
	}
}
