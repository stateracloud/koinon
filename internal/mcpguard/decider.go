// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"

	"github.com/stateracloud/koinon/internal/db/store"
)

// The guard decides in three steps: the rules (Decide), then a Decider for what the rules leave
// open (NeedsDecision), then, if the Decider escalates, a person. The Decider is the seam for
// another step between the rules and a person (for example a judge model); today the only one is
// PersonDecider, so every NeedsDecision is an ADR 0042 approval of the exact call, as ADR 0091
// decision 5 says.

// Pending is a call the rules left open: everything a Decider needs to decide it, and to show it.
type Pending struct {
	Org    store.Org
	Agent  store.Agent
	Server store.McpServer
	// Tool is the tool's published row.
	Tool      store.McpTool
	Arguments json.RawMessage
	// Verdict is the rules' outcome (Action NeedsDecision), with its reasons and any match.
	Verdict Verdict
	// Destination is the call's address arguments; PayloadBytes the size of everything else.
	Destination  map[string]string
	PayloadBytes int
	// Risk is what an approval card shows (reasons, sink class, destination, legs, match).
	Risk map[string]any
}

// Decider decides a call the rules left open. Its Outcome is final: Pass lets the call on,
// Refuse and Approve (parked for a person) fail it with Outcome.Text for the agent to read. An
// error fails the call closed.
type Decider interface {
	DecidePending(ctx context.Context, p Pending) (Outcome, error)
}

// ApproveFunc records an ADR 0042 approval request for exactly this call of an auto-tier tool
// (mcpask.Server.RequestApproval) with the guard's risk on the card, and returns the text the agent
// reads ("approval requested: id=...").
type ApproveFunc func(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool store.McpTool, args json.RawMessage, risk map[string]any) (string, error)

// PersonDecider escalates every call to a person: an ADR 0042 approval of the exact call, with the
// destination and the payload on the card.
type PersonDecider struct {
	Approve ApproveFunc
}

// DecidePending implements Decider.
func (d PersonDecider) DecidePending(ctx context.Context, p Pending) (Outcome, error) {
	v := p.Verdict
	v.Action = Approve
	if d.Approve == nil {
		v.Action = Refuse
		text := "refused by the leak guard: this call needs a person's approval, and approvals are not available here"
		v.Detail = text
		return Outcome{Verdict: v, Text: text}, nil
	}
	text, err := d.Approve(ctx, p.Org, p.Agent, p.Server, p.Tool, p.Arguments, p.Risk)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Verdict: v, Text: text}, nil
}
