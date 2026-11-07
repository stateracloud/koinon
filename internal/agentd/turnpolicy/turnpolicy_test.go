// SPDX-License-Identifier: Apache-2.0

package turnpolicy_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
	"github.com/stateracloud/koinon/internal/money"
)

// These specs drive the Controller with events and times. They need no database, no clock and no
// goroutines: each rule that the fixes to the turn policy added is one spec (the commits are named
// in old issue 449).

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const (
	wallClock = 30 * time.Minute
	notice    = 5 * time.Minute
)

// cfg is a run that started at t0 with a 30 minute cap, a 5 minute notice and room for 3 refusals
// and 2 retries.
func cfg(edit ...func(*turnpolicy.Config)) turnpolicy.Config {
	c := turnpolicy.Config{
		CapEnd:          t0.Add(wallClock),
		WallClock:       wallClock,
		WrapUpNotice:    notice,
		WrapUpFraction:  0.8,
		CancelGrace:     10 * time.Second,
		MaxRefusedTurns: 3,
		ModelRetries:    2,
		RetryBackoff:    time.Second,
		MaxRetryAfter:   time.Minute,
	}
	for _, e := range edit {
		e(&c)
	}
	return c
}

func ev(k turnpolicy.EventKind) turnpolicy.Event { return turnpolicy.Event{Kind: k} }

func done(r turnpolicy.Result) turnpolicy.Event {
	return turnpolicy.Event{Kind: turnpolicy.EvPromptDone, Result: r}
}

func noticeResult(recorded bool) turnpolicy.Event {
	return turnpolicy.Event{Kind: turnpolicy.EvNoticeResult, Recorded: recorded}
}

func usage(requests int64, spend string) turnpolicy.Event {
	return turnpolicy.Event{Kind: turnpolicy.EvUsage, Usage: turnpolicy.Usage{Requests: requests, Spend: money.MustParse(spend)}}
}

var (
	endTurn   = turnpolicy.Result{Kind: turnpolicy.ResultEndTurn}
	cancelled = turnpolicy.Result{Kind: turnpolicy.ResultCancelled}
	refusal   = turnpolicy.Result{Kind: turnpolicy.ResultCancelled, Refusals: true}
	transient = turnpolicy.Result{Kind: turnpolicy.ResultFailed, Failure: turnpolicy.Failure{Class: turnpolicy.ClassTransient}}
	fatal     = turnpolicy.Result{Kind: turnpolicy.ResultFailed, Failure: turnpolicy.Failure{Class: "fatal"}}
)

// start begins the first turn at t0 and answers the ledger read with a snapshot far from any limit.
func start(c *turnpolicy.Controller, now time.Time) turnpolicy.Action {
	a := c.Begin(now)
	if a.Kind == turnpolicy.ActReadUsage {
		return c.Step(usage(1, "0.01"), now)
	}
	return a
}

// sending starts a turn and expects a plain prompt.
func sending(c *turnpolicy.Controller) turnpolicy.Action {
	a := start(c, t0)
	Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
	Expect(a.Notice).To(BeNil())
	return a
}

var _ = Describe("the wall-clock cap", func() {
	It("cancels the turn at the cap and ends the run as canceled", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.WrapUpNotice = 0 }))
		a := sending(c)
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))

		a = c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock))
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelCap))
		Expect(a.Grace).To(Equal(10 * time.Second))

		// However the cancel goes, the run ended at its cap.
		for _, e := range []turnpolicy.Event{done(cancelled), ev(turnpolicy.EvCancelTimeout), ev(turnpolicy.EvCancelFailed), ev(turnpolicy.EvShutdown)} {
			c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.WrapUpNotice = 0 }))
			sending(c)
			c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock))
			end := c.Step(e, t0.Add(wallClock))
			Expect(end.Kind).To(Equal(turnpolicy.ActEnd))
			Expect(end.Outcome.Kind).To(Equal(turnpolicy.OutCapReached))
			Expect(end.Ended).To(Equal(turnpolicy.EndedCanceled))
		}
	})

	It("counts parked time in the cap's end, not in the run's limit", func() {
		parked := 10 * time.Minute
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.CapEnd = t0.Add(wallClock + parked) }))
		a := sending(c)
		// The notice is WrapUpNotice before the cap's end, so after the parked time.
		Expect(a.Deadline).To(Equal(t0.Add(wallClock + parked - notice)))
	})

	It("ends a turn that the harness ended by itself while the cap was near without cancelling", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(endTurn), t0.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutEndTurn))
		Expect(a.Ended).To(Equal(turnpolicy.EndedTurn))
	})

	It("ignores a tick that arrives before the deadline", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.WrapUpNotice = 0 }))
		sending(c)
		a := c.Step(ev(turnpolicy.EvTick), t0.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))
	})
})

var _ = Describe("the wrap-up notice for time", func() {
	nearCap := t0.Add(wallClock - notice)

	It("interrupts the turn WrapUpNotice before the cap, then sends the notice as the next prompt", func() {
		c := turnpolicy.New(cfg())
		a := sending(c)
		Expect(a.Deadline).To(Equal(nearCap))

		a = c.Step(ev(turnpolicy.EvTick), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelWrap))
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindTime))

		// The cancelled turn ends two seconds later: the remaining time is read at that point.
		a = c.Step(done(cancelled), nearCap.Add(2*time.Second))
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		Expect(a.Suppress).To(BeFalse())
		Expect(a.Cause.Remaining).To(Equal(notice - 2*time.Second))
		Expect(a.Ended).To(Equal(turnpolicy.EndedWrapUp))

		a = c.Step(noticeResult(true), nearCap.Add(2*time.Second))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Lead).To(Equal(turnpolicy.LeadWrapUp))
		Expect(a.Cause.Remaining).To(Equal(notice - 2*time.Second))
		// One notice per run: the next turn arms none.
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))
	})

	It("ends as fenced when another owner holds the run (the row was not changed)", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		c.Step(done(cancelled), nearCap)
		a := c.Step(noticeResult(false), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutFenced))
	})

	It("does not answer a cancelled turn with a prompt when the worker was stopped meanwhile", func() {
		for stop, want := range map[turnpolicy.Stop]turnpolicy.OutcomeKind{
			turnpolicy.StopFenced: turnpolicy.OutFenced, turnpolicy.StopShutdown: turnpolicy.OutShutdown,
		} {
			c := turnpolicy.New(cfg())
			sending(c)
			c.Step(ev(turnpolicy.EvTick), nearCap)
			a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultCancelled, Stop: stop}), nearCap)
			Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
			Expect(a.Outcome.Kind).To(Equal(want))
		}
	})

	It("treats a turn that fails while it is cancelled as the interruption, not as a failure", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultErrored}), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
	})

	It("honours a turn that ended on its own while the timer fired (end_turn after an ask_user)", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		a := c.Step(done(endTurn), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutEndTurn))
	})

	It("arms no notice when the turn starts less than the notice before the cap", func() {
		c := turnpolicy.New(cfg())
		a := c.Begin(t0.Add(wallClock - notice/2))
		// Inside the window: the notice is recorded first, not armed.
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		a = c.Step(noticeResult(true), t0.Add(wallClock-notice/2))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))
	})
})

var _ = Describe("a turn that starts inside a window", func() {
	inWindow := t0.Add(wallClock - 2*time.Minute)

	It("records the notice first and prepends it only when it was the owner that recorded it", func() {
		c := turnpolicy.New(cfg())
		a := c.Begin(inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindTime))
		Expect(a.Cause.Remaining).To(Equal(2 * time.Minute))
		a = c.Step(noticeResult(true), inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).NotTo(BeNil())
		Expect(a.Notice.Remaining).To(Equal(2 * time.Minute))
		Expect(a.Lead).To(Equal(turnpolicy.LeadOriginal))
	})

	It("sends no notice when another owner recorded it", func() {
		c := turnpolicy.New(cfg())
		c.Begin(inWindow)
		a := c.Step(noticeResult(false), inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).To(BeNil())
	})

	It("sends none when the run already carries one", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.NoticeRecorded = true }))
		a := c.Begin(inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).To(BeNil())
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))
	})

	It("gives a run past its cap no notice: the turn is cancelled at once", func() {
		c := turnpolicy.New(cfg())
		late := t0.Add(wallClock + time.Minute)
		a := c.Begin(late)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).To(BeNil())
		a = c.Step(ev(turnpolicy.EvTick), late)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelCap))
	})

	It("sends none when the time limit is not longer than the notice", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.WallClock = notice; c.CapEnd = t0.Add(notice) }))
		a := c.Begin(t0.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).To(BeNil())
	})

	It("keeps a retry's note and puts the notice in front of it", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(done(transient), t0)
		c.Step(ev(turnpolicy.EvRecorded), t0)
		// The backoff ends inside the window.
		a := c.Step(ev(turnpolicy.EvTick), inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		a = c.Step(noticeResult(true), inWindow)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Lead).To(Equal(turnpolicy.LeadRetry))
		Expect(a.Notice).NotTo(BeNil())
	})
})

var _ = Describe("the wrap-up notice for requests and spend", func() {
	limits := func(c *turnpolicy.Config) {
		c.MaxRequests = 100
		c.MaxSpend = money.MustParse("2.00")
		c.WrapUpNotice = 0
	}

	It("asks the driver to poll the ledger while the turn runs and interrupts at the fraction", func() {
		c := turnpolicy.New(cfg(limits))
		a := sending(c)
		Expect(a.PollUsage).To(BeTrue())

		a = c.Step(usage(79, "0.10"), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))
		Expect(a.PollUsage).To(BeTrue())

		a = c.Step(usage(80, "0.10"), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelWrap))
		Expect(a.Cause).To(Equal(turnpolicy.Cause{Kind: turnpolicy.KindRequests, Requests: 80, MaxRequests: 100}))

		a = c.Step(done(cancelled), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		a = c.Step(noticeResult(true), t0)
		Expect(a.Lead).To(Equal(turnpolicy.LeadWrapUp))
		Expect(a.PollUsage).To(BeFalse(), "one notice per run")
	})

	It("names the nearer limit by fraction", func() {
		c := turnpolicy.New(cfg(limits))
		sending(c)
		a := c.Step(usage(85, "1.80"), t0)
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindSpend))
		Expect(a.Cause.Spent).To(Equal(money.MustParse("1.80")))
		Expect(a.Cause.MaxSpend).To(Equal(money.MustParse("2.00")))
	})

	It("reads the ledger before a turn that could start inside a window", func() {
		c := turnpolicy.New(cfg(limits))
		a := c.Begin(t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActReadUsage))
		a = c.Step(usage(90, "0.10"), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindRequests))
		a = c.Step(noticeResult(true), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).NotTo(BeNil())
		Expect(a.PollUsage).To(BeFalse())
	})

	It("goes on without a notice when the ledger cannot be read", func() {
		c := turnpolicy.New(cfg(limits))
		c.Begin(t0)
		a := c.Step(ev(turnpolicy.EvUsageUnavailable), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Notice).To(BeNil())
		Expect(a.PollUsage).To(BeTrue())
	})

	It("takes the time limit when it is nearer by fraction than the request limit", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.MaxRequests = 100 }))
		now := t0.Add(wallClock - time.Minute) // 97% through the time limit
		c.Begin(now)
		a := c.Step(usage(85, "0"), now)
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindTime))
	})

	It("takes the request limit when it is nearer than the time limit", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.MaxRequests = 100 }))
		now := t0.Add(wallClock - 4*time.Minute) // 87% through the time limit
		c.Begin(now)
		a := c.Step(usage(95, "0"), now)
		Expect(a.Cause.Kind).To(Equal(turnpolicy.KindRequests))
	})

	It("sends no usage poll when the fraction is off or there is no count limit", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.MaxRequests = 100; c.WrapUpFraction = -1 }))
		Expect(sending(c).PollUsage).To(BeFalse())
		c = turnpolicy.New(cfg())
		Expect(sending(c).PollUsage).To(BeFalse())
	})
})

var _ = Describe("a harness that ignores the wrap-up cancel", func() {
	nearCap := t0.Add(wallClock - notice)

	ignored := func(timeout turnpolicy.EventKind) (*turnpolicy.Controller, turnpolicy.Action) {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		return c, c.Step(ev(timeout), nearCap.Add(10*time.Second))
	}

	It("arms the notice row without an event, once the grace passes or the cancel fails", func() {
		for _, k := range []turnpolicy.EventKind{turnpolicy.EvCancelTimeout, turnpolicy.EvCancelFailed} {
			_, a := ignored(k)
			Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice))
			Expect(a.Suppress).To(BeTrue(), "the event and the metric mean the agent was told")
			Expect(a.Ended).To(Equal(turnpolicy.EndedNone))
		}
	})

	It("goes on waiting for the turn and the cap, sends no second prompt and arms no notice", func() {
		c, _ := ignored(turnpolicy.EvCancelTimeout)
		a := c.Step(noticeResult(true), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))

		a = c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock))
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelCap))
	})

	It("does not fail the run when the lost row is a suppression", func() {
		c, _ := ignored(turnpolicy.EvCancelTimeout)
		a := c.Step(noticeResult(false), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))
	})

	It("waits for the cap when the harness honours the cancel late", func() {
		c, _ := ignored(turnpolicy.EvCancelTimeout)
		c.Step(noticeResult(true), nearCap)
		a := c.Step(done(cancelled), nearCap.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActWait), "not a failed turn and not a second prompt")
		Expect(a.Deadline).To(Equal(t0.Add(wallClock)))

		a = c.Step(ev(turnpolicy.EvTick), nearCap.Add(2*time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))

		a = c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock))
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutCapReached))
		Expect(a.Ended).To(Equal(turnpolicy.EndedCanceled))
	})

	It("ends a late cancel's wait when the worker is fenced or stops", func() {
		for ek, want := range map[turnpolicy.EventKind]turnpolicy.OutcomeKind{
			turnpolicy.EvFenced: turnpolicy.OutFenced, turnpolicy.EvShutdown: turnpolicy.OutShutdown,
		} {
			c, _ := ignored(turnpolicy.EvCancelTimeout)
			c.Step(noticeResult(true), nearCap)
			c.Step(done(cancelled), nearCap)
			a := c.Step(ev(ek), nearCap)
			Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
			Expect(a.Outcome.Kind).To(Equal(want))
		}
	})

	It("lets a refusal that lands with the late cancel win over the wait", func() {
		c, _ := ignored(turnpolicy.EvCancelTimeout)
		c.Step(noticeResult(true), nearCap)
		a := c.Step(done(refusal), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Record).To(Equal(turnpolicy.RecordRefusal))
	})

	It("classifies the error of a turn that was not cancelled again", func() {
		c, _ := ignored(turnpolicy.EvCancelTimeout)
		c.Step(noticeResult(true), nearCap)
		Expect(c.NeedsClassification()).To(BeTrue())
	})
})

var _ = Describe("a refusal that lands with the cancel", func() {
	nearCap := t0.Add(wallClock - notice)

	It("wins over the wrap-up: the agent is told what was refused, and the notice stays armed", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		a := c.Step(done(refusal), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Ended).To(Equal(turnpolicy.EndedRefused))
		a = c.Step(ev(turnpolicy.EvRecorded), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActMarkNotice), "the next turn starts inside the window")
	})

	It("is not answered with a prompt when a cancel reached the worker", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultCancelled, Refusals: true, Stop: turnpolicy.StopFenced}), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutFenced))
		Expect(a.Ended).To(Equal(turnpolicy.EndedRefused))
	})
})

var _ = Describe("refused turns", func() {
	It("continues the turn with a note, up to MaxRefusedTurns", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		for i := 1; i <= 3; i++ {
			a := c.Step(done(refusal), t0)
			Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
			Expect(a.Count).To(Equal(i))
			a = c.Step(ev(turnpolicy.EvRecorded), t0)
			Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
			Expect(a.Lead).To(Equal(turnpolicy.LeadRefusal))
		}
		a := c.Step(done(refusal), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutRefusalsExceeded))
		Expect(a.Outcome.Refused).To(Equal(3))
	})

	It("counts only consecutive refused turns", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.MaxRefusedTurns = 1 }))
		sending(c)
		c.Step(done(refusal), t0)
		c.Step(ev(turnpolicy.EvRecorded), t0)
		// A failed turn in between is progress.
		c.Step(done(transient), t0)
		c.Step(ev(turnpolicy.EvRecorded), t0)
		c.Step(ev(turnpolicy.EvTick), t0.Add(time.Second))
		a := c.Step(done(refusal), t0.Add(time.Second))
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord), "the count started over")
	})

	It("does not reset the count when a wrap-up interrupts", func() {
		c := turnpolicy.New(cfg(func(c *turnpolicy.Config) { c.MaxRefusedTurns = 1 }))
		sending(c)
		c.Step(done(refusal), t0)
		c.Step(ev(turnpolicy.EvRecorded), t0)
		nearCap := t0.Add(wallClock - notice)
		c.Step(ev(turnpolicy.EvTick), nearCap)
		c.Step(done(cancelled), nearCap)
		c.Step(noticeResult(true), nearCap)
		a := c.Step(done(refusal), nearCap)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutRefusalsExceeded))
	})

	It("ends a cancelled turn that nothing refused as a failure of the harness", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(cancelled), t0)
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutHarnessCancelled))
		Expect(a.Ended).To(Equal(turnpolicy.EndedFailed))
	})
})

var _ = Describe("model retries", func() {
	It("repeats a turn that failed with a transient error, after the backoff, in the same session", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(transient), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Record).To(Equal(turnpolicy.RecordRetry))
		Expect(a.Attempt).To(Equal(1))
		Expect(a.Delay).To(Equal(time.Second))
		Expect(a.Class).To(Equal(turnpolicy.ClassTransient))
		Expect(a.Ended).To(Equal(turnpolicy.EndedFailed))

		a = c.Step(ev(turnpolicy.EvRecorded), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActSleep))
		Expect(a.Deadline).To(Equal(t0.Add(time.Second)))

		a = c.Step(ev(turnpolicy.EvTick), t0.Add(time.Second))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Lead).To(Equal(turnpolicy.LeadRetry))
		Expect(a.Class).To(Equal(turnpolicy.ClassTransient))
	})

	It("doubles the backoff and gives up after ModelRetries", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		now := t0
		for want, retry := range []time.Duration{time.Second, 2 * time.Second} {
			a := c.Step(done(transient), now)
			Expect(a.Attempt).To(Equal(want + 1))
			Expect(a.Delay).To(Equal(retry))
			c.Step(ev(turnpolicy.EvRecorded), now)
			now = now.Add(retry)
			Expect(c.Step(ev(turnpolicy.EvTick), now).Kind).To(Equal(turnpolicy.ActSendPrompt))
		}
		a := c.Step(done(transient), now)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutRetriesExhausted))
		Expect(a.Outcome.Retries).To(Equal(2))
	})

	It("waits out a throttled provider's Retry-After, or gives up when it asks for more than MaxRetryAfter", func() {
		throttled := func(after time.Duration) turnpolicy.Result {
			return turnpolicy.Result{Kind: turnpolicy.ResultFailed, Failure: turnpolicy.Failure{Class: turnpolicy.ClassThrottled, RetryAfter: after}}
		}
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(throttled(20*time.Second)), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Delay).To(Equal(20 * time.Second))

		c = turnpolicy.New(cfg())
		sending(c)
		a = c.Step(done(throttled(2*time.Minute)), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutRetriesExhausted))
		Expect(a.Outcome.Retries).To(Equal(0))
	})

	It("does not retry any other failure", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(fatal), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutFailed))
		Expect(a.Ended).To(Equal(turnpolicy.EndedFailed))
	})

	It("ends the wait when the worker is fenced or stops", func() {
		for ek, want := range map[turnpolicy.EventKind]turnpolicy.OutcomeKind{
			turnpolicy.EvFenced: turnpolicy.OutFenced, turnpolicy.EvShutdown: turnpolicy.OutShutdown,
		} {
			c := turnpolicy.New(cfg())
			sending(c)
			c.Step(done(transient), t0)
			c.Step(ev(turnpolicy.EvRecorded), t0)
			a := c.Step(ev(ek), t0)
			Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
			Expect(a.Outcome.Kind).To(Equal(want))
		}
	})

	It("does not wake early", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(done(transient), t0)
		c.Step(ev(turnpolicy.EvRecorded), t0)
		a := c.Step(ev(turnpolicy.EvTick), t0.Add(time.Millisecond))
		Expect(a.Kind).To(Equal(turnpolicy.ActSleep))
	})
})

var _ = Describe("fences, cancels and shutdown", func() {
	It("cancels the turn when the control plane canceled the run, then ends as fenced", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(turnpolicy.Event{Kind: turnpolicy.EvFenced, FenceCanceled: true}, t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelFence))
		for _, e := range []turnpolicy.Event{done(cancelled), ev(turnpolicy.EvCancelTimeout), ev(turnpolicy.EvCancelFailed)} {
			c := turnpolicy.New(cfg())
			sending(c)
			c.Step(turnpolicy.Event{Kind: turnpolicy.EvFenced, FenceCanceled: true}, t0)
			end := c.Step(e, t0)
			Expect(end.Outcome.Kind).To(Equal(turnpolicy.OutFenced))
			Expect(end.Ended).To(Equal(turnpolicy.EndedFenced))
		}
	})

	It("ends as fenced at once when the run was taken over", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(ev(turnpolicy.EvFenced), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutFenced))
	})

	It("ends on shutdown", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(ev(turnpolicy.EvShutdown), t0)
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutShutdown))
		Expect(a.Ended).To(Equal(turnpolicy.EndedShutdown))
	})

	It("ends on shutdown while the wrap-up cancel is awaited", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock-notice))
		a := c.Step(ev(turnpolicy.EvShutdown), t0.Add(wallClock-notice))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutShutdown))
	})

	It("passes an error it does not interpret through", func() {
		c := turnpolicy.New(cfg())
		sending(c)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultPassthrough}), t0)
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutPassthrough))
		Expect(a.Ended).To(Equal(turnpolicy.EndedPassthrough))
	})

	It("asks for the classification of an error only while no cancel is awaited", func() {
		c := turnpolicy.New(cfg())
		Expect(c.NeedsClassification()).To(BeFalse())
		sending(c)
		Expect(c.NeedsClassification()).To(BeTrue())
		c.Step(ev(turnpolicy.EvTick), t0.Add(wallClock-notice))
		Expect(c.NeedsClassification()).To(BeFalse())
	})
})

var _ = DescribeTable("the retry delay",
	func(f turnpolicy.Failure, retry int, want time.Duration) {
		Expect(turnpolicy.Delay(f, retry, 2*time.Second)).To(Equal(want))
	},
	Entry("the first backoff is the base", turnpolicy.Failure{Class: turnpolicy.ClassTransient}, 0, 2*time.Second),
	Entry("it doubles", turnpolicy.Failure{Class: turnpolicy.ClassTransient}, 3, 16*time.Second),
	Entry("it is capped at a minute", turnpolicy.Failure{Class: turnpolicy.ClassTransient}, 9, time.Minute),
	Entry("a throttled request waits the Retry-After", turnpolicy.Failure{Class: turnpolicy.ClassThrottled, RetryAfter: 7 * time.Second}, 3, 7*time.Second),
	Entry("a throttled request without one backs off", turnpolicy.Failure{Class: turnpolicy.ClassThrottled}, 1, 4*time.Second),
)
