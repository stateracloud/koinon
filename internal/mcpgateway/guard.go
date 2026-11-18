// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Direction says which way a message travels on an upstream connection.
type Direction int

const (
	// FromUpstream is a message the target sent to the gateway.
	FromUpstream Direction = iota
	// ToUpstream is a message the gateway sends to the target.
	ToUpstream
)

// Kind is the JSON-RPC kind of a message.
type Kind int

const (
	// KindRequest is a call: it has an ID and expects a response.
	KindRequest Kind = iota
	// KindNotification is a request without an ID.
	KindNotification
	// KindResult is a successful response.
	KindResult
	// KindError is an error response.
	KindError
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindNotification:
		return "notification"
	case KindResult:
		return "result"
	case KindError:
		return "error"
	}
	return "unknown"
}

// Message is one raw JSON-RPC message on an upstream connection, with who it belongs to.
type Message struct {
	Target string
	Agent  gateway.AgentRef
	Dir    Direction
	Kind   Kind
	// Method is the method of a request or notification. For a result or an error it is the method
	// of the request it answers, or "" when the gateway did not send that request (an unmatched
	// response).
	Method string
	// Raw is the message as it is on the wire. The guard reads it and keeps no reference to it.
	Raw []byte
}

// Verdict is the guard's decision on one message.
type Verdict struct {
	// Refuse withholds the message from its reader.
	Refuse bool
	// Reason is shown to the agent and is safe to log: it never carries message content.
	Reason string
}

// Guard decides on every message of every upstream connection. Inspect must fail closed: an error
// inside the guard is a refusal. It may block (a call held for a person's approval).
type Guard interface {
	Inspect(ctx context.Context, m Message) Verdict
}

// Violation names a server that ignored what the gateway advertised.
type Violation string

const (
	// ViolationSampling is a sampling request from a server that was told the gateway offers none.
	ViolationSampling Violation = "sampling"
	// ViolationElicitation is an elicitation request from such a server.
	ViolationElicitation Violation = "elicitation"
	// ViolationInputRequired is an input_required reply (MCP multi round-trip requests).
	ViolationInputRequired Violation = "input_required"
	// ViolationUnknownRequest is a server request with a method the gateway does not know.
	ViolationUnknownRequest Violation = "unknown_request"
)

// ViolationReporter is the optional part of a Guard that wants to hear of a Violation, so it can
// taint the run (ADR 0097 decision 5). The gateway refuses the request itself either way.
type ViolationReporter interface {
	Violation(ctx context.Context, m Message, v Violation)
}

// AllowGuard passes every message. It is for tests and for a gateway whose guard is not configured
// only when the config has no org target; the real gateway refuses an org target without a guard.
type AllowGuard struct{}

// Inspect implements Guard.
func (AllowGuard) Inspect(context.Context, Message) Verdict { return Verdict{} }

// AllowedMethods are the JSON-RPC methods the gateway forwards and accepts (ADR 0097 decision 5).
// Any other method is refused. The two lists are the methods an agent may send to the gateway, and
// the methods the gateway sends to a target.
var (
	// AgentMethods are the methods an agent may send to the gateway.
	AgentMethods = []string{
		"initialize", "ping", "tools/list", "tools/call",
		"notifications/initialized", "notifications/cancelled",
	}
	// UpstreamRequests are the methods the gateway sends to a target.
	UpstreamRequests = []string{
		"initialize", "ping", "tools/list", "tools/call",
		"notifications/initialized", "notifications/cancelled",
	}
	// UpstreamPushes are the methods a target may send to the gateway: notifications it may push
	// and the one request the gateway answers itself.
	UpstreamPushes = []string{
		"ping", "roots/list",
		"notifications/message", "notifications/progress", "notifications/cancelled",
		"notifications/tools/list_changed", "notifications/resources/list_changed",
		"notifications/prompts/list_changed", "notifications/resources/updated",
	}
)
