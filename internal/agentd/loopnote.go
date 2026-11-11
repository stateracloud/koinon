// SPDX-License-Identifier: Apache-2.0

package agentd

import "github.com/stateracloud/koinon/internal/agentd/loopguard"

// A stuck agent is told once, then the run ends (ADR 0030 amendment 2026-11-11, issue 119). The turn
// driver (turn.go) acts on loopguard's finding; internal/agentd/turnpolicy decides the order.

// kindLoopNudge is the run event of the nudge: {pattern}.
const kindLoopNudge = "run.loop_nudge"

// The actions of the loop metric.
const (
	loopNudge = "nudge"
	loopStop  = "stop"
)

// loopNote is the prompt that tells the agent it is looping. It names what was seen and asks for a
// different approach, or for an end to the turn with what is blocking it.
func loopNote(p loopguard.Pattern) string {
	return "[Koinon] You seem to be stuck in a loop: you " + p.Describe() + ". Do not repeat it. " +
		"Change your approach: try something different, or, if you cannot make progress, end your turn " +
		"with a reply that says what is blocking you. If you repeat this, Koinon stops the run."
}

// loopStopReason is why the run failed with class loop.
func loopStopReason(p loopguard.Pattern) string {
	return "the agent kept looping after it was asked to change its approach: it " + p.Describe()
}
