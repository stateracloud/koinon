// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"fmt"

	"github.com/stateracloud/koinon/internal/leakguard"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Action is what the guard does with one tool call.
// NoRunApprovalText is the refusal of a call that needs a person's approval while its agent works
// on no task: an approval belongs to a run, so none can be made (the e2e asserts these words).
const NoRunApprovalText = "refused by the leak guard: this call needs a person's approval, which is only possible while the agent works on a task"

type Action string

const (
	// Pass lets the call through.
	Pass Action = "pass"
	// Refuse fails the call with a reason the agent can read (the byte cap, the URL rule, a check
	// that could not finish).
	Refuse Action = "refuse"
	// NeedsDecision is the rules' third outcome: the call may carry private data out of the org, and
	// the rules alone do not let it go. A Decider takes it from here (today: a person, ADR 0042).
	NeedsDecision Action = "needs_decision"
	// Approve is the Decider's escalation: the call is parked for an ADR 0042 approval of that
	// exact call.
	Approve Action = "approve"
)

// Reasons, the fixed label set of the decision metric (ADR 0023: no IDs).
const (
	ReasonInternal  = "internal"          // none or internal sink: carries nothing out
	ReasonClean     = "clean"             // external, nothing matched, within the limits
	ReasonBothLegs  = "both_legs"         // external-high with both legs: approval
	ReasonTaint     = "taint"             // the payload carries private text: approval
	ReasonByteCap   = "byte_cap"          // external-low over the byte cap with both legs: refusal
	ReasonURLRule   = "url_rule"          // the ADR 0084 URL shape rule or its budgets: refusal
	ReasonCheck     = "check"             // the taint check ran out of a budget: refusal (fail closed)
	ReasonAmbiguous = "ambiguous_address" // an address argument with more than one reading: refusal
	ReasonError     = "error"             // the guard failed: refusal (fail closed)
	ReasonIdentity  = "identity"          // the call does not come from a known agent: refusal
	ReasonNoRun     = "no_run"            // an external call outside a running run: refusal
	ReasonExempt    = "exempt"            // a call of Koinon's own executor or probe
	ReasonAskTarget = "ask_target"        // approval-gated tool: mcpask records the one card
	// ReasonFeedStalled marks a call decided while agentd's feed lagged: its legs were taken as both,
	// so a person decides it, never a Decider (ADR 0091 decision 4, ADR 0092).
	ReasonFeedStalled = "feed_stalled"
	// ReasonReplyUnseen marks a call decided while a call the guard passed had no result it saw (an
	// error reply, or a cut call; #254): its legs were taken as both, so a person decides it.
	ReasonReplyUnseen = "reply_unseen"
)

// Facts are what the decision table reads about one call (ADR 0091 decision 5).
type Facts struct {
	Class sinkclass.Class
	// BothLegs: the session holds untrusted input and private data.
	BothLegs bool
	// Taint is the text match on the payload (only run for an external class). Its zero value is
	// a check that did not run, which only an internal class may have.
	Taint leakguard.Decision
	// URL is the URL rule and its budgets (only for external-low; zero value Allow false is
	// treated as not applicable when URLChecked is false).
	URL        leakguard.Decision
	URLChecked bool
	// OverCap: external-low with both legs, and the call would take the run past the byte cap.
	OverCap bool
	// CapUsed and Cap are for the refusal text.
	CapUsed, Cap, PayloadBytes int
}

// Verdict is the decision on one call.
type Verdict struct {
	Action Action
	// Reason is one of the Reason constants; Reasons lists every reason that applied (for the card:
	// both_legs and taint can apply together, and make one approval).
	Reason  string
	Reasons []string
	// Detail is the text the agent reads on a refusal.
	Detail string
	Match  *leakguard.Match
}

// Decide is the decision table of ADR 0091 decision 5, with the text match of decision 6 on top:
// the rules. Their outcome is Pass, Refuse or NeedsDecision; a NeedsDecision goes to the guard's
// Decider. It is pure: the caller gathers the facts and charges the budgets.
func Decide(f Facts) Verdict {
	switch f.Class {
	case sinkclass.None, sinkclass.Internal:
		// They carry nothing out; the ADR 0057 tiers govern writes.
		return Verdict{Action: Pass, Reason: ReasonInternal}
	case sinkclass.ExternalLow, sinkclass.ExternalHigh:
	default:
		// Not a class the guard knows: the cautious reading is external-high (sinkclass.Default).
		f.Class = sinkclass.ExternalHigh
	}
	// failClosed: a text match that could not finish refuses the call.
	if !f.Taint.Allow && f.Taint.Reason != leakguard.ReasonTaint {
		detail := f.Taint.Detail
		if detail == "" {
			detail = "the leak guard could not check this call"
		}
		return Verdict{Action: Refuse, Reason: ReasonCheck, Detail: detail}
	}
	matched := f.Taint.Reason == leakguard.ReasonTaint
	if f.Class == sinkclass.ExternalLow {
		if f.URLChecked && !f.URL.Allow {
			return Verdict{Action: Refuse, Reason: ReasonURLRule, Detail: f.URL.Detail}
		}
		if f.BothLegs && f.OverCap {
			return Verdict{Action: Refuse, Reason: ReasonByteCap, Detail: fmt.Sprintf(
				"refused by the leak guard: this run has read untrusted input and private data, so it may send at most %d bytes "+
					"to hosts outside the org; it has sent %d and this call would add %d. Do not retry or rephrase this call.",
				f.Cap, f.CapUsed, f.PayloadBytes)}
		}
		if matched {
			return Verdict{Action: NeedsDecision, Reason: ReasonTaint, Reasons: []string{ReasonTaint}, Match: f.Taint.Match}
		}
		return Verdict{Action: Pass, Reason: ReasonClean}
	}
	// external-high
	switch {
	case f.BothLegs && matched:
		return Verdict{Action: NeedsDecision, Reason: ReasonBothLegs, Reasons: []string{ReasonBothLegs, ReasonTaint}, Match: f.Taint.Match}
	case f.BothLegs:
		return Verdict{Action: NeedsDecision, Reason: ReasonBothLegs, Reasons: []string{ReasonBothLegs}}
	case matched:
		return Verdict{Action: NeedsDecision, Reason: ReasonTaint, Reasons: []string{ReasonTaint}, Match: f.Taint.Match}
	}
	return Verdict{Action: Pass, Reason: ReasonClean}
}
