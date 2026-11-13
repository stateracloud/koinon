// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
)

// recordTimeout bounds the write of one decision record.
const recordTimeout = 3 * time.Second

// recordDecision writes the decision of one call to leak_guard_decisions (lab run evidence, #926):
// every decision, passes included, with the sink class, the verdict, its reasons, the length of the
// text match and the size of the payload in bytes. Never an argument, a payload or a matched text.
//
// The record is evidence, not a control: a write that fails does not change the verdict (the guard
// already failed closed on what it could not decide). The failure is logged and counted
// (koinon_api_mcp_guard_record_errors_total) so a gap in the evidence is visible. A call refused
// before the guard knew its org (an unknown agent or target) and a call to one of Koinon's own
// tools or its executor have no record: there is no org-scoped server to name.
func (g *Guard) recordDecision(ctx context.Context, call Call, out Outcome, f decisionFacts) {
	if f.who == nil {
		return
	}
	// The check's deadline may be spent; the record still matters.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	class := string(f.class)
	if class == "" {
		class = "none"
	}
	v := out.Verdict
	p := store.InsertLeakGuardDecisionParams{
		OrgID: f.who.org.ID, AgentID: f.who.agent.ID, Generation: f.generation,
		Server: f.who.server.Name, Tool: call.Tool, SinkClass: class, Action: string(v.Action),
		Reason: v.Reason, Reasons: append([]string{}, v.Reasons...), PayloadBytes: int32(min(f.payloadBytes, 1<<30)), //nolint:gosec // bounded
	}
	if m := v.Match; m != nil {
		p.MatchCount, p.MatchLength, p.MatchEncoding = 1, int32(min(m.Length, 1<<30)), m.Encoding //nolint:gosec // bounded
	}
	if f.legs != nil {
		p.UntrustedInput.Bool, p.UntrustedInput.Valid = f.legs.UntrustedInput, true
		p.PrivateData.Bool, p.PrivateData.Valid = f.legs.PrivateData, true
	}
	if err := g.q.InsertLeakGuardDecision(ctx, p); err != nil {
		g.metrics.recordFailed()
		g.log.Error("mcp guard: could not record the decision", "target", call.Target, "tool", call.Tool, "err", err)
	}
}
