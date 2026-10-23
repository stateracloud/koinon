// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/gateway/standalone"
)

// AgentgatewayImage is the agentgateway release Koinon targets (ADR 0048).
const AgentgatewayImage = "cr.agentgateway.dev/agentgateway:v1.6.0"

const testModel = "fake/test-model"

// skipWithoutDocker skips the spec when no container runtime is reachable.
func skipWithoutDocker(ctx context.Context) {
	p, err := testcontainers.NewDockerProvider()
	if err == nil {
		defer func() { _ = p.Close() }()
		err = p.Health(ctx)
	}
	if err != nil {
		Skip("Docker is not available: " + err.Error())
	}
}

// fakeUpstream is an OpenAI-compatible chat completions server that reports
// 10 input and 10 output tokens per request and counts requests in hits.
func fakeUpstream(hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
}

// fakeTools is an MCP server with the two parking tools and a bypass tool
// that no agent is granted (spike step 6). Each tool answers with the agent
// header it received.
func fakeTools() *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-tools", Version: "0.0.1"}, nil)
	type args struct {
		Title string `json:"title,omitempty"`
	}
	for _, name := range []string{"create_pull_request", "get_approval_status", "direct_create_pull_request"} {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name},
			func(_ context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
				agent := strings.Join(req.Extra.Header.Values(gateway.AgentHeader), ",")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " reached by " + agent}}}, nil, nil
			})
	}
	// The readiness prompt every agent must see, and one no rule admits.
	for _, name := range []string{gateway.ReadyPrompt, "hidden_prompt"} {
		srv.AddPrompt(&mcp.Prompt{Name: name, Description: name},
			func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: name}}}}, nil
			})
	}
	// agentgateway reaches it through the testcontainers SSH tunnel, which
	// arrives on localhost with Host: host.testcontainers.internal.
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
}

func port(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

type bearer struct {
	key  string
	base http.RoundTripper
	// spoof, when set, is sent as the agent header, which agentgateway must
	// overwrite.
	spoof string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	if b.spoof != "" {
		r.Header.Set(gateway.AgentHeader, b.spoof)
	}
	return b.base.RoundTrip(r)
}

var _ = Describe("agentgateway v1.6.0 with a rendered config", Ordered, func() {
	var (
		cfgDir  string
		driver  *standalone.Driver
		state   gateway.State
		llmURL  string
		keyA    gateway.IssuedKey // limited agent
		keyB    gateway.IssuedKey // unlimited agent, no tool grants
		ctr     *testcontainers.DockerContainer
		hits    atomic.Int64 // requests that reached the upstream
		options = standalone.Options{DatabaseURL: "sqlite:///tmp/agentgateway.db?mode=rwc"}
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
		skipWithoutDocker(ctx)

		upstream := fakeUpstream(&hits)
		DeferCleanup(upstream.Close)
		tools := fakeTools()
		DeferCleanup(tools.Close)

		var err error
		keyA, err = gateway.NewKey(agentA)
		Expect(err).NotTo(HaveOccurred())
		keyB, err = gateway.NewKey(agentB)
		Expect(err).NotTo(HaveOccurred())

		state = gateway.State{
			Models: gateway.ModelUpstream{
				Provider: "fake",
				BaseURL:  fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, port(upstream)),
				// $50 per 1M tokens: 20 tokens cost $0.001 per request.
				Models:    []gateway.Model{{Name: testModel, Price: &gateway.Price{Input: "50", Output: "50"}}},
				Overrides: map[string]any{"temperature": 0},
			},
			MCP: []gateway.MCPTarget{{
				Name: "github",
				URL:  fmt.Sprintf("http://%s:%d/mcp", testcontainers.HostInternal, port(tools)),
			}},
		}
		state.AddKey(keyA.Key)
		state.AddKey(keyB.Key)
		state.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 0.0015, Window: 24 * time.Hour})
		state.GrantTools(agentA, gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}})

		cfgDir = GinkgoT().TempDir()
		Expect(os.Chmod(cfgDir, 0o755)).To(Succeed())
		file := standalone.FileDeliverer{Path: filepath.Join(cfgDir, "config.yaml")}
		first, _, err := standalone.Render(state, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Deliver(ctx, first, "")).To(Succeed())

		ctr, err = testcontainers.Run(ctx, AgentgatewayImage,
			testcontainers.WithCmd("-f", "/cfg/config.yaml"),
			testcontainers.WithExposedPorts("4000/tcp", "15020/tcp", "15021/tcp"),
			testcontainers.WithHostPortAccess(port(upstream), port(tools)),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, cfgDir+":/cfg:ro")
			}),
			testcontainers.WithWaitStrategyAndDeadline(time.Minute,
				wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
		)
		DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
		Expect(err).NotTo(HaveOccurred())

		llmURL, err = ctr.PortEndpoint(ctx, "4000/tcp", "http")
		Expect(err).NotTo(HaveOccurred())
		statsURL, err := ctr.PortEndpoint(ctx, "15020/tcp", "http")
		Expect(err).NotTo(HaveOccurred())

		driver = &standalone.Driver{
			Options:   options,
			Deliverer: file,
			Waiter:    standalone.ProbeWaiter{GatewayURL: llmURL, StatsURL: statsURL, Interval: 200 * time.Millisecond},
		}
		// Same state again: confirms the waiter sees the initial revision.
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && ctr != nil {
			if logs, err := ctr.Logs(ctx); err == nil {
				b, _ := io.ReadAll(logs)
				AddReportEntry("agentgateway logs", string(b))
			}
		}
	})

	It("detects reloads with the probe key, which never reaches the upstream", func(ctx SpecContext) {
		// BeforeAll's Apply waited on the initial revision's probe.
		Expect(hits.Load()).To(BeZero())
		_, rev, err := standalone.Render(state, options)
		Expect(err).NotTo(HaveOccurred())
		// Not even with a served model name: the zero budget (429, with a
		// database) or the authorization rule (403) refuses the key first.
		code, body := chatModel(ctx, standalone.ReloadProbeKey(rev), testModel)
		Expect(code).To(BeElementOf(http.StatusTooManyRequests, http.StatusForbidden), body)

		// A new revision: its probe key is accepted only once it is live.
		extra, err := gateway.NewKey(agentB)
		Expect(err).NotTo(HaveOccurred())
		state.AddKey(extra.Key)
		next, err := driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		Expect(next).NotTo(Equal(rev))
		code, _ = chatModel(ctx, standalone.ReloadProbeKey(rev), testModel)
		Expect(code).To(Equal(http.StatusUnauthorized), "the previous revision's probe key is gone")
		code, body = chatModel(ctx, extra.Material, testModel)
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(hits.Load()).To(Equal(int64(1)), "only the real key's request reached the upstream")

		// A revision that was never delivered is never live.
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		Expect(driver.Waiter.Wait(short, "0000000000000000")).To(MatchError(context.DeadlineExceeded))

		Expect(state.RevokeKey(extra.ID)).To(BeTrue())
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(2*time.Minute))

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

	It("shows each agent only its granted tools", func(ctx SpecContext) {
		Expect(toolNames(ctx, keyA.Material)).To(Equal([]string{"create_pull_request", "get_approval_status"}))
		Expect(toolNames(ctx, keyB.Material)).To(BeEmpty())

		s, err := mcpSession(ctx, keyA.Material)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = s.Close() }()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "direct_create_pull_request"})
		if err == nil {
			Expect(res.IsError).To(BeTrue(), "bypass tool must not be callable")
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					Expect(t.Text).NotTo(ContainSubstring("reached"))
				}
			}
		}
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "get_approval_status"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
	})

	It("lists the readiness prompt to every agent, and no other prompt", func(ctx SpecContext) {
		for _, key := range []string{keyA.Material, keyB.Material} {
			s, err := mcpSession(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			res, err := s.ListPrompts(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Prompts).To(ConsistOf(HaveField("Name", gateway.ReadyPrompt)))
			_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: gateway.ReadyPrompt})
			Expect(err).NotTo(HaveOccurred())
			_, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "hidden_prompt"})
			Expect(err).To(HaveOccurred())
			_ = s.Close()
		}
	})

	It("tells the target which agent calls, overwriting a client-supplied header", func(ctx SpecContext) {
		for _, spoof := range []string{"", agentB.String()} {
			s, err := mcpSessionAs(ctx, keyA.Material, spoof)
			Expect(err).NotTo(HaveOccurred())
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "get_approval_status"})
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

	It("reports a config agentgateway rejects", func(ctx SpecContext) {
		bad := standalone.FileDeliverer{Path: filepath.Join(cfgDir, "config.yaml")}
		Expect(bad.Deliver(ctx, []byte("llm: {models: [{name: 1, provider: nope}]}\n"), "")).To(Succeed())
		err := driver.Waiter.Wait(ctx, "never-rendered")
		Expect(err).To(MatchError(standalone.ErrConfigRejected))

		// The last good config keeps serving, and the next Apply recovers.
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(time.Minute))
})

var _ = Describe("golden files", func() {
	It("are accepted by agentgateway --validate-only", func(ctx SpecContext) {
		skipWithoutDocker(ctx)
		dir, err := filepath.Abs("testdata")
		Expect(err).NotTo(HaveOccurred())
		// The full fixture names a provider key file; validation opens it.
		secrets := GinkgoT().TempDir()
		Expect(os.Chmod(secrets, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(secrets, "api_key"), []byte("not-a-real-key"), 0o644)).To(Succeed()) //nolint:gosec // dummy
		for _, f := range fixtures() {
			ctr, err := testcontainers.Run(ctx, AgentgatewayImage,
				testcontainers.WithCmd("--validate-only", "-f", "/cfg/"+f.name+".yaml"),
				testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
					hc.Binds = append(hc.Binds, dir+":/cfg:ro", secrets+":/secrets/upstream:ro")
				}),
				testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForExit()),
			)
			DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
			Expect(err).NotTo(HaveOccurred())
			st, err := ctr.State(ctx)
			Expect(err).NotTo(HaveOccurred())
			logs, _ := ctr.Logs(ctx)
			out, _ := io.ReadAll(logs)
			Expect(st.ExitCode).To(Equal(0), "%s: %s", f.name, out)
		}
	}, NodeTimeout(3*time.Minute))
})
