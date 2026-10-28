// SPDX-License-Identifier: Apache-2.0

// Package gatewaytest is the G1 suite: Ginkgo specs every gateway driver must pass against its real
// agentgateway (plan items G1 and G2). A driver's test package supplies an [Env] that starts its
// gateway, and calls [Register].
package gatewaytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
)

// Env is one driver under test.
type Env struct {
	// Name prefixes the spec names, for example "standalone" or "kubernetes".
	Name string
	// Host is how the gateway reaches servers on the test host, a hostname or an IP address.
	Host string
	// KeyFile returns the path to put in Provider.APIKeyFile or MCPTarget.CredentialFile for a
	// file Start was given under name.
	KeyFile func(name string) string
	// Start brings up an empty gateway that can reach the given test-host ports, and writes files
	// (name to content) where KeyFile(name) points. The specs then call Apply on the returned
	// driver. It skips the spec when the environment is missing.
	Start func(ctx context.Context, hostPorts []int, files map[string]string) (*Gateway, error)
	// GatewayBudgets is true when the gateway itself blocks a key over its limit with 429 (standalone
	// mode). In Kubernetes mode the ledger revokes the key instead, which that driver's own suite tests.
	GatewayBudgets bool
}

// Gateway is a started gateway under test.
type Gateway struct {
	Driver gateway.ModelGateway
	// URL is the base URL of the LLM and MCP listener as the test reaches it.
	URL string
	// Logs returns the gateway's recent log, for failure reports. Optional.
	Logs func(ctx context.Context) string
	// Stop tears the gateway down. Optional.
	Stop func(ctx context.Context) error
}

const testModel = "fake/test-model"

var (
	agentA = gateway.AgentRef{Org: "acme", Name: "repo-helper"}
	agentB = gateway.AgentRef{Org: "acme", Name: "triage"}
	agentG = gateway.AgentRef{Org: "globex", Name: "analyst"}
)

// Register adds the G1 specs for env to the current Ginkgo tree.
func Register(env Env) {
	llmSpec(env)
	mcpServersSpec(env)
	loneTargetSpec(env)
}

func llmSpec(env Env) {
	Describe(env.Name+": models, keys and tools (G1)", Ordered, func() {
		var (
			driver gateway.ModelGateway
			logs   func(context.Context) string
			state  gateway.State
			llmURL string
			keyA   gateway.IssuedKey // limited agent
			keyB   gateway.IssuedKey // unlimited agent, no tool grants
			keyG   gateway.IssuedKey // an agent of the second org, globex
			ups    map[string]*taggedUpstream
			// redirector answers 307 to the "private" upstream; redirects counts what it was asked.
			redirector *httptest.Server
			redirects  atomic.Int64
			hits       atomic.Int64 // requests that reached the upstream
		)

		chatModel := func(ctx context.Context, key, model string) (int, string) {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewBufferString(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		chat := func(ctx context.Context, key string) (int, string) { return chatModel(ctx, key, testModel) }

		mcpSessionAs := func(ctx context.Context, key, spoof string) (*mcp.ClientSession, error) {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			return client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             llmURL + "/mcp",
				HTTPClient:           &http.Client{Transport: bearer{key: key, base: http.DefaultTransport, spoof: spoof}},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, nil)
		}
		mcpSession := func(ctx context.Context, key string) (*mcp.ClientSession, error) {
			return mcpSessionAs(ctx, key, "")
		}

		toolNames := func(ctx context.Context, key string) []string {
			s, err := mcpSession(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = s.Close() }()
			res, err := s.ListTools(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			var names []string
			for _, t := range res.Tools {
				names = append(names, t.Name)
			}
			slices.Sort(names)
			return names
		}

		BeforeAll(func(ctx SpecContext) {
			upstream := fakeUpstream(&hits)
			DeferCleanup(upstream.Close)
			// Two orgs, each with its own upstream; both call their model testModel.
			ups = map[string]*taggedUpstream{"globex": newTaggedUpstream("globex"), "guarded": newTaggedUpstream("guarded"), "private": newTaggedUpstream("private")}
			// A provider that answers every request with a 307 to a private address (here the "private" fake).
			redirector = newServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirects.Add(1)
				http.Redirect(w, &http.Request{}, fmt.Sprintf("http://%s:%d/v1/chat/completions", env.Host, port(ups["private"].Server)), http.StatusTemporaryRedirect)
			}))
			DeferCleanup(redirector.Close)
			for _, u := range ups {
				DeferCleanup(u.Close)
			}
			tools := fakeTools()
			DeferCleanup(tools.Close)

			var err error
			keyA, err = gateway.NewKey(agentA)
			Expect(err).NotTo(HaveOccurred())
			keyB, err = gateway.NewKey(agentB)
			Expect(err).NotTo(HaveOccurred())
			keyG, err = gateway.NewKey(agentG)
			Expect(err).NotTo(HaveOccurred())

			hostURL := func(p int) string { return fmt.Sprintf("http://%s:%d/v1", env.Host, p) }
			state = gateway.State{
				Providers: []gateway.Provider{
					{
						Org: "acme", Name: "upstream", Type: "fake", BaseURL: hostURL(port(upstream)),
						// $50 per 1M tokens: 20 tokens cost $0.001 per request.
						Models: []gateway.Model{
							{Name: testModel, Price: &gateway.Price{Input: "50", Output: "50"}},
							{Name: "acme-only", Price: &gateway.Price{Input: "1", Output: "1"}},
							// A plain name that is spelled like globex's gateway-wide id.
							{Name: "globex/" + testModel, Price: &gateway.Price{Input: "1", Output: "1"}},
							{Name: "unpriced"},
						},
					},
					{
						Org: "acme", Name: "redirecting", Type: "fake", BaseURL: hostURL(port(redirector)),
						Models: []gateway.Model{{Name: "redirect-model", Price: &gateway.Price{Input: "1", Output: "1"}}},
					},
					{
						Org: "globex", Name: "upstream", Type: "fake", BaseURL: hostURL(port(ups["globex"].Server)),
						APIKeyFile: env.KeyFile("globex_key"),
						Models:     []gateway.Model{{Name: testModel, Price: &gateway.Price{Input: "1", Output: "1"}}},
					},
					{
						Org: "globex", Name: "guarded", Type: "openrouter", BaseURL: hostURL(port(ups["guarded"].Server)),
						Models: []gateway.Model{
							{Name: "guarded-model", Guard: true, Price: &gateway.Price{Input: "1", Output: "1"}},
							{Name: "open-model", Price: &gateway.Price{Input: "1", Output: "1"}},
						},
					},
				},
				MCP: []gateway.MCPTarget{{
					Name: "github",
					URL:  fmt.Sprintf("http://%s:%d/mcp", env.Host, port(tools)),
				}},
			}
			state.AddKey(keyA.Key)
			state.AddKey(keyB.Key)
			state.AddKey(keyG.Key)
			state.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 0.0015})
			state.GrantTools(agentA, gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}})

			gw, err := env.Start(ctx, []int{port(upstream), port(tools), port(ups["globex"].Server), port(ups["guarded"].Server), port(ups["private"].Server), port(redirector)},
				map[string]string{"globex_key": "globex-upstream-key"})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			driver, logs, llmURL = gw.Driver, gw.Logs, gw.URL
			_, err = driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		Describe("org-scoped models (plan item PV1)", func() {
			It("routes each org's plain model name to its own provider", func(ctx SpecContext) {
				code, body := chat(ctx, keyG.Material)
				Expect(code).To(Equal(http.StatusOK), body)
				Expect(body).To(ContainSubstring("answered by globex"))
				code, body = chat(ctx, keyB.Material)
				Expect(code).To(Equal(http.StatusOK), body)
				Expect(body).NotTo(ContainSubstring("globex"))
			})

			It("keeps an org's key off another org's models", func(ctx SpecContext) {
				before := ups["globex"].hits.Load()
				// acme-only exists only in acme: the globex key may not ask for it.
				code, body := chatModel(ctx, keyG.Material, "acme-only")
				Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), body)
				// and an acme key cannot reach globex's models.
				code, body = chatModel(ctx, keyB.Material, "open-model")
				Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), body)
				Expect(ups["globex"].hits.Load()).To(Equal(before))
			})

			It("does not serve another org's gateway-wide id to a key, however the name is spelled", func(ctx SpecContext) {
				g := ups["globex"].hits.Load() + ups["guarded"].hits.Load()
				// acme asks for globex's models by their <org>/<model> ids.
				for _, id := range []string{"globex/" + testModel, "globex/guarded-model", "globex/open-model"} {
					if id == "globex/"+testModel {
						continue // acme declared a plain model with this name; see below
					}
					code, body := chatModel(ctx, keyB.Material, id)
					Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), id+": "+body)
				}
				Expect(ups["globex"].hits.Load() + ups["guarded"].hits.Load()).To(Equal(g))

				// acme declared a plain model named like globex's id. It is acme's model: acme's upstream
				// answers, and globex's upstream sees nothing.
				before := hits.Load()
				code, body := chatModel(ctx, keyB.Material, "globex/"+testModel)
				Expect(code).To(Equal(http.StatusOK), body)
				Expect(hits.Load()).To(Equal(before + 1))
				Expect(ups["globex"].hits.Load() + ups["guarded"].hits.Load()).To(Equal(g))

				// and globex's key cannot reach acme's model of that name.
				code, body = chatModel(ctx, keyG.Material, "globex/"+testModel)
				Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), body)
				Expect(ups["globex"].hits.Load() + ups["guarded"].hits.Load()).To(Equal(g))
				Expect(hits.Load()).To(Equal(before + 1))
			})

			It("does not follow a redirect from a provider to another address", func(ctx SpecContext) {
				// A provider the egress policy allows could still answer 307 to a private address; the
				// gateway must hand that answer back, not connect there with the provider's credentials.
				before := ups["private"].hits.Load()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions",
					bytes.NewBufferString(`{"model":"redirect-model","messages":[{"role":"user","content":"hi"}]}`))
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+keyB.Material)
				// Do not follow redirects ourselves: we want to see what the gateway answers.
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				resp, err := client.Do(req)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = resp.Body.Close() }()
				raw, _ := io.ReadAll(resp.Body)
				code, body := resp.StatusCode, string(raw)
				Expect(redirects.Load()).To(BeNumerically(">=", 1), "the request reached the redirecting provider")
				Expect(ups["private"].hits.Load()).To(Equal(before), "agentgateway followed the redirect: "+body)
				Expect(code).NotTo(Equal(http.StatusOK), body)
			})

			It("does not let the client pick another org by header", func(ctx SpecContext) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions",
					bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, testModel)))
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+keyB.Material)
				req.Header.Set(gateway.OrgHeader, "globex")
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = resp.Body.Close() }()
				b, _ := io.ReadAll(resp.Body)
				Expect(string(b)).NotTo(ContainSubstring("globex"))
			})

			It("serves no unpriced model", func(ctx SpecContext) {
				code, body := chatModel(ctx, keyB.Material, "unpriced")
				Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), body)
			})

			It("forces the routing guard into the request only for guarded models", func(ctx SpecContext) {
				code, body := chatModel(ctx, keyG.Material, "guarded-model")
				Expect(code).To(Equal(http.StatusOK), body)
				var got map[string]any
				Expect(json.Unmarshal([]byte(ups["guarded"].lastBody()), &got)).To(Succeed())
				Expect(got).To(HaveKeyWithValue("provider", map[string]any{"zdr": true, "data_collection": "deny"}))

				code, body = chatModel(ctx, keyG.Material, "open-model")
				Expect(code).To(Equal(http.StatusOK), body)
				got = nil
				Expect(json.Unmarshal([]byte(ups["guarded"].lastBody()), &got)).To(Succeed())
				Expect(got).NotTo(HaveKey("provider"))
			})

			It("always asks a streaming upstream for usage, whatever the client sent", func(ctx SpecContext) {
				send := func(body string) {
					GinkgoHelper()
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewBufferString(body))
					Expect(err).NotTo(HaveOccurred())
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+keyG.Material)
					resp, err := http.DefaultClient.Do(req)
					Expect(err).NotTo(HaveOccurred())
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))
				}
				upstreamOptions := func() any {
					var got map[string]any
					Expect(json.Unmarshal([]byte(ups["guarded"].lastBody()), &got)).To(Succeed())
					return got["stream_options"]
				}
				msgs := `"messages":[{"role":"user","content":"hi"}]`
				include := map[string]any{"include_usage": true}

				send(`{"model":"open-model","stream":true,` + msgs + `}`)
				Expect(upstreamOptions()).To(Equal(include), "no stream_options from the client")
				send(`{"model":"open-model","stream":true,"stream_options":{"include_usage":false},` + msgs + `}`)
				Expect(upstreamOptions()).To(Equal(include), "the client asked for no usage")
				send(`{"model":"open-model","stream":true,"stream_options":{},` + msgs + `}`)
				Expect(upstreamOptions()).To(Equal(include), "an empty stream_options")
				send(`{"model":"open-model","stream":false,"stream_options":{"include_usage":true},` + msgs + `}`)
				Expect(upstreamOptions()).To(BeNil(), "not streamed: no stream_options goes upstream")
				send(`{"model":"open-model",` + msgs + `}`)
				Expect(upstreamOptions()).To(BeNil())
			})

			It("passes cache_control markers through to the upstream unchanged (ADR 0068)", func(ctx SpecContext) {
				marker := map[string]any{"type": "ephemeral"}
				block := func(text string) map[string]any {
					return map[string]any{"type": "text", "text": text, "cache_control": marker}
				}
				sent := map[string]any{
					"model": "open-model", "session_id": "01234567-run",
					"messages": []any{
						map[string]any{"role": "system", "content": []any{block("You are a helpful agent.")}},
						map[string]any{"role": "user", "content": []any{block("first question")}},
						map[string]any{"role": "assistant", "content": "answer"},
						map[string]any{"role": "user", "content": []any{block("second question")}},
					},
					"tools": []any{map[string]any{
						"type": "function", "cache_control": marker,
						"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
					}},
				}
				raw, err := json.Marshal(sent)
				Expect(err).NotTo(HaveOccurred())
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewReader(raw))
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+keyG.Material)
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))

				var got map[string]any
				Expect(json.Unmarshal([]byte(ups["guarded"].lastBody()), &got)).To(Succeed())
				Expect(got["messages"]).To(Equal(sent["messages"]), "the content blocks and their cache_control reach the upstream as sent")
				Expect(got).To(HaveKeyWithValue("session_id", "01234567-run"), "OpenRouter's sticky-routing field reaches the upstream")
				Expect(got["tools"]).To(Equal(sent["tools"]), "the tool's cache_control reaches the upstream as sent")
			})

			It("sends the provider's key file content as the upstream credential", func(ctx SpecContext) {
				// The fake upstream does not check credentials; the config names the file and
				// agentgateway accepted it (--validate-only and the reload would fail on a missing file).
				code, body := chat(ctx, keyG.Material)
				Expect(code).To(Equal(http.StatusOK), body)
			})
		})

		It("rejects requests without a key", func(ctx SpecContext) {
			code, _ := chat(ctx, "")
			Expect(code).To(Equal(http.StatusUnauthorized))
			code, _ = chat(ctx, "kgw_not-a-real-key")
			Expect(code).To(Equal(http.StatusUnauthorized))
		})

		It("serves a valid key and reports usage", func(ctx SpecContext) {
			code, body := chat(ctx, keyB.Material)
			Expect(code).To(Equal(http.StatusOK), body)
			Expect(body).To(ContainSubstring(`"prompt_tokens":10`))
		})

		if env.GatewayBudgets {
			It("blocks a key over its limit with 429 budget_exceeded", func(ctx SpecContext) {
				var codes []int
				var last string
				for range 5 {
					code, body := chat(ctx, keyA.Material)
					codes = append(codes, code)
					last = body
					if code == http.StatusTooManyRequests {
						break
					}
				}
				// $0.001 per request against $0.0015: two pass (the second overshoots
				// by less than one response, ADR 0041), the third is blocked.
				Expect(codes).To(Equal([]int{200, 200, 429}))
				Expect(last).To(ContainSubstring("budget_exceeded"))

				code, _ := chat(ctx, keyB.Material)
				Expect(code).To(Equal(http.StatusOK), "other agents are not affected")
			})
		}

		It("shows each agent only its granted tools", func(ctx SpecContext) {
			Expect(toolNames(ctx, keyA.Material)).To(Equal([]string{"github_create_pull_request", "github_get_approval_status"}))
			Expect(toolNames(ctx, keyB.Material)).To(BeEmpty())

			s, err := mcpSession(ctx, keyA.Material)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = s.Close() }()
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "github_direct_create_pull_request"})
			if err == nil {
				Expect(res.IsError).To(BeTrue(), "bypass tool must not be callable")
				for _, c := range res.Content {
					if t, ok := c.(*mcp.TextContent); ok {
						Expect(t.Text).NotTo(ContainSubstring("reached"))
					}
				}
			}
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "github_get_approval_status"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsError).To(BeFalse())
		})

		It("lists the readiness prompt to every agent, and no other prompt", func(ctx SpecContext) {
			for _, key := range []string{keyA.Material, keyB.Material} {
				s, err := mcpSession(ctx, key)
				Expect(err).NotTo(HaveOccurred())
				res, err := s.ListPrompts(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Prompts).To(ConsistOf(HaveField("Name", gateway.ReadyPromptExposed)))
				_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPromptExposed})
				Expect(err).NotTo(HaveOccurred())
				_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "github_hidden_prompt"})
				Expect(err).To(HaveOccurred())
				_ = s.Close()
			}
		})

		It("tells the target which agent calls, overwriting a client-supplied header", func(ctx SpecContext) {
			for _, spoof := range []string{"", agentB.String()} {
				s, err := mcpSessionAs(ctx, keyA.Material, spoof)
				Expect(err).NotTo(HaveOccurred())
				res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "github_get_approval_status"})
				_ = s.Close()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.IsError).To(BeFalse())
				Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("get_approval_status reached by " + agentA.String()))
			}
		})

		It("rejects MCP requests without a key", func(ctx SpecContext) {
			_, err := mcpSession(ctx, "kgw_not-a-real-key")
			Expect(err).To(HaveOccurred())
		})

		It("rejects a revoked key after re-render and reload", func(ctx SpecContext) {
			Expect(state.RevokeKey(keyB.ID)).To(BeTrue())
			rotated, err := driver.IssueKey(ctx, agentB)
			Expect(err).NotTo(HaveOccurred())
			state.AddKey(rotated.Key)

			start := time.Now()
			_, err = driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry("reload latency", time.Since(start).String())

			code, _ := chat(ctx, keyB.Material)
			Expect(code).To(Equal(http.StatusUnauthorized))
			code, body := chat(ctx, rotated.Material)
			Expect(code).To(Equal(http.StatusOK), body)
		})
	})
}

func mcpServersSpec(env Env) {
	Describe(env.Name+": org MCP servers (MC1)", Ordered, func() {
		var (
			llmURL     string
			logs       func(context.Context) string
			acmeLog    = &headerLog{}
			bearerLog  = &headerLog{}
			openLog    = &headerLog{}
			globexLog  = &headerLog{}
			keyAcme    gateway.IssuedKey // acme agent allowed acme-wiki and acme-bearer
			keyAcme2   gateway.IssuedKey // acme agent with no MCP grants
			keyGlobex  gateway.IssuedKey // globex agent allowed globex-wiki
			keyCross   gateway.IssuedKey // acme agent wrongly granted a globex target and a dead one
			agentAcme  = gateway.AgentRef{Org: "acme", Name: "wiki-reader"}
			agentAcme2 = gateway.AgentRef{Org: "acme", Name: "no-mcp"}
			agentGlob  = gateway.AgentRef{Org: "globex", Name: "wiki-reader"}
			agentCross = gateway.AgentRef{Org: "acme", Name: "cross"}
		)

		session := func(ctx context.Context, key string) *mcp.ClientSession {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             llmURL + "/mcp",
				HTTPClient:           &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			return s
		}
		names := func(ctx context.Context, key string) []string {
			res, err := session(ctx, key).ListTools(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			var out []string
			for _, t := range res.Tools {
				out = append(out, t.Name)
			}
			return out
		}

		BeforeAll(func(ctx SpecContext) {
			acme := fakeRemote("acme-wiki", acmeLog, "X-Api-Key")
			bearerSrv := fakeRemote("acme-bearer", bearerLog, "Authorization")
			globex := fakeRemote("globex-wiki", globexLog, "X-Api-Key")
			open := fakeRemote("acme-open", openLog, "Authorization")
			DeferCleanup(func() { open.CloseClientConnections(); go open.Close() })
			tools := fakeTools()
			DeferCleanup(func() { acme.CloseClientConnections(); acme.Close() })
			DeferCleanup(func() { bearerSrv.CloseClientConnections(); bearerSrv.Close() })
			DeferCleanup(func() { globex.CloseClientConnections(); globex.Close() })
			DeferCleanup(func() { tools.CloseClientConnections(); tools.Close() })
			// A server that is down: a port nothing listens on.
			dead := newServer(http.NotFoundHandler())
			deadPort := port(dead)
			dead.Close()

			host := func(p int) string { return "http://" + env.Host + ":" + itoa(p) + "/mcp" }
			var err error
			for ref, dst := range map[gateway.AgentRef]*gateway.IssuedKey{agentAcme: &keyAcme, agentAcme2: &keyAcme2, agentGlob: &keyGlobex, agentCross: &keyCross} {
				*dst, err = gateway.NewKey(ref)
				Expect(err).NotTo(HaveOccurred())
			}
			state := gateway.State{MCP: []gateway.MCPTarget{
				{Name: "github", URL: host(port(tools))},
				{Name: "acme-wiki", Org: "acme", URL: host(port(acme)), CredentialHeader: "X-Api-Key", CredentialFile: env.KeyFile("acme-wiki-key")}, //nolint:gosec // dummy
				{Name: "acme-bearer", Org: "acme", URL: host(port(bearerSrv)), CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: env.KeyFile("acme-bearer-key")},
				{Name: "globex-wiki", Org: "globex", URL: host(port(globex)), CredentialHeader: "X-Api-Key", CredentialFile: env.KeyFile("globex-wiki-key")}, //nolint:gosec // dummy
				{Name: "acme-dead", Org: "acme", URL: host(deadPort)},
				{Name: "acme-open", Org: "acme", URL: host(port(open))},
			}}
			for _, k := range []gateway.IssuedKey{keyAcme, keyAcme2, keyGlobex, keyCross} {
				state.AddKey(k.Key)
			}
			state.GrantTools(agentAcme,
				gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"read_item", "write_item"}},
				gateway.ToolGrant{Target: "acme-bearer", Tools: []string{"read_item"}},
				gateway.ToolGrant{Target: "acme-dead", Tools: []string{"read_item"}},
				gateway.ToolGrant{Target: "acme-open", Tools: []string{"read_item"}},
			)
			state.GrantTools(agentGlob, gateway.ToolGrant{Target: "globex-wiki", Tools: []string{"read_item"}})
			// A grant that names another org's target must not work: the condition hides it.
			state.GrantTools(agentCross, gateway.ToolGrant{Target: "globex-wiki", Tools: []string{"read_item", "write_item"}})

			// A trailing newline is part of the file as kubelet writes it only if the Secret has one.
			gw, err := env.Start(ctx, []int{port(acme), port(bearerSrv), port(globex), port(tools), port(open)}, map[string]string{
				"acme-wiki-key": "not-a-real-token-acme", "acme-bearer-key": "not-a-real-token-bearer", "globex-wiki-key": "not-a-real-token-globex",
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			logs, llmURL = gw.Logs, gw.URL
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		It("lists an agent only the granted tools of its own org's servers", func(ctx SpecContext) {
			got := names(ctx, keyAcme.Material)
			AddReportEntry("acme tool names", got)
			Expect(got).To(ConsistOf("acme-wiki_read_item", "acme-wiki_write_item", "acme-bearer_read_item", "acme-open_read_item"))
			// One visible target is still prefixed? (the platform's github target is hidden by the grants, not the condition)
			Expect(names(ctx, keyGlobex.Material)).To(ConsistOf("globex-wiki_read_item"))
			Expect(names(ctx, keyAcme2.Material)).To(BeEmpty())
		})

		It("never shows or serves another org's server, even to an agent granted it", func(ctx SpecContext) {
			Expect(names(ctx, keyCross.Material)).To(BeEmpty())
			before := len(globexLog.values("X-Api-Key"))
			callsBefore := len(globexLog.values("Mcp-Method"))
			for _, tool := range []string{"globex-wiki_read_item", "read_item", "globex-wiki_write_item"} {
				res, err := session(ctx, keyCross.Material).CallTool(ctx, &mcp.CallToolParams{Name: tool})
				if err == nil {
					Expect(res.IsError).To(BeTrue(), tool)
				}
			}
			// The globex server was not even contacted for the other org's key (a per-target
			// condition in both modes since agentgateway v1.7, ADR 0058).
			Expect(globexLog.values("X-Api-Key")).To(HaveLen(before))
			Expect(globexLog.values("Mcp-Method")).NotTo(ContainElement("tools/call"), "no call reached the server; saw %d requests", len(globexLog.values("Mcp-Method"))-callsBefore)
		})

		It("injects the credential header at the gateway and does not let the agent set it", func(ctx SpecContext) {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             llmURL + "/mcp",
				HTTPClient:           &http.Client{Transport: headerSetter{key: keyAcme.Material, name: "X-Api-Key", value: "agent-chose-this", base: http.DefaultTransport}},
				DisableStandaloneSSE: true, MaxRetries: -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-wiki_read_item"})
			Expect(err).NotTo(HaveOccurred())
			text := res.Content[0].(*mcp.TextContent).Text
			acmeLog.mu.Lock()
			for _, h := range acmeLog.seen {
				AddReportEntry("upstream", fmt.Sprint(h.Values("X-Api-Key"), h.Values("Authorization")))
			}
			acmeLog.mu.Unlock()
			Expect(text).To(Equal("acme-wiki saw not-a-real-token-acme"))
			Expect(acmeLog.values("X-Api-Key")).NotTo(ContainElement("agent-chose-this"))

			res, err = session(ctx, keyAcme.Material).CallTool(ctx, &mcp.CallToolParams{Name: "acme-bearer_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("acme-bearer saw Bearer not-a-real-token-bearer"))

			// The one tool the grant did not name is refused.
			res, err = session(ctx, keyAcme.Material).CallTool(ctx, &mcp.CallToolParams{Name: "acme-bearer_write_item"})
			if err == nil {
				Expect(res.IsError).To(BeTrue())
			}
		})

		It("does not forward the agent's gateway key to a server without a credential", func(ctx SpecContext) {
			res, err := session(ctx, keyAcme.Material).CallTool(ctx, &mcp.CallToolParams{Name: "acme-open_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("acme-open saw "))
			Expect(strings.Join(openLog.values("Authorization"), ",")).NotTo(ContainSubstring(gateway.KeyPrefix))
		})

		It("drops a credential header the agent sent to a server that has none", func(ctx SpecContext) {
			client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             llmURL + "/mcp",
				HTTPClient:           &http.Client{Transport: headerSetter{key: keyAcme.Material, name: "X-Api-Key", value: "agent-chose-this", base: http.DefaultTransport}},
				DisableStandaloneSSE: true, MaxRetries: -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-open_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Content[0].(*mcp.TextContent).Text).To(Equal("acme-open saw "))
			Expect(openLog.values("X-Api-Key")).NotTo(ContainElement("agent-chose-this"))
		})

		It("does not expose the credential to the agent in what it receives", func(ctx SpecContext) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/mcp", strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+keyAcme.Material)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			all := resp.Header.Get("Mcp-Session-Id") + string(b)
			for k, v := range resp.Header {
				all += k + strings.Join(v, ",")
			}
			Expect(all).NotTo(ContainSubstring("not-a-real-token"))
		})

		It("keeps serving the other servers while one is down", func(ctx SpecContext) {
			got := names(ctx, keyAcme.Material)
			Expect(got).To(ContainElement("acme-wiki_read_item"))
			Expect(got).NotTo(ContainElement(HavePrefix("acme-dead")))
		})
	})
}

func loneTargetSpec(env Env) {
	Describe(env.Name+": one org MCP server and no credential", Ordered, func() {
		var (
			llmURL         string
			log            = &headerLog{}
			keyAcme, keyGl gateway.IssuedKey
		)
		BeforeAll(func(ctx SpecContext) {
			srv := fakeRemote("acme-only", log, "Authorization")
			DeferCleanup(func() { srv.CloseClientConnections(); go srv.Close() })
			var err error
			keyAcme, err = gateway.NewKey(gateway.AgentRef{Org: "acme", Name: "reader"})
			Expect(err).NotTo(HaveOccurred())
			keyGl, err = gateway.NewKey(gateway.AgentRef{Org: "globex", Name: "reader"})
			Expect(err).NotTo(HaveOccurred())
			state := gateway.State{MCP: []gateway.MCPTarget{{Name: "acme-only", Org: "acme", URL: "http://" + env.Host + ":" + itoa(port(srv)) + "/mcp"}}}
			state.AddKey(keyAcme.Key)
			state.AddKey(keyGl.Key)
			state.GrantTools(keyAcme.Agent, gateway.ToolGrant{Target: "acme-only", Tools: []string{"read_item"}})
			state.GrantTools(keyGl.Agent, gateway.ToolGrant{Target: "acme-only", Tools: []string{"read_item"}})
			gw, err := env.Start(ctx, []int{port(srv)}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			llmURL = gw.URL
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		connect := func(ctx context.Context, key string) *mcp.ClientSession {
			client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
			s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint: llmURL + "/mcp", HTTPClient: &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}},
				DisableStandaloneSSE: true, MaxRetries: -1,
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = s.Close() })
			return s
		}

		It("prefixes the tool whatever number of targets a key sees, and does not forward the gateway key", func(ctx SpecContext) {
			s := connect(ctx, keyAcme.Material)
			res, err := s.ListTools(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Tools).To(HaveLen(1))
			Expect(res.Tools[0].Name).To(Equal("acme-only_read_item"))
			call, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-only_read_item"})
			Expect(err).NotTo(HaveOccurred())
			Expect(call.Content[0].(*mcp.TextContent).Text).To(Equal("acme-only saw "))
			Expect(strings.Join(log.values("Authorization"), ",")).NotTo(ContainSubstring(gateway.KeyPrefix))
		})

		It("shows the lone org target to no other org", func(ctx SpecContext) {
			before := len(log.values("Mcp-Method"))
			s := connect(ctx, keyGl.Material)
			res, err := s.ListTools(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Tools).To(BeEmpty())
			Expect(log.values("Mcp-Method")).To(HaveLen(before), "the server was not contacted for the other org's key")
			_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "acme-only_read_item"})
			if err == nil {
				Expect(log.values("Mcp-Method")).NotTo(ContainElement("tools/call"))
			}
		})
	})
}
