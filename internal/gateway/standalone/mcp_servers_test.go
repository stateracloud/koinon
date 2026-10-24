// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

// headerLog records the headers of the requests a fake remote MCP server received.
type headerLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (l *headerLog) add(h http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, h.Clone())
}

func (l *headerLog) values(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, h := range l.seen {
		out = append(out, h.Values(name)...)
	}
	return out
}

// fakeRemote is a remote MCP server with two tools that answers each call with the credential
// header it received, and records every request's headers.
func fakeRemote(name string, log *headerLog, credHeader string) *httptest.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	type args struct{}
	for _, tool := range []string{"read_item", "write_item"} {
		mcp.AddTool(srv, &mcp.Tool{Name: tool, Description: tool, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool == "read_item"}},
			func(_ context.Context, req *mcp.CallToolRequest, _ args) (*mcp.CallToolResult, any, error) {
				got := strings.Join(req.Extra.Header.Values(credHeader), ",")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " saw " + got}}}, nil, nil
			})
	}
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.Header)
		inner.ServeHTTP(w, r)
	}))
}

var _ = Describe("agentgateway v1.6.0 with org MCP servers (plan item MC1)", Ordered, func() {
	var (
		llmURL     string
		ctr        *testcontainers.DockerContainer
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
		skipWithoutDocker(ctx)
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
		dead := httptest.NewServer(http.NotFoundHandler())
		deadPort := port(dead)
		dead.Close()

		host := func(p int) string { return "http://" + testcontainers.HostInternal + ":" + itoa(p) + "/mcp" }
		var err error
		for ref, dst := range map[gateway.AgentRef]*gateway.IssuedKey{agentAcme: &keyAcme, agentAcme2: &keyAcme2, agentGlob: &keyGlobex, agentCross: &keyCross} {
			*dst, err = gateway.NewKey(ref)
			Expect(err).NotTo(HaveOccurred())
		}
		state := gateway.State{MCP: []gateway.MCPTarget{
			{Name: "github", URL: host(port(tools))},
			{Name: "acme-wiki", Org: "acme", URL: host(port(acme)), CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/mcp/acme-wiki-key"},
			{Name: "acme-bearer", Org: "acme", URL: host(port(bearerSrv)), CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: "/secrets/mcp/acme-bearer-key"},
			{Name: "globex-wiki", Org: "globex", URL: host(port(globex)), CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/mcp/globex-wiki-key"},
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

		cfgDir := GinkgoT().TempDir()
		Expect(os.Chmod(cfgDir, 0o755)).To(Succeed())
		secretsDir := GinkgoT().TempDir()
		Expect(os.Chmod(secretsDir, 0o755)).To(Succeed())
		// A trailing newline is part of the file as kubelet writes it only if the Secret has one.
		for name, v := range map[string]string{
			"acme-wiki-key": "not-a-real-token-acme", "acme-bearer-key": "not-a-real-token-bearer", "globex-wiki-key": "not-a-real-token-globex",
		} {
			Expect(os.WriteFile(filepath.Join(secretsDir, name), []byte(v), 0o644)).To(Succeed()) //nolint:gosec // dummy
		}
		body, _, err := standalone.Render(state, standalone.Options{})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(cfgDir, "config.yaml"), body, 0o644)).To(Succeed()) //nolint:gosec // dummy

		ctr, err = testcontainers.Run(ctx, AgentgatewayImage,
			testcontainers.WithCmd("-f", "/cfg/config.yaml"),
			testcontainers.WithExposedPorts("4000/tcp", "15021/tcp"),
			testcontainers.WithHostPortAccess(port(acme), port(bearerSrv), port(globex), port(tools), port(open)),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, cfgDir+":/cfg:ro", secretsDir+":/secrets/mcp:ro")
			}),
			testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
		)
		DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
		Expect(err).NotTo(HaveOccurred())
		llmURL, err = ctr.PortEndpoint(ctx, "4000/tcp", "http")
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
		for _, tool := range []string{"globex-wiki_read_item", "read_item", "globex-wiki_write_item"} {
			res, err := session(ctx, keyCross.Material).CallTool(ctx, &mcp.CallToolParams{Name: tool})
			if err == nil {
				Expect(res.IsError).To(BeTrue(), tool)
			}
		}
		// The globex server was not even contacted for the other org's key.
		Expect(globexLog.values("X-Api-Key")).To(HaveLen(before))
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

type headerSetter struct {
	key, name, value string
	base             http.RoundTripper
}

func (h headerSetter) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set(h.name, h.value)
	return h.base.RoundTrip(r)
}

func itoa(n int) string { return strconv.Itoa(n) }
