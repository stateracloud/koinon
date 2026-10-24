// SPDX-License-Identifier: Apache-2.0

package mcpask_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

type agentHeader struct{ value string }

func (a agentHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(gateway.AgentHeader, a.value)
	return http.DefaultTransport.RoundTrip(r)
}

// remote is a fake remote MCP server that records the credential header of the calls it gets.
type remote struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	meta  []string
}

func newRemote() *remote {
	r := &remote{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "create_issue", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, req.Extra.Header.Get("X-Api-Key"))
			r.meta = append(r.meta, req.Params.Meta["dev.koinon/approval"].(string))
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "created #7 from " + string(req.Params.Arguments)}}}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	r.Server = httptest.NewServer(h)
	return r
}

var _ = Describe("the ask-tier proxy", func() {
	var (
		org, user pgtype.UUID
		slug      string
		agent     store.Agent
		run       store.Run
		svc       *mcpservers.Service
		server    mcpservers.Server
		remoteSrv *remote
		proxy     *httptest.Server
	)

	BeforeEach(func(ctx SpecContext) {
		org, user, slug = pgtest.NewOrg(ctx, pool)
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Flavor: store.AgentFlavorOpencode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"instructions":"x"}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		t, err := q.CreateTicket(ctx, store.CreateTicketParams{OrgID: org, Title: "fix", AgentID: agent.ID, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent.ID, TicketID: t.ID, MaxWallClockSeconds: 1800, MaxTokens: 200_000})
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())

		remoteSrv = newRemote()
		DeferCleanup(func() { remoteSrv.CloseClientConnections(); go remoteSrv.Close() })
		u, _ := url.Parse(remoteSrv.URL)
		policy := catalog.URLPolicy{AllowedHosts: []string{u.Host}}
		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(policy)
		server, err = svc.Create(ctx, org, audit.System(), user, mcpservers.Input{
			Name: "tracker", URL: remoteSrv.URL + "/mcp", CredentialHeader: "X-Api-Key", CredentialValue: "not-a-real-token",
		})
		Expect(err).NotTo(HaveOccurred())
		sid := uuidOf(server.ID)
		_, err = svc.SyncTools(ctx, org, sid, []mcpservers.ListedTool{
			{Name: "create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
			{Name: "search", Description: "Search"},
			{Name: "drop_table", Description: "Drop"},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SetTier(ctx, org, audit.System(), server.ID, "search", "auto")
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SetTier(ctx, org, audit.System(), server.ID, "drop_table", "never")
		Expect(err).NotTo(HaveOccurred())
		Expect(q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: org, AgentID: agent.ID, ServerID: sid})).To(Succeed())

		mux := http.NewServeMux()
		mux.Handle(mcpask.Path, mcpask.New(pool, mcpask.Options{}).Handler())
		proxy = httptest.NewServer(mux)
		DeferCleanup(func() { proxy.CloseClientConnections(); go proxy.Close() })
	})

	session := func(ctx SpecContext, who string) *mcp.ClientSession {
		GinkgoHelper()
		client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: proxy.URL + mcpask.Path + "tracker", HTTPClient: &http.Client{Transport: agentHeader{value: who}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = s.Close() })
		return s
	}
	names := func(ctx SpecContext, s *mcp.ClientSession) []string {
		GinkgoHelper()
		res, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		var out []string
		for _, t := range res.Tools {
			out = append(out, t.Name)
		}
		return out
	}
	me := func() string { return slug + "/" + agent.ID.String() }

	It("offers the ask-tier tools with the server's schema, and no other tier's", func(ctx SpecContext) {
		s := session(ctx, me())
		Expect(names(ctx, s)).To(ConsistOf("create_issue", "get_approval_status"))
		res, _ := s.ListTools(ctx, nil)
		for _, t := range res.Tools {
			if t.Name == "create_issue" {
				raw, _ := json.Marshal(t.InputSchema)
				Expect(string(raw)).To(ContainSubstring(`"title"`))
				Expect(t.Description).To(ContainSubstring("Needs human approval"))
				Expect(t.Description).To(ContainSubstring("Create an issue"))
			}
		}
	})

	It("offers nothing to an agent that is not allowed the server, or to a stranger", func(ctx SpecContext) {
		Expect(q.DeleteAgentMCPServers(ctx, store.DeleteAgentMCPServersParams{OrgID: org, AgentID: agent.ID})).To(Succeed())
		Expect(names(ctx, session(ctx, me()))).To(BeEmpty())
		Expect(names(ctx, session(ctx, "nobody/00000000-0000-0000-0000-000000000000"))).To(BeEmpty())
		Expect(names(ctx, session(ctx, "not an agent"))).To(BeEmpty())
	})

	It("narrows to the tools an allowance names", func(ctx SpecContext) {
		Expect(q.DeleteAgentMCPServers(ctx, store.DeleteAgentMCPServersParams{OrgID: org, AgentID: agent.ID})).To(Succeed())
		Expect(q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: org, AgentID: agent.ID, ServerID: uuidOf(server.ID), Tools: []string{"search"}})).To(Succeed())
		Expect(names(ctx, session(ctx, me()))).To(ConsistOf("get_approval_status"))
	})

	It("records a call as an approval request and answers at once, never touching the server", func(ctx SpecContext) {
		s := session(ctx, me())
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "create_issue", Arguments: map[string]any{"title": "b", "a": 1}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		Expect(res.Content[0].(*mcp.TextContent).Text).To(HavePrefix("approval requested: id="))
		Expect(remoteSrv.calls).To(BeEmpty())

		rows, err := q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: org, TicketID: run.TicketID})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].Connector).To(Equal("mcp"))
		Expect(rows[0].Target).To(Equal("mcp:tracker"))
		Expect(rows[0].Tool).To(Equal("create_issue"))
		Expect(rows[0].ArgsJson).To(Equal(`{"a":1,"title":"b"}`), "canonical: sorted keys")
		Expect(rows[0].Status).To(Equal(store.ApprovalStatusPending))

		By("the same call again is the same request")
		_, err = s.CallTool(ctx, &mcp.CallToolParams{Name: "create_issue", Arguments: map[string]any{"a": 1, "title": "b"}})
		Expect(err).NotTo(HaveOccurred())
		rows, _ = q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: org, TicketID: run.TicketID})
		Expect(rows).To(HaveLen(1))
	})

	It("refuses a tool that is not in the ask tier", func(ctx SpecContext) {
		s := session(ctx, me())
		for _, tool := range []string{"search", "drop_table", "nope"} {
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool})
			if err == nil {
				Expect(res.IsError).To(BeTrue(), tool)
			}
		}
		rows, _ := q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: org, TicketID: run.TicketID})
		Expect(rows).To(BeEmpty())
	})

	It("lets Koinon execute the approved call with the credential, and only then", func(ctx SpecContext) {
		s := session(ctx, me())
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "create_issue", Arguments: map[string]any{"title": "b"}})
		Expect(err).NotTo(HaveOccurred())
		rows, _ := q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: org, TicketID: run.TicketID})
		Expect(rows).To(HaveLen(1))

		u, _ := url.Parse(remoteSrv.URL)
		svcApprovals := approvals.New(pool, approvals.Options{Keys: ring, URLPolicy: catalog.URLPolicy{AllowedHosts: []string{u.Host}}})
		got, err := svcApprovals.Approve(ctx, org, rows[0].ID, user, rows[0].ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusExecuted), got.Error.String)
		Expect(remoteSrv.calls).To(Equal([]string{"not-a-real-token"}))
		Expect(remoteSrv.meta).To(Equal([]string{rows[0].ID.String()}))
		Expect(string(got.Result)).To(ContainSubstring("created #7"))
		Expect(strings.Contains(string(got.Result), "not-a-real-token")).To(BeFalse())
	})

	It("does not execute a call whose tool an admin has since moved to never", func(ctx SpecContext) {
		s := session(ctx, me())
		_, _ = s.CallTool(ctx, &mcp.CallToolParams{Name: "create_issue", Arguments: map[string]any{"title": "b"}})
		rows, _ := q.ListApprovalRequestsForTicket(ctx, store.ListApprovalRequestsForTicketParams{OrgID: org, TicketID: run.TicketID})
		_, err := svc.SetTier(ctx, org, audit.System(), server.ID, "create_issue", "never")
		Expect(err).NotTo(HaveOccurred())
		u, _ := url.Parse(remoteSrv.URL)
		svcApprovals := approvals.New(pool, approvals.Options{Keys: ring, URLPolicy: catalog.URLPolicy{AllowedHosts: []string{u.Host}}})
		got, err := svcApprovals.Approve(ctx, org, rows[0].ID, user, rows[0].ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(got.Error.String).To(ContainSubstring("never tier"))
		Expect(remoteSrv.calls).To(BeEmpty())
	})
})

func uuidOf(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
