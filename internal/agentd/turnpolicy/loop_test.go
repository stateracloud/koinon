// SPDX-License-Identifier: Apache-2.0

package turnpolicy_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd/turnpolicy"
)

// A loop is nudged once, in the turn after the interrupted one; the next loop ends the run (ADR 0030
// amendment 2026-11-11, issue 119).
var _ = Describe("the loop nudge", func() {
	loop := func(p string) turnpolicy.Event { return turnpolicy.Event{Kind: turnpolicy.EvLoop, Pattern: p} }
	cancelled := turnpolicy.Result{Kind: turnpolicy.ResultCancelled}

	// startedAndLooping begins the first turn and reports a loop in it.
	startedAndLooping := func(c *turnpolicy.Controller, now time.Time) turnpolicy.Action {
		a := c.Begin(now)
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.WatchLoop).To(BeTrue(), "a running prompt is watched for loops")
		return c.Step(loop("same_action"), now.Add(time.Minute))
	}

	It("interrupts the turn, records the nudge and sends it as the next prompt", func() {
		c := turnpolicy.New(cfg())
		now := t0
		a := startedAndLooping(c, now)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelLoop))

		a = c.Step(done(cancelled), now.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Record).To(Equal(turnpolicy.RecordLoopNudge))
		Expect(a.Pattern).To(Equal("same_action"))
		Expect(a.Ended).To(Equal(turnpolicy.EndedLoop))

		a = c.Step(ev(turnpolicy.EvRecorded), now.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Lead).To(Equal(turnpolicy.LeadLoop))
		Expect(a.Pattern).To(Equal("same_action"))
		Expect(a.WatchLoop).To(BeTrue(), "the pattern can continue")
	})

	It("ends the run with OutLoop when the pattern continues after the nudge", func() {
		c := turnpolicy.New(cfg())
		now := t0
		startedAndLooping(c, now)
		c.Step(done(cancelled), now)
		c.Step(ev(turnpolicy.EvRecorded), now)

		a := c.Step(loop("same_error"), now.Add(2*time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelLoopStop))
		a = c.Step(done(cancelled), now.Add(2*time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutLoop))
		Expect(a.Outcome.Pattern).To(Equal("same_error"))
		Expect(a.Outcome.NudgeUndelivered).To(BeFalse(), "the nudge went out")
		Expect(a.Ended).To(Equal(turnpolicy.EndedLoop))
	})

	It("ends the run when the interrupted turn ends on its own: end_turn is the agent's answer", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultEndTurn}), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutEndTurn))
	})

	It("does not nudge a harness that ignores the cancel, and the next loop ends the run", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		a := c.Step(ev(turnpolicy.EvCancelTimeout), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActWait))
		a = c.Step(loop("monologue"), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActCancel))
		Expect(a.Why).To(Equal(turnpolicy.CancelLoopStop))
		a = c.Step(done(cancelled), t0)
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutLoop))
		Expect(a.Outcome.NudgeUndelivered).To(BeTrue(), "the agent was never told it loops")
	})

	It("nudges when the harness ends the interrupted turn cancelled after the grace", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		Expect(c.Step(ev(turnpolicy.EvCancelTimeout), t0).Kind).To(Equal(turnpolicy.ActWait))
		a := c.Step(done(cancelled), t0.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActRecord))
		Expect(a.Record).To(Equal(turnpolicy.RecordLoopNudge))
		Expect(a.Pattern).To(Equal("same_action"))
		a = c.Step(ev(turnpolicy.EvRecorded), t0.Add(time.Minute))
		Expect(a.Kind).To(Equal(turnpolicy.ActSendPrompt))
		Expect(a.Lead).To(Equal(turnpolicy.LeadLoop))
		// The nudge is used: the next loop ends the run, and the agent was told.
		a = c.Step(loop("same_error"), t0.Add(2*time.Minute))
		Expect(a.Why).To(Equal(turnpolicy.CancelLoopStop))
		a = c.Step(done(cancelled), t0.Add(2*time.Minute))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutLoop))
		Expect(a.Outcome.NudgeUndelivered).To(BeFalse())
	})

	It("ends as a loop when a second loop follows an ignored cancel and the turn ends cancelled", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		c.Step(ev(turnpolicy.EvCancelTimeout), t0)
		a := c.Step(loop("monologue"), t0)
		Expect(a.Why).To(Equal(turnpolicy.CancelLoopStop))
		a = c.Step(done(cancelled), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutLoop))
		Expect(a.Outcome.NudgeUndelivered).To(BeTrue())
	})

	It("tells the agent about a refusal first and keeps the nudge for the next loop", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultCancelled, Refusals: true}), t0)
		Expect(a.Record).To(Equal(turnpolicy.RecordRefusal))
		a = c.Step(ev(turnpolicy.EvRecorded), t0)
		Expect(a.Lead).To(Equal(turnpolicy.LeadRefusal))
		a = c.Step(loop("alternating"), t0)
		Expect(a.Why).To(Equal(turnpolicy.CancelLoop))
	})

	It("lets a fence or a shutdown win over the nudge", func() {
		c := turnpolicy.New(cfg())
		startedAndLooping(c, t0)
		a := c.Step(done(turnpolicy.Result{Kind: turnpolicy.ResultCancelled, Stop: turnpolicy.StopFenced}), t0)
		Expect(a.Kind).To(Equal(turnpolicy.ActEnd))
		Expect(a.Outcome.Kind).To(Equal(turnpolicy.OutFenced))
	})
})
