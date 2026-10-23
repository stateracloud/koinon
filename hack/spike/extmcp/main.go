// SPDX-License-Identifier: Apache-2.0

// Command extmcp is throwaway spike code for step 6.5 of docs/spikes/openshell.md: an
// agentgateway ExtMCP processor (proto/ext_mcp.proto, copied from agentgateway v1.6.0)
// that judges tools/call arguments, which neither OpenShell's MCP rules nor CEL tool
// rules can see. It only allows create_pull_request into base "main", and a title of
// "slow:<seconds>" makes it sleep, to observe the gateway's processor timeout.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/stateracloud/koinon/hack/spike/extmcp/gen"
)

type server struct {
	gen.UnimplementedExtMcpServer
	log *slog.Logger
}

func (s *server) CheckRequest(_ context.Context, r *gen.McpRequest) (*gen.McpRequestResult, error) {
	start := time.Now()
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Base  string `json:"base"`
			Title string `json:"title"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(r.GetMcpRequest(), &call)

	if secs, ok := strings.CutPrefix(call.Arguments.Title, "slow:"); ok {
		n, _ := strconv.Atoi(secs)
		time.Sleep(time.Duration(n) * time.Second)
	}

	result := &gen.McpRequestResult{Result: &gen.McpRequestResult_Pass{Pass: &gen.Pass{}}}
	verdict := "pass"
	if r.GetMethod() == "tools/call" && call.Name == "create_pull_request" && call.Arguments.Base != "main" {
		verdict = "deny"
		result.Result = &gen.McpRequestResult_Error{Error: &gen.AuthorizationError{
			Code:   gen.AuthorizationError_PERMISSION_DENIED,
			Reason: "pull requests may only target main",
		}}
	}
	s.log.Info("check request", "method", r.GetMethod(), "services", r.GetServiceNames(), "tool", call.Name,
		"base", call.Arguments.Base, "verdict", verdict, "elapsed_ms", time.Since(start).Milliseconds())
	return result, nil
}

func (s *server) CheckResponse(_ context.Context, r *gen.McpResponse) (*gen.McpResponseResult, error) {
	s.log.Info("check response", "method", r.GetMethod())
	return &gen.McpResponseResult{Result: &gen.McpResponseResult_Pass{Pass: &gen.Pass{}}}, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	g := grpc.NewServer()
	gen.RegisterExtMcpServer(g, &server{log: log})
	log.Info("serving ExtMcp on :9000")
	if err := g.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
