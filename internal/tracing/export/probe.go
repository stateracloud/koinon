// SPDX-License-Identifier: Apache-2.0

package export

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// NewTraceID returns a random, non-zero trace ID. Koinon assigns trace IDs (ADR 0074, decision 2).
func NewTraceID() [16]byte {
	var id [16]byte
	for id == [16]byte{} {
		_, _ = rand.Read(id[:])
	}
	return id
}

// NewSpanID returns a random, non-zero span ID.
func NewSpanID() [8]byte {
	var id [8]byte
	for id == [8]byte{} {
		_, _ = rand.Read(id[:])
	}
	return id
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func boolean(k string, v bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}}
}

// TestTrace builds the synthetic trace of the test before saving (decision 9): a root span and one
// child, no content, koinon.test = true. org, when set, is koinon.org. resource adds the
// destination's resource attributes to the resource.
func TestTrace(now time.Time, org string, resource map[string]string) *coltracepb.ExportTraceServiceRequest {
	traceID, rootID, childID := NewTraceID(), NewSpanID(), NewSpanID()
	attrs := func(op string, extra ...*commonpb.KeyValue) []*commonpb.KeyValue {
		out := []*commonpb.KeyValue{
			str(semconv.GenAIOperationName, op),
			boolean(semconv.KoinonTest, true),
		}
		if org != "" {
			out = append(out, str(semconv.KoinonOrg, org))
		}
		return append(out, extra...)
	}
	start := uint64(now.Add(-200 * time.Millisecond).UnixNano())
	mid := uint64(now.Add(-150 * time.Millisecond).UnixNano())
	end := uint64(now.UnixNano())
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", semconv.ServiceName)}}
	for _, k := range sortedKeys(resource) {
		if k != "service.name" {
			res.Attributes = append(res.Attributes, str(k, resource[k]))
		}
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:  res,
		SchemaUrl: semconv.SchemaURL,
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: semconv.ScopeName},
			SchemaUrl: semconv.SchemaURL,
			Spans: []*tracepb.Span{
				{
					TraceId: traceID[:], SpanId: rootID[:],
					Name: semconv.SpanName(semconv.OperationInvokeAgent, "koinon-test"), Kind: tracepb.Span_SPAN_KIND_INTERNAL,
					StartTimeUnixNano: start, EndTimeUnixNano: end,
					Attributes: attrs(semconv.OperationInvokeAgent, str(semconv.GenAIAgentName, "koinon-test")),
				},
				{
					TraceId: traceID[:], SpanId: childID[:], ParentSpanId: rootID[:],
					Name: semconv.SpanName(semconv.OperationChat, "koinon-test"), Kind: tracepb.Span_SPAN_KIND_CLIENT,
					StartTimeUnixNano: mid, EndTimeUnixNano: end,
					Attributes: attrs(semconv.OperationChat, str(semconv.GenAIRequestModel, "koinon-test"), str(semconv.GenAIProviderName, "koinon")),
				},
			},
		}},
	}}}
}

// Outcome is what a test trace showed.
type Outcome struct {
	// OK: the backend answered 2xx and rejected no span.
	OK bool
	// Status is the HTTP status, 0 when there was no answer.
	Status int
	// Rejected is the number of spans refused in a partial success.
	Rejected int64
	// Message is a short, secret-free explanation; empty when OK.
	Message string
}

// Test sends the synthetic trace to d and says whether the backend took it: 2xx and no rejected
// span (decision 9). org and the destination's resource attributes go on the trace.
func (c *Client) Test(ctx context.Context, d Destination, org string, resource map[string]string, now time.Time) Outcome {
	res, err := c.Export(ctx, d, TestTrace(now, org, resource))
	if err != nil {
		var ee *Error
		if errors.As(err, &ee) {
			return Outcome{Status: ee.Status, Message: ee.Message}
		}
		return Outcome{Message: "the test trace could not be sent"}
	}
	if res.Rejected > 0 {
		msg := fmt.Sprintf("the backend accepted the request but rejected %d span(s)", res.Rejected)
		return Outcome{Status: res.Status, Rejected: res.Rejected, Message: msg}
	}
	return Outcome{OK: true, Status: res.Status}
}
