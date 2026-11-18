// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Outcome is how a tool call ended, as the audit row and the span say it.
type Outcome string

const (
	// OutcomeOK is a call whose result reached the agent.
	OutcomeOK Outcome = "ok"
	// OutcomeToolError is a call the target answered with a tool error (IsError).
	OutcomeToolError Outcome = "tool_error"
	// OutcomeRefusedGrant is a call to a tool the agent has no grant for, or to a target it cannot see.
	OutcomeRefusedGrant Outcome = "refused_grant"
	// OutcomeRefusedGuard is a call the guard refused, on the request or on the result.
	OutcomeRefusedGuard Outcome = "refused_guard"
	// OutcomeUpstreamError is a call that failed before a result: the target is down or the
	// credential failed.
	OutcomeUpstreamError Outcome = "upstream_error"
)

// AuditRow is what the gateway records for one tool call. It names no argument, no result and no
// token value: those can hold a secret (ADR 0097 decision 5).
type AuditRow struct {
	Agent    gateway.AgentRef
	Target   string
	Tool     string
	Outcome  Outcome
	Verdict  string // the guard's reason on a refusal, "" otherwise
	CredKind string // CredentialNone, CredentialStatic, ...
	Duration time.Duration
}

// Audit stores one row per tool call. A failing Audit must not fail the call: the gateway logs it.
type Audit interface {
	Record(ctx context.Context, row AuditRow) error
}
