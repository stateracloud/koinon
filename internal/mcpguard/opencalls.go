// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// The open-call ledger (#254, ADR 0097 step 1: a reply the guard never saw sets both legs).
//
// agentgateway v1.7.0 asks the guard about a tools/call and its successful result only: a JSON-RPC
// error reply, and a call the gateway or the server cut short, reach the agent unseen, and an error
// can carry what a result would have (a server's error text quoting the private record). So the
// guard opens a call when it passes one whose result would set a leg, and closes it when it records
// that result (CheckResult). While a call is open the session's decisions take both legs
// (ReasonReplyUnseen: a person decides, never a Decider); once it is older than UnseenAfter, both
// legs are recorded on the session for good and the call goes.
//
// What it covers: servers that end a call honestly, in one result or one error, and calls the
// gateway or the server cut. A result is matched to its call by target and tool, the oldest open
// call first: agentgateway v1.7.0 gives the guard no JSON-RPC id, and it runs the response hook on
// every response on the call's stream, whatever its id, and keeps relaying after the terminal
// message (agentgateway#4245, handler.rs wrap_with_guardrails and into_sse_stream). So a hostile
// server that sends an error for the call and then a response of its own, with any id, closes the
// call: the guard sees the stray response, never the error. Koinon cannot close that alone.
//
// Not covered: that server; notifications on a call's stream; anything else on a stream that ends
// in a guarded result. The guard cannot tell they happened; the gateway of ADR 0097 step 2 (v0.2)
// hands it every message. Server-initiated requests are covered by the client meanwhile (#269, whose
// exit condition is "the step 2 gateway refuses these itself"). A reply of a method the guard has no
// reading for is not a gap of this kind: CheckUnseen takes both legs for it.

// DefaultUnseenAfter is how long a passed call may wait for its guarded result before its reply
// counts as unseen for good. Tool calls can take minutes; a call still open after this did not end
// in a result the guard saw.
const DefaultUnseenAfter = 10 * time.Minute

// openCall records that the guard passed tool on target for c's session. A caller without a sandbox
// session opens nothing: the result of such a call is withheld anyway (record), and its external
// calls are refused.
func (g *Guard) openCall(ctx context.Context, c caller, target, tool string) error {
	gen, ok, err := Generation(ctx, g.q, c.org.ID, c.agent.ID)
	if err != nil || !ok {
		return err
	}
	sess := Session{Org: c.org.ID, Agent: c.agent.ID, Generation: gen}
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := ensure(ctx, q, sess); err != nil {
			return err
		}
		return q.OpenLeakGuardCall(ctx, store.OpenLeakGuardCallParams{
			OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, Target: target, Tool: tool,
			OpenedAt: pgtype.Timestamptz{Time: g.now(), Valid: true},
		})
	})
}

// closeCall closes the oldest open call of tool on target in sess: the guard recorded a result for
// it. Without the JSON-RPC id this trusts the server's stream to carry one terminal message for the
// call (see the top of this file): a response a server appends after its own error closes the call.
func (g *Guard) closeCall(ctx context.Context, sess Session, target, tool string) error {
	_, err := g.q.CloseLeakGuardCall(ctx, store.CloseLeakGuardCallParams{
		OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, Target: target, Tool: tool,
	})
	return err
}

// unseenReplies is the session's calls whose replies the guard has not seen. With expire, the calls
// older than UnseenAfter are closed and both legs recorded on the session (returned in legs); without,
// nothing is written (mcpask's card). open is the number of calls still open.
func (g *Guard) unseenReplies(ctx context.Context, sess Session, expire bool) (legs *Legs, open int64, err error) {
	key := store.CountLeakGuardOpenCallsParams{OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation}
	if !expire {
		open, err = g.q.CountLeakGuardOpenCalls(ctx, key)
		return nil, open, err
	}
	err = pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.ExpireLeakGuardCalls(ctx, store.ExpireLeakGuardCallsParams{
			OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation,
			Before: pgtype.Timestamptz{Time: g.now().Add(-g.unseenAfter), Valid: true},
		})
		if err != nil {
			return err
		}
		if n > 0 {
			both := Legs{UntrustedInput: true, PrivateData: true}
			row, err := q.AddLeakGuardLegs(ctx, store.AddLeakGuardLegsParams{
				OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, UntrustedInput: true, PrivateData: true,
			})
			if err != nil {
				return err
			}
			legs = &Legs{UntrustedInput: row.UntrustedInput, PrivateData: row.PrivateData}
			g.metrics.unseen(n)
			g.metrics.legs(both)
			g.log.Warn("mcp guard: calls the guard passed never ended in a result it saw (an error reply, or a cut call); the session now holds both legs",
				"agent", sess.Agent.String(), "generation", sess.Generation, "calls", n, "after", g.unseenAfter)
		}
		open, err = q.CountLeakGuardOpenCalls(ctx, key)
		return err
	})
	return legs, open, err
}
