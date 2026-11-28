// SPDX-License-Identifier: Apache-2.0

package e2e_test

// The restore drill's MCP credential (issue #1335). test/e2e/backup-restore.sh runs these specs, one per
// process: label restore-seal before the backup, label restore-open after the restore and the release. The
// quickstart pass leaves them out (label restore).
//
// restore-seal adds a server with a fake credential, makes sure the credential is sealed to the gateway
// recipient key (envelope v2), allows the quickstart's agent to call it, and calls a tool through
// koinon-mcp-gateway. restore-open calls the tool again on the restored install: koinon-gateway must open the
// restored credential with the restored key, and the audit row of the call names that key. Then it stores a new
// credential and requires envelope v2: a restore that leaves fewer koinon-gateway copies than the chart expects
// keeps the format gate shut, and new credentials would stay in v1 with no other signal.
//
// Both specs open their own port-forwards: the restore replaces the koinon-api and koinon-mcp-gateway pods, so
// run.sh's forward to koinon-api does not reach the restored install.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
)

const (
	// restoreServer has the credential that the backup holds; restoreAfterServer gets one after the restore.
	restoreServer      = "restore-drill"
	restoreAfterServer = "restore-drill-after"
)

// restoreEnv is what the restore specs share: a kubectl bound to the kind cluster, a client signed in through
// a new port-forward to koinon-api, the default org, and an admin token for the REST calls.
type restoreEnv struct {
	kubectl string
	c       *e2eclient.Client
	org     string
	admin   func(ctx context.Context, method, path string, body ...string) (int, []byte)
}

func newRestoreEnv(ctx context.Context) restoreEnv {
	GinkgoHelper()
	e := restoreEnv{kubectl: os.Getenv("KOINON_E2E_KUBECTL")}
	Expect(e.kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL (a kubectl bound to the e2e cluster's context)")
	Expect(installMCPPath(ctx, e.kubectl).koinon).To(BeTrue(),
		"the restore drill opens a stored credential through koinon-gateway: it needs gateway.mcp.driver=koinon (#1335)")
	e.c, e.org = storiesSignIn(ctx, "http://"+portForward(ctx, e.kubectl, "svc/koinon-api", "8080"))
	tok, err := e.c.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
		OrgId: e.org, Name: "restore-drill-" + strconv.FormatInt(time.Now().UnixNano(), 36), Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
	})
	Expect(err).NotTo(HaveOccurred())
	e.admin = func(ctx context.Context, method, path string, body ...string) (int, []byte) {
		var rdr io.Reader
		if len(body) > 0 {
			rdr = strings.NewReader(body[0])
		}
		req, err := http.NewRequestWithContext(ctx, method, e.c.Base+"/admin/v1"+path, rdr)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tok.GetSecret())
		if rdr != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	return e
}

// serverID is the ID of the org's server with this name, or "".
func (e restoreEnv) serverID(ctx context.Context, name string) string {
	GinkgoHelper()
	got, err := e.c.McpServers.ListServers(ctx, &mcpserverv1alpha1.ListServersRequest{OrgId: e.org})
	Expect(err).NotTo(HaveOccurred())
	for _, s := range got.GetServers() {
		if s.GetName() == name {
			return s.GetId()
		}
	}
	return ""
}

// createServer adds a server with the fake credential (fakeMCPKey: the value the fake MCP server expects,
// obviously not a secret), or returns the one a former run of the drill added.
func (e restoreEnv) createServer(ctx context.Context, name string) string {
	GinkgoHelper()
	if id := e.serverID(ctx, name); id != "" {
		return id
	}
	created, err := e.c.McpServers.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
		OrgId: e.org, Name: name, Url: fakeMCPURL, CredentialHeader: "X-Api-Key", CredentialValue: fakeMCPKey, CaBundle: fakeMCPCA(),
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(created.GetServer().GetHasCredential()).To(BeTrue())
	return created.GetServer().GetId()
}

// credentialEnvelope reads the envelope header of a server's stored credential: its format ("2" is sealed to a
// gateway recipient key) and the key ID it names. It reads no credential byte.
func (e restoreEnv) credentialEnvelope(ctx context.Context, g Gomega, serverID string) (format, keyID string) {
	rows := psqlPrimary(ctx, g, e.kubectl, "SELECT coalesce(credential->>'fmt', ''), coalesce(credential->>'recipient_key_id', '') "+
		"FROM mcp_servers WHERE id = '"+serverID+"' AND credential IS NOT NULL")
	g.Expect(rows).To(HaveLen(1), "the server %s has a stored credential", serverID)
	f := strings.SplitN(rows[0], "|", 2)
	g.Expect(f).To(HaveLen(2), rows[0])
	return f[0], f[1]
}

// drillAgent is the quickstart's active agent (Repo helper). It has a gateway key Secret. The quickstart deletes its
// tracker server at the end, so the drill allows its own server for the agent (restore-seal).
func (e restoreEnv) drillAgent(ctx context.Context) string {
	GinkgoHelper()
	var id string
	Eventually(func(g Gomega) {
		rows := psqlPrimary(ctx, g, e.kubectl, "SELECT id FROM agents WHERE org_id = '"+e.org+"' AND display_name = 'Repo helper' "+
			"AND status = 'active' ORDER BY id LIMIT 1")
		g.Expect(rows).To(HaveLen(1), "the quickstart pass leaves an active agent named Repo helper: run the drill after the quickstart pass")
		id = rows[0]
	}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(5 * time.Second).Should(Succeed())
	return id
}

// lastCallLogID is the newest mcp_call_log row ID, 0 for none.
func (e restoreEnv) lastCallLogID(ctx context.Context) int64 {
	GinkgoHelper()
	var id int64
	Eventually(func(g Gomega) {
		rows := psqlPrimary(ctx, g, e.kubectl, "SELECT coalesce(max(id), 0) FROM mcp_call_log")
		g.Expect(rows).To(HaveLen(1))
		n, err := strconv.ParseInt(rows[0], 10, 64)
		g.Expect(err).NotTo(HaveOccurred())
		id = n
	}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(5 * time.Second).Should(Succeed())
	return id
}

// callRestoreTool calls the restore server's read_item through koinon-mcp-gateway with the agent's own gateway
// key, and requires the fake server's answer. The fake answers only a request that carries the credential, so a
// credential that the gateway cannot open makes the call fail: the gateway refuses it, and nothing goes upstream.
// After since (an mcp_call_log ID) the audit row of the call must be ok and name keyID, the gateway key that
// opened the credential.
func (e restoreEnv) callRestoreTool(ctx context.Context, agentID, nonce, keyID string) {
	GinkgoHelper()
	since := e.lastCallLogID(ctx)
	key := agentGatewayKey(ctx, e.kubectl, e.org, agentID)
	path := installMCPPath(ctx, e.kubectl)
	local := portForward(ctx, e.kubectl, path.service, path.port)
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-e2e-restore", Version: "0"}, nil)
	// The gateway learns the agent's allowance and the server from the reconciler's State: after a change, or on
	// pods that a restore just started, that takes a few passes.
	Eventually(func(g Gomega) {
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: "http://" + local + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{key: key}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		g.Expect(err).NotTo(HaveOccurred())
		defer func() { _ = s.Close() }()
		tools, err := s.ListTools(ctx, nil)
		g.Expect(err).NotTo(HaveOccurred())
		name := ""
		for _, t := range tools.Tools {
			if strings.HasSuffix(t.Name, "-"+restoreServer+"_read_item") {
				name = t.Name
			}
		}
		g.Expect(name).NotTo(BeEmpty(), "the agent sees %s's read_item through koinon-gateway", restoreServer)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"title": nonce}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsError).To(BeFalse(), "the call failed; its content says why: %s", toolText(res))
		g.Expect(toolText(res)).To(ContainSubstring("read_item ok: " + nonce))
	}).WithContext(ctx).WithTimeout(racescale.D(3 * time.Minute)).WithPolling(5 * time.Second).Should(Succeed())

	By("the fake server saw the call with the credential")
	fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMCP_URL"), "/")
	Expect(fake).NotTo(BeEmpty(), "KOINON_E2E_FAKEMCP_URL")
	Expect(fakeMCPRequests(ctx, fake)).To(ContainElement(SatisfyAll(
		HaveField("Tool", "read_item"), HaveField("Args", ContainSubstring(nonce)), HaveField("Authorized", true))))

	By("the audit row of the call names the gateway key that opened the credential")
	Eventually(func(g Gomega) {
		rows := psqlPrimary(ctx, g, e.kubectl, fmt.Sprintf("SELECT outcome, credential_kind, recipient_key_id FROM mcp_call_log "+
			"WHERE id > %d AND target LIKE '%%%s' AND tool LIKE '%%read_item' ORDER BY id DESC LIMIT 1", since, restoreServer))
		g.Expect(rows).To(Equal([]string{"ok|static|" + keyID}))
	}).WithContext(ctx).WithTimeout(racescale.D(time.Minute)).WithPolling(5 * time.Second).Should(Succeed())
}

var _ = Describe("the restore drill's MCP credential", Label("restore"), func() {
	It("seals a fake credential to the gateway key and calls a tool with it, before the backup", Label("restore-seal"), NodeTimeout(racescale.D(19*time.Minute)), func(ctx SpecContext) {
		e := newRestoreEnv(ctx)

		By("a server with a fake credential is added, and the prober marks it healthy")
		id := e.createServer(ctx, restoreServer)
		Eventually(func(g Gomega) {
			got, err := e.c.McpServers.ListServers(ctx, &mcpserverv1alpha1.ListServersRequest{OrgId: e.org})
			g.Expect(err).NotTo(HaveOccurred())
			for _, s := range got.GetServers() {
				if s.GetId() == id {
					g.Expect(s.GetHealth()).To(Equal(mcpserverv1alpha1.Health_HEALTH_HEALTHY), s.GetHealthReason())
				}
			}
			lt, err := e.c.McpServers.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: e.org, ServerId: id})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(lt.GetTools()).To(ContainElement(HaveField("Name", "read_item")))
		}).WithContext(ctx).WithTimeout(racescale.D(8 * time.Minute)).WithPolling(5 * time.Second).Should(Succeed())

		By("the credential is sealed to the active gateway recipient key (envelope v2)")
		// koinon-api writes v2 once the format gate has been open for its hold. A credential written before that is
		// in v1: keys seal-mcp moves it, and refuses while the gate is shut.
		var keyID string
		Eventually(func(g Gomega) {
			format, k := e.credentialEnvelope(ctx, g, id)
			if format != "2" {
				out, err := runKubectl(ctx, e.kubectl, "-n", "koinon", "exec", "deploy/koinon-api", "--", "koinon-api", "keys", "seal-mcp", "--yes")
				g.Expect(err).NotTo(HaveOccurred(), "keys seal-mcp: %s", out)
				format, k = e.credentialEnvelope(ctx, g, id)
			}
			g.Expect(format).To(Equal("2"), "the credential of %s is not sealed to a gateway key", restoreServer)
			keyID = k
		}).WithContext(ctx).WithTimeout(racescale.D(6 * time.Minute)).WithPolling(15 * time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(psqlPrimary(ctx, g, e.kubectl, "SELECT state FROM gateway_recipient_keys WHERE key_id = '"+keyID+"'")).
				To(Equal([]string{"active"}), "the credential names the active gateway key")
		}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(5 * time.Second).Should(Succeed())

		By("read_item runs without approval, and the quickstart's agent may call the server")
		setToolTierE2E(ctx, e.admin, id, "read_item", "auto")
		// A read-only lookup with no address arguments: sink class none, so the leak guard has nothing to stop.
		setToolSinkE2E(ctx, e.admin, id, "read_item", `{"sinkClass":"none","derivation":"fixed","addressArgs":[]}`)
		agent := e.drillAgent(ctx)
		cur, err := e.c.McpServers.GetAgentServers(ctx, &mcpserverv1alpha1.GetAgentServersRequest{OrgId: e.org, AgentId: agent})
		Expect(err).NotTo(HaveOccurred())
		allowed := cur.GetServers()
		has := false
		for _, a := range allowed {
			has = has || a.GetServerId() == id
		}
		if !has {
			// The set replaces the list: keep the quickstart's servers.
			allowed = append(allowed, &mcpserverv1alpha1.Allowance{ServerId: id})
			_, err = e.c.McpServers.SetAgentServers(ctx, &mcpserverv1alpha1.SetAgentServersRequest{OrgId: e.org, AgentId: agent, Servers: allowed})
			Expect(err).NotTo(HaveOccurred())
		}

		By("a call through koinon-mcp-gateway opens the credential with the gateway key")
		e.callRestoreTool(ctx, agent, "restore-drill-before-backup", keyID)
		_, _ = fmt.Fprintf(GinkgoWriter, "restore drill: %s's credential is sealed to %s\n", restoreServer, keyID)
	})

	It("opens the restored credential through koinon-gateway, and seals a new one in v2, after the restore", Label("restore-open"), NodeTimeout(racescale.D(19*time.Minute)), func(ctx SpecContext) {
		e := newRestoreEnv(ctx)

		By("the restored server still holds its credential in envelope v2")
		id := e.serverID(ctx, restoreServer)
		Expect(id).NotTo(BeEmpty(), "the restore brought back the server %s (restore-seal adds it before the backup)", restoreServer)
		var keyID string
		Eventually(func(g Gomega) {
			format, k := e.credentialEnvelope(ctx, g, id)
			g.Expect(format).To(Equal("2"))
			keyID = k
		}).WithContext(ctx).WithTimeout(racescale.D(30 * time.Second)).WithPolling(5 * time.Second).Should(Succeed())

		By("a call through koinon-mcp-gateway opens the restored credential with the restored gateway key")
		e.callRestoreTool(ctx, e.drillAgent(ctx), "restore-drill-after-restore", keyID)

		By("a credential stored after the restore is sealed to the gateway key (envelope v2)")
		// The format gate opens only when every expected copy of koinon-api, the reconciler and koinon-gateway
		// reports v2, and then after its hold. A restore that leaves fewer copies than the chart expects keeps it
		// shut, and this credential stays in v1. Each write while the gate holds is v1, so write again until v2.
		after := e.createServer(ctx, restoreAfterServer)
		DeferCleanup(func(ctx context.Context) {
			_, _ = e.c.McpServers.DeleteServer(ctx, &mcpserverv1alpha1.DeleteServerRequest{OrgId: e.org, ServerId: after})
		})
		Eventually(func(g Gomega) {
			format, _ := e.credentialEnvelope(ctx, g, after)
			if format != "2" {
				value := fakeMCPKey
				_, err := e.c.McpServers.UpdateServer(ctx, &mcpserverv1alpha1.UpdateServerRequest{OrgId: e.org, ServerId: after, CredentialValue: &value})
				g.Expect(err).NotTo(HaveOccurred())
				format, _ = e.credentialEnvelope(ctx, g, after)
			}
			g.Expect(format).To(Equal("2"), "a credential written after the restore is not in v2: is the format gate shut (fewer copies than the chart expects)?")
		}).WithContext(ctx).WithTimeout(racescale.D(6 * time.Minute)).WithPolling(15 * time.Second).Should(Succeed())
	})
})

// runKubectl runs kubectl and returns its combined output.
func runKubectl(ctx context.Context, kubectl string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, kubectl, args...).CombinedOutput()
	return string(out), err
}
