// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Result is one tools/call result as the gateway reports it.
type Result struct {
	Target, Agent, Tool string
	// Result is the JSON-RPC result (the CallToolResult), or nil for an error reply.
	Result json.RawMessage
}

// resultLegs are the legs a result of tool t sets: the tool's own declaration where it has one,
// else its server's (ADR 0091 decision 4).
func resultLegs(srv store.McpServer, d sinkclass.Declaration) Legs {
	l := Legs{UntrustedInput: srv.ExposesUntrustedInput, PrivateData: srv.ExposesPrivateData}
	if d.ResultUntrustedInput != nil {
		l.UntrustedInput = *d.ResultUntrustedInput
	}
	if d.ResultPrivateData != nil {
		l.PrivateData = *d.ResultPrivateData
	}
	return l
}

// CheckResult records what a result brings into the agent's session: its legs, and the
// fingerprints of a result that carries private data. An error is returned and the gateway
// withholds the result (fail closed): a result the guard did not record could be leaked unseen.
func (g *Guard) CheckResult(ctx context.Context, r Result) error {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	c, refused, err := g.identify(ctx, r.Agent, r.Target)
	if err != nil {
		return err
	}
	if refused != nil {
		return refusedError{text: refused.Text}
	}
	if c.exempt {
		return nil
	}
	if c.own {
		legs := OwnResultLegs(r.Tool)
		if legs.Any() {
			// An own tool nobody declared sets both legs on the agent's session; find the agent.
			org, agent, o, err := g.agentOf(ctx, c.ref)
			if err != nil {
				return err
			}
			if o != nil {
				return refusedError{text: o.Text}
			}
			c.org, c.agent = org, agent
		}
		return g.record(ctx, c, legs, r.Result)
	}
	_, decl, err := g.tool(ctx, c, r.Tool)
	if err != nil {
		return err
	}
	return g.record(ctx, c, resultLegs(c.server, decl), r.Result)
}

func (g *Guard) record(ctx context.Context, c caller, legs Legs, result json.RawMessage) error {
	if !legs.Any() {
		return nil
	}
	gen, ok, err := Generation(ctx, g.q, c.org.ID, c.agent.ID)
	if err != nil {
		return err
	}
	if !ok {
		// Without a sandbox there is no session to carry the legs. The agent's next external call
		// is refused for the same reason, so nothing leaks through the gap.
		return refusedError{text: "refused by the leak guard: this agent has no sandbox session yet"}
	}
	pol, err := loadPolicy(ctx, g.q, c.org)
	if err != nil {
		return err
	}
	var texts []string
	if legs.PrivateData && len(result) > 0 {
		texts = Strings(result)
	}
	_, err = g.state.Record(ctx, Session{Org: c.org.ID, Agent: c.agent.ID, Generation: gen}, legs, pol.threshold(), texts)
	return err
}

// RecordApproved records the result of a call a person approved and Koinon's executor ran for the
// agent (approvals.Options.OnMCPResult): the executor's own call is exempt at the gateway, so its
// result reaches the agent's session here.
func (g *Guard) RecordApproved(ctx context.Context, row store.ApprovalRequest, srv store.McpServer, result json.RawMessage) error {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	slug, err := g.q.GetOrgSlug(ctx, row.OrgID)
	if err != nil {
		return err
	}
	org := store.Org{ID: row.OrgID, Slug: slug}
	agent, err := g.q.GetAgent(ctx, store.GetAgentParams{OrgID: row.OrgID, ID: row.AgentID})
	if err != nil {
		return err
	}
	c := caller{org: org, agent: agent, server: srv}
	_, decl, err := g.tool(ctx, c, row.Tool)
	if err != nil {
		return err
	}
	return g.record(ctx, c, resultLegs(c.server, decl), result)
}

// Assess decides an ask-tier call without charging anything, for mcpask's single card: the
// returned risk is nil when the guard has nothing to add. Its error refuses the call, and its text
// is safe for the agent to read.
func (g *Guard) Assess(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool string, args json.RawMessage) (map[string]any, error) {
	risk, err := g.assessAsk(ctx, org, agent, srv, tool, args)
	if err != nil {
		if _, ok := RefusalText(err); !ok {
			g.log.Error("mcp guard: assessing an ask-tier call failed", "org", org.Slug, "tool", tool, "err", err)
			return nil, refusedError{text: errText}
		}
	}
	return risk, err
}

func (g *Guard) assessAsk(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool string, args json.RawMessage) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	c := caller{org: org, agent: agent, server: srv}
	_, decl, err := g.tool(ctx, c, tool)
	if err != nil {
		return nil, err
	}
	a, err := g.assess(ctx, c, decl, args, false)
	if err != nil {
		return nil, err
	}
	if a.refused != nil {
		return nil, refusedError{text: a.refused.Text}
	}
	switch a.verdict.Action {
	case NeedsDecision:
		return a.risk(), nil
	case Refuse:
		return nil, refusedError{text: a.verdict.Detail}
	}
	return nil, nil
}

// refusedError is a refusal with text the agent may read.
type refusedError struct{ text string }

func (e refusedError) Error() string { return e.text }

// RefusalText returns the agent-readable text of a guard refusal, and false for any other error.
func RefusalText(err error) (string, bool) {
	var r refusedError
	if errors.As(err, &r) {
		return r.text, true
	}
	return "", false
}
