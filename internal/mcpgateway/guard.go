// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

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
	// Org is the org of the target ("" for a platform target), so a rule can tell an org's own server
	// from one of Koinon's.
	Org string
	// Credential is the kind of credential the gateway sends to the target for this agent
	// (CredentialNone, CredentialStatic, CredentialError, ...). It never holds a value.
	Credential string
	Agent      gateway.AgentRef
	Dir        Direction
	Kind       Kind
	// Method is the method of a request or notification. For a result or an error it is the method
	// of the request it answers, or "" when the gateway did not send that request (an unmatched
	// response).
	Method string
	// Tool is the tool name of a tools/call: on the request, and on the result or error that answers
	// it. It is "" for any other message.
	Tool string
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

// Call is one tools/call, as the request-side check sees it, after the grant check and before the
// target is contacted. Arguments are the agent's raw arguments: the checker reads them and neither
// logs nor keeps them.
type Call struct {
	Agent      gateway.AgentRef
	Target     string
	Org        string
	Tool       string
	Credential string
	Arguments  json.RawMessage
}

// CallChecker is the optional part of a Guard that decides on a whole tool call: the ask tier and the
// request-side leak check (ADR 0097 decision 5, ADR 0057 item 7, ADR 0091). It may block while a person
// decides. The gateway runs it as middleware, after the grant check; a refusal ends the call before
// any upstream message is sent.
type CallChecker interface {
	CheckCall(ctx context.Context, c Call) Verdict
}

// LoadChecker is the optional part of a Guard whose rules load after the process starts. While Loaded
// returns an error the rules are not in force, so every route closes: no message passes and no call
// is sent (ADR 0120 section 2: a rule that fails to load closes the route).
type LoadChecker interface {
	Loaded() error
}

// Reasons the gateway gives when the guard itself fails. They are fixed texts: they carry no error text.
const (
	reasonGuardNotLoaded = "refused: the guard rules are not loaded"
	reasonGuardFailed    = "refused: the guard failed to decide"
)

// failClosed wraps the configured guard. It is the only way the gateway calls the guard: a guard that
// is not loaded, or that panics, refuses; it never passes.
type failClosed struct {
	g   Guard
	log *slog.Logger
	m   *Metrics
}

// loaded reports that the guard's rules are in force. A LoadChecker that panics counts as not loaded.
func (f failClosed) loaded() (ok bool) {
	defer func() {
		if recover() != nil {
			f.m.guardFailure(guardPanic)
			ok = false
		}
	}()
	if lc, isLC := f.g.(LoadChecker); isLC && lc.Loaded() != nil {
		f.m.guardFailure(guardNotLoaded)
		return false
	}
	return true
}

func (f failClosed) Inspect(ctx context.Context, m Message) (v Verdict) {
	if !f.loaded() {
		return Verdict{Refuse: true, Reason: reasonGuardNotLoaded}
	}
	defer func() {
		if recover() != nil {
			f.log.Error("guard panicked", "target", m.Target, "kind", m.Kind.String())
			f.m.guardFailure(guardPanic)
			v = Verdict{Refuse: true, Reason: reasonGuardFailed}
		}
	}()
	return f.g.Inspect(ctx, m)
}

// checkCall runs the call-level check, if the guard has one. A guard with none passes the call: the
// message guard still sees its wire messages.
func (f failClosed) checkCall(ctx context.Context, c Call) (v Verdict) {
	if !f.loaded() {
		return Verdict{Refuse: true, Reason: reasonGuardNotLoaded}
	}
	cc, ok := f.g.(CallChecker)
	if !ok {
		return Verdict{}
	}
	defer func() {
		if recover() != nil {
			f.log.Error("guard panicked", "target", c.Target, "kind", "call")
			f.m.guardFailure(guardPanic)
			v = Verdict{Refuse: true, Reason: reasonGuardFailed}
		}
	}()
	return cc.CheckCall(ctx, c)
}

// Violation implements ViolationReporter, so a panic in the real reporter cannot end a connection.
func (f failClosed) Violation(ctx context.Context, m Message, v Violation) {
	r, ok := f.g.(ViolationReporter)
	if !ok {
		return
	}
	defer func() {
		if recover() != nil {
			f.m.guardFailure(guardPanic)
		}
	}()
	r.Violation(ctx, m, v)
}

// Fixed label values of the gateway's metrics (ADR 0023): no org, agent, target or tool name.
const (
	guardNotLoaded = "not_loaded"
	guardPanic     = "panic"

	dropCredentialNeedsHTTPS = "credential_needs_https" //nolint:gosec // a metric label, not a credential
	dropInvalidName          = "invalid_name"
	dropDuplicateName        = "duplicate_name"
	dropNoURL                = "no_url"
	dropOrgNeedsGuard        = "org_needs_guard"
)

// Metrics are the gateway's Prometheus counters. Every label has a fixed set of values (ADR 0023).
// A nil *Metrics is valid and counts nothing.
type Metrics struct {
	// NotesDropped counts server notifications dropped because the inspection queue was full.
	NotesDropped prometheus.Counter
	// GuardFailures counts messages and calls refused because the guard was not loaded or panicked.
	GuardFailures *prometheus.CounterVec
	// Dropped counts config entries ApplyMCP left out, which closes their route, by reason.
	Dropped *prometheus.CounterVec
}

// NewMetrics creates the counters and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		NotesDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_gateway_mcp_notifications_dropped_total",
			Help: "Server notifications the MCP path dropped because its inspection queue was full.",
		}),
		GuardFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_mcp_guard_failures_total",
			Help: "MCP messages and calls refused because the guard was not loaded or failed, by reason.",
		}, []string{"reason"}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_mcp_config_dropped_total",
			Help: "MCP config entries left out of the applied config, which closes their route, by reason.",
		}, []string{"reason"}),
	}
	for _, r := range []string{guardNotLoaded, guardPanic} {
		m.GuardFailures.WithLabelValues(r)
	}
	for _, r := range []string{dropCredentialNeedsHTTPS, dropInvalidName, dropDuplicateName, dropNoURL, dropOrgNeedsGuard} {
		m.Dropped.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.NotesDropped, m.GuardFailures, m.Dropped)
	}
	return m
}

func (m *Metrics) guardFailure(reason string) {
	if m != nil {
		m.GuardFailures.WithLabelValues(reason).Inc()
	}
}

func (m *Metrics) noteDropped() {
	if m != nil {
		m.NotesDropped.Inc()
	}
}

func (m *Metrics) dropped(reason string) {
	if m != nil {
		m.Dropped.WithLabelValues(reason).Inc()
	}
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
