// SPDX-License-Identifier: Apache-2.0

// Package ledger is Koinon's minimal spend ledger (plan item L1; ADRs 0041,
// 0049).
//
//   - Ingestion: agentgateway exports its access log over OTLP/HTTP (protobuf)
//     to koinon-api, which serves [Handler]. Each LLM request record carries
//     the gateway key and agent (koinon.key, koinon.agent), a per-request id
//     (koinon.request), the requested model, token counts and the USD cost
//     from the model catalog. [Ingester] maps it to the agent's run at the
//     record's time and writes one spend_entries row, idempotent on the
//     request id.
//   - Run limits (ADR 0080): a recorded request adds its tokens to the run's reported total
//     (tokens are reported, not enforced), and every recorded or settled entry of a run is
//     followed by scheduler.CheckRunLimits, which cancels the run at its model-request or spend
//     limit after the agent's, org's and platform's budgets were checked ([enforce]).
//   - Cost truth (plan item PV2): the ledger recomputes each request's cost from the raw
//     usage and the dated price ([prices.Resolve]) and records the price version it used.
//     That cost counts for limits and reports; agentgateway's own cost is kept beside it and
//     a difference over 1% flags the price entry. Usage a provider did not report is estimated
//     (package estimate), marked, and counted toward limits.
//   - Budgets (plan item BG1, package budget): an agent's daily limit, the org's monthly cap
//     and the platform ceiling. koinon-reconciler pushes the smallest remaining amount to
//     agentgateway as the key's budget, net of the spend already recorded ([AgentLimit]), so a
//     budget counter the gateway lost (pod replacement, key rotation) cannot hand out the
//     full limit again. When recorded spend reaches a limit, the ingester cancels the run
//     (every open run of the org for an org-wide limit) with a task event naming the limit.
//
// Unpriced models are not served at all (the gateway drivers render only
// priced models), so every charged request has a cost. A record that still
// arrives without one is kept with priced = false.
package ledger

import (
	"time"

	"github.com/stateracloud/koinon/internal/budget"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// Window is the agent spend-limit window: the UTC day (agentgateway aligns 24h budget windows
// to the Unix epoch, so it starts at midnight UTC and the ledger uses the same boundaries).
const Window = budget.DayWindow

// RunGrace attributes a request that completes shortly after its run finished
// (for example a response in flight when a run limit canceled the run) to
// that run.
const RunGrace = time.Minute

// WindowStart returns the start of the window containing t.
func WindowStart(t time.Time) time.Time { return budget.DayStart(t) }

// Entry is one request from agentgateway's access log.
type Entry struct {
	// RequestID is unique per request (koinon.request).
	RequestID string
	// KeyID is the gateway key (koinon.key).
	KeyID string
	// Agent is the key's agent (koinon.agent): the org slug and the agent ID.
	Agent gateway.AgentRef
	Model string
	// InputTokens includes cache reads and writes; OutputTokens includes reasoning
	// (agentgateway's consistent semantics). Known say whether the gateway logged the side.
	InputTokens, OutputTokens         int64
	InputKnown, OutputKnown           bool
	CacheReadTokens, CacheWriteTokens int64
	ReasoningTokens                   int64
	// CostUSD is the decimal cost as the gateway logged it; empty when the gateway could not
	// price the request. It is compared with Koinon's own computation, never charged.
	CostUSD string
	// Status is the HTTP status the gateway answered (0 when not logged).
	Status int64
	// MaxTokens is the request's max_tokens (0 when not logged).
	MaxTokens int64
	// RequestBytes and ResponseBytes are -1 when not logged.
	RequestBytes, ResponseBytes int64
	// ResponseID is the provider's response ID from a response header (koinon.response_id: an
	// OpenRouter generation ID); empty when the provider sent none.
	ResponseID string
	// Format is the wire format the model was served in (koinon.format: modelformat.OpenAI or
	// modelformat.Anthropic); a record without the field is OpenAI, the only format served before
	// it existed. Stream is koinon.stream: the response was streamed.
	Format modelformat.Format
	Stream bool
	// Error is koinon.error, why the proxy failed the request ("Upstream" for a stream the upstream
	// cut); empty when the field is absent or the request did not fail.
	Error string
	// Cut is set by the ingester: a response with logged output usage that was cut anyway, because
	// the proxy logged an error for it, or it is a streamed Anthropic-format response whose run was
	// canceled at or before the record's time. Its logged usage is partial, not final.
	Cut bool
	// Time is when the gateway logged the request.
	Time time.Time
}
