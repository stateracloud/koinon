// SPDX-License-Identifier: Apache-2.0

package mcpask_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
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

// fakeGateway stands in for agentgateway on the executor's path: it serves the tool the gateway would
// name "<target>_<tool>" and records the raw arguments, the bearer key and how often it was called.
type fakeGateway struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string // raw arguments of each tools/call
	keys  []string
	meta  []string
	// dropCall makes the gateway close the connection on a tools/call, after reading it.
	dropCall bool
}

func newFakeGateway(tool string) *fakeGateway {
	g := &fakeGateway{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: tool, InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.calls = append(g.calls, string(req.Params.Arguments))
			g.keys = append(g.keys, req.Extra.Header.Get("Authorization"))
			if m, ok := req.Params.Meta["dev.koinon/approval"].(string); ok {
				g.meta = append(g.meta, m)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "created #7"}}}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.dropCall && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"tools/call"`) {
				g.mu.Lock()
				g.calls = append(g.calls, "dropped")
				g.mu.Unlock()
				hj, _ := w.(http.Hijacker)
				c, _, _ := hj.Hijack()
				_ = c.Close()
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		h.ServeHTTP(w, r)
	}))
	return g
}

func (g *fakeGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

type staticKeys struct{ key string }

func (k staticKeys) Key(context.Context, pgtype.UUID) (string, error) { return k.key, nil }

var _ = Describe("the ask-tier proxy", func() {
	var (
		org, user pgtype.UUID
		slug      string
		agent     store.Agent
		run       store.Run
		svc       *mcpservers.Service
		server    mcpservers.Server
		gw        *fakeGateway
		proxy     *httptest.Server
	)

	BeforeEach(func(ctx SpecContext) {
		org, user, slug = pgtest.NewOrg(ctx, pool)
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"instructions":"x"}`), CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: "fix", AgentID: agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent.ID, TaskID: t.ID, MaxWallClockSeconds: 1800})
		Expect(err).NotTo(HaveOccurred())
		claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "test", LeaseSeconds: 60})
		Expect(err).NotTo(HaveOccurred())
		run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())

		gw = newFakeGateway(slug + "-tracker_create_issue")
		DeferCleanup(func() { gw.CloseClientConnections(); go gw.Close() })
		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		server, err = svc.Create(ctx, org, audit.System("koinon-test:spec"), user, mcpservers.Input{
			Name: "tracker", URL: "https://mcp.example.com/mcp", CredentialHeader: "X-Api-Key", CredentialValue: "not-a-real-token",
		})
		Expect(err).NotTo(HaveOccurred())
		sid := uuidOf(server.ID)
		_, err = svc.SyncTools(ctx, org, sid, []mcpservers.ListedTool{
			{Name: "create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
			{Name: "search", Description: "Search"},
			{Name: "drop_table", Description: "Drop"},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "search", "auto")
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "drop_table", "never")
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
		Expect(gw.count()).To(BeZero())

		rows, err := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: org, TaskID: run.TaskID})
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
		rows, _ = q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: org, TaskID: run.TaskID})
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
		rows, _ := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: org, TaskID: run.TaskID})
		Expect(rows).To(BeEmpty())
	})

	exec := func() *approvals.Service {
		return approvals.New(pool, approvals.Options{Keys: ring, GatewayMCPURL: gw.URL + "/mcp", ExecKeys: staticKeys{key: "not-a-real-key"}})
	}
	request := func(ctx SpecContext, args map[string]any) store.ApprovalRequest {
		GinkgoHelper()
		_, err := session(ctx, me()).CallTool(ctx, &mcp.CallToolParams{Name: "create_issue", Arguments: args})
		Expect(err).NotTo(HaveOccurred())
		rows, _ := q.ListApprovalRequestsForTask(ctx, store.ListApprovalRequestsForTaskParams{OrgID: org, TaskID: run.TaskID})
		Expect(rows).NotTo(BeEmpty())
		return rows[len(rows)-1]
	}

	It("lets Koinon execute the approved call through the gateway with the executor key and the exact arguments, once", func(ctx SpecContext) {
		// A big integer and 1.0 must arrive as approved, not decoded into float64 and re-encoded.
		row := request(ctx, map[string]any{"title": "b", "big": json.Number("12345678901234567890"), "ratio": json.Number("1.0")})
		Expect(row.ArgsJson).To(ContainSubstring(`"big":12345678901234567890`))
		Expect(row.ArgsJson).To(ContainSubstring(`"ratio":1.0`))
		got, err := exec().Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusExecuted), got.Error.String)
		Expect(gw.calls).To(Equal([]string{row.ArgsJson}), "the gateway saw exactly the approved bytes, once")
		Expect(gw.keys).To(Equal([]string{"Bearer not-a-real-key"}))
		Expect(gw.meta).To(Equal([]string{row.ID.String()}))
		Expect(string(got.Result)).To(ContainSubstring("created #7"))
		Expect(string(got.Result)).NotTo(ContainSubstring("not-a-real"))
		By("executing again does nothing")
		_, err = exec().Execute(ctx, org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.count()).To(Equal(1))
	})

	It("never runs the call twice: after the request was sent a lost answer is a failure with an unknown outcome", func(ctx SpecContext) {
		row := request(ctx, map[string]any{"title": "b"})
		gw.dropCall = true
		got, err := exec().Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusFailed))
		Expect(got.Error.String).To(ContainSubstring("outcome unknown"))
		Expect(gw.count()).To(Equal(1))
		gw.dropCall = false
		_, err = exec().Execute(ctx, org, row.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.count()).To(Equal(1), "a failed request is not retried")
	})

	It("retries a call that failed before it was sent", func(ctx SpecContext) {
		row := request(ctx, map[string]any{"title": "b"})
		down := approvals.New(pool, approvals.Options{Keys: ring, GatewayMCPURL: "http://127.0.0.1:1/mcp", ExecKeys: staticKeys{key: "k"}, ExecutionLease: time.Second})
		got, err := down.Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusApproved), "left approved for the sweeper to retry")
		Eventually(func(g Gomega) {
			got, err = exec().Execute(ctx, org, row.ID) // the sweeper's retry, once the lease has passed
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Status).To(Equal(store.ApprovalStatusExecuted), got.Error.String)
		}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
		Expect(gw.count()).To(Equal(1))
	})

	It("refuses a call whose server, tool version, tier or allowance changed since the request", func(ctx SpecContext) {
		sid := uuidOf(server.ID)
		change := map[string]func(ctx SpecContext){
			"never tier": func(ctx SpecContext) {
				_, err := svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", "never")
				Expect(err).NotTo(HaveOccurred())
			},
			"changed description": func(ctx SpecContext) {
				_, err := svc.SyncTools(ctx, org, sid, []mcpservers.ListedTool{{Name: "create_issue", Description: "Create an issue. Now with a twist.", InputSchema: json.RawMessage(`{"type":"object"}`)}})
				Expect(err).NotTo(HaveOccurred())
			},
			"allowance removed": func(ctx SpecContext) {
				Expect(q.DeleteAgentMCPServers(ctx, store.DeleteAgentMCPServersParams{OrgID: org, AgentID: agent.ID})).To(Succeed())
			},
			"tool no longer offered": func(ctx SpecContext) {
				_, err := svc.SyncTools(ctx, org, sid, []mcpservers.ListedTool{{Name: "search", Description: "Search"}})
				Expect(err).NotTo(HaveOccurred())
			},
			"new URL": func(ctx SpecContext) {
				u := "https://mcp2.example.com/mcp"
				_, err := svc.Update(ctx, org, audit.System("koinon-test:spec"), server.ID, mcpservers.Patch{URL: &u})
				Expect(err).NotTo(HaveOccurred())
			},
		}
		for name, fn := range change {
			By(name)
			row := request(ctx, map[string]any{"title": name})
			fn(ctx)
			got, err := exec().Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(store.ApprovalStatusFailed), name)
			// Put the world back for the next case.
			_, _ = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", "ask")
			_ = q.InsertAgentMCPServer(ctx, store.InsertAgentMCPServerParams{OrgID: org, AgentID: agent.ID, ServerID: sid})
			u := "https://mcp.example.com/mcp"
			_, _ = svc.Update(ctx, org, audit.System("koinon-test:spec"), server.ID, mcpservers.Patch{URL: &u})
			_, _ = svc.SyncTools(ctx, org, sid, []mcpservers.ListedTool{{Name: "create_issue", Description: "Create an issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)}})
			_, _ = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", "ask")
		}
		Expect(gw.count()).To(BeZero())
	})

	It("runs an approved call whose tool has moved to the auto tier since: a person approved this exact call", func(ctx SpecContext) {
		// The gateway opens the executor's key to the auto tier too, for the leak guard's cards (ADR
		// 0091), so the call reaches the server; only the never tier stops an approved call.
		row := request(ctx, map[string]any{"title": "b"})
		_, err := svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", "auto")
		Expect(err).NotTo(HaveOccurred())
		got, err := exec().Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status).To(Equal(store.ApprovalStatusExecuted), got.Error.String)
		Expect(gw.calls).To(Equal([]string{row.ArgsJson}))
	})

	Describe("scoped approvals", func() {
		sid := func() pgtype.UUID { return uuidOf(server.ID) }
		scoped := func(ctx SpecContext) store.ApprovalScope {
			GinkgoHelper()
			row := request(ctx, map[string]any{"title": "first"})
			_, err := exec().ApproveScoped(ctx, org, row.ID, user, row.ArgsSha256, "", approvals.Scope{Kind: approvals.ScopeAgent, Until: time.Now().Add(7 * 24 * time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			scopes, err := q.ListActiveApprovalScopesForAgent(ctx, store.ListActiveApprovalScopesForAgentParams{OrgID: org, AgentID: agent.ID, Now: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(scopes).To(HaveLen(1))
			Expect(scopes[0].CallBinding).NotTo(BeEmpty(), "pinned to the version that was approved")
			return scopes[0]
		}

		It("lets the next call of the same server and tool run, and names the scope on the timeline", func(ctx SpecContext) {
			sc := scoped(ctx)
			next := request(ctx, map[string]any{"title": "second"})
			Expect(next.Status).To(Equal(store.ApprovalStatusApproved))
			Expect(next.ScopeID).To(Equal(sc.ID))
			var found bool
			events, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: org, TaskID: run.TaskID, AfterSeq: 0, MaxEvents: 100})
			Expect(err).NotTo(HaveOccurred())
			for _, ev := range events {
				var p map[string]any
				if ev.Kind == approvals.KindRequested && json.Unmarshal(ev.Payload, &p) == nil && p["scope_id"] == sc.ID.String() {
					Expect(p).To(HaveKeyWithValue("scope_kind", "agent"))
					found = true
				}
			}
			Expect(found).To(BeTrue(), "the requested event carries the scope")
		})

		It("parks the call again once the server republishes the tool or its URL changes", func(ctx SpecContext) {
			scoped(ctx)
			Expect(request(ctx, map[string]any{"title": "same version"}).Status).To(Equal(store.ApprovalStatusApproved))

			_, err := svc.SyncTools(ctx, org, sid(), []mcpservers.ListedTool{{Name: "create_issue", Description: "Create an issue. Now also deletes things.", InputSchema: json.RawMessage(`{"type":"object"}`)}})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.SetTier(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", "ask") // an admin put it back to ask without reviewing
			Expect(err).NotTo(HaveOccurred())
			Expect(request(ctx, map[string]any{"title": "after change"}).Status).To(Equal(store.ApprovalStatusPending))

			By("the URL changing parks it too")
			u := "https://mcp2.example.com/mcp"
			_, err = svc.Update(ctx, org, audit.System("koinon-test:spec"), server.ID, mcpservers.Patch{URL: &u})
			Expect(err).NotTo(HaveOccurred())
			Expect(request(ctx, map[string]any{"title": "after url"}).Status).To(Equal(store.ApprovalStatusPending))
		})

		It("never covers a tool that needs two approvers", func(ctx SpecContext) {
			scoped(ctx)
			_, err := svc.SetTwoApprovers(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", true)
			Expect(err).NotTo(HaveOccurred())
			next := request(ctx, map[string]any{"title": "two"})
			Expect(next.Status).To(Equal(store.ApprovalStatusPending))
			Expect(next.ScopeID.Valid).To(BeFalse())
			Expect(next.ApprovalsRequired).To(BeEquivalentTo(2))
		})
	})

	Describe("two approvers", func() {
		var second, third pgtype.UUID
		BeforeEach(func(ctx SpecContext) {
			mk := func() pgtype.UUID {
				_, u, _ := pgtest.NewOrg(ctx, pool)
				return u
			}
			second, third = mk(), mk()
			_, err := svc.SetTwoApprovers(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", true)
			Expect(err).NotTo(HaveOccurred())
		})

		It("needs two distinct approvers, neither the task's creator, and runs once after the second", func(ctx SpecContext) {
			row := request(ctx, map[string]any{"title": "b"})
			Expect(row.ApprovalsRequired).To(BeEquivalentTo(2))
			a := exec()

			_, err := a.Approve(ctx, org, row.ID, user, row.ArgsSha256, "")
			Expect(err).To(MatchError(approvals.ErrRequesterApproves), "the task's creator is user")

			got, err := a.Approve(ctx, org, row.ID, second, row.ArgsSha256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(store.ApprovalStatusPending), "one approval is not enough")
			Expect(got.FirstApprover).To(Equal(second))
			Expect(gw.count()).To(BeZero())
			var first int
			Expect(pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'approval.first_approve' AND actor_id = $2`,
				org, second.String()).Scan(&first)).To(Succeed())
			Expect(first).To(Equal(1))

			_, err = a.Approve(ctx, org, row.ID, second, row.ArgsSha256, "")
			Expect(err).To(MatchError(approvals.ErrSameApprover))
			Expect(gw.count()).To(BeZero())

			got, err = a.Approve(ctx, org, row.ID, third, row.ArgsSha256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(store.ApprovalStatusExecuted), got.Error.String)
			Expect(got.DecidedBy).To(Equal(third))
			Expect(gw.count()).To(Equal(1))
		})

		It("covers a request recorded before the flag was turned on", func(ctx SpecContext) {
			_, err := svc.SetTwoApprovers(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", false)
			Expect(err).NotTo(HaveOccurred())
			row := request(ctx, map[string]any{"title": "early"})
			Expect(row.ApprovalsRequired).To(BeEquivalentTo(1))
			_, err = svc.SetTwoApprovers(ctx, org, audit.System("koinon-test:spec"), server.ID, "create_issue", true)
			Expect(err).NotTo(HaveOccurred())
			got, err := exec().Approve(ctx, org, row.ID, second, row.ArgsSha256, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(store.ApprovalStatusPending), "one approval is not enough any more")
			Expect(got.ApprovalsRequired).To(BeEquivalentTo(2))
			Expect(gw.count()).To(BeZero())
		})

		It("lets either approver deny, and one approval of a one-approver tool still suffices", func(ctx SpecContext) {
			row := request(ctx, map[string]any{"title": "deny me"})
			_, err := exec().Approve(ctx, org, row.ID, second, row.ArgsSha256, "")
			Expect(err).NotTo(HaveOccurred())
			got, err := exec().Deny(ctx, org, row.ID, third, "no")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status).To(Equal(store.ApprovalStatusDenied))
			Expect(gw.count()).To(BeZero())
		})
	})
})

func uuidOf(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
