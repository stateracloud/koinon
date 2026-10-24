// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/catalog"
)

// MaxResultBytes bounds the result of an approved call that is stored and shown to the agent.
const MaxResultBytes = 64 << 10

// CallRequest is one approved tool call to a remote server.
type CallRequest struct {
	URL       string
	Transport string
	Header    string
	Prefix    string
	// Value is the credential's secret.
	Value string
	Tool  string
	// Arguments is the call's JSON object.
	Arguments json.RawMessage
	// Idempotency is passed to the server as _meta "dev.koinon/approval", so a server that dedupes
	// can treat a retry of the same approved call as one.
	Idempotency string
}

// ErrPermanent wraps a failure that retrying the same call will not fix: the server answered with
// a protocol error such as an unknown tool or invalid arguments.
var ErrPermanent = errors.New("the server refused the call")

type headerTransport struct {
	header, value string
	base          http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if h.header != "" {
		r.Header.Set(h.header, h.value)
	}
	return h.base.RoundTrip(r)
}

// CallTool calls a tool on a remote server directly from this process, for Koinon to execute an
// approved ask-tier call (ADR 0042). It dials only addresses the egress policy allows, follows no
// redirects and sends the credential to the server's URL alone. The result is the call's content
// (bounded by MaxResultBytes) and whether the tool reported an error; an error the tool reports is
// a result, not a failure of the call.
func CallTool(ctx context.Context, policy catalog.URLPolicy, c CallRequest) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var args map[string]any
	if len(c.Arguments) > 0 {
		if err := json.Unmarshal(c.Arguments, &args); err != nil {
			return nil, fmt.Errorf("%w: the recorded arguments are not a JSON object", ErrPermanent)
		}
	}
	httpClient := &http.Client{
		Transport: headerTransport{header: c.Header, value: c.Prefix + c.Value, base: &http.Transport{
			DialContext:           policy.DialContext(&net.Dialer{Timeout: 10 * time.Second}),
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var transport mcp.Transport
	if c.Transport == TransportSSE {
		transport = &mcp.SSEClientTransport{Endpoint: c.URL, HTTPClient: httpClient}
	} else {
		transport = &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon", Version: "0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the server: %w", err)
	}
	defer func() { _ = session.Close() }()
	params := &mcp.CallToolParams{Name: c.Tool, Arguments: args}
	if c.Idempotency != "" {
		params.Meta = mcp.Meta{"dev.koinon/approval": c.Idempotency}
	}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		var werr *jsonrpc.Error
		if errors.As(err, &werr) {
			return nil, fmt.Errorf("%w: %s", ErrPermanent, werr.Message)
		}
		return nil, fmt.Errorf("call the tool: %w", err)
	}
	out := map[string]any{"isError": res.IsError}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable result", ErrPermanent)
	}
	if len(raw) > MaxResultBytes {
		out["truncated"] = true
		out["content"] = []map[string]string{{"type": "text", "text": fmt.Sprintf("The result is %d bytes; Koinon keeps only the first %d.", len(raw), MaxResultBytes/2)}, {"type": "text", "text": string(raw[:MaxResultBytes/2])}}
	} else {
		var content any
		_ = json.Unmarshal(raw, &content)
		out["content"] = content
	}
	if res.StructuredContent != nil {
		if sc, err := json.Marshal(res.StructuredContent); err == nil && len(sc) <= MaxResultBytes {
			var v any
			_ = json.Unmarshal(sc, &v)
			out["structuredContent"] = v
		}
	}
	return out, nil
}
