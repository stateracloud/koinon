// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// MCPToolLister names the tools agentgateway offers over /mcp, the way a session's harness sees them:
// each tool as "<gateway target>_<tool>". agentd asks it when a run starts (issue #326), so a server
// the gateway dropped (it runs failOpen, so an unreachable target is skipped without an error) is
// found at the moment the session would have missed it, not a probe interval later.
type MCPToolLister interface {
	ListTools(ctx context.Context) ([]string, error)
}

const (
	mcpListTimeout   = 20 * time.Second
	mcpListMaxPages  = 20
	mcpListClientTag = "koinon-agentd-mcp-check"
)

// GatewayMCPLister is the MCPToolLister that runs initialize and tools/list through the gateway's
// /mcp, the call the reconciler's prober makes (reconciler/mcpprobe.go). The key is read from a file
// on each call, so a rotated Secret is picked up; it must open the tools of every server the agents
// may use (the gateway's grants decide), and it never leaves agentd.
type GatewayMCPLister struct {
	// URL is the gateway's base URL; /mcp is appended.
	URL string
	// KeyFile holds the gateway key the listing authenticates with.
	KeyFile string
}

type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(req)
}

// ListTools implements MCPToolLister.
func (l GatewayMCPLister) ListTools(ctx context.Context) ([]string, error) {
	raw, err := os.ReadFile(l.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the MCP check key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("the MCP check key file %s is empty", l.KeyFile)
	}
	ctx, cancel := context.WithTimeout(ctx, mcpListTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: mcpListClientTag, Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             strings.TrimSuffix(l.URL, "/") + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{key: key, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var names []string
	var cursor string
	for range mcpListMaxPages {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		for _, t := range res.Tools {
			names = append(names, t.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return names, nil
}

// gatewayMissingMCPServers is the direct check: the servers the agent may use, not disabled or
// removed and probed at least once, that have no tool in what the gateway offers. A server the
// reconciler has not yet applied to the gateway (never probed) is not a miss. The error is the
// listing's, for the caller to fall back to the health record.
func (r *runner) gatewayMissingMCPServers(ctx context.Context, lister MCPToolLister) ([]missingMCPServer, error) {
	rows, err := r.q.ListAgentMCPServersOfAgent(ctx, store.ListAgentMCPServersOfAgentParams{
		OrgID: r.run.OrgID, AgentID: r.run.AgentID,
	})
	if err != nil {
		return nil, err
	}
	var allowed []store.McpServer
	for _, a := range rows {
		s, err := r.q.GetMCPServer(ctx, store.GetMCPServerParams{OrgID: r.run.OrgID, ID: a.ServerID})
		if err != nil {
			return nil, err
		}
		if s.DisabledReason.Valid || s.RemovedAt.Valid || s.Health == store.McpHealthUnknown {
			continue
		}
		allowed = append(allowed, s)
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	slug, err := r.q.GetOrgSlug(ctx, r.run.OrgID)
	if err != nil {
		return nil, err
	}
	tools, err := lister.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	var out []missingMCPServer
	for _, s := range allowed {
		prefix := mcpservers.TargetName(slug, s.Name) + "_"
		served := false
		for _, t := range tools {
			if strings.HasPrefix(t, prefix) {
				served = true
				break
			}
		}
		if !served {
			out = append(out, missingMCPServer{Name: s.Name, Reason: "the gateway offers no tools from it"})
		}
	}
	return out, nil
}
