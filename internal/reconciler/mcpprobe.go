// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
	"github.com/stateracloud/koinon/internal/safego"
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
// sees. The reason is only what the gateway returned: the credential never leaves the gateway, so the
// prober does not ask the server itself.

const (
	mcpProbeTimeout    = 20 * time.Second
	mcpProbeParallel   = 8
	mcpMaxToolPages    = 20
	reasonNoTools      = "the gateway got no tools from the server: it is unreachable, rejected the credential or lists none (the gateway does not say which)"
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
			safego.Do(r.log, "mcp-probe", func() { r.ProbeAll(ctx) })
		}
	}
}

// ProbeAll probes every served server once.
func (r *Reconciler) ProbeAll(ctx context.Context) {
	probes := r.plan.Probes()
	sem := make(chan struct{}, mcpProbeParallel)
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		sem <- struct{}{}
		safego.Go(ctx, r.log, "mcp-probe", func(ctx context.Context) {
			defer wg.Done()
			defer func() { <-sem }()
			r.probeOne(ctx, p)
		})
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
func (r *Reconciler) listTools(ctx context.Context, p gatewayplan.Probe) ([]mcpservers.ListedTool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-mcp-prober", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             fmt.Sprintf("http://%s:%d/mcp", r.cfg.Gateway.Host, r.cfg.Gateway.Port),
		HTTPClient:           &http.Client{Transport: bearerTransport{key: p.Material, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var out []mcpservers.ListedTool
	prefix := p.Target + "_"
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

func (r *Reconciler) probeOne(ctx context.Context, p gatewayplan.Probe) {
	ctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout)
	defer cancel()
	log := r.log.With("org", p.OrgSlug, "server", p.Name)
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
		// Only what the gateway returned: the server's credential never leaves the gateway, so the
		// prober cannot ask the server itself why.
		if _, changed, err := r.mcp.RecordProbe(ctx, p.Org, p.Server, false, reason, false); errors.Is(err, mcpservers.ErrNotFound) {
			return
		} else if err != nil {
			log.Warn("recording an MCP probe failed", "err", err)
		} else if changed {
			log.Warn("MCP server is unhealthy; its tools are withdrawn from agents", "reason", reason)
		}
		return
	}
	res, err := r.mcp.SyncTools(ctx, p.Org, p.Server, tools)
	if err != nil {
		log.Warn("syncing MCP tools failed", "err", err)
		return
	}
	if len(res.New)+len(res.Changed) > 0 {
		log.Info("MCP tools synced", "new", len(res.New), "changed", len(res.Changed), "absent", len(res.Absent))
	}
	if _, changed, err := r.mcp.RecordProbe(ctx, p.Org, p.Server, true, "", true); err != nil {
		log.Warn("recording an MCP probe failed", "err", err)
	} else if changed {
		log.Info("MCP server is healthy")
	}
}
