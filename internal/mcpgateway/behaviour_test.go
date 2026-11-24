// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// mcpDriver lets the behaviour suite drive the MCP path alone: it applies the MCP half of a State.
type mcpDriver struct{ gw *mcpgateway.Gateway }

func (d mcpDriver) IssueKey(_ context.Context, a gateway.AgentRef) (gateway.IssuedKey, error) {
	return gateway.NewKey(a)
}

// Usage has nothing to read: the MCP path records no model usage.
func (d mcpDriver) Usage() gateway.UsageReader { return gateway.NoUsage{} }

func (d mcpDriver) Apply(ctx context.Context, s gateway.State) (gateway.Revision, error) {
	return d.gw.ApplyMCP(ctx, s.MCPConfig())
}

// The MCP cases of the gateway behaviour suite (internal/gateway/gatewaytest), run against the
// in-process MCP path as koinon-gateway serves it. The model path is not served here (issue 279).
var _ = func() bool {
	dir, err := os.MkdirTemp("", "koinon-mcpgateway-behaviour-")
	if err != nil {
		panic(err)
	}
	gatewaytest.RegisterMCP(gatewaytest.Env{
		Name:    "koinon-gateway",
		Host:    "127.0.0.1",
		KeyFile: func(name string) string { return filepath.Join(dir, name) },
		// The gateway scrubs the credentials it opened from what agents get (#1174).
		ScrubsCredentials: true,
		Start: func(_ context.Context, _ []int, files map[string]string, guard gatewaytest.Guard) (*gatewaytest.Gateway, error) {
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					return nil, err
				}
			}
			// The guard runs in the process, as in koinon-gateway (ADR 0097 decision 5). No guard is
			// AllowGuard, which serves no org target.
			var logs syncBuffer
			gw := mcpgateway.New(mcpgateway.Options{
				Guard:  guard.Local,
				Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			mux := http.NewServeMux()
			mux.Handle("/mcp", gw.Handler())
			srv := httptest.NewServer(mux)
			return &gatewaytest.Gateway{
				Driver: mcpDriver{gw: gw},
				URL:    srv.URL,
				Logs:   func(context.Context) string { return logs.String() },
				Stop: func(context.Context) error {
					gw.Drain()
					srv.CloseClientConnections()
					srv.Close()
					return nil
				},
			}, nil
		},
	})
	return true
}()
