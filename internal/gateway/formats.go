// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"regexp"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// Header is one request header the gateway sets on the way to a model's upstream. The value is a
// literal, replacing whatever the client sent.
type Header struct{ Name, Value string }

// BodyField is one request body field the gateway sets from a CEL expression (llmRequest is the
// parsed request), replacing the client's value.
type BodyField struct{ Field, Expression string }

// AnthropicVersion is the Messages API version the gateway pins on every anthropic-format request,
// the version OpenCode itself sends (ADR 0069 decision 3). agentgateway v1.7.0 forwards the client's
// version unless a header set pins it.
const AnthropicVersion = "2023-06-01"

// StreamUsageExpr makes every streamed chat completion ask the upstream for usage. agentgateway
// v1.7.0 adds stream_options.include_usage itself only when the client sent no stream_options at all
// (llm/mod.rs, process_completions_request), so a client sending {"include_usage": false} or an empty
// object would otherwise get its streamed requests charged without usage. For a request that is not
// streamed the expression is null, which OpenAI-compatible upstreams accept and which drops the field
// (stream_options is an error without stream).
const StreamUsageExpr = `has(llmRequest.stream) && llmRequest.stream == true ? {"include_usage": true} : null`

// WorkspaceIDPattern is the rule for Provider.AnthropicWorkspaceID (ADR 0069 decision 5).
const WorkspaceIDPattern = `^wrkspc_[A-Za-z0-9]+$`

var workspaceID = regexp.MustCompile(WorkspaceIDPattern)

// Rendering is how both drivers render a model of one format (ADR 0069 decision 3). Every model is a
// `custom` provider whatever its format; the format only picks what is in this row.
type Rendering struct {
	// Standalone and Kubernetes name the agentgateway format in the local config file
	// (`completions`) and in the AgentgatewayModel CRD (`Completions`).
	Standalone, Kubernetes string
	// Headers are the request headers set on the model's upstream request. p is the model's provider.
	Headers func(p Provider) []Header
	// Body are the request body fields the gateway sets.
	Body []BodyField
}

// renderings is the one table: a new format is one row, rendered by both drivers.
var renderings = map[modelformat.Format]Rendering{
	modelformat.OpenAI: {
		Standalone: "completions",
		Kubernetes: "Completions",
		Body:       []BodyField{{Field: "stream_options", Expression: StreamUsageExpr}},
	},
	// Messages streams carry usage in message_start and message_delta, so there is no stream_options.
	modelformat.Anthropic: {
		Standalone: "messages",
		Kubernetes: "Messages",
		Headers: func(p Provider) []Header {
			h := []Header{{Name: "anthropic-version", Value: AnthropicVersion}}
			if p.AnthropicWorkspaceID != "" {
				h = append(h, Header{Name: "anthropic-workspace-id", Value: p.AnthropicWorkspaceID})
			}
			return h
		},
	},
}

// RenderingOf returns the rendering of m's format. An unset format is OpenAI.
func RenderingOf(m Model) (Rendering, error) {
	f := m.Format
	if f == "" {
		f = modelformat.OpenAI
	}
	r, ok := renderings[f]
	if !ok {
		return Rendering{}, fmt.Errorf("gateway: no rendering for model format %q", f)
	}
	return r, nil
}

// RequestHeaders returns the headers to set on the upstream request of a model of p.
func (r Rendering) RequestHeaders(p Provider) []Header {
	if r.Headers == nil {
		return nil
	}
	return r.Headers(p)
}
