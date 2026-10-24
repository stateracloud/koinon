// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// The prober (plan item MC1). Every MCP probe interval it connects to the gateway with each served
// server's probe key, as an agent would, runs initialize and tools/list, and records the result:
//
//   - tools came back: the server is healthy, and the list is synced into mcp_tools (new tools enter
//     the ask tier, changed ones are flagged for review);
//   - nothing came back, or the gateway refused: a failure. Three in a row make the server unhealthy
//     with a reason (its tools drop out of every agent's grants on the next pass); one success makes
//     it healthy again.
//
// The gateway runs with failureMode failOpen, so a target it cannot reach is skipped without an error
// and its tools are simply missing from the list; "no tools" is therefore the failure the prober
// sees. Before the failure that makes a server unhealthy is recorded, the prober asks the server
// directly (mcpservers.Diagnose) why, so the Servers page can say "connection refused" or "the server
// rejected the credential" instead of guessing.

// mcpProbe is one server the prober lists.
type mcpProbe struct {
	org        pgtype.UUID
	orgSlug    string
	server     pgtype.UUID
	name       string
	target     string
	material   string
	row        store.McpServer
	credential string
}

const (
	mcpProbeTimeout    = 20 * time.Second
	mcpProbeParallel   = 8
	mcpMaxToolPages    = 20
	reasonNoTools      = "the gateway got no tools from the server: it is unreachable, rejected the credential or lists none"
	reasonGatewayError = "the gateway refused or failed the probe"
)

// RunProber probes until ctx ends. It is started by Run and only has work on the replica that
// leads.
func (r *Reconciler) RunProber(ctx context.Context) {
	t := time.NewTicker(r.cfg.MCPProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every served server once.
func (r *Reconciler) ProbeAll(ctx context.Context) {
	r.mu.Lock()
	probes := slices.Clone(r.probes)
	r.mu.Unlock()
	sem := make(chan struct{}, mcpProbeParallel)
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.probeOne(ctx, p)
		}()
	}
	wg.Wait()
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

// listTools lists the server's tools through the gateway, with the gateway's prefix removed.
func (r *Reconciler) listTools(ctx context.Context, p mcpProbe) ([]mcpservers.ListedTool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-mcp-prober", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             fmt.Sprintf("http://%s:%d/mcp", r.cfg.Gateway.Host, r.cfg.Gateway.Port),
		HTTPClient:           &http.Client{Transport: bearerTransport{key: p.material, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var out []mcpservers.ListedTool
	prefix := p.target + "_"
	var cursor string
	for range mcpMaxToolPages {
		res, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		for _, t := range res.Tools {
			lt := mcpservers.ListedTool{Name: strings.TrimPrefix(t.Name, prefix), Description: t.Description}
			if raw, err := jsonOf(t.InputSchema); err == nil {
				lt.InputSchema = raw
			}
			if t.Annotations != nil {
				lt.ReadOnlyHint = t.Annotations.ReadOnlyHint
			}
			out = append(out, lt)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

func jsonOf(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return b, err
}

func (r *Reconciler) probeOne(ctx context.Context, p mcpProbe) {
	ctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout)
	defer cancel()
	log := r.log.With("org", p.orgSlug, "server", p.name)
	tools, err := r.listTools(ctx, p)
	reason := ""
	switch {
	case err != nil:
		log.Debug("MCP probe failed", "err", err)
		reason = reasonGatewayError
	case len(tools) == 0:
		reason = reasonNoTools
	}
	if reason != "" {
		cur, err := r.serverByID(ctx, p.org, p.server)
		if errors.Is(err, errNoServer) {
			return
		}
		if err == nil && int(cur.ConsecutiveFailures)+1 >= mcpservers.FailureThreshold {
			d := r.cfg.DiagnoseMCP(ctx, r.cfg.URLPolicy, mcpservers.PreflightRequest{
				URL: p.row.Url, Transport: string(p.row.Transport), Header: p.row.CredentialHeader.String,
				Prefix: p.row.CredentialPrefix, Value: p.credential,
			})
			if d.Reason != "" {
				reason = d.Reason
			}
		}
		if _, changed, err := r.mcp.RecordProbe(ctx, p.org, p.server, false, reason, false); err != nil {
			log.Warn("recording an MCP probe failed", "err", err)
		} else if changed {
			log.Warn("MCP server is unhealthy; its tools are withdrawn from agents", "reason", reason)
		}
		return
	}
	res, err := r.mcp.SyncTools(ctx, p.org, p.server, tools)
	if err != nil {
		log.Warn("syncing MCP tools failed", "err", err)
		return
	}
	if len(res.New)+len(res.Changed) > 0 {
		log.Info("MCP tools synced", "new", len(res.New), "changed", len(res.Changed), "absent", len(res.Absent))
	}
	if _, changed, err := r.mcp.RecordProbe(ctx, p.org, p.server, true, "", true); err != nil {
		log.Warn("recording an MCP probe failed", "err", err)
	} else if changed {
		log.Info("MCP server is healthy")
	}
}
