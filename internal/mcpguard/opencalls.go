// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"errors"
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
// hands it every message. Behind agentgateway, server-initiated requests are left to the client;
// koinon-gateway, the default MCP driver, refuses sampling and elicitation itself and answers
// roots/list (internal/mcpgateway, #269). A reply of a method the guard has no
// reading for is not a gap of this kind: CheckUnseen takes both legs for it.

// The turn mark (#1057). A model that calls two tools in one turn has the reply of neither when it
// makes the second call, so the reply of the first cannot be in the second call's arguments, and
// the guard must not count it as unseen. The mark of an open call is the live run and its feed_read
// (agentd's count of tool calls the harness finished, failed ones too) and its worker epoch when the
// call opened. A decision that reads the same count, in the same run and epoch, is taken as the same
// turn: no reply of the open call reached the model.
//
// Residual: this holds only while the count follows the harness. agentd reads each finished tool call
// off the harness's stream and a writer copies the count to the database, and the harness is only
// assumed to write the update before it makes its next call (ADR 0091 amendment 2026-11-03). Lag in
// the pipe, the reader or the writer, or a harness that gives a reply to the model without a
// finished update (a cut call left in progress), keeps an unseen reply from counting until it ends.
// The loss is both legs of that reply, not the legs of one tool. The guard cannot close this from the
// database alone.
//
// A result closes the newest open call of its tool and target: its mark is the highest, so the older
// calls, which count at least as much, stay. Once the count is higher, a reply may have reached the model
// (an error reply and a cut call finish a tool call as well), and the open call counts as unseen
// again: both legs. This errs the safe way: a tool call that finishes in the same turn also raises
// the count. A call with no mark (no live run or feed) always counts, and an open call that is
// older than UnseenAfter sets both legs for good, as before. It is a recorded count, not a time
// window.

// turnMark is the live run of an agent and the count of finished tool calls of its feed. It is
// the zero value when the agent has no live run or no feed.
type turnMark struct {
	Run   pgtype.UUID
	Epoch pgtype.Int8
	Read  pgtype.Int8
}

// DefaultUnseenAfter is how long a passed call may wait for its guarded result before its reply
// counts as unseen for good. Tool calls can take minutes; a call still open after this did not end
// in a result the guard saw.
const DefaultUnseenAfter = 10 * time.Minute

// currentMark reads the turn mark now, without waiting for the feed. The mark may be above the count
// of the call's own arrival, which is safe: the call's own finish is counted after it opens, so the
// count always ends above the mark once its reply can reach the model.
func (g *Guard) currentMark(ctx context.Context, c caller) (turnMark, error) {
	run, err := g.q.GetLiveRunForAgent(ctx, store.GetLiveRunForAgentParams{OrgID: c.org.ID, AgentID: c.agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return turnMark{}, nil
	}
	if err != nil {
		return turnMark{}, err
	}
	feed, err := g.q.GetLeakGuardFeed(ctx, store.GetLeakGuardFeedParams{OrgID: c.org.ID, RunID: run.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return turnMark{}, nil
	}
	if err != nil {
		return turnMark{}, err
	}
	return turnMark{Run: run.ID, Epoch: pgtype.Int8{Int64: feed.FeedEpoch, Valid: true}, Read: pgtype.Int8{Int64: feed.FeedRead, Valid: true}}, nil
}

// openCall records that the guard passed tool on target for c's session. A caller without a sandbox
// session opens nothing: the result of such a call is withheld anyway (record), and its external
// calls are refused.
func (g *Guard) openCall(ctx context.Context, c caller, target, tool string) error {
	gen, ok, err := Generation(ctx, g.q, c.org.ID, c.agent.ID)
	if err != nil || !ok {
		return err
	}
	sess := Session{Org: c.org.ID, Agent: c.agent.ID, Generation: gen}
	mark, err := g.currentMark(ctx, c)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := ensure(ctx, q, sess); err != nil {
			return err
		}
		return q.OpenLeakGuardCall(ctx, store.OpenLeakGuardCallParams{
			OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, Target: target, Tool: tool,
			OpenedAt: pgtype.Timestamptz{Time: g.now(), Valid: true},
			MarkRun:  mark.Run, MarkEpoch: mark.Epoch, MarkRead: mark.Read,
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
// nothing is written (mcpask's card). open is the number of calls still open whose reply may have
// reached the model before the decision under mark (see the turn mark above).
func (g *Guard) unseenReplies(ctx context.Context, sess Session, mark turnMark, expire bool) (legs *Legs, open int64, err error) {
	key := store.CountLeakGuardOpenCallsParams{
		OrgID: sess.Org, AgentID: sess.Agent, Generation: sess.Generation, RunID: mark.Run, FeedEpoch: mark.Epoch, FeedRead: mark.Read,
	}
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
