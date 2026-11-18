// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// RegisterMCP adds the specs of the MCP path alone for env: the cases a gateway that serves only the
// MCP path (koinon-gateway, ADR 0097 step 2) must pass, with no model path and no leak guard. Env.Start
// is given guardURL "" and a Driver that applies the MCP half of a State. The targets are platform
// targets (Org ""): a gateway without a guard refuses an org's target, so the cases that need one
// (the guard specs of [Register]) are not here.
func RegisterMCP(env Env) {
	mcpPathSpec(env)
}

func mcpPathSpec(env Env) {
	Describe(env.Name+": the MCP path alone", Ordered, func() {
		var (
			gw                                  *Gateway
			alphaLog, betaLog                   = &headerLog{}, &headerLog{}
			keyReader, keyBoth, keyNone, keyOld gateway.IssuedKey
			state                               gateway.State
		)
		agentReader := gateway.AgentRef{Org: "acme", Name: "reader"}
		agentBoth := gateway.AgentRef{Org: "acme", Name: "both"}
		agentNone := gateway.AgentRef{Org: "acme", Name: "no-grant"}
		agentOld := gateway.AgentRef{Org: "globex", Name: "rotating"}

		connect := func(ctx context.Context, key string) *mcp.ClientSession {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             gw.URL + "/mcp",
				HTTPClient:           &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			return s
		}
		toolNames := func(ctx context.Context, key string) []string {
			res, err := connect(ctx, key).ListTools(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			var out []string
			for _, t := range res.Tools {
				out = append(out, t.Name)
			}
			return out
		}
		status := func(ctx context.Context, key string) int {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/mcp",
				bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode
		}

		BeforeAll(func(ctx SpecContext) {
			alpha := fakeRemote("alpha", alphaLog, "X-Api-Key")
			beta := fakeRemote("beta", betaLog, "X-Api-Key")
			DeferCleanup(func() { alpha.CloseClientConnections(); go alpha.Close() })
			DeferCleanup(func() { beta.CloseClientConnections(); go beta.Close() })
			var err error
			for _, k := range []struct {
				dst *gateway.IssuedKey
				a   gateway.AgentRef
			}{{&keyReader, agentReader}, {&keyBoth, agentBoth}, {&keyNone, agentNone}, {&keyOld, agentOld}} {
				*k.dst, err = gateway.NewKey(k.a)
				Expect(err).NotTo(HaveOccurred())
			}
			url := func(s string, p int) string { return "http://" + env.Host + ":" + itoa(p) + "/mcp" + s }
			state = gateway.State{MCPState: gateway.MCPState{MCP: []gateway.MCPTarget{
				{Name: "alpha", URL: url("", port(alpha)), CredentialHeader: "X-Api-Key", CredentialFile: env.KeyFile("mcp_key")}, //nolint:gosec // a header name and a test key file, not a credential
				{Name: "beta", URL: url("", port(beta))},
			}}}
			for _, k := range []gateway.IssuedKey{keyReader, keyBoth, keyNone, keyOld} {
				state.AddKey(k.Key)
			}
			state.GrantTools(agentReader, gateway.ToolGrant{Target: "alpha", Tools: []string{"read_item"}})
			state.GrantTools(agentBoth, gateway.ToolGrant{Target: "alpha", All: true}, gateway.ToolGrant{Target: "beta", Tools: []string{"read_item"}})
			state.GrantTools(agentOld, gateway.ToolGrant{Target: "beta", All: true})
			gw, err = env.Start(ctx, []int{port(alpha), port(beta)}, map[string]string{"mcp_key": "koinon_test_not-a-real-token-alpha\n"}, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		It("refuses a request without a key, and one with a key it does not know, with 401", func(ctx SpecContext) {
			Expect(status(ctx, "")).To(Equal(http.StatusUnauthorized))
			Expect(status(ctx, gateway.KeyPrefix+"koinon_test_not-a-real-key")).To(Equal(http.StatusUnauthorized))
		})

		It("lists the granted tools only, each named target_tool", func(ctx SpecContext) {
			Expect(toolNames(ctx, keyReader.Material)).To(ConsistOf("alpha_read_item"))
			Expect(toolNames(ctx, keyBoth.Material)).To(ConsistOf("alpha_read_item", "alpha_write_item", "beta_read_item"))
			Expect(toolNames(ctx, keyNone.Material)).To(BeEmpty())
		})

		It("sends a call to the target that owns the tool, with the target's credential and never the gateway key", func(ctx SpecContext) {
			s := connect(ctx, keyReader.Material)
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "alpha_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeFalse())
			Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("alpha saw koinon_test_not-a-real-token-alpha"))
			Expect(strings.Join(alphaLog.values("Authorization"), ",")).NotTo(ContainSubstring(gateway.KeyPrefix))
			Expect(strings.Join(alphaLog.values("X-Api-Key"), ",")).To(ContainSubstring("koinon_test_not-a-real-token-alpha"))
		})

		It("sends no credential to a target that has none", func(ctx SpecContext) {
			s := connect(ctx, keyBoth.Material)
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "beta_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("beta saw "))
			Expect(betaLog.values("X-Api-Key")).To(BeEmpty())
		})

		It("refuses a tool the key has no grant for, and never contacts the target for it", func(ctx SpecContext) {
			before := len(betaLog.values("Mcp-Method"))
			s := connect(ctx, keyReader.Material)
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "beta_read_item"})
			Expect(err != nil || res.IsError).To(BeTrue(), "a call without a grant must fail")
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "alpha_write_item"})
			Expect(err != nil || res.IsError).To(BeTrue(), "a tool outside the grant must fail")
			Expect(betaLog.values("Mcp-Method")).To(HaveLen(before))
		})

		It("serves a revoked key's refusal on the request after the Apply that revokes it", func(ctx SpecContext) {
			Expect(status(ctx, keyOld.Material)).To(Equal(http.StatusOK))
			Expect(state.RevokeKey(keyOld.ID)).To(BeTrue())
			_, err := gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
			Expect(status(ctx, keyOld.Material)).To(Equal(http.StatusUnauthorized))
		})
	})
}
