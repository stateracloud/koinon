// SPDX-License-Identifier: Apache-2.0

// Package ledger is Koinon's minimal spend ledger (plan item L1; ADRs 0041,
// 0049).
//
//   - Ingestion: a gateway driver yields Koinon usage records ([Entry], ADR 0097 decision 3):
//     the gateway key and agent, a per-request id, the model, token counts in disjoint buckets
//     and, optionally, the gateway's own cost. For agentgateway, package agentgateway receives
//     the access log over OTLP/HTTP and maps it; the ledger knows no gateway's field names.
//     [Ingester] maps a record to the agent's run at the record's time and writes one
//     spend_entries row, idempotent on the request id.
//   - Run limits (ADR 0081): a recorded request adds its tokens to the run's reported total
//     (tokens are reported, not enforced), and every recorded or settled entry of a run is
//     followed by scheduler.CheckRunLimits, which cancels the run at its model-request or spend
//     limit after the agent's, org's and platform's budgets were checked ([enforce]).
//   - Cost truth (plan item PV2): the ledger recomputes each request's cost from the raw
//     usage and the dated price ([prices.Resolver.Find]; the one place that prices a usage, old issue 373) and records the price version it used.
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

// Entry is one request as the ledger records it: the Koinon usage record a gateway driver yields
// (ADR 0097 decision 3). The ledger never reads a gateway's own fields.
type Entry = gateway.UsageRecord

// CheckRecord reports why a usage record cannot be recorded: the reject kind (a Reject* constant, the
// metric label) and a message for the exporter. It returns "" for a valid record. A driver calls it
// after mapping, so every driver is held to the same rules.
func CheckRecord(r Entry) (kind, reason string) {
	switch {
	case r.RequestID == "" || len(r.RequestID) > 128:
		return RejectInvalidReq, "missing or invalid request ID"
	case r.KeyID == "":
		return RejectMissingKey, "missing key ID"
	case r.InputTokens < 0 || r.OutputTokens < 0 || r.CacheReadTokens < 0 || r.CacheWriteTokens < 0 || r.ReasoningTokens < 0:
		return RejectNegativeToken, "negative token counts"
	}
	return "", ""
}
