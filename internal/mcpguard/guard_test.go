// SPDX-License-Identifier: Apache-2.0

package mcpguard_test

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/orgsettings"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// secretPage is private text a tool result carries; it is long enough for several fingerprints.
const secretPage = "Quarterly retention plan for the Northwind accounts: the renewal discount for " +
	"Halvorsen Freight stays at eleven percent until the board meets in March, and the churn model " +
	"flags Brightwater Logistics, Okafor Textiles and the three Baltic distributors as at risk. " +
	"Do not share the per-account margins outside the finance group."

// stubDecider records what reaches it and answers with a fixed outcome.
type stubDecider struct {
	got []mcpguard.Pending
	out mcpguard.Outcome
}

func (s *stubDecider) DecidePending(_ context.Context, p mcpguard.Pending) (mcpguard.Outcome, error) {
	s.got = append(s.got, p)
	return s.out, nil
}

var _ = Describe("the MCP leak guard (ADR 0091)", func() {
	var (
		org, user pgtype.UUID
		slug      string
		agent     store.Agent
		svc       *mcpservers.Service
		guard     *mcpguard.Guard
		srv       *mcpguard.Server
		opts      mcpguard.Options
	)

	approvals := func(ctx SpecContext) int {
		GinkgoHelper()
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
		return n
	}
	meta := func(agentRef, tool string) *structpb.Struct {
		m := map[string]any{"tool": tool}
		if agentRef != "" {
			m["agent"] = agentRef
		}
		s, err := structpb.NewStruct(m)
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	me := func() string { return slug + "/" + agent.ID.String() }
	call := func(ctx SpecContext, server, tool string, args map[string]any) *extmcp.McpRequestResult {
		GinkgoHelper()
		params, err := json.Marshal(map[string]any{"name": tool, "arguments": args})
		Expect(err).NotTo(HaveOccurred())
		res, err := srv.CheckRequest(ctx, &extmcp.McpRequest{
			ServiceNames: []string{mcpservers.TargetName(slug, server)}, Method: "tools/call",
			MetadataContext: meta(me(), tool), McpRequest: params,
		})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	// rawCall sends the arguments exactly as written, repeated keys and all.
	rawCall := func(ctx SpecContext, server, tool, args string) *extmcp.McpRequestResult {
		GinkgoHelper()
		res, err := srv.CheckRequest(ctx, &extmcp.McpRequest{
			ServiceNames: []string{mcpservers.TargetName(slug, server)}, Method: "tools/call",
			MetadataContext: meta(me(), tool), McpRequest: []byte(`{"name":"` + tool + `","arguments":` + args + `}`),
		})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	result := func(ctx SpecContext, server, tool, text string) *extmcp.McpResponseResult {
		GinkgoHelper()
		body, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
		Expect(err).NotTo(HaveOccurred())
		res, err := srv.CheckResponse(ctx, &extmcp.McpResponse{
			ServiceNames: []string{mcpservers.TargetName(slug, server)}, Method: "tools/call",
			MetadataContext: meta(me(), tool), McpResponse: body,
		})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	passed := func(r *extmcp.McpRequestResult) bool { return r.GetPass() != nil }
	reason := func(r *extmcp.McpRequestResult) string { return r.GetError().GetReason() }
	declare := func(ctx SpecContext, server mcpservers.Server, tool string, d sinkclass.Declaration) {
		GinkgoHelper()
		tools, err := svc.Tools(ctx, org, server.ID)
		Expect(err).NotTo(HaveOccurred())
		hash := ""
		for _, t := range tools {
			if t.Name == tool {
				hash = t.ContentHash
			}
		}
		Expect(hash).NotTo(BeEmpty(), tool)
		_, err = svc.SetToolDeclaration(ctx, org, audit.System(), server.ID, tool, hash, d)
		Expect(err).NotTo(HaveOccurred())
	}
	newServer := func(ctx SpecContext, name string, legs []string, tools ...string) mcpservers.Server {
		GinkgoHelper()
		s, err := svc.Create(ctx, org, audit.System(), user, mcpservers.Input{Name: name, URL: "https://" + name + ".example.com/mcp"})
		Expect(err).NotTo(HaveOccurred())
		var listed []mcpservers.ListedTool
		for _, t := range tools {
			listed = append(listed, mcpservers.ListedTool{Name: t, Description: t})
		}
		_, err = svc.SyncTools(ctx, org, uuidOf(s.ID), listed)
		Expect(err).NotTo(HaveOccurred())
		for _, t := range tools {
			_, err = svc.SetTier(ctx, org, audit.System(), s.ID, t, "auto")
			Expect(err).NotTo(HaveOccurred())
		}
		if legs != nil {
			s, err = svc.SetDataClass(ctx, org, audit.System(), s.ID, mcpservers.Declaration{Kind: mcpservers.KindOther, Legs: legs})
			Expect(err).NotTo(HaveOccurred())
		}
		return s
	}

	var tracker, wiki, web mcpservers.Server

	BeforeEach(func(ctx SpecContext) {
		org, user, slug = pgtest.NewOrg(ctx, pool)
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Flavor: store.AgentFlavorOpencode, CreatedBy: user})
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
		_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: claimed.ID, Epoch: claimed.Epoch})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: org, Workspace: "w-" + slug})
		Expect(err).NotTo(HaveOccurred())
		names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
		_, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
			OrgID: org, AgentID: agent.ID, Workspace: "w-" + slug,
			SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{OrgID: org, AgentID: agent.ID, State: store.SandboxStateReady, HomeGeneration: "gen-" + slug})
		Expect(err).NotTo(HaveOccurred())

		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		// tracker: both legs (the default declaration), an external-high tool, an external-low
		// fetch tool and a code-host write.
		tracker = newServer(ctx, "tracker", nil, "post_message", "read_issue", "fetch_page", "commit_file", "announce", "ping")
		declare(ctx, tracker, "fetch_page", sinkclass.Declaration{Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}})
		declare(ctx, tracker, "commit_file", sinkclass.Declaration{Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}})
		// announce and ping name a channel, but their class is fixed: the channel is data.
		declare(ctx, tracker, "announce", sinkclass.Declaration{Class: sinkclass.ExternalHigh, Derivation: sinkclass.Fixed, AddressArgs: []string{"channel"}})
		declare(ctx, tracker, "ping", sinkclass.Declaration{Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed, AddressArgs: []string{"channel"}})
		// wiki: private data only.
		wiki = newServer(ctx, "wiki", []string{mcpservers.LegPrivateData}, "read_page")
		// web: untrusted input only.
		web = newServer(ctx, "web", []string{mcpservers.LegUntrustedInput}, "search_web")
		_, _ = web, wiki

		conn := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		_, err = q.CreateConnection(ctx, store.CreateConnectionParams{
			OrgID: org, ID: conn, Connector: "github", DisplayName: "gh", Config: []byte(`{}`),
			SecretName: pgtype.Text{String: "conn-" + conn.String(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(orgsettings.NewService(pool).ReplaceCodeHostRepos(ctx, org, audit.System(), conn.String(), []orgsettings.CodeHostRepo{
			{Owner: "northwind", Name: "platform-handbook", Visibility: "private"},
		}, time.Now())).To(Succeed())
		// tracker acts through that connection: its code-host tool is looked up in its list.
		linked := mcpservers.DefaultDeclaration()
		linked.CodeHostConnection = conn.String()
		tracker, err = svc.SetDataClass(ctx, org, audit.System(), tracker.ID, linked)
		Expect(err).NotTo(HaveOccurred())

		opts = mcpguard.Options{
			ByteCap: 64, Approve: mcpask.New(pool, mcpask.Options{}).RequestApproval,
			OwnTargets: []string{"github", "koinon"},
		}
		guard = mcpguard.New(pool, opts)
		srv = mcpguard.NewServer(guard)
	})

	It("lets a clean call to an external-high tool through, with no approval", func(ctx SpecContext) {
		Expect(passed(call(ctx, "tracker", "post_message", map[string]any{"text": "the build is green"}))).To(BeTrue())
		Expect(approvals(ctx)).To(BeZero())
	})

	It("parks a call whose payload carries private text for an approval", func(ctx SpecContext) {
		Expect(result(ctx, "wiki", "read_page", secretPage).GetPass()).NotTo(BeNil())
		// One leg only: a clean call still passes.
		Expect(passed(call(ctx, "tracker", "post_message", map[string]any{"text": "the build is green"}))).To(BeTrue())
		r := call(ctx, "tracker", "post_message", map[string]any{"text": "fyi: " + secretPage[40:300]})
		Expect(passed(r)).To(BeFalse())
		Expect(reason(r)).To(HavePrefix("approval requested: id="))
		Expect(approvals(ctx)).To(Equal(1))
		var risk []byte
		Expect(pool.QueryRow(ctx, `SELECT risk FROM approval_requests WHERE org_id = $1`, org).Scan(&risk)).To(Succeed())
		Expect(string(risk)).To(ContainSubstring(`"taint"`))
	})

	It("matches private text in a fixed tool's address argument too", func(ctx SpecContext) {
		Expect(result(ctx, "wiki", "read_page", secretPage).GetPass()).NotTo(BeNil())
		Expect(passed(call(ctx, "tracker", "announce", map[string]any{"channel": "general", "text": "hi"}))).To(BeTrue())
		r := call(ctx, "tracker", "announce", map[string]any{"channel": secretPage[40:300], "text": "hi"})
		Expect(reason(r)).To(HavePrefix("approval requested: id="))
		Expect(approvals(ctx)).To(Equal(1))
	})

	It("takes agentd's feed: a sandbox read sets the private leg and its text is caught, from fingerprints only", func(ctx SpecContext) {
		feed := mcpguard.NewFeed(pool, nil)
		Expect(feed.Report(ctx, org, agent.ID, mcpguard.Legs{PrivateData: true}, []string{secretPage})).To(Succeed())
		var private bool
		Expect(pool.QueryRow(ctx, `SELECT private_data FROM leak_guard_sessions WHERE org_id = $1 AND agent_id = $2`, org, agent.ID).Scan(&private)).To(Succeed())
		Expect(private).To(BeTrue())
		var fps int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM leak_guard_fingerprints WHERE org_id = $1`, org).Scan(&fps)).To(Succeed())
		Expect(fps).To(BeNumerically(">", 0))

		Expect(passed(call(ctx, "tracker", "post_message", map[string]any{"text": "the build is green"}))).To(BeTrue())
		r := call(ctx, "tracker", "post_message", map[string]any{"text": "fyi: " + secretPage[40:300]})
		Expect(reason(r)).To(HavePrefix("approval requested"))
		var risk []byte
		Expect(pool.QueryRow(ctx, `SELECT risk FROM approval_requests WHERE org_id = $1`, org).Scan(&risk)).To(Succeed())
		Expect(string(risk)).To(ContainSubstring(`"taint"`))
	})

	It("refuses agentd's feed for an agent with no home generation", func(ctx SpecContext) {
		other := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		Expect(mcpguard.NewFeed(pool, nil).Report(ctx, org, other, mcpguard.Legs{PrivateData: true}, nil)).To(MatchError(mcpguard.ErrNoGeneration))
	})

	When("the session holds both legs", func() {
		BeforeEach(func(ctx SpecContext) {
			Expect(result(ctx, "tracker", "read_issue", "an issue someone outside filed").GetPass()).NotTo(BeNil())
		})

		It("lets the internal write to northwind/platform-handbook through", func(ctx SpecContext) {
			r := call(ctx, "tracker", "commit_file", map[string]any{"owner": "northwind", "repo": "platform-handbook", "content": secretPage})
			Expect(passed(r)).To(BeTrue(), reason(r))
			Expect(approvals(ctx)).To(BeZero())
		})

		It("treats a repository outside the scope, or an address that does not parse, as external-high", func(ctx SpecContext) {
			r := call(ctx, "tracker", "commit_file", map[string]any{"owner": "someone-else", "repo": "dump", "content": "x"})
			Expect(reason(r)).To(HavePrefix("approval requested"))
			r = call(ctx, "tracker", "commit_file", map[string]any{"owner": "northwind/x", "repo": "platform-handbook", "content": "y"})
			Expect(reason(r)).To(HavePrefix("approval requested"))
		})

		It("treats the scope's own repository as external-high on a server linked to no code-host connection", func(ctx SpecContext) {
			_, err := svc.SetDataClass(ctx, org, audit.System(), tracker.ID, mcpservers.DefaultDeclaration())
			Expect(err).NotTo(HaveOccurred())
			r := call(ctx, "tracker", "commit_file", map[string]any{"owner": "northwind", "repo": "platform-handbook", "content": "x"})
			Expect(reason(r)).To(HavePrefix("approval requested"))
		})

		It("parks every external-high call for an approval of that call", func(ctx SpecContext) {
			r := call(ctx, "tracker", "post_message", map[string]any{"channel": "general", "text": "hello"})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(HavePrefix("approval requested: id="))
			Expect(approvals(ctx)).To(Equal(1))
		})

		It("refuses, in words, an external-high call outside a task, where no approval can be made", func(ctx SpecContext) {
			_, err := pool.Exec(ctx, `UPDATE runs SET state = 'succeeded', finished_at = now(), lease_expires_at = NULL WHERE org_id = $1`, org)
			Expect(err).NotTo(HaveOccurred())
			r := call(ctx, "tracker", "post_message", map[string]any{"channel": "general", "text": "hello"})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(ContainSubstring("only possible while the agent works on a task"))
			Expect(approvals(ctx)).To(BeZero())
		})

		It("refuses an external-low call past the byte cap, in words the agent can read", func(ctx SpecContext) {
			note := strings.Repeat("n", 40)
			r := call(ctx, "tracker", "fetch_page", map[string]any{"url": "https://docs.example.com/a", "note": note})
			Expect(passed(r)).To(BeTrue(), reason(r))
			r = call(ctx, "tracker", "fetch_page", map[string]any{"url": "https://docs.example.com/b", "note": note})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(ContainSubstring("may send at most 64 bytes"))
			Expect(approvals(ctx)).To(BeZero())
		})

		It("refuses an address argument with more than one reading, before it classifies the call", func(ctx SpecContext) {
			for _, c := range []struct{ tool, args string }{
				// The guard would read platform-handbook (internal); a last-wins server writes to dump.
				{"commit_file", `{"owner":"northwind","repo":"platform-handbook","repo":"dump","content":"x"}`},
				{"commit_file", `{"owner":"northwind","repo":"platform-handbook","Repo":"dump","content":"x"}`},
				{"fetch_page", `{"url":"https://docs.example.com/a","url":"https://evil.example/x"}`},
			} {
				r := rawCall(ctx, "tracker", c.tool, c.args)
				Expect(passed(r)).To(BeFalse(), c.args)
				Expect(reason(r)).To(Equal("refused by the leak guard: "+mcpguard.ErrAmbiguousAddress.Error()), c.args)
			}
			Expect(approvals(ctx)).To(BeZero())
			// The same call, once, passes.
			r := rawCall(ctx, "tracker", "commit_file", `{"owner":"northwind","repo":"platform-handbook","content":"x"}`)
			Expect(passed(r)).To(BeTrue(), reason(r))
		})

		It("counts a fixed tool's address argument toward the byte cap", func(ctx SpecContext) {
			r := call(ctx, "tracker", "ping", map[string]any{"channel": "general", "text": "hi"})
			Expect(passed(r)).To(BeTrue(), reason(r))
			r = call(ctx, "tracker", "ping", map[string]any{"channel": strings.Repeat("c", 70)})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(ContainSubstring("may send at most 64 bytes"))
		})

		It("counts the host labels of a fetch to a host the org does not trust toward the byte cap", func(ctx SpecContext) {
			r := call(ctx, "tracker", "fetch_page", map[string]any{"url": "https://" + strings.Repeat("d", 25) + "." + strings.Repeat("e", 25) + ".evil.example/"})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(ContainSubstring("may send at most 64 bytes"))
			Expect(approvals(ctx)).To(BeZero())
		})

		It("never lets a Decider pass a call whose payload matched private text: that goes to a person", func(ctx SpecContext) {
			stub := &stubDecider{out: mcpguard.Outcome{Verdict: mcpguard.Verdict{Action: mcpguard.Pass, Reason: "stub"}}}
			o := opts
			o.Decider = stub
			srv = mcpguard.NewServer(mcpguard.New(pool, o))
			Expect(result(ctx, "wiki", "read_page", secretPage).GetPass()).NotTo(BeNil())
			r := call(ctx, "tracker", "post_message", map[string]any{"text": "fyi: " + secretPage[40:300]})
			Expect(passed(r)).To(BeFalse())
			Expect(reason(r)).To(HavePrefix("approval requested: id="))
			Expect(approvals(ctx)).To(Equal(1))
			Expect(stub.got).To(BeEmpty())
			// Both legs alone, with no match, is what the Decider may decide.
			Expect(passed(call(ctx, "tracker", "post_message", map[string]any{"text": "the build is green"}))).To(BeTrue())
			Expect(stub.got).To(HaveLen(1))
		})

		It("routes a call the rules leave open through the Decider", func(ctx SpecContext) {
			stub := &stubDecider{out: mcpguard.Outcome{Verdict: mcpguard.Verdict{Action: mcpguard.Refuse, Reason: "stub"}, Text: "stub says no"}}
			o := opts
			o.Decider = stub
			srv = mcpguard.NewServer(mcpguard.New(pool, o))
			r := call(ctx, "tracker", "post_message", map[string]any{"channel": "general", "text": "hello"})
			Expect(reason(r)).To(Equal("stub says no"))
			Expect(stub.got).To(HaveLen(1))
			Expect(stub.got[0].Verdict.Action).To(Equal(mcpguard.NeedsDecision))
			Expect(stub.got[0].Verdict.Reasons).To(ConsistOf(mcpguard.ReasonBothLegs))
			Expect(stub.got[0].Destination).To(BeEmpty())
			Expect(stub.got[0].PayloadBytes).To(BeNumerically(">", 0))
			Expect(approvals(ctx)).To(BeZero())
			// A clean pass and the internal write never reach it.
			call(ctx, "tracker", "commit_file", map[string]any{"owner": "northwind", "repo": "platform-handbook", "content": "x"})
			Expect(stub.got).To(HaveLen(1))
		})
	})

	It("refuses a call without an agent identity", func(ctx SpecContext) {
		params, _ := json.Marshal(map[string]any{"name": "post_message", "arguments": map[string]any{}})
		r, err := srv.CheckRequest(ctx, &extmcp.McpRequest{
			ServiceNames: []string{mcpservers.TargetName(slug, "tracker")}, Method: "tools/call",
			MetadataContext: meta("", "post_message"), McpRequest: params,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.GetError().GetCode()).To(Equal(extmcp.AuthorizationError_PERMISSION_DENIED))
	})

	It("lets Koinon's own tools through and refuses an unknown target", func(ctx SpecContext) {
		params, _ := json.Marshal(map[string]any{"name": "ask_user", "arguments": map[string]any{}})
		r, err := srv.CheckRequest(ctx, &extmcp.McpRequest{ServiceNames: []string{"koinon"}, Method: "tools/call", MetadataContext: meta(me(), "ask_user"), McpRequest: params})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.GetPass()).NotTo(BeNil())
		r, err = srv.CheckRequest(ctx, &extmcp.McpRequest{ServiceNames: []string{"elsewhere"}, Method: "tools/call", MetadataContext: meta(me(), "x"), McpRequest: params})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.GetError()).NotTo(BeNil())
	})

	It("fails closed when it cannot reach its database", func(ctx SpecContext) {
		cfg := pool.Config()
		broken, err := pgxpool.NewWithConfig(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		broken.Close()
		srv = mcpguard.NewServer(mcpguard.New(broken, opts))
		r := call(ctx, "tracker", "post_message", map[string]any{"text": "hello"})
		Expect(r.GetError().GetCode()).To(Equal(extmcp.AuthorizationError_PERMISSION_DENIED))
		Expect(reason(r)).To(ContainSubstring("could not check this call"))
		res := result(ctx, "wiki", "read_page", secretPage)
		Expect(res.GetError()).NotTo(BeNil())
		Expect(res.GetError().GetReason()).To(ContainSubstring("withheld"))
	})
})

func uuidOf(s string) pgtype.UUID {
	var u pgtype.UUID
	Expect(u.Scan(s)).To(Succeed())
	return u
}
