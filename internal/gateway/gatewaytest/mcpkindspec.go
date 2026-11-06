// SPDX-License-Identifier: Apache-2.0

// This file drives MCP logging and sampling on purpose: the gateway must guard those
// server-to-client channels even though go-sdk marks them deprecated (SEP-2577, still on the wire).
//
//nolint:staticcheck // SA1019: the deprecated features are the thing under test
package gatewaytest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// The canaries the fake server puts in each kind of message it sends.
const (
	canaryResult   = "CANARY-TOOL-RESULT"
	canaryDesc     = "CANARY-TOOL-DESCRIPTION"
	canaryRPCError = "CANARY-RPC-ERROR"
	canaryLog      = "CANARY-LOG-NOTIFICATION"
	canarySampling = "CANARY-SAMPLING-REQUEST"
	canaryElicit   = "CANARY-ELICITATION-REQUEST"
	canaryResource = "CANARY-RESOURCE"
	canaryPrompt   = "CANARY-PROMPT"
)

// canaryServer is an MCP server that says something untrusted in every kind of message a server
// can send an agent: a tool result, a tool description, a JSON-RPC error reply, a log
// notification, server-initiated sampling and elicitation requests, a resource and a prompt.
func canaryServer(log *headerLog) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "canary", Version: "0.0.1"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}},
	})
	type args struct{}
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: canaryDesc},
		func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
			return text(canaryResult), nil, nil
		})
	// A raw ToolHandler's error is a JSON-RPC error reply, not an IsError result.
	srv.AddTool(&mcp.Tool{Name: "rpc_error", Description: "fails", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New(canaryRPCError)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "log", Description: "logs"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
			_ = req.Session.Log(ctx, &mcp.LoggingMessageParams{Level: "error", Data: canaryLog})
			return text("logged"), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "sample", Description: "samples"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
			_, _ = req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{
				MaxTokens: 8, Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: canarySampling}}},
			})
			return text("sampled"), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "elicit", Description: "elicits"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
			_, _ = req.Session.Elicit(ctx, &mcp.ElicitParams{
				Mode: "form", Message: canaryElicit, RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			})
			return text("elicited"), nil, nil
		})
	srv.AddResource(&mcp.Resource{URI: "canary://r", Name: "r", MIMEType: "text/plain"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "canary://r", MIMEType: "text/plain", Text: canaryResource}}}, nil
		})
	srv.AddPrompt(&mcp.Prompt{Name: "canary_prompt", Description: "prompt"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: canaryPrompt}}}}, nil
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.Header)
		inner.ServeHTTP(w, r)
	}))
}

// mcpKindSpec is the guard coverage of ADR 0097 decision 4: for each kind of MCP message a server
// can send an agent, whether the leak guard saw it (#254). The property is Koinon's, not the
// gateway's: untrusted text from a server reaches the agent only after the guard saw it, because
// the guard's verdict is what sets the session's untrusted-input flag (ADR 0091). A kind the
// gateway refuses, or does not deliver at all, satisfies it too.
//
// A kind that reaches the agent unseen is an expected failure naming #254 on a driver that lists
// it in Env.KnownGaps. Koinon's fail-closed rule for it, with the real leak guard, is not part of
// this suite (it uses a fake guard); see the follow-up on #272.
func mcpKindSpec(env Env) {
	const target = "acme-canary"
	agent := gateway.AgentRef{Org: "acme", Name: "canary-reader"}

	type kind struct {
		name   string
		canary string
		// gap is the issue that tracks a kind the gateway does not show the guard ("" when it does).
		gap string
		// trigger makes the server send the kind; whatever the agent reads goes to read.
		trigger func(ctx context.Context, s *mcp.ClientSession, read func(string)) error
	}
	callTool := func(name string) func(context.Context, *mcp.ClientSession, func(string)) error {
		return func(ctx context.Context, s *mcp.ClientSession, read func(string)) error {
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: target + "_" + name})
			if err != nil {
				read(err.Error())
				return err
			}
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					read(t.Text)
				}
			}
			return nil
		}
	}
	kinds := []kind{
		{name: "a tools/call reply", canary: canaryResult, trigger: callTool("echo")},
		{name: "a tools/list reply (a tool description)", canary: canaryDesc, gap: "#254", trigger: func(ctx context.Context, s *mcp.ClientSession, read func(string)) error {
			res, err := s.ListTools(ctx, nil)
			if err != nil {
				return err
			}
			for _, t := range res.Tools {
				read(t.Description)
			}
			return nil
		}},
		{name: "a JSON-RPC error reply", canary: canaryRPCError, gap: "#254", trigger: callTool("rpc_error")},
		{name: "a log notification", canary: canaryLog, gap: "#254", trigger: func(ctx context.Context, s *mcp.ClientSession, read func(string)) error {
			_ = s.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "debug"})
			return callTool("log")(ctx, s, read)
		}},
		{name: "a server-initiated sampling request", canary: canarySampling, gap: "#254", trigger: callTool("sample")},
		{name: "a server-initiated elicitation request", canary: canaryElicit, trigger: callTool("elicit")},
		{name: "a resources/read reply", canary: canaryResource, trigger: func(ctx context.Context, s *mcp.ClientSession, read func(string)) error {
			res, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "canary://r"})
			if err != nil {
				return err
			}
			for _, c := range res.Contents {
				read(c.Text)
			}
			return nil
		}},
		{name: "a prompts/get reply", canary: canaryPrompt, trigger: func(ctx context.Context, s *mcp.ClientSession, read func(string)) error {
			res, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: target + "_canary_prompt"})
			if err != nil {
				return err
			}
			for _, m := range res.Messages {
				if t, ok := m.Content.(*mcp.TextContent); ok {
					read(t.Text)
				}
			}
			return nil
		}},
	}

	Describe(env.Name+": guard coverage per MCP message kind (ADR 0097; koinon #254)", Ordered, ContinueOnFailure, func() {
		var (
			url, key string
			logs     func(context.Context) string
			guard    *FakeGuard
		)
		BeforeAll(func(ctx SpecContext) {
			srv := canaryServer(&headerLog{})
			DeferCleanup(func() { srv.CloseClientConnections(); go srv.Close() })
			guard = startGuard()
			k, err := gateway.NewKey(agent)
			Expect(err).NotTo(HaveOccurred())
			key = k.Material
			state := gateway.State{MCP: []gateway.MCPTarget{{Name: target, Org: "acme", URL: "http://" + env.Host + ":" + itoa(port(srv)) + "/mcp"}}}
			state.AddKey(k.Key)
			state.GrantTools(agent, gateway.ToolGrant{Target: target, Tools: []string{"echo", "rpc_error", "log", "sample", "elicit"}})
			gw, err := env.Start(ctx, []int{port(srv), guard.Port()}, nil, guard.URL(env.Host))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			url, logs = gw.URL, gw.Logs
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		for _, k := range kinds {
			It("shows the guard "+k.name+" before the agent reads it", func(ctx SpecContext) {
				var mu sync.Mutex
				var read strings.Builder
				add := func(s string) {
					mu.Lock()
					defer mu.Unlock()
					read.WriteString(s + "\n")
				}
				client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, &mcp.ClientOptions{
					LoggingMessageHandler: func(_ context.Context, r *mcp.LoggingMessageRequest) {
						if s, ok := r.Params.Data.(string); ok {
							add(s)
						}
					},
					CreateMessageHandler: func(_ context.Context, r *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
						for _, m := range r.Params.Messages {
							if t, ok := m.Content.(*mcp.TextContent); ok {
								add(t.Text)
							}
						}
						return &mcp.CreateMessageResult{Role: "assistant", Model: "none", Content: &mcp.TextContent{Text: "no"}}, nil
					},
					ElicitationHandler: func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
						add(r.Params.Message)
						return &mcp.ElicitResult{Action: "decline"}, nil
					},
				})
				s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
					Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
					MaxRetries: -1,
				}, nil)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = s.Close() }()

				// A refusal or an error is an outcome, not a failure: only what the agent read counts.
				err = k.trigger(ctx, s, add)
				time.Sleep(time.Second) // notifications arrive after the reply

				mu.Lock()
				delivered := strings.Contains(read.String(), k.canary)
				mu.Unlock()
				var seen []string
				for _, c := range append(guard.Requests(), guard.Responses()...) {
					if strings.Contains(c.Body, k.canary) {
						seen = append(seen, c.Method)
					}
				}
				AddReportEntry("outcome", map[string]any{"delivered to the agent": delivered, "seen by the guard as": seen, "trigger error": errString(err)})

				check := func() {
					if delivered {
						Expect(seen).NotTo(BeEmpty(), "%s reached the agent and the guard never saw it", k.name)
					}
				}
				if k.gap != "" && env.KnownGaps[k.gap] != "" {
					// An undelivered kind satisfies the property, so the gap is closed (or the trigger is broken).
					if !delivered {
						Fail(k.name + " did not reach the agent: the gap is closed or the trigger is broken; remove the KnownGap")
					}
					KnownGap(env, k.gap, func() { Expect(seen).NotTo(BeEmpty(), "%s reached the agent and the guard never saw it", k.name) })
					return
				}
				check()
			}, SpecTimeout(time.Minute))
		}
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
