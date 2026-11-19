// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// GatewayGuard adapts Guard to mcpgateway.Guard for Koinon's own MCP gateway (ADR 0097 decision 5, #278).
// It is the same decision table and the same Postgres state agentgateway's ExtMcp hook uses: the
// gateway calls it in process, so there is no network hop per call.
//
//   - CheckCall is the request-side check of a tools/call (Guard.Check: sink class, URL rule, text
//     match, byte count, the person's approval). A call that needs a person's decision is refused when
//     the Guard has no Approve function; the gateway has none in this slice, so it fails closed.
//   - Inspect sees every raw message. A tools/call result or error records its legs and fingerprints
//     (Guard.CheckResult); a message of a kind the guard has no reading for, and a reply it cannot match
//     to a call, takes both legs on the session (Guard.CheckUnseen, #254). The results of initialize,
//     tools/list and ping, and the notifications the gateway does not forward, set no leg: the
//     agentgateway path never showed them to the guard either.
//   - Violation taints the session of a server that ignored what the gateway advertised.
//   - Loaded is an error until Load has reached the database: the gateway closes every route
//     meanwhile (ADR 0120 section 2).
type GatewayGuard struct {
	g      *Guard
	pool   *pgxpool.Pool
	log    *slog.Logger
	loaded atomic.Bool
}

var (
	_ mcpgateway.Guard             = (*GatewayGuard)(nil)
	_ mcpgateway.CallChecker       = (*GatewayGuard)(nil)
	_ mcpgateway.LoadChecker       = (*GatewayGuard)(nil)
	_ mcpgateway.ViolationReporter = (*GatewayGuard)(nil)
)

// NewGatewayGuard returns the adapter. The route stays closed until Load succeeds.
func NewGatewayGuard(g *Guard, log *slog.Logger) *GatewayGuard {
	if log == nil {
		log = slog.Default()
	}
	return &GatewayGuard{g: g, pool: g.pool, log: log}
}

// ErrNotLoaded is what Loaded returns before Load has reached the database.
var ErrNotLoaded = errors.New("mcpguard: the guard has not reached its database")

// Load checks that the guard's database answers, and opens the route. It does not close it again
// on a later error: a decision that cannot be recorded is refused on its own (fail closed).
func (a *GatewayGuard) Load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := a.pool.Ping(ctx); err != nil {
		return err
	}
	a.loaded.Store(true)
	return nil
}

// Loaded implements mcpgateway.LoadChecker.
func (a *GatewayGuard) Loaded() error {
	if !a.loaded.Load() {
		return ErrNotLoaded
	}
	return nil
}

// CheckCall implements mcpgateway.CallChecker.
func (a *GatewayGuard) CheckCall(ctx context.Context, c mcpgateway.Call) mcpgateway.Verdict {
	out := a.g.Check(ctx, Call{Target: c.Target, Agent: c.Agent.String(), Tool: c.Tool, Arguments: c.Arguments})
	if out.Verdict.Action == Pass {
		return mcpgateway.Verdict{}
	}
	return mcpgateway.Verdict{Refuse: true, Reason: out.Text}
}

// Inspect implements mcpgateway.Guard.
func (a *GatewayGuard) Inspect(ctx context.Context, m mcpgateway.Message) mcpgateway.Verdict {
	if m.Dir == mcpgateway.ToUpstream {
		// The gateway's own requests: the call check ran before the call, and the method list is the
		// gateway's. Nothing comes into the agent's session by this direction.
		return mcpgateway.Verdict{}
	}
	agent := m.Agent.String()
	switch m.Kind {
	case mcpgateway.KindResult, mcpgateway.KindError:
		switch m.Method {
		case "tools/call":
			if m.Tool == "" {
				return a.unseen(ctx, agent, m)
			}
			var result json.RawMessage
			if m.Kind == mcpgateway.KindResult {
				result = resultOf(m.Raw)
			}
			err := a.g.CheckResult(ctx, Result{Target: m.Target, Agent: agent, Tool: m.Tool, Result: result})
			return a.verdict(err, "result", m)
		case "initialize", "tools/list", "ping":
			return mcpgateway.Verdict{}
		}
		// No request of ours is behind it, or a method the guard does not read.
		return a.unseen(ctx, agent, m)
	case mcpgateway.KindNotification:
		// No leg is set: the gateway relays none of a server's notifications to the agent today. A slice
		// that relays one (progress, log) puts server text into the session and must revisit this line.
		return mcpgateway.Verdict{}
	case mcpgateway.KindRequest:
		switch m.Method {
		case "ping", "roots/list":
			return mcpgateway.Verdict{}
		}
		return a.unseen(ctx, agent, m)
	}
	return a.unseen(ctx, agent, m)
}

// Violation implements mcpgateway.ViolationReporter: a server that ignored what it was told takes both
// legs on the session, so the run is no longer trusted (ADR 0097 decision 5).
func (a *GatewayGuard) Violation(ctx context.Context, m mcpgateway.Message, v mcpgateway.Violation) {
	a.log.Warn("mcp server ignored what the gateway advertised", "target", m.Target, "violation", string(v))
	if err := a.g.CheckUnseen(ctx, m.Agent.String(), m.Target, m.Raw); err != nil {
		a.log.Error("mcp guard: recording a violation failed", "target", m.Target, "err", err)
	}
}

func (a *GatewayGuard) unseen(ctx context.Context, agent string, m mcpgateway.Message) mcpgateway.Verdict {
	return a.verdict(a.g.CheckUnseen(ctx, agent, m.Target, m.Raw), "unseen", m)
}

// verdict turns the guard's error into a refusal. The reason is the guard's fixed text, never the error.
func (a *GatewayGuard) verdict(err error, what string, m mcpgateway.Message) mcpgateway.Verdict {
	if err == nil {
		return mcpgateway.Verdict{}
	}
	text, ok := RefusalText(err)
	if !ok {
		a.log.Error("mcp guard: recording a message failed", "what", what, "target", m.Target, "err", err)
		text = "withheld by the leak guard: it could not record this reply (an internal error); try again later"
	}
	return mcpgateway.Verdict{Refuse: true, Reason: text}
}

// resultOf is the "result" member of a JSON-RPC response, or nil.
func resultOf(raw []byte) json.RawMessage {
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return r.Result
}
