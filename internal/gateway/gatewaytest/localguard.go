// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// Guard is the MCP leak guard a spec starts for a gateway, in the two forms a gateway can call it. The
// zero Guard is no guard: every call to an org's server is refused.
//
// A gateway that calls the guard over the network (agentgateway's ExtMcp hook) uses URL. A gateway that
// runs the guard in its own process (koinon-gateway, ADR 0097 decision 5) uses Local. Both forms are one
// guard: a refusal set on it, the checks it records and a stop hold for both.
type Guard struct {
	// URL is the ExtMcp processor as a gateway reaches it at Env.Host ("" for none).
	URL string
	// Local is the guard as an mcpgateway.Gateway calls it (nil for none).
	Local mcpgateway.Guard
}

// Ref is the guard in both forms, as a gateway reaches it on host.
func (g *FakeGuard) Ref(host string) Guard {
	return Guard{URL: g.URL(host), Local: fakeLocal{g}}
}

// errGuardStopped is what the in-process form reports once the guard is stopped.
var errGuardStopped = errors.New("gatewaytest: the guard is stopped")

// fakeLocal is a FakeGuard as an in-process gateway calls it. It records what the ExtMcp form records:
// a call check as a request, and each message a target sent as a response. The method of a response is
// the method of the message (or of the request it answers), so a spec can tell a tools/call result from
// a tools/list result or a notification. Body is the raw message, which holds the result.
type fakeLocal struct{ g *FakeGuard }

var (
	_ mcpgateway.Guard       = fakeLocal{}
	_ mcpgateway.CallChecker = fakeLocal{}
	_ mcpgateway.LoadChecker = fakeLocal{}
)

// Loaded is an error once the guard is stopped, so the gateway refuses every call: the in-process form
// of a guard that does not answer.
func (l fakeLocal) Loaded() error {
	if l.g.stopped.Load() {
		return errGuardStopped
	}
	return nil
}

// CheckCall implements mcpgateway.CallChecker.
func (l fakeLocal) CheckCall(_ context.Context, c mcpgateway.Call) mcpgateway.Verdict {
	body, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{c.Tool, c.Arguments})
	if err != nil {
		return mcpgateway.Verdict{Refuse: true, Reason: "gatewaytest: bad arguments"}
	}
	l.g.mu.Lock()
	defer l.g.mu.Unlock()
	l.g.requests = append(l.g.requests, GuardCall{
		Targets: []string{c.Target}, Method: gateway.GuardMethod, Agent: c.Agent.String(), Tool: c.Tool, Body: string(body),
	})
	if reason, refused := l.g.refuse[c.Tool]; refused {
		return mcpgateway.Verdict{Refuse: true, Reason: reason}
	}
	return mcpgateway.Verdict{}
}

// Inspect implements mcpgateway.Guard. The gateway's own requests pass: CheckCall saw the call first.
func (l fakeLocal) Inspect(_ context.Context, m mcpgateway.Message) mcpgateway.Verdict {
	if m.Dir == mcpgateway.ToUpstream {
		return mcpgateway.Verdict{}
	}
	l.g.mu.Lock()
	defer l.g.mu.Unlock()
	l.g.responses = append(l.g.responses, GuardCall{
		Targets: []string{m.Target}, Method: m.Method, Agent: m.Agent.String(), Tool: m.Tool, Body: string(m.Raw),
	})
	if reason, withheld := l.g.withhold[m.Tool]; withheld && m.Tool != "" {
		return mcpgateway.Verdict{Refuse: true, Reason: reason}
	}
	return mcpgateway.Verdict{}
}
