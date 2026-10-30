// SPDX-License-Identifier: Apache-2.0

// Package semconv holds the attribute and span names of Koinon's run traces (ADR 0074, decision 3).
//
// The GenAI names follow the OpenTelemetry GenAI conventions at one pinned commit, because they are
// in Development and move without a release. Moving to a newer commit is a reviewed change: change
// Commit and SchemaURL here, update the golden file of emitted spans, and say so in the ADR. The
// names are plain constants so the router, agentd and the tests spell them one way.
package semconv

import "strings"

// Commit is the pinned commit of open-telemetry/semantic-conventions-genai (2026-10-28).
const Commit = "3fe84604a581b3d50dd5b4bd29383b462d2a5018"

// SchemaURL is set as the schema_url of every resource and scope Koinon exports. The conventions
// repository has no schema file or release yet, so it names the pinned tree.
const SchemaURL = "https://github.com/open-telemetry/semantic-conventions-genai/tree/" + Commit

// ScopeName is the instrumentation scope of the spans Koinon makes.
const ScopeName = "github.com/stateracloud/koinon/tracing"

// ServiceName is the service.name resource attribute of Koinon's traces.
const ServiceName = "koinon"

// GenAI attributes at the pinned commit. The names say "tokens" and "cache", not credentials.
//
//nolint:gosec // G101 matches "token" in attribute names
const (
	GenAIOperationName          = "gen_ai.operation.name"
	GenAIProviderName           = "gen_ai.provider.name"
	GenAIRequestModel           = "gen_ai.request.model"
	GenAIResponseModel          = "gen_ai.response.model"
	GenAIResponseFinishReasons  = "gen_ai.response.finish_reasons"
	GenAIConversationID         = "gen_ai.conversation.id"
	GenAIAgentID                = "gen_ai.agent.id"
	GenAIAgentName              = "gen_ai.agent.name"
	GenAIAgentVersion           = "gen_ai.agent.version"
	GenAIUsageInputTokens       = "gen_ai.usage.input_tokens"
	GenAIUsageOutputTokens      = "gen_ai.usage.output_tokens"
	GenAIUsageCacheReadTokens   = "gen_ai.usage.cache_read.input_tokens"
	GenAIUsageCacheWriteTokens  = "gen_ai.usage.cache_write.input_tokens"
	GenAIUsageReasoningTokens   = "gen_ai.usage.reasoning.output_tokens"
	GenAIToolName               = "gen_ai.tool.name"
	GenAIToolCallID             = "gen_ai.tool.call.id"
	GenAIToolType               = "gen_ai.tool.type"
	GenAIToolCallArguments      = "gen_ai.tool.call.arguments"
	GenAIToolCallResult         = "gen_ai.tool.call.result"
	GenAIInputMessages          = "gen_ai.input.messages"
	GenAIOutputMessages         = "gen_ai.output.messages"
	GenAITransferMode           = "gen_ai.transfer.mode"
	GenAITransferTargetName     = "gen_ai.transfer.target.name"
	GenAITransferModeReturnCall = "return_to_caller"

	// GenAIUsageCost is not in the conventions. Langfuse and Opik read it, so Koinon writes the
	// ledger's cost there too (decision 5).
	GenAIUsageCost = "gen_ai.usage.cost"

	// SessionID groups a run's turns in Langfuse and others (decision 2).
	SessionID = "session.id"
)

// Operation names (gen_ai.operation.name) and the span-name prefixes built from them.
const (
	OperationChat        = "chat"
	OperationInvokeAgent = "invoke_agent"
	OperationExecuteTool = "execute_tool"
)

// Koinon's own attributes, events and span links (decisions 2, 3, 5, 7, 9).
const (
	KoinonOrg             = "koinon.org"
	KoinonRun             = "koinon.run"
	KoinonTask            = "koinon.task"
	KoinonTurn            = "koinon.turn"
	KoinonRequest         = "koinon.request"
	KoinonKey             = "koinon.key"
	KoinonAgent           = "koinon.agent"
	KoinonRequestedByKind = "koinon.requested_by.kind"
	KoinonRequestedByID   = "koinon.requested_by.id"
	KoinonOutcome         = "koinon.outcome"
	KoinonFailureClass    = "koinon.failure_class"
	KoinonCostUSD         = "koinon.cost_usd"
	KoinonGatewayCostUSD  = "koinon.gateway_cost_usd"
	KoinonCostPending     = "koinon.cost_pending"
	KoinonToolKind        = "koinon.tool.kind"
	KoinonToolStatus      = "koinon.tool.status"
	KoinonSpansDropped    = "koinon.spans_dropped"
	// KoinonTest marks the synthetic trace of the test before saving a destination.
	KoinonTest = "koinon.test"
	// KoinonParkReason is the attribute of the koinon.park event.
	KoinonParkReason = "koinon.park.reason"
	// KoinonWaitSeconds is the attribute of the koinon.wait event.
	KoinonWaitSeconds = "koinon.wait.seconds"

	// KoinonContentAttributes lists, on a span, the names of its attributes that carry content (prompts,
	// completions, thinking, tool arguments and results). agentd sets it on every span that has any,
	// and only when the org's content level is above none; the router strips or scrubs exactly these
	// attributes per destination level and drops the marker itself.
	KoinonContentAttributes = "koinon.content.attributes"

	// EventPark ends a turn's trace at an approval, ask_user or ask_agent.
	EventPark = "koinon.park"
	// EventWait is on the next turn's root: how long the run waited.
	EventWait = "koinon.wait"

	// ToolTypeFunction is gen_ai.tool.type for ACP tool calls.
	ToolTypeFunction = "function"
)

// RequestedBy kinds.
const (
	RequestedByUser    = "user"
	RequestedByTrigger = "trigger"
	RequestedByAgent   = "agent"
)

// SpanName builds "{operation} {subject}", the name of a span in the conventions: "chat gpt-5",
// "invoke_agent triage", "execute_tool read_file". An empty subject gives the bare operation.
func SpanName(operation, subject string) string {
	if subject == "" {
		return operation
	}
	return operation + " " + subject
}

// IsKoinon reports whether an attribute name is one of Koinon's own.
func IsKoinon(name string) bool { return strings.HasPrefix(name, "koinon.") }
