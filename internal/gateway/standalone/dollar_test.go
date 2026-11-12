// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// agentgateway's file loader runs shellexpand's environment expansion over the whole config file
// before it parses it (#668). gateway.State.Validate refuses '$' in the org-supplied strings, so no
// State reaches the loader with one; this spec puts '$' in each of them past Validate (Render a
// State with a placeholder in each field, put the '$' text, escaped as Render escapes, in place of
// the placeholder), then checks against the pinned image that each upstream receives the text
// exactly as written. Each value carries the forms the expansion reads:
// $NAME of a set and of an unset variable, ${NAME}, ${NAME:-default}, a positional $1, an escape
// $$, and a trailing '$'. The prefix is a quoted YAML string and carries " #", which would start a
// comment outside the quotes.
var _ = Describe("agentgateway v1.7.0 and '$' in org strings (standalone only, #668)", Ordered, func() {
	const (
		model = "fake/test-model"
		cred  = "koinon_test_not-a-real-token-dollar"
		// Placeholders that pass Validate, each written once into the rendered file.
		phLLM    = "zzdollarllm"
		phMCP    = "zzdollarmcp"
		phHeader = "X-Zzdollarhdr"
		phPrefix = "Zzdollarpfx "
		// What each field carries instead. A header name is an HTTP token: no braces, no space.
		rawLLM    = "l$HOME-${HOME}-$NOPE-${NOPE:-d}-$1-$$-z$"
		rawMCP    = "m$HOME-${HOME}-$NOPE-${NOPE:-d}-$1-$$-z$"
		rawHeader = "X-H$HOME-$NOPE-$1-$$-$"
		rawPrefix = "P $HOME ${HOME} #$NOPE ${NOPE:-d} $1 $$ $ "
	)
	var (
		r       *running
		key     gateway.IssuedKey
		agent   = gateway.AgentRef{Org: "acme", Name: "dollar"}
		mu      sync.Mutex
		llmPath []string
		mcpReqs []*http.Request
	)

	BeforeAll(func(ctx SpecContext) {
		llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			llmPath = append(llmPath, req.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		DeferCleanup(llm.Close)
		srv := mcp.NewServer(&mcp.Implementation{Name: "acme-wiki", Version: "0.0.1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "read_item", Description: "read_item"},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
			})
		inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
			&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			mcpReqs = append(mcpReqs, req.Clone(context.Background()))
			mu.Unlock()
			inner.ServeHTTP(w, req)
		}))
		DeferCleanup(func() { remote.CloseClientConnections(); remote.Close() })
		guard, err := gatewaytest.NewFakeGuard()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(guard.Stop)

		r, err = startStandalone(ctx, []int{portOf(llm), portOf(remote), guard.Port()}, map[string]string{"dollar-key": cred},
			guard.URL(testcontainers.HostInternal))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(r.Stop)
		key, err = gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())

		base := func(s *httptest.Server) string { return "http://" + testcontainers.HostInternal + ":" + itoaPort(s) }
		state := gateway.State{
			Providers: []gateway.Provider{{
				Org: "acme", Name: "dollar", Type: "fake", BaseURL: base(llm) + "/v1/" + phLLM,
				Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
			}},
			MCP: []gateway.MCPTarget{{
				Name: "acme-wiki", Org: "acme", URL: base(remote) + "/mcp/" + phMCP,
				CredentialHeader: phHeader, CredentialPrefix: phPrefix, CredentialFile: "/secrets/upstream/dollar-key",
			}},
		}
		state.AddKey(key.Key)
		state.GrantTools(agent, gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"read_item"}})

		file, rev, err := standalone.Render(state, r.driver.Options)
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.Contains(file, []byte("$"))).To(BeFalse(), "the rendered file has no '$' of its own, so only the substituted values are escaped")
		// Each placeholder is replaced by its raw value as Render would escape it.
		escaped := func(raw string) []byte { return standalone.EscapeLoader([]byte(raw)) }
		for ph, raw := range map[string]string{phLLM: rawLLM, phMCP: rawMCP, phHeader: rawHeader, phPrefix: rawPrefix} {
			Expect(bytes.Count(file, []byte(ph))).To(BeNumerically(">=", 1), "the placeholder %q is in the rendered file", ph)
			file = bytes.ReplaceAll(file, []byte(ph), escaped(raw))
		}
		// The header name also appears lower-cased, where the client's own copy is dropped.
		file = bytes.ReplaceAll(file, []byte(strings.ToLower(phHeader)), escaped(strings.ToLower(rawHeader)))
		Expect(bytes.Contains(bytes.ToLower(file), []byte("zzdollar"))).To(BeFalse(), "every placeholder was replaced")
		AddReportEntry("delivered config", string(file))

		w := standalone.ProbeWaiter{GatewayURL: r.URL, StatsURL: r.statsURL, Interval: 200 * time.Millisecond, Reload: containerReloadLog{logs: r.Logs}}
		prepared := w.Prepare(ctx)
		Expect(r.file.Deliver(ctx, file, rev)).To(Succeed())
		Expect(w.Wait(prepared, rev)).To(Succeed(), "agentgateway loads the escaped file")
	}, NodeTimeout(3*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && r != nil {
			AddReportEntry("agentgateway logs", r.Logs(ctx))
		}
	})

	It("sends the provider URL path as written", func(ctx SpecContext) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))
		mu.Lock()
		defer mu.Unlock()
		Expect(llmPath).To(ContainElement(HavePrefix("/v1/"+rawLLM+"/")), "paths the upstream saw: %q", llmPath)
	})

	It("sends the MCP URL path, the credential header name and its prefix as written", func(ctx SpecContext) {
		client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test", Version: "0.0.1"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: r.URL + "/mcp",
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				req = req.Clone(req.Context())
				req.Header.Set("Authorization", "Bearer "+key.Material)
				return http.DefaultTransport.RoundTrip(req)
			})},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = s.Close() }()
		res, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).NotTo(BeEmpty())

		mu.Lock()
		defer mu.Unlock()
		Expect(mcpReqs).NotTo(BeEmpty())
		for _, req := range mcpReqs {
			Expect(req.URL.Path).To(Equal("/mcp/"+rawMCP), "the MCP URL path")
			var got []string
			for name, v := range req.Header {
				if strings.EqualFold(name, rawHeader) {
					got = append(got, v...)
				}
			}
			Expect(got).To(Equal([]string{rawPrefix + cred}), "the credential under the header %q; headers: %v", rawHeader, req.Header)
		}
	})
})
