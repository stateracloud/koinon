// SPDX-License-Identifier: Apache-2.0

package router

import (
	"regexp"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// DefaultContentLimit is the most bytes one content attribute keeps (ADR 0074, decision 6).
const DefaultContentLimit = 16 << 10

// contentNames are the attributes that carry content whether or not a span lists them in
// koinon.content.attributes: a span that forgot to list one is still treated as content, never sent
// at level none.
var contentNames = map[string]bool{
	semconv.GenAIInputMessages:     true,
	semconv.GenAIOutputMessages:    true,
	semconv.GenAIToolCallArguments: true,
	semconv.GenAIToolCallResult:    true,
}

// copyParams is what turns one span into the copy a destination receives.
type copyParams struct {
	// Level is the destination's content level.
	Level scrub.Level
	// Always scrubs every non-content string (credentials only, the always tier). Content scrubs the
	// content attributes at Level. Secrets are the org's known secret values in both.
	Always, Content *scrub.Scrubber
	// Limit is the most bytes one content attribute keeps.
	Limit int
	// Platform replaces names with IDs (decision 3).
	Platform bool
	// Dropped, when above zero, is set as koinon.spans_dropped (on a root span).
	Dropped int64
}

// newCopyParams builds the scrubbers of one destination copy. observe counts redactions by kind.
func newCopyParams(level scrub.Level, secrets *scrub.Secrets, limit int, platform bool, observe func(scrub.Kind)) copyParams {
	if limit <= 0 {
		limit = DefaultContentLimit
	}
	return copyParams{
		Level:    level,
		Always:   scrub.New(scrub.Config{Level: scrub.LevelFull, Secrets: secrets, OnRedaction: observe}),
		Content:  scrub.New(scrub.Config{Level: level, Secrets: secrets, OnRedaction: observe}),
		Limit:    limit,
		Platform: platform,
	}
}

// copySpan returns the copy of in for one destination (decisions 3, 6 and 8) and leaves in alone:
//
//   - the attributes listed in koinon.content.attributes (and the four known content names) are
//     removed at level none, and scrubbed and cut to the limit at redacted and full; the marker is
//     removed at every level;
//   - every other string, in attributes, events, links, the name and the status, goes through the
//     always tier of the scrubber (credentials), so a secret in a tool name or an error message does
//     not leave either;
//   - on the platform's copy names become IDs.
func copySpan(in *tracepb.Span, p copyParams) *tracepb.Span {
	sp := proto.Clone(in).(*tracepb.Span)

	content := map[string]bool{}
	for k := range contentNames {
		content[k] = true
	}
	for _, kv := range sp.Attributes {
		if kv.Key == semconv.KoinonContentAttributes {
			for _, name := range stringList(kv.Value) {
				content[name] = true
			}
		}
	}

	out := sp.Attributes[:0]
	for _, kv := range sp.Attributes {
		switch {
		case kv.Key == semconv.KoinonContentAttributes:
		case content[kv.Key]:
			if p.Level == scrub.LevelNone {
				continue
			}
			s, ok := kv.Value.GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				continue // content that is not a string is not something the scrubber can vouch for
			}
			cleaned, _ := p.Content.Content(s.StringValue, p.Limit)
			out = append(out, kvStr(kv.Key, cleaned))
		default:
			scrubValue(kv.Value, p.Always)
			out = append(out, kv)
		}
	}
	sp.Attributes = out

	sp.Name = p.Always.Text(sp.Name)
	for _, ev := range sp.Events {
		ev.Name = p.Always.Text(ev.Name)
		for _, kv := range ev.Attributes {
			scrubValue(kv.Value, p.Always)
		}
	}
	for _, l := range sp.Links {
		for _, kv := range l.Attributes {
			scrubValue(kv.Value, p.Always)
		}
	}
	if sp.Status != nil {
		sp.Status.Message = p.Always.Text(sp.Status.Message)
	}
	if p.Platform {
		platformNames(sp)
	}
	if p.Dropped > 0 {
		sp.Attributes = setAttr(sp.Attributes, kvInt(semconv.KoinonSpansDropped, p.Dropped))
	}
	return sp
}

// scrubValue scrubs the strings of v in place, arrays and maps included.
func scrubValue(v *commonpb.AnyValue, s *scrub.Scrubber) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		x.StringValue = s.Text(x.StringValue)
	case *commonpb.AnyValue_ArrayValue:
		for _, e := range x.ArrayValue.GetValues() {
			scrubValue(e, s)
		}
	case *commonpb.AnyValue_KvlistValue:
		for _, kv := range x.KvlistValue.GetValues() {
			scrubValue(kv.Value, s)
		}
	}
}

// mcpTool matches mcp__<server>__<tool>. The server's name is the org's.
var mcpTool = regexp.MustCompile(`^mcp__[^_]+(?:_[^_]+)*__(.+)$`)

// platformNames replaces what names an org's things with the IDs the span already carries
// (decision 3: the platform's copy carries IDs only). The agent's name becomes its ID in the
// attribute and in the span name; a transfer target's name (an agent) is removed; an MCP tool loses
// its server's name.
func platformNames(sp *tracepb.Span) {
	agentID, _ := attrString(sp.Attributes, semconv.GenAIAgentID)
	name, hadName := attrString(sp.Attributes, semconv.GenAIAgentName)
	if hadName {
		repl := agentID
		if repl == "" {
			repl = "unknown"
		}
		if sp.Name == semconv.SpanName(semconv.OperationInvokeAgent, name) {
			sp.Name = semconv.SpanName(semconv.OperationInvokeAgent, repl)
		}
		sp.Attributes = setAttr(sp.Attributes, kvStr(semconv.GenAIAgentName, repl))
	}
	out := sp.Attributes[:0]
	for _, kv := range sp.Attributes {
		if kv.Key == semconv.GenAITransferTargetName {
			continue
		}
		if kv.Key == semconv.GenAIToolName {
			if tool, ok := kv.Value.GetValue().(*commonpb.AnyValue_StringValue); ok {
				if m := mcpTool.FindStringSubmatch(tool.StringValue); m != nil {
					old := tool.StringValue
					tool.StringValue = "mcp__" + m[1]
					if sp.Name == semconv.SpanName(semconv.OperationExecuteTool, old) {
						sp.Name = semconv.SpanName(semconv.OperationExecuteTool, tool.StringValue)
					}
				}
			}
		}
		out = append(out, kv)
	}
	sp.Attributes = out
}
