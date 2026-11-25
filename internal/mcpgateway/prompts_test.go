// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// promptServer is an MCP server with the readiness prompt and one other prompt. It records the name
// of each prompts/get it serves.
func promptServer(name string, got *[]string, mu *sync.Mutex) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0"}, nil)
	for _, p := range []string{gateway.ReadyPrompt, "hidden_prompt"} {
		s.AddPrompt(&mcp.Prompt{Name: p, Description: name + " " + p}, func(_ context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			mu.Lock()
			*got = append(*got, r.Params.Name)
			mu.Unlock()
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: name + " " + p}}}}, nil
		})
	}
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
}

var _ = Describe("the readiness prompt (ADR 0052)", func() {
	var (
		front              *httptest.Server
		cfg                gateway.MCPConfig
		gw                 *mcpgateway.Gateway
		keyGrant, keyNone  gateway.IssuedKey
		mu                 sync.Mutex
		readyGot, wikiGot  []string
		agentGrant         = gateway.AgentRef{Org: "acme", Name: "wiki-reader"}
		agentNone          = gateway.AgentRef{Org: "globex", Name: "no-mcp"}
		ready, wiki, other *httptest.Server
	)

	BeforeEach(func(ctx SpecContext) {
		readyGot, wikiGot = nil, nil
		ready = promptServer("koinon-tools", &readyGot, &mu)
		wiki = promptServer("acme-wiki", &wikiGot, &mu)
		other = promptServer("other", new([]string), &mu)
		for _, s := range []*httptest.Server{ready, wiki, other} {
			DeferCleanup(func() { s.CloseClientConnections(); s.Close() })
		}
		gw = mcpgateway.New(mcpgateway.Options{Guard: &recordingGuard{refuse: map[string]string{}}})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front = httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		keyGrant, keyNone = mustKey(agentGrant), mustKey(agentNone)
		cfg = gateway.MCPConfig{
			Keys: []gateway.Key{keyGrant.Key, keyNone.Key},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{
					{Name: gateway.ReadyTarget, URL: ready.URL + "/mcp"},
					{Name: "acme-wiki", Org: "acme", URL: wiki.URL + "/mcp"},
					{Name: "other", URL: other.URL + "/mcp"},
				},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{
					agentGrant: {{Target: "acme-wiki", All: true}, {Target: "other", All: true}},
				},
			},
		}
		_, err := gw.ApplyMCP(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
	})

	It("lists only the readiness prompt of the platform target to every key, granted or not", func(ctx SpecContext) {
		for _, key := range []string{keyGrant.Material, keyNone.Material} {
			res, err := connect(ctx, front.URL, key).ListPrompts(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Prompts).To(HaveExactElements(HaveField("Name", gateway.ReadyPromptExposed)))
		}
	})

	It("gets the readiness prompt from the platform target only, and refuses every other name", func(ctx SpecContext) {
		s := connect(ctx, front.URL, keyGrant.Material)
		res, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPromptExposed})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Messages).To(HaveExactElements(HaveField("Content", HaveField("Text", "koinon-tools "+gateway.ReadyPrompt))))
		for _, name := range []string{
			gateway.ReadyTarget + "_hidden_prompt", "acme-wiki_" + gateway.ReadyPrompt, "other_" + gateway.ReadyPrompt, gateway.ReadyPrompt,
		} {
			_, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: name})
			Expect(err).To(HaveOccurred(), name)
		}
		mu.Lock()
		defer mu.Unlock()
		Expect(readyGot).To(Equal([]string{gateway.ReadyPrompt}), "the platform target served only the readiness prompt")
		Expect(wikiGot).To(BeEmpty(), "an org target is never asked for a prompt")
	})

	It("lists no prompt when the readiness target does not answer, and still lists the tools", func(ctx SpecContext) {
		ready.CloseClientConnections()
		ready.Close()
		s := connect(ctx, front.URL, keyGrant.Material)
		res, err := s.ListPrompts(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Prompts).To(BeEmpty())
		_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPromptExposed})
		Expect(err).To(HaveOccurred())
		_, err = s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
	})
})
