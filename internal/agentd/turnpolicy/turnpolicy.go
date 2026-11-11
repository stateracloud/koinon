// SPDX-License-Identifier: Apache-2.0

// Package turnpolicy decides what happens to a run's turn: the wall-clock cap, the wrap-up notice,
// refused tool calls, model retries and how a fence or a cancel ends it (ADR 0081, ADR 0050, and
// ADR 0087 for this split).
//
// The policy is a pure state machine. A driver (internal/agentd) owns the goroutines, the ACP
// connection, the ledger reads and the database writes. It feeds the Controller events together with
// the time they happened at, and carries out the Action that comes back. The package sees only what
// it is given: no clock, no database, no goroutines, no classification of harness errors (the driver
// turns a prompt error into the facts of a Result).
//
// Every Action that waits carries its next Deadline. The driver arms one timer for it and feeds
// EvTick when it fires. Parked time stays in the cap's input: Config.CapEnd is StartedAt +
// MaxWallClock + ParkedSeconds.
//
// Ordering rules the machine keeps (each has a spec):
//
//   - a run past its cap gets no notice (its turn is cancelled at once, so the agent could not act
//     on it, and the event and the metric must mean the agent was told);
//   - a refusal that lands with a cancel wins over the wrap-up, the cap and the fence;
//   - a cancel the harness honours late is not a failure: the run waits for the cap;
//   - only the owner that recorded the notice sends it;
//   - a notice interrupts a turn once per limit window, and one notice is sent per run;
//   - a loop (EvLoop) is nudged once per run, in the turn after the interrupted one, and the second
//     loop ends the turns with OutLoop (ADR 0030 amendment 2026-11-11).
package turnpolicy

import (
	"time"

	"github.com/stateracloud/koinon/internal/money"
)

// Kind names the limit a wrap-up notice is about. The values are those of runlimits.Kind.
type Kind string

// The limits that have a window.
const (
	KindTime     Kind = "time"
	KindRequests Kind = "requests"
	KindSpend    Kind = "spend"
)

// Class is a failure class, as far as the policy interprets one. The values are those of the
// store's failure classes; any other class is simply not retried.
type Class string

// The classes the policy retries.
const (
	ClassTransient Class = "transient"
	ClassThrottled Class = "throttled"
)

// Retryable reports whether Koinon repeats a turn that failed with this class by itself.
func (c Class) Retryable() bool { return c == ClassTransient || c == ClassThrottled }

// Config is everything the policy is given about the run and the platform.
type Config struct {
	// CapEnd is when the wall-clock cap is reached: StartedAt + MaxWallClock + ParkedSeconds.
	CapEnd time.Time
	// WallClock is the run's own time limit, without parked time.
	WallClock time.Duration
	// WrapUpNotice is how long before the cap the time notice goes out; zero is none.
	WrapUpNotice time.Duration
	// WrapUpFraction is how far through a request or spend limit the notice goes out; zero is none.
	WrapUpFraction float64
	// MaxRequests and MaxSpend are the run's count limits; zero is no limit. The ledger enforces
	// them (ADR 0081); the policy only reads them to time the notice.
	MaxRequests int64
	MaxSpend    money.USD
	// NoticeRecorded: the run already carries a notice (this owner or an earlier one sent it).
	NoticeRecorded bool

	// CancelGrace is how long a cancelled turn has to end.
	CancelGrace time.Duration
	// MaxRefusedTurns is how many consecutive refused turns the agent may cause.
	MaxRefusedTurns int
	// ModelRetries, RetryBackoff and MaxRetryAfter rule the retry of a failed model request.
	ModelRetries  int
	RetryBackoff  time.Duration
	MaxRetryAfter time.Duration
}

// Cause is the limit a wrap-up notice is about and where the run stands against it.
type Cause struct {
	Kind Kind
	// Remaining is the time left, for the time limit.
	Remaining time.Duration
	// Requests and MaxRequests, Spent and MaxSpend, for the request and spend limits.
	Requests, MaxRequests int64
	Spent, MaxSpend       money.USD
}

// Usage is a snapshot of what the ledger says the run has used.
type Usage struct {
	Requests int64
	Spend    money.USD
}

// Lead is which text opens a prompt.
type Lead int

// The leads.
const (
	LeadOriginal Lead = iota // the prompt the driver was given
	LeadWrapUp               // the wrap-up note of the Action's Cause
	LeadRefusal              // the note that tells the agent what was refused
	LeadRetry                // the note that tells the agent its model request is retried
	LeadLoop                 // the note that tells the agent it is looping (Pattern)
)

// ActionKind is what the driver does next.
type ActionKind int

// The actions.
const (
	// ActReadUsage: read the ledger and answer EvUsage (or EvUsageUnavailable).
	ActReadUsage ActionKind = iota + 1
	// ActMarkNotice: run MarkRunWrapUpNotice and answer EvNoticeResult. Unless Suppress is set, a
	// recorded notice also records the run event and counts the metric.
	ActMarkNotice
	// ActSendPrompt: start a prompt (Lead, Notice) and wait for events. The driver resets its loop
	// detector: a prompt is a new user message.
	ActSendPrompt
	// ActWait: wait for events until Deadline.
	ActWait
	// ActCancel: send session/cancel and wait Grace for the prompt to end: EvPromptDone,
	// EvCancelTimeout or EvCancelFailed.
	ActCancel
	// ActRecord: record a run event (Record) and answer EvRecorded.
	ActRecord
	// ActSleep: wait Delay (until Deadline) and answer EvTick.
	ActSleep
	// ActEnd: the turns are over with Outcome.
	ActEnd
)

// CancelWhy is the reason for an ActCancel.
type CancelWhy int

// The reasons to cancel a turn.
const (
	CancelCap      CancelWhy = iota + 1 // the wall-clock cap is reached
	CancelFence                         // the control plane canceled the run
	CancelWrap                          // a limit is near: tell the agent
	CancelLoop                          // the agent is looping: interrupt the turn, then nudge it
	CancelLoopStop                      // the agent kept looping after the nudge: end the run
)

// RecordKind is the event of an ActRecord.
type RecordKind int

// The events the policy has recorded.
const (
	RecordRefusal   RecordKind = iota + 1 // run.refusal: Count refused turns so far
	RecordRetry                           // run.retry: attempt Attempt after Delay, for Class
	RecordLoopNudge                       // run.loop_nudge: the agent is told it loops (Pattern)
)

// Ended says how the turn that just ended did, for the run's trace. The zero value is a step that
// did not end a turn.
type Ended int

// How a turn ends.
const (
	EndedNone Ended = iota
	EndedTurn
	EndedWrapUp
	EndedLoop
	EndedRefused
	EndedFailed
	EndedCanceled
	EndedFenced
	EndedShutdown
	EndedPassthrough
)

// Action is the next thing the driver does.
type Action struct {
	Kind  ActionKind
	Ended Ended
	// Deadline is when the driver feeds EvTick; zero is never. The driver arms one timer for it.
	Deadline time.Time

	// ActSendPrompt: Lead opens the text (LeadWrapUp uses Cause, LeadRetry uses Class); Notice, when
	// set, is a wrap-up note prepended to it. PollUsage asks for an EvUsage every UsagePoll.
	Lead      Lead
	Class     Class
	Notice    *Cause
	PollUsage bool
	// WatchLoop: feed EvLoop when the loop detector reports a pattern (while a prompt is running).
	WatchLoop bool
	// Pattern: the loop pattern of an ActRecord (RecordLoopNudge) and of an ActSendPrompt (LeadLoop).
	Pattern string

	// ActMarkNotice, ActCancel (CancelWrap) and ActSendPrompt (LeadWrapUp): the limit.
	Cause    Cause
	Suppress bool

	// ActCancel.
	Why   CancelWhy
	Grace time.Duration

	// ActRecord: Count for a refusal, Attempt and Delay (and Class) for a retry. ActSleep: Delay.
	Record  RecordKind
	Count   int
	Attempt int
	Delay   time.Duration

	// ActEnd.
	Outcome Outcome
}

// OutcomeKind is how the turns ended.
type OutcomeKind int

// The outcomes.
const (
	OutEndTurn          OutcomeKind = iota + 1 // the agent ended its turn
	OutCapReached                              // the wall-clock cap
	OutFailed                                  // a failure that is not retried (the driver has it)
	OutRetriesExhausted                        // a retryable failure that ran out of retries
	OutRefusalsExceeded                        // the agent kept causing refusals
	OutHarnessCancelled                        // the harness cancelled a turn nobody cancelled
	OutFenced                                  // the worker may no longer write
	OutShutdown                                // the pool is stopping
	OutPassthrough                             // an error the driver classified that the policy does not interpret
	OutLoop                                    // the agent kept looping after the nudge (Outcome.Pattern)
)

// Outcome is the end of the turns.
type Outcome struct {
	Kind OutcomeKind
	// Retries is how many retries were made (OutRetriesExhausted).
	Retries int
	// Refused is how many consecutive refused turns the agent caused before this one
	// (OutRefusalsExceeded).
	Refused int
	// Pattern is the loop pattern that ended the turns (OutLoop).
	Pattern string
	// NudgeUndelivered: the harness ignored the loop cancel, so the agent was never told it loops
	// (OutLoop).
	NudgeUndelivered bool
}

// ResultKind is how a prompt ended, as the driver reads it.
type ResultKind int

// The kinds of result.
const (
	// ResultEndTurn: the harness ended the turn.
	ResultEndTurn ResultKind = iota + 1
	// ResultCancelled: the harness ended the turn as cancelled. Refusals says whether agentd refused
	// tool calls during it.
	ResultCancelled
	// ResultErrored: the prompt returned an error the driver has not classified (it classifies
	// only when NeedsClassification is true).
	ResultErrored
	// ResultFailed: a failure (a classified error, or a stop reason that ends a turn without
	// finishing it): Failure says what the policy needs of it.
	ResultFailed
	// ResultPassthrough: an error the driver classified into something the policy does not
	// interpret (the connection dropped, a budget stop, the worker is fenced). It ends the turns.
	ResultPassthrough
)

// Stop says whether the worker was stopped when a result arrived.
type Stop int

// The ways a worker is stopped.
const (
	StopNone Stop = iota
	StopFenced
	StopShutdown
)

// Failure is what the policy needs of a failure; the driver keeps the rest.
type Failure struct {
	Class      Class
	RetryAfter time.Duration
}

// Result is how a prompt ended.
type Result struct {
	Kind ResultKind
	// Refusals: agentd refused tool calls during the turn (ResultCancelled, ResultErrored).
	Refusals bool
	Failure  Failure
	// Stop: the worker was fenced or is shutting down (read for ResultCancelled and ResultErrored).
	Stop Stop
}

// EventKind is what happened.
type EventKind int

// The events.
const (
	EvPromptDone       EventKind = iota + 1 // the prompt ended: Result
	EvTick                                  // a Deadline passed
	EvUsage                                 // a ledger snapshot: Usage
	EvUsageUnavailable                      // the ledger could not be read
	EvFenced                                // the worker was fenced: FenceCanceled
	EvShutdown                              // the pool is stopping
	EvCancelFailed                          // session/cancel could not be sent
	EvCancelTimeout                         // the harness did not end its turn within the grace
	EvNoticeResult                          // the notice row was written: Recorded
	EvRecorded                              // the run event was recorded
	EvLoop                                  // the loop detector found a pattern: Pattern
)

// Event is what the driver feeds the Controller.
type Event struct {
	Kind   EventKind
	Result Result
	Usage  Usage
	// Recorded (EvNoticeResult): the row was changed. False means another owner holds the run.
	Recorded bool
	// FenceCanceled (EvFenced): the control plane canceled the run; the turn is cancelled before
	// the worker lets go.
	FenceCanceled bool
	// Pattern (EvLoop) names the stuck pattern.
	Pattern string
}

type phase int

const (
	phaseIdle phase = iota
	phaseUsage
	phaseNoticePre
	phasePrompt
	phaseCancel
	phaseSuppress
	phaseNoticePost
	phaseLate
	phaseRecord
	phaseSleep
	phaseDone
)

// Controller is the policy of one run's turns.
type Controller struct {
	cfg   Config
	phase phase

	// noticeSent: the notice is recorded or suppressed; none is armed any more.
	noticeSent bool
	// refused counts consecutive refused turns; retries counts the retries made.
	refused, retries int

	lead      Lead
	leadClass Class
	leadCause Cause

	// pending is the cause of the notice being marked or of the cancel in flight.
	pending Cause
	why     CancelWhy
	// timeArmed and usageArmed: the time and usage notices can still fire in this turn.
	timeArmed, usageArmed bool
	// cancelIgnored: the wrap-up cancel went out and the harness ignored it for the grace.
	cancelIgnored bool

	// loopNudged: the one nudge of the run is used (sent, or not deliverable); loopUndelivered: it
	// was not deliverable. loopPattern is the pattern being nudged; nextLoop makes the record's
	// follow-up the nudge prompt.
	loopNudged      bool
	loopUndelivered bool
	loopPattern     string
	// loopCancelTimedOut: the cancel that interrupted the first loop was ignored for the grace. A
	// turn that then ends cancelled is the late cancel: the nudge is deliverable after all.
	loopCancelTimedOut bool
	nextLoop           bool

	// next is the record's follow-up.
	nextSleep bool
	failure   Failure
	delay     time.Duration
	sleepEnd  time.Time
}

// New returns the policy of a run's turns.
func New(cfg Config) *Controller {
	return &Controller{cfg: cfg, noticeSent: cfg.NoticeRecorded}
}

// Begin starts the first turn.
func (c *Controller) Begin(now time.Time) Action { return c.begin(now) }

// NeedsClassification: the next EvPromptDone with an error is classified by the driver (a failure
// class, a gateway refusal, a budget stop). While a cancel is awaited it is not: the turn ended
// because it was cancelled.
func (c *Controller) NeedsClassification() bool { return c.phase == phasePrompt }

// Step feeds one event, which happened at now, and returns the next action.
func (c *Controller) Step(ev Event, now time.Time) Action {
	switch c.phase {
	case phaseUsage:
		switch ev.Kind {
		case EvUsage:
			u := ev.Usage
			return c.proceed(now, &u)
		case EvUsageUnavailable:
			return c.proceed(now, nil)
		}
	case phaseNoticePre:
		if ev.Kind == EvNoticeResult {
			c.noticeSent = true
			if ev.Recorded {
				cause := c.pending
				return c.send(now, &cause)
			}
			return c.send(now, nil)
		}
	case phasePrompt:
		return c.inPrompt(ev, now)
	case phaseCancel:
		return c.inCancel(ev, now)
	case phaseSuppress:
		if ev.Kind == EvNoticeResult {
			// Arm no new notice; the run ends at the limit as it does without one.
			c.noticeSent = true
			c.cancelIgnored = true
			c.phase = phasePrompt
			return c.wait(ActWait)
		}
	case phaseNoticePost:
		if ev.Kind == EvNoticeResult {
			c.noticeSent = true
			if !ev.Recorded {
				return c.end(OutFenced, EndedNone) // another owner holds the run
			}
			c.lead, c.leadCause = LeadWrapUp, c.pending
			return c.begin(now)
		}
	case phaseLate:
		switch ev.Kind {
		case EvTick:
			if !now.Before(c.cfg.CapEnd) {
				return c.end(OutCapReached, EndedCanceled)
			}
		case EvFenced:
			return c.end(OutFenced, EndedFenced)
		case EvShutdown:
			return c.end(OutShutdown, EndedShutdown)
		}
	case phaseRecord:
		if ev.Kind == EvRecorded {
			if c.nextLoop {
				c.nextLoop = false
				c.lead = LeadLoop
				return c.begin(now)
			}
			if c.nextSleep {
				c.phase = phaseSleep
				c.sleepEnd = now.Add(c.delay)
				return Action{Kind: ActSleep, Delay: c.delay, Deadline: c.sleepEnd}
			}
			c.lead = LeadRefusal
			return c.begin(now)
		}
	case phaseSleep:
		switch ev.Kind {
		case EvTick:
			if !now.Before(c.sleepEnd) {
				c.retries++
				c.lead, c.leadClass = LeadRetry, c.failure.Class
				return c.begin(now)
			}
		case EvFenced:
			return c.end(OutFenced, EndedNone)
		case EvShutdown:
			return c.end(OutShutdown, EndedNone)
		}
	case phaseDone:
		return Action{Kind: ActEnd, Outcome: Outcome{Kind: OutShutdown}}
	}
	return c.wait(ActWait)
}

// The wrap-up notice is armed only while none was sent and a limit has a window.

func (c *Controller) timeNotice() bool {
	return c.cfg.WrapUpNotice > 0 && c.cfg.WallClock > c.cfg.WrapUpNotice
}

func (c *Controller) countNotice() bool {
	return c.cfg.WrapUpFraction > 0 && (c.cfg.MaxRequests > 0 || c.cfg.MaxSpend > 0)
}

func (c *Controller) wants() bool {
	return (c.timeNotice() || c.countNotice()) && !c.noticeSent
}

// timeCause is the time limit's window now: ok when the cap is within WrapUpNotice and not passed.
// A run already past its cap gets no notice.
func (c *Controller) timeCause(now time.Time) (Cause, bool) {
	remaining := c.cfg.CapEnd.Sub(now)
	return Cause{Kind: KindTime, Remaining: remaining}, c.timeNotice() && remaining > 0 && remaining <= c.cfg.WrapUpNotice
}

// usageCause is the request or spend limit that has reached its window in u, the nearer by
// fraction.
func (c *Controller) usageCause(u Usage) (Cause, bool) {
	if !c.countNotice() {
		return Cause{}, false
	}
	var best Cause
	bestFraction, found := 0.0, false
	consider := func(cause Cause, f float64) {
		if f >= c.cfg.WrapUpFraction && (!found || f > bestFraction) {
			best, bestFraction, found = cause, f, true
		}
	}
	if c.cfg.MaxRequests > 0 {
		consider(Cause{Kind: KindRequests, Requests: u.Requests, MaxRequests: c.cfg.MaxRequests},
			float64(u.Requests)/float64(c.cfg.MaxRequests))
	}
	if c.cfg.MaxSpend > 0 {
		consider(Cause{Kind: KindSpend, Spent: u.Spend, MaxSpend: c.cfg.MaxSpend}, money.Ratio(u.Spend, c.cfg.MaxSpend))
	}
	return best, found
}

// due is the limit whose window the run is in, the nearest of the three, for a turn that starts.
func (c *Controller) due(now time.Time, u *Usage) (Cause, bool) {
	t, tok := c.timeCause(now)
	var uc Cause
	uok := false
	if u != nil {
		uc, uok = c.usageCause(*u)
	}
	switch {
	case tok && uok:
		// The nearer by fraction of its limit.
		if cap := c.cfg.WallClock; cap > 0 && 1-float64(t.Remaining)/float64(cap) >= uc.fraction() {
			return t, true
		}
		return uc, true
	case tok:
		return t, true
	}
	return uc, uok
}

// fraction is how far through its limit the cause is, for the count limits.
func (c Cause) fraction() float64 {
	switch c.Kind {
	case KindRequests:
		return float64(c.Requests) / float64(c.MaxRequests)
	case KindSpend:
		return money.Ratio(c.Spent, c.MaxSpend)
	}
	return 0
}

// begin starts a turn: it reads the ledger first when a window could already hold the run.
func (c *Controller) begin(now time.Time) Action {
	if c.wants() && c.countNotice() {
		c.phase = phaseUsage
		return Action{Kind: ActReadUsage}
	}
	return c.proceed(now, nil)
}

// proceed records the notice of a turn that starts inside a window (a resume, a retry, a takeover):
// the turn is not interrupted, and only the owner that recorded the notice prepends it.
func (c *Controller) proceed(now time.Time, u *Usage) Action {
	if c.wants() {
		if cause, due := c.due(now, u); due {
			c.phase = phaseNoticePre
			c.pending = cause
			return Action{Kind: ActMarkNotice, Cause: cause}
		}
	}
	return c.send(now, nil)
}

func (c *Controller) send(now time.Time, notice *Cause) Action {
	c.phase = phasePrompt
	c.cancelIgnored, c.loopCancelTimedOut = false, false
	c.timeArmed = c.wants() && c.timeNotice() && c.cfg.CapEnd.Sub(now) > c.cfg.WrapUpNotice
	c.usageArmed = c.wants() && c.countNotice()
	a := c.wait(ActSendPrompt)
	a.Lead, a.Class, a.Cause, a.Notice = c.lead, c.leadClass, c.leadCause, notice
	if c.lead == LeadLoop {
		a.Pattern = c.loopPattern
	}
	return a
}

// wait returns an action of kind k that carries the deadline and the polling of the current phase.
func (c *Controller) wait(k ActionKind) Action {
	a := Action{Kind: k}
	switch c.phase {
	case phasePrompt:
		a.Deadline = c.cfg.CapEnd
		if c.timeArmed {
			a.Deadline = c.cfg.CapEnd.Add(-c.cfg.WrapUpNotice)
		}
		a.PollUsage = c.usageArmed
		a.WatchLoop = true
	case phaseLate:
		a.Deadline = c.cfg.CapEnd
	case phaseSleep:
		a.Kind, a.Delay, a.Deadline = ActSleep, c.delay, c.sleepEnd
	}
	return a
}

func (c *Controller) end(k OutcomeKind, ended Ended) Action {
	c.phase = phaseDone
	return Action{Kind: ActEnd, Ended: ended, Outcome: Outcome{Kind: k}}
}

func (c *Controller) endStop(s Stop, ended Ended) Action {
	if s == StopFenced {
		return c.end(OutFenced, ended)
	}
	return c.end(OutShutdown, ended)
}

func (c *Controller) cancel(why CancelWhy, cause Cause) Action {
	c.phase = phaseCancel
	c.why = why
	c.pending = cause
	return Action{Kind: ActCancel, Why: why, Cause: cause, Grace: c.cfg.CancelGrace}
}

func (c *Controller) inPrompt(ev Event, now time.Time) Action {
	switch ev.Kind {
	case EvPromptDone:
		r := ev.Result
		if c.cancelIgnored && r.Kind == ResultCancelled {
			if r.Refusals {
				return c.refusedTurn(r)
			}
			if c.loopCancelTimedOut {
				// The harness honoured the loop interrupt late: go on with the loop flow, as if
				// it had ended the turn within the grace. The nudge is deliverable after all.
				c.loopCancelTimedOut = false
				if r.Stop != StopNone {
					return c.endStop(r.Stop, EndedLoop)
				}
				c.loopUndelivered = false
				return c.loopNudgeRecord()
			}
			// The harness honoured Koinon's own cancel late. No second prompt is sent; the run
			// ends at the cap as it does without a notice, not as a failed turn.
			c.phase = phaseLate
			return c.wait(ActWait)
		}
		return c.turnResult(r)
	case EvFenced:
		if ev.FenceCanceled {
			return c.cancel(CancelFence, Cause{})
		}
		return c.end(OutFenced, EndedFenced)
	case EvShutdown:
		return c.end(OutShutdown, EndedShutdown)
	case EvTick:
		if !now.Before(c.cfg.CapEnd) {
			return c.cancel(CancelCap, Cause{})
		}
		if c.timeArmed && !now.Before(c.cfg.CapEnd.Add(-c.cfg.WrapUpNotice)) {
			c.timeArmed = false
			return c.cancel(CancelWrap, Cause{Kind: KindTime, Remaining: c.cfg.CapEnd.Sub(now)})
		}
	case EvUsage:
		if c.usageArmed {
			if cause, ok := c.usageCause(ev.Usage); ok {
				c.usageArmed = false
				return c.cancel(CancelWrap, cause)
			}
		}
	case EvLoop:
		// One nudge per run, then the run ends: a bounded exception to "no agent nannies".
		c.loopPattern = ev.Pattern
		if c.loopNudged {
			return c.cancel(CancelLoopStop, Cause{})
		}
		return c.cancel(CancelLoop, Cause{})
	}
	return c.wait(ActWait)
}

func (c *Controller) inCancel(ev Event, now time.Time) Action {
	switch c.why {
	case CancelCap:
		return c.end(OutCapReached, EndedCanceled)
	case CancelFence:
		return c.end(OutFenced, EndedFenced)
	case CancelLoopStop:
		a := c.end(OutLoop, EndedLoop)
		a.Outcome.Pattern, a.Outcome.NudgeUndelivered = c.loopPattern, c.loopUndelivered
		return a
	case CancelLoop:
		return c.inLoopCancel(ev)
	}
	switch ev.Kind {
	case EvShutdown:
		return c.end(OutShutdown, EndedShutdown)
	case EvCancelFailed, EvCancelTimeout:
		// The harness ignored session/cancel: a second session/prompt must not overlap the first.
		// Arm no new notice. The notice row is set without an event or a metric, because those mean
		// that the agent was told.
		c.phase = phaseSuppress
		return Action{Kind: ActMarkNotice, Suppress: true}
	case EvPromptDone:
		r := ev.Result
		switch r.Kind {
		case ResultCancelled, ResultErrored:
			// A refused tool call may be why the turn ended; the agent must still be told.
			if r.Refusals {
				return c.refusedTurn(r)
			}
			// A cancel can arrive with the cancelled turn; it must not be answered with a prompt.
			if r.Stop != StopNone {
				return c.endStop(r.Stop, EndedWrapUp)
			}
			cause := c.pending
			if cause.Kind == KindTime {
				cause.Remaining = c.cfg.CapEnd.Sub(now)
			}
			c.phase = phaseNoticePost
			c.pending = cause
			return Action{Kind: ActMarkNotice, Cause: cause, Ended: EndedWrapUp}
		}
		// The turn ended on its own while the cancel was sent (for example end_turn after an
		// ask_user, where the run must park and must not get another prompt). Honour it; if a next
		// turn starts inside the window, it gets the notice prepended.
		return c.turnResult(r)
	}
	return Action{Kind: ActWait}
}

// inLoopCancel waits for the turn that was interrupted to tell the agent it loops. A turn that ends
// another way is honoured as it is (the agent is told after a refusal; end_turn ends the run). A
// harness that ignores the cancel cannot be nudged without overlapping prompts: the nudge is spent
// undelivered and the turn goes on, so the next loop ends the run with a reason that says the agent
// was never told. Keeping the nudge for later would not help: a looping turn that ignores cancels
// does not end cancelled, so the nudge would wait while the loop goes on spending budget.
func (c *Controller) inLoopCancel(ev Event) Action {
	switch ev.Kind {
	case EvShutdown:
		return c.end(OutShutdown, EndedShutdown)
	case EvCancelFailed, EvCancelTimeout:
		c.phase = phasePrompt
		c.cancelIgnored, c.loopNudged, c.loopUndelivered = true, true, true
		c.loopCancelTimedOut = true
		return c.wait(ActWait)
	case EvPromptDone:
		r := ev.Result
		switch r.Kind {
		case ResultCancelled, ResultErrored:
			if r.Refusals {
				return c.refusedTurn(r)
			}
			if r.Stop != StopNone {
				return c.endStop(r.Stop, EndedLoop)
			}
			return c.loopNudgeRecord()
		}
		return c.turnResult(r)
	}
	return Action{Kind: ActWait}
}

// loopNudgeRecord spends the one nudge: record it, then send it as the next prompt.
func (c *Controller) loopNudgeRecord() Action {
	c.loopNudged = true
	c.phase = phaseRecord
	c.nextSleep, c.nextLoop = false, true
	return Action{Kind: ActRecord, Ended: EndedLoop, Record: RecordLoopNudge, Pattern: c.loopPattern}
}

// turnResult acts on how a turn ended on its own.
func (c *Controller) turnResult(r Result) Action {
	switch r.Kind {
	case ResultEndTurn:
		return c.end(OutEndTurn, EndedTurn)
	case ResultCancelled:
		if r.Refusals {
			return c.refusedTurn(r)
		}
		return c.end(OutHarnessCancelled, EndedFailed)
	case ResultFailed:
		return c.failedTurn(r.Failure)
	}
	return c.end(OutPassthrough, EndedPassthrough)
}

// refusedTurn: the harness ended the turn because agentd refused a request. Tell the agent what
// was refused and let it go on; this is not a retry of a failed model request.
func (c *Controller) refusedTurn(r Result) Action {
	// A cancel (a person's, the budget's) can reach the worker after the harness ended the turn as
	// cancelled; it must not be answered with another prompt.
	if r.Stop != StopNone {
		return c.endStop(r.Stop, EndedRefused)
	}
	if c.refused >= c.cfg.MaxRefusedTurns {
		a := c.end(OutRefusalsExceeded, EndedRefused)
		a.Outcome.Refused = c.refused
		return a
	}
	c.refused++
	c.phase = phaseRecord
	c.nextSleep = false
	return Action{Kind: ActRecord, Ended: EndedRefused, Record: RecordRefusal, Count: c.refused}
}

// failedTurn retries a model request that failed with a transient or throttled error, after the
// backoff or the provider's Retry-After. Every other failure ends the turns at once.
func (c *Controller) failedTurn(f Failure) Action {
	// Only consecutive refused turns count: any other outcome is progress.
	c.refused = 0
	if !f.Class.Retryable() {
		return c.end(OutFailed, EndedFailed)
	}
	delay := Delay(f, c.retries, c.cfg.RetryBackoff)
	if c.retries >= c.cfg.ModelRetries || delay > c.cfg.MaxRetryAfter {
		a := c.end(OutRetriesExhausted, EndedFailed)
		a.Outcome.Retries = c.retries
		return a
	}
	c.phase = phaseRecord
	c.nextSleep = true
	c.failure, c.delay = f, delay
	return Action{Kind: ActRecord, Ended: EndedFailed, Record: RecordRetry, Class: f.Class, Attempt: c.retries + 1, Delay: delay}
}

// Delay is how long to wait before retry number retry (from 0): the provider's Retry-After for a
// throttled request, otherwise the exponential backoff from base, capped at one minute.
func Delay(f Failure, retry int, base time.Duration) time.Duration {
	if f.Class == ClassThrottled && f.RetryAfter > 0 {
		return f.RetryAfter
	}
	return min(base<<min(retry, 10), time.Minute)
}
