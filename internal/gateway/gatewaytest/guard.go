// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"net"
	"strconv"
	"sync"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above
	"google.golang.org/grpc"

	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
)

// FakeGuard stands in for koinon-api's MCP leak guard (internal/mcpguard, ADR 0091): an ExtMcp
// server on the test host that passes every call, except the tools a spec tells it to refuse or
// whose result it withholds, and records what the gateway sent it.
type FakeGuard struct {
	extmcp.UnimplementedExtMcpServer

	srv  *grpc.Server
	port int

	mu        sync.Mutex
	refuse    map[string]string // tool (as the gateway names it) -> reason, before the call
	withhold  map[string]string // tool -> reason, on the result
	requests  []GuardCall
	responses []GuardCall
}

// GuardCall is one check the gateway asked the guard for.
type GuardCall struct {
	Targets []string
	Method  string
	// Agent and Tool are the processor's metadata (gateway.GuardMetadata).
	Agent, Tool string
	// Headers are the names of the request headers the gateway forwarded.
	Headers []string
	// Body is the call's params (request) or its result (response), as JSON.
	Body string
}

// NewFakeGuard starts a guard on every interface, so a gateway in a container or a cluster reaches
// it at Env.Host.
func NewFakeGuard() (*FakeGuard, error) {
	l, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec,noctx // the kind cluster reaches the test host over the network
	if err != nil {
		return nil, err
	}
	g := &FakeGuard{srv: grpc.NewServer(), refuse: map[string]string{}, withhold: map[string]string{}}
	_, p, _ := net.SplitHostPort(l.Addr().String())
	g.port, _ = strconv.Atoi(p)
	extmcp.RegisterExtMcpServer(g.srv, g)
	go func() { _ = g.srv.Serve(l) }()
	return g, nil
}

// startGuard starts a FakeGuard for the current container and stops it at its end.
func startGuard() *FakeGuard {
	GinkgoHelper()
	g, err := NewFakeGuard()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(g.Stop)
	return g
}

// Port is the guard's port on the test host.
func (g *FakeGuard) Port() int { return g.port }

// URL is the guard's address for Options.GuardURL, as a gateway reaches it on host.
func (g *FakeGuard) URL(host string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(g.port))
}

// Stop takes the guard down, as when koinon-api is not running.
func (g *FakeGuard) Stop() { g.srv.Stop() }

// Refuse makes the guard refuse a call to tool before it reaches the server.
func (g *FakeGuard) Refuse(tool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse[tool] = reason
}

// Withhold makes the guard withhold the result of a call to tool.
func (g *FakeGuard) Withhold(tool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.withhold[tool] = reason
}

// Requests and Responses are the checks so far.
func (g *FakeGuard) Requests() []GuardCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GuardCall(nil), g.requests...)
}

// Responses: see Requests.
func (g *FakeGuard) Responses() []GuardCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GuardCall(nil), g.responses...)
}

func metaField(m interface{ AsMap() map[string]any }, key string) string {
	s, _ := m.AsMap()[key].(string)
	return s
}

// CheckRequest implements extmcp.ExtMcpServer.
func (g *FakeGuard) CheckRequest(_ context.Context, req *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	c := GuardCall{Targets: req.GetServiceNames(), Method: req.GetMethod(), Body: string(req.GetMcpRequest())}
	if m := req.GetMetadataContext(); m != nil {
		c.Agent, c.Tool = metaField(m, "agent"), metaField(m, "tool")
	}
	for _, h := range req.GetHeaders() {
		c.Headers = append(c.Headers, h.GetKey())
	}
	g.mu.Lock()
	g.requests = append(g.requests, c)
	reason, refused := g.refuse[c.Tool]
	g.mu.Unlock()
	if refused {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: reason,
		}}}, nil
	}
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: &extmcp.Pass{}}}, nil
}

// CheckResponse implements extmcp.ExtMcpServer.
func (g *FakeGuard) CheckResponse(_ context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	c := GuardCall{Targets: resp.GetServiceNames(), Method: resp.GetMethod(), Body: string(resp.GetMcpResponse())}
	if m := resp.GetMetadataContext(); m != nil {
		c.Agent, c.Tool = metaField(m, "agent"), metaField(m, "tool")
	}
	g.mu.Lock()
	g.responses = append(g.responses, c)
	reason, withheld := g.withhold[c.Tool]
	g.mu.Unlock()
	if withheld {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: reason,
		}}}, nil
	}
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}}, nil
}
