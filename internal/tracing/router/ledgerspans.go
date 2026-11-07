// SPDX-License-Identifier: Apache-2.0

package router

import (
	"math"
	"strconv"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// ModelCall is one spend_entries row of a run, as the router needs it (ADR 0074, decisions 3 and 5).
type ModelCall struct {
	RequestID string
	Model     string
	// Provider is the type of the model's provider ("openrouter", "anthropic"); empty when the model
	// is no longer in the catalog.
	Provider string
	// ModelID is the catalog model's ID; empty when the model is gone.
	ModelID string
	AgentID string
	TaskID  string
	// Token counts as the ledger keeps them: Input includes cache reads and writes, Output includes
	// reasoning, which is what the GenAI conventions mean by the same names.
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	// ReasoningKnown: the provider reported the thinking split, so Reasoning is a figure even at 0
	// (spend_entries.reasoning_known, #78).
	ReasoningKnown bool
	// Cost is Koinon's cost (the ledger's); nil when the entry is not priced.
	Cost *float64
	// GatewayCost is what agentgateway logged; nil when it logged none.
	GatewayCost *float64
	// Provisional: the provider's own figure for the request is still pending, so Cost may change.
	Provisional bool
	Estimated   bool
	// UpstreamStatus is the provider's error status, 0 when the call succeeded.
	UpstreamStatus int
	Time           time.Time
}

// pending says the entry's cost is not final: it has none yet, or a provider figure is awaited.
func (c ModelCall) pending() bool { return c.Cost == nil || c.Provisional }

// turnUsage is a turn's totals over its model calls.
type turnUsage struct {
	Calls                                           int
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	ReasoningKnown                                  bool
	Cost                                            float64
	Pending                                         bool
	// ModelID is the first call's model ID, for the root.
	ModelID string
}

// modelCallSpans makes the chat spans of a turn, children of its root, and the turn's totals. root
// supplies the attributes every span of the run carries.
func modelCallSpans(root *tracepb.Span, trace [16]byte, seq int32, calls []ModelCall) ([]*tracepb.Span, turnUsage) {
	rootID := semconv.RootSpanID(trace)
	var (
		spans []*tracepb.Span
		use   turnUsage
	)
	for _, c := range calls {
		at := uint64(c.Time.UnixNano())
		provider := c.Provider
		if provider == "" {
			provider = "_OTHER"
		}
		id := semconv.ModelCallSpanID(trace, c.RequestID)
		sp := &tracepb.Span{
			TraceId: trace[:], SpanId: id[:], ParentSpanId: rootID[:],
			Name: semconv.SpanName(semconv.OperationChat, c.Model), Kind: tracepb.Span_SPAN_KIND_CLIENT,
			// The ledger knows when the gateway logged the request, not how long it took (option D
			// brings the real timing), so the span is a point at that time.
			StartTimeUnixNano: at, EndTimeUnixNano: at,
		}
		a := sp.Attributes
		for _, k := range []string{semconv.GenAIConversationID, semconv.SessionID, semconv.KoinonRun, semconv.KoinonOrg, semconv.KoinonTask, semconv.GenAIAgentID, semconv.GenAIAgentName} {
			if v, ok := attrString(root.Attributes, k); ok {
				a = append(a, kvStr(k, v))
			}
		}
		if _, ok := attrString(a, semconv.KoinonTask); !ok && c.TaskID != "" {
			a = append(a, kvStr(semconv.KoinonTask, c.TaskID))
		}
		if _, ok := attrString(a, semconv.GenAIAgentID); !ok && c.AgentID != "" {
			a = append(a, kvStr(semconv.GenAIAgentID, c.AgentID))
		}
		a = append(a,
			kvInt(semconv.KoinonTurn, int64(seq)),
			kvStr(semconv.GenAIOperationName, semconv.OperationChat),
			kvStr(semconv.GenAIProviderName, provider),
			kvStr(semconv.GenAIRequestModel, c.Model),
			kvInt(semconv.GenAIUsageInputTokens, c.Input),
			kvInt(semconv.GenAIUsageOutputTokens, c.Output),
			kvInt(semconv.GenAIUsageCacheReadTokens, c.CacheRead),
			kvInt(semconv.GenAIUsageCacheWriteTokens, c.CacheWrite),
		)
		// The ledger keeps 0 when the provider reported no thinking split (agentgateway logs none for
		// Anthropic Messages, #78), so the attribute is left out rather than claim no thinking happened
		// (old issue 316). A reported split is written, 0 included.
		if c.ReasoningKnown || c.Reasoning > 0 {
			a = append(a, kvInt(semconv.GenAIUsageReasoningTokens, c.Reasoning))
		}
		a = append(a, kvStr(semconv.KoinonRequest, c.RequestID))
		if c.ModelID != "" {
			a = append(a, kvStr(semconv.KoinonModelID, c.ModelID))
		}
		if c.Cost != nil {
			a = append(a, kvDouble(semconv.GenAIUsageCost, round12(*c.Cost)), kvDouble(semconv.KoinonCostUSD, round12(*c.Cost)))
		}
		if c.GatewayCost != nil {
			a = append(a, kvDouble(semconv.KoinonGatewayCostUSD, round12(*c.GatewayCost)))
		}
		if c.pending() {
			a = append(a, kvBool(semconv.KoinonCostPending, true))
		}
		if c.Estimated {
			a = append(a, kvBool("koinon.usage_estimated", true))
		}
		if c.UpstreamStatus != 0 {
			a = append(a, kvStr("error.type", httpStatusText(c.UpstreamStatus)))
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
		}
		sp.Attributes = a
		spans = append(spans, sp)

		if use.ModelID == "" {
			use.ModelID = c.ModelID
		}
		use.Calls++
		use.Input += c.Input
		use.Output += c.Output
		use.CacheRead += c.CacheRead
		use.CacheWrite += c.CacheWrite
		use.Reasoning += c.Reasoning
		use.ReasoningKnown = use.ReasoningKnown || c.ReasoningKnown
		if c.Cost != nil {
			use.Cost += *c.Cost
		}
		use.Pending = use.Pending || c.pending()
	}
	return spans, use
}

// putTotals sets the turn's totals on its root span (decision 3). They are written even when the turn
// made no model call, so a backend shows zero rather than a missing figure; the reasoning tokens are
// the exception, left out when no call reported a split (old issue 316, #78), since 0 would read as "no thinking".
func putTotals(root *tracepb.Span, u turnUsage) {
	a := root.Attributes
	a = setAttr(a, kvInt(semconv.GenAIUsageInputTokens, u.Input))
	a = setAttr(a, kvInt(semconv.GenAIUsageOutputTokens, u.Output))
	a = setAttr(a, kvInt(semconv.GenAIUsageCacheReadTokens, u.CacheRead))
	a = setAttr(a, kvInt(semconv.GenAIUsageCacheWriteTokens, u.CacheWrite))
	if u.ReasoningKnown || u.Reasoning > 0 {
		a = setAttr(a, kvInt(semconv.GenAIUsageReasoningTokens, u.Reasoning))
	} else {
		a = delAttr(a, semconv.GenAIUsageReasoningTokens)
	}
	a = setAttr(a, kvDouble(semconv.GenAIUsageCost, round12(u.Cost)))
	a = setAttr(a, kvDouble(semconv.KoinonCostUSD, round12(u.Cost)))
	a = setAttr(a, kvInt("koinon.model_calls", int64(u.Calls)))
	if u.Pending {
		a = setAttr(a, kvBool(semconv.KoinonCostPending, true))
	}
	if u.ModelID != "" {
		a = setAttr(a, kvStr(semconv.KoinonModelID, u.ModelID))
	}
	root.Attributes = a
}

// round12 keeps the ledger's 12 decimal places and drops float noise from summing.
func round12(f float64) float64 { return math.Round(f*1e12) / 1e12 }

func httpStatusText(code int) string { return "http_" + strconv.Itoa(code) }
