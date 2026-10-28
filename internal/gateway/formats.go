// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"slices"
	"strings"

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

// serverToolsExpr keeps only the tools the agent's harness runs itself: the ones without a type or of
// type "custom". Everything else is a tool Anthropic runs on its side (web_search, web_fetch,
// code_execution, the memory and tool-search tools, MCP toolsets, and any type added later), or a
// versioned client tool (bash, text_editor, computer) that Koinon's harnesses do not send. An allow
// rule fails closed for new server tools.
//
// The body field is set per model, so it also reaches a chat-completions request sent to an
// anthropic-format model, whose tools are all `type: "function"`. The filter applies to Messages
// requests only; any other path keeps its tools.
const serverToolsExpr = `has(llmRequest.tools) ? (request.path == "/v1/messages" ? llmRequest.tools.filter(t, !has(t.type) || t.type == "custom") : llmRequest.tools) : null`

// Rendering is how both drivers render a model of one format (ADR 0069 decision 3). Every model is a
// `custom` provider whatever its format; the format only picks what is in this row.
type Rendering struct {
	// Standalone and Kubernetes name the agentgateway format in the local config file
	// (`completions`) and in the AgentgatewayModel CRD (`Completions`).
	Standalone, Kubernetes string
	// Path is the route the format is served on. The sandbox profile allows POST on exactly the
	// paths of the table.
	Path string
	// Headers are the request headers set on the model's upstream request. p is the model's provider.
	Headers func(p Provider) []Header
	// Remove are the client-supplied request headers dropped before the request leaves the gateway.
	// A header that Headers sets needs no entry: setting replaces the client's value.
	Remove func(p Provider) []string
	// Body are the request body fields the gateway sets; the expression null drops a field the
	// request format types, but leaves an explicit null in one it does not.
	Body []BodyField
	// Reject are body fields whose presence refuses the request (403): fields the gateway cannot
	// remove, because a transformation would add them to every request as null.
	Reject []string
}

// renderings is the one table: a new format is one row, rendered by both drivers.
var renderings = map[modelformat.Format]Rendering{
	modelformat.OpenAI: {
		Standalone: "completions",
		Kubernetes: "Completions",
		Path:       "/v1/chat/completions",
		Body:       []BodyField{{Field: "stream_options", Expression: StreamUsageExpr}},
	},
	// Messages streams carry usage in message_start and message_delta, so there is no stream_options.
	//
	// An agent is untrusted, and the Messages API lets a request reach beyond the gateway and the org:
	// anthropic-workspace-id picks the workspace an unscoped key acts in (possibly another tenant's),
	// anthropic-beta enables the MCP connector (Anthropic itself calls the URL, which bypasses the
	// sandbox's egress rules), code execution, web fetch and long context, and mcp_servers and the
	// server tools use those features. The gateway sets the version and the provider's workspace,
	// drops the beta header and the workspace header of a provider without one, and removes the
	// body fields.
	modelformat.Anthropic: {
		Standalone: "messages",
		Kubernetes: "Messages",
		Path:       "/v1/messages",
		Headers: func(p Provider) []Header {
			h := []Header{{Name: "anthropic-version", Value: AnthropicVersion}}
			if p.AnthropicWorkspaceID != "" {
				h = append(h, Header{Name: "anthropic-workspace-id", Value: p.AnthropicWorkspaceID})
			}
			return h
		},
		Remove: func(p Provider) []string {
			r := []string{"anthropic-beta"}
			if p.AnthropicWorkspaceID == "" {
				r = append(r, "anthropic-workspace-id")
			}
			return r
		},
		Body:   []BodyField{{Field: "tools", Expression: serverToolsExpr}},
		Reject: []string{"mcp_servers"},
	},
}

// RoutePaths returns the path of every format's route, sorted: the paths a sandbox may POST to.
func RoutePaths() []string {
	var out []string
	for _, r := range renderings {
		out = append(out, r.Path)
	}
	slices.Sort(out)
	return out
}

// RenderingOf returns the rendering of m's format; an unset or unknown format is an error.
func RenderingOf(m Model) (Rendering, error) {
	r, ok := renderings[m.Format]
	if !ok {
		return Rendering{}, fmt.Errorf("gateway: no rendering for model format %q", m.Format)
	}
	return r, nil
}

// RemoveHeaders returns the client headers to drop on the upstream request of a model of p.
func (r Rendering) RemoveHeaders(p Provider) []string {
	if r.Remove == nil {
		return nil
	}
	return r.Remove(p)
}

// RequestHeaders returns the headers to set on the upstream request of a model of p.
func (r Rendering) RequestHeaders(p Provider) []Header {
	if r.Headers == nil {
		return nil
	}
	return r.Headers(p)
}

// FormatLogExpr is the CEL expression of the access-log field LogFieldFormat: the format whose route
// the request used, from the table's paths.
func FormatLogExpr() string {
	expr := `""`
	for _, f := range modelformat.All() {
		if r, ok := renderings[f]; ok {
			expr = fmt.Sprintf(`request.path == %q ? %q : %s`, r.Path, string(f), expr)
		}
	}
	return expr
}

// RejectExpr is the CEL condition under which a request is admitted as far as body fields go: none of
// the fields any format rejects is present. "" when no format rejects a field.
func RejectExpr() string {
	var parts []string
	for _, f := range modelformat.All() {
		for _, field := range renderings[f].Reject {
			parts = append(parts, "!has(llmRequest."+field+")")
		}
	}
	return strings.Join(parts, " && ")
}
