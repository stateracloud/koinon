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
//   - Run caps: a recorded request adds its tokens to the run through
//     scheduler.RecordTokenUsage, so the run's token cap (plan item A4) works.
//   - Spend limit: each agent has one USD limit from its template
//     (limits.spendUSD) per [Window]. koinon-reconciler pushes it to
//     agentgateway as the key's budget, net of the spend already recorded in
//     the current window ([AgentLimit]), so a budget counter the gateway lost
//     (pod replacement, key rotation) cannot hand out the full limit again.
//     When recorded spend reaches the limit, the ingester cancels the run with
//     a ticket event.
//
// Unpriced models are not served at all (the gateway drivers render only
// priced models), so every charged request has a cost. A record that still
// arrives without one is kept with priced = false.
package ledger

import (
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
)

// Window is the spend-limit window. agentgateway aligns budget windows to the
// Unix epoch, so this one starts at midnight UTC; the ledger uses the same
// boundaries.
const Window = 24 * time.Hour

// RunGrace attributes a request that completes shortly after its run finished
// (for example a response in flight when the token cap canceled the run) to
// that run.
const RunGrace = time.Minute

// WindowStart returns the start of the window containing t.
func WindowStart(t time.Time) time.Time { return t.UTC().Truncate(Window) }

// Entry is one charged request from agentgateway's access log.
type Entry struct {
	// RequestID is unique per request (koinon.request).
	RequestID string
	// KeyID is the gateway key (koinon.key).
	KeyID string
	// Agent is the key's agent (koinon.agent): the org slug and the agent ID.
	Agent        gateway.AgentRef
	Model        string
	InputTokens  int64
	OutputTokens int64
	// CostUSD is the decimal cost as the gateway logged it; empty when the
	// gateway could not price the request.
	CostUSD string
	// Time is when the gateway logged the request.
	Time time.Time
}
