// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/test/scenarios"
)

const (
	fakeMCPURL = "http://fakemcp.koinon-e2e-fakes.svc:8080/mcp"
	fakeMCPKey = "not-a-real-token"
)

type fakeMCPRecord struct {
	Method              string `json:"method"`
	MCPMethod           string `json:"mcpMethod"`
	Authorized          bool   `json:"authorized"`
	Agent               string `json:"agent"`
	ClientAuthorization bool   `json:"clientAuthorization"`
	Tool                string `json:"tool"`
	Args                string `json:"args"`
}

func fakeMCPRequests(ctx context.Context, base string) []fakeMCPRecord {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/_fake/requests", nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	var out []fakeMCPRecord
	Expect(json.NewDecoder(res.Body).Decode(&out)).To(Succeed())
	return out
}

type bearerTransport struct{ key string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpServerE2E registers the fake remote MCP server through the admin API and follows it through the
// whole path: refused OAuth server, probe through the real gateway (which injects the credential),
// tool sync into the ask tier, tiers, the agent's allowlist, and a tool call by the agent's own
// gateway key.
func mcpServerE2E(ctx context.Context, admin func(context.Context, string, string, ...string) (int, []byte), org, agentID string) string {
	GinkgoHelper()
	fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMCP_URL"), "/")
	kubectl := os.Getenv("KOINON_E2E_KUBECTL")
	Expect(fake).NotTo(BeEmpty(), "KOINON_E2E_FAKEMCP_URL")
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL (a kubectl bound to the e2e cluster's context)")

	By("a server that asks each user to sign in is refused")
	code, body := admin(ctx, http.MethodPost, "/mcp-servers", `{"name":"oauthy","url":"http://fakemcp.koinon-e2e-fakes.svc:8080/oauth-mcp"}`)
	Expect(code).To(Equal(http.StatusBadRequest), string(body))
	Expect(string(body)).To(ContainSubstring("OAuth"))

	By("a server with a credential is added")
	code, body = admin(ctx, http.MethodPost, "/mcp-servers",
		fmt.Sprintf(`{"name":"tracker","url":%q,"credentialHeader":"X-Api-Key","credentialValue":%q}`, fakeMCPURL, fakeMCPKey))
	Expect(code).To(Equal(http.StatusCreated), string(body))
	var srv struct {
		ID            string `json:"id"`
		HasCredential bool   `json:"hasCredential"`
	}
	Expect(json.Unmarshal(body, &srv)).To(Succeed())
	Expect(srv.HasCredential).To(BeTrue())
	Expect(string(body)).NotTo(ContainSubstring(fakeMCPKey))

	By("the reconciler serves it, the prober lists its tools through the gateway and marks it healthy")
	// A new credential reaches the gateway through the kubelet's Secret sync, then the config reloads.
	Eventually(func(g Gomega) {
		code, body := admin(ctx, http.MethodGet, "/mcp-servers/"+srv.ID)
		g.Expect(code).To(Equal(http.StatusOK), string(body))
		var s struct {
			Health, HealthReason string
			ToolsSyncedAt        *time.Time
		}
		g.Expect(json.Unmarshal(body, &s)).To(Succeed())
		g.Expect(s.Health).To(Equal("healthy"), s.HealthReason)
		g.Expect(s.ToolsSyncedAt).NotTo(BeNil())
	}).WithContext(ctx).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	code, body = admin(ctx, http.MethodGet, "/mcp-servers/"+srv.ID+"/tools")
	Expect(code).To(Equal(http.StatusOK), string(body))
	var tools struct {
		Tools []struct {
			Name         string `json:"name"`
			Tier         string `json:"tier"`
			ReadOnlyHint bool   `json:"readOnlyHint"`
		} `json:"tools"`
	}
	Expect(json.Unmarshal(body, &tools)).To(Succeed())
	Expect(tools.Tools).To(ConsistOf(
		HaveField("Name", "read_item"), HaveField("Name", "write_item")))
	for _, tl := range tools.Tools {
		Expect(tl.Tier).To(Equal("ask"), "every new tool starts in the ask tier")
		Expect(tl.ReadOnlyHint).To(Equal(tl.Name == "read_item"))
	}

	By("the gateway injected the credential on everything it sent, and did not forward the prober's key")
	recs := fakeMCPRequests(ctx, fake)
	Expect(recs).NotTo(BeEmpty())
	anonymous := 0
	for _, r := range recs {
		if r.Agent == "" {
			// koinon-api's add-time OAuth check: direct, and it carries no credential.
			Expect(r.Authorized).To(BeFalse(), "the add-time check must not carry the credential: %+v", r)
			anonymous++
			continue
		}
		Expect(r.Authorized).To(BeTrue(), "every request the gateway sent carried the credential: %+v", r)
		Expect(r.ClientAuthorization).To(BeFalse(), "the gateway's own key must not reach the server: %+v", r)
	}
	Expect(anonymous).To(BeNumerically(">=", 1), "the OAuth check reached the server anonymously")
	Expect(recs).To(ContainElement(HaveField("Agent", "default/mcp-probe-tracker")))

	By("an admin confirms read_item as auto and allows the agent")
	code, body = admin(ctx, http.MethodPut, "/mcp-servers/"+srv.ID+"/tools/read_item/tier", `{"tier":"auto"}`)
	Expect(code).To(Equal(http.StatusOK), string(body))
	code, body = admin(ctx, http.MethodPut, "/agents/"+agentID+"/mcp-servers", fmt.Sprintf(`{"servers":[{"serverId":%q}]}`, srv.ID))
	Expect(code).To(Equal(http.StatusOK), string(body))

	By("the agent's own gateway key sees read_item directly and write_item only behind approval")
	key := agentGatewayKey(ctx, kubectl, org, agentID)
	local := portForward(ctx, kubectl, "svc/koinon-gateway", "8080")
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-e2e", Version: "0"}, nil)
	var session *mcp.ClientSession
	var names []string
	Eventually(func(g Gomega) {
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: "http://" + local + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{key: key}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		g.Expect(err).NotTo(HaveOccurred())
		res, err := s.ListTools(ctx, nil)
		g.Expect(err).NotTo(HaveOccurred())
		names = names[:0]
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		g.Expect(names).To(ContainElement(HaveSuffix("default-tracker_read_item")))
		session = s
	}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	DeferCleanup(func() { _ = session.Close() })
	Expect(names).NotTo(ContainElement(HaveSuffix("default-tracker_write_item")), "an ask-tier tool is not on the server's own target")
	Expect(names).To(ContainElement(HaveSuffix(scenarios.PullRequest.TrackerAskTool)), "it is behind the approval proxy")

	By("a call to the auto tool goes through the gateway to the server, with the credential and the agent's identity")
	var read string
	for _, n := range names {
		if strings.HasSuffix(n, "default-tracker_read_item") {
			read = n
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: read, Arguments: map[string]any{"title": "from the e2e"}})
	Expect(err).NotTo(HaveOccurred())
	Expect(res.IsError).To(BeFalse())
	Expect(res.Content[0].(*mcp.TextContent).Text).To(ContainSubstring("read_item ok: from the e2e"))
	var called *fakeMCPRecord
	for _, r := range fakeMCPRequests(ctx, fake) {
		if r.Tool == "read_item" {
			called = &r
		}
	}
	Expect(called).NotTo(BeNil(), "the server saw the tool call")
	Expect(called.Agent).To(Equal("default/" + agentID))
	Expect(called.Authorized).To(BeTrue())
	return srv.ID
}

// agentGatewayKey reads the agent's gateway key from its Secret, with a kubectl bound to the e2e
// cluster. The key never leaves this process.
func agentGatewayKey(ctx context.Context, kubectl, org, agentID string) string {
	GinkgoHelper()
	var o, a pgtype.UUID
	Expect(o.Scan(org)).To(Succeed())
	Expect(a.Scan(agentID)).To(Succeed())
	name := execution.AgentObjectNames(o.Bytes, a.Bytes).KeySecret
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "secret", name, "-o", "jsonpath={.data.api_key}").Output()
	Expect(err).NotTo(HaveOccurred())
	raw, err := base64.StdEncoding.DecodeString(string(out))
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// portForward forwards a free local port to target's port in the koinon namespace until the spec ends.
func portForward(ctx context.Context, kubectl, target, port string) string {
	GinkgoHelper()
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	_, p, _ := net.SplitHostPort(l.Addr().String())
	Expect(l.Close()).To(Succeed())
	cmd := exec.CommandContext(context.WithoutCancel(ctx), kubectl, "-n", "koinon", "port-forward", target, p+":"+port)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := "127.0.0.1:" + p
	Eventually(func() error {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err
	}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
	return addr
}
