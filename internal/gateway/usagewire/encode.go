// SPDX-License-Identifier: Apache-2.0

// Package usagewire is the sending side of the usage record (ADR 0097 decision 3). The spend ledger
// reads usage records from OTLP logs exports, and agentgateway is not the only gateway that sends
// them: koinon-gateway's model path (package modelgateway) encodes its own records into the same
// attributes, so koinon-api has one receiver (package agentgateway) and the ledger one record.
package usagewire

import (
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
)

// attrRequestModel and attrCostTotal are the two attributes of agentgateway's own access log that
// the receiver reads next to the koinon.* ones.
const (
	attrRequestModel = "gen_ai.request.model"
	attrCostTotal    = "agw.ai.usage.cost.total"
)

// scopeName names the instrumentation scope of the exported records.
const scopeName = "koinon-gateway"

// Encode returns the OTLP logs export of records. The receiver (agentgateway.Decode) maps it back to
// the same records: the encoder adds the cache and reasoning parts to the totals the receiver's
// attributes carry (agentgateway reports totals), and the receiver subtracts them again, so the
// record's buckets stay disjoint end to end.
func Encode(records []gateway.UsageRecord) *collogspb.ExportLogsServiceRequest {
	lrs := make([]*logspb.LogRecord, 0, len(records))
	for _, r := range records {
		lrs = append(lrs, logRecord(r))
	}
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", scopeName)}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: scopeName}, LogRecords: lrs}},
	}}}
}

func logRecord(r gateway.UsageRecord) *logspb.LogRecord {
	at := r.Time
	if at.IsZero() {
		at = time.Now()
	}
	attrs := []*commonpb.KeyValue{
		str(agentgateway.LogFieldKey, r.KeyID),
		str(agentgateway.LogFieldAgent, r.Agent.String()),
		str(agentgateway.LogFieldRequest, r.RequestID),
		str(attrRequestModel, r.Model),
		str(agentgateway.LogFieldFormat, string(r.Format)),
		boolean(agentgateway.LogFieldStream, r.Stream),
	}
	if r.Status > 0 {
		attrs = append(attrs, integer(agentgateway.LogFieldStatus, r.Status))
	}
	if r.UpstreamKnown {
		attrs = append(attrs, boolean(agentgateway.LogFieldUpstream, r.Upstream))
	}
	if r.Error != "" {
		attrs = append(attrs, str(agentgateway.LogFieldError, r.Error))
	}
	if r.MaxTokens > 0 {
		attrs = append(attrs, integer(agentgateway.LogFieldMaxTokens, r.MaxTokens))
	}
	if r.RequestBytes >= 0 {
		attrs = append(attrs, integer(agentgateway.LogFieldRequestBytes, r.RequestBytes))
	}
	if r.ResponseID != "" {
		attrs = append(attrs, str(agentgateway.LogFieldResponseID, r.ResponseID))
	}
	if r.CostUSD != "" {
		attrs = append(attrs, str(attrCostTotal, r.CostUSD))
	}
	// Totals, as agentgateway logs them: the input includes the cache reads and writes, the output
	// includes the reasoning. The receiver takes the parts out again.
	if r.InputKnown {
		attrs = append(attrs,
			integer(agentgateway.LogFieldInput, r.InputTokens+r.CacheReadTokens+r.CacheWriteTokens),
			integer(agentgateway.LogFieldCacheRead, r.CacheReadTokens),
			integer(agentgateway.LogFieldCacheWrite, r.CacheWriteTokens))
	}
	if r.OutputKnown {
		out := r.OutputTokens
		if r.ReasoningKnown {
			out += r.ReasoningTokens
		}
		attrs = append(attrs, integer(agentgateway.LogFieldOutput, out))
	}
	if r.ReasoningKnown {
		attrs = append(attrs, integer(agentgateway.LogFieldReasoning, r.ReasoningTokens))
	}
	ns := uint64(at.UnixNano())
	return &logspb.LogRecord{TimeUnixNano: ns, ObservedTimeUnixNano: ns, Attributes: attrs}
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func integer(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func boolean(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}
