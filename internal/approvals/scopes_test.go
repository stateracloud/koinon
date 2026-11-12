// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
)

// testClock is a clock the specs move.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// cleanPR is a pull request without push-and-run risk (docs only).
func cleanPR(branch string) map[string]any {
	return map[string]any{
		"branch": branch, "title": "Weekly update", "body": "The weekly change.",
		"files": []map[string]any{{"path": "docs/weekly.md", "content": "# Weekly\n" + branch + "\n"}},
	}
}

// useClock replaces the fixture's tool server with one on the given clock.
func (f *fixture) useClock(c *testClock) {
	GinkgoHelper()
	tools, err := githubtools.New(pool, githubtools.Options{
		Now: c.Now,
		Execute: func(ctx context.Context, org, id pgtype.UUID) {
			_, _ = f.svc.Execute(ctx, org, id)
		},
		LeakCheck: cleanLeakCheck,
	})
	Expect(err).NotTo(HaveOccurred())
	f.tools = httptest.NewServer(tools.Handler())
	DeferCleanup(f.tools.Close)
}

// nextRun ends the current run and starts a run of the same agent on a new task, as the next
// scheduled run would.
func (f *fixture) nextRun(ctx context.Context) {
	GinkgoHelper()
	_, err := q.FinishRun(ctx, store.FinishRunParams{OrgID: f.org, ID: f.run.ID, Epoch: f.run.Epoch, Outcome: store.RunStateSucceeded})
	Expect(err).NotTo(HaveOccurred())
	f.task, err = q.CreateTask(ctx, store.CreateTaskParams{OrgID: f.org, Title: "weekly update", AgentID: f.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: f.owner})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: f.org, AgentID: f.agent.ID, TaskID: f.task.ID, MaxWallClockSeconds: 1800})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: f.org, OwnerID: "agentd-test", LeaseSeconds: 600})
	Expect(err).NotTo(HaveOccurred())
	f.run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: f.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
}

func (f *fixture) auditActions(ctx context.Context, like string) []string {
	GinkgoHelper()
	rows, err := pool.Query(ctx, `SELECT action FROM audit_events WHERE org_id = $1 AND action LIKE $2 ORDER BY chain_seq`, f.org, like)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		Expect(rows.Scan(&a)).To(Succeed())
		out = append(out, a)
	}
	return out
}

var _ = Describe("Scoped approvals", func() {
	var (
		f     *fixture
		clock *testClock
	)

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx, "standard")
		clock = &testClock{t: time.Now()}
		f.useClock(clock)
	})

	approveWith := func(ctx context.Context, req githubtools.Result, sc approvals.Scope) store.ApprovalRequest {
		GinkgoHelper()
		row, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", sc)
		Expect(err).NotTo(HaveOccurred())
		return row
	}

	It("lets an agent scope carry the next run's call, until it is revoked or ends", func(ctx SpecContext) {
		By("run 1: the first call parks for approval; the admin approves it for this agent for two weeks")
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-1"))
		Expect(first.Status).To(Equal("pending"))
		row := approveWith(ctx, first, approvals.Scope{Kind: approvals.ScopeAgent, Until: clock.Now().Add(14 * 24 * time.Hour)})
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
		Expect(row.ScopeID.Valid).To(BeFalse(), "the approving request itself was decided by a person")

		By("the scope is listed on the agent and audited")
		scopes, err := f.svc.ListScopes(ctx, f.org, f.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes).To(HaveLen(1))
		sc := scopes[0]
		Expect(sc.Kind).To(Equal(store.ApprovalScopeKindAgent))
		Expect(sc.Connector).To(Equal("github"))
		Expect(sc.Tool).To(Equal(githubtools.ToolCreatePullRequest))
		Expect(sc.Target).To(Equal("github:acme/widgets"))
		Expect(sc.CreatedBy).To(Equal(f.admin))
		Expect(f.auditActions(ctx, "approval_scope.%")).To(Equal([]string{"approval_scope.create"}))

		By("run 2 (a later task): the same kind of call does not park, and the timeline names the scope")
		f.nextRun(ctx)
		second := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-2"))
		Expect(second.Status).To(Equal("approved"))
		Eventually(func() store.ApprovalStatus { return f.get(ctx, second.ID).Status }).
			WithTimeout(10 * time.Second).Should(Equal(store.ApprovalStatusExecuted))
		req2 := f.get(ctx, second.ID)
		Expect(req2.ScopeID).To(Equal(sc.ID))
		Expect(req2.Decision).To(Equal(store.ApprovalDecisionAllow))
		pending, err := q.ListPendingApprovalRequests(ctx, f.org)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty())
		var requested map[string]any
		for _, ev := range f.taskEvents(ctx) {
			if ev.Kind == approvals.KindRequested {
				Expect(json.Unmarshal(ev.Payload, &requested)).To(Succeed())
			}
		}
		Expect(requested).To(HaveKeyWithValue("scope_id", sc.ID.String()))
		Expect(requested).To(HaveKeyWithValue("scope_kind", "agent"))
		Expect(f.gh.Snapshot("widgets").Pulls).To(HaveLen(2))
		Expect(f.gh.Snapshot("widgets").Commits[f.gh.Snapshot("widgets").Refs[f.branch("lcm-week-2")]].Message).To(ContainSubstring("(scoped approval)"))

		By("a different tool, or a call that could push and run code, still needs a person")
		comment := f.call(ctx, githubtools.ToolCommentOnIssue, map[string]any{"issue_number": 3, "body": "hi"})
		Expect(comment.Status).To(Equal("pending"))
		risky := cleanPR("koinon/lcm-week-2b")
		risky["files"] = []map[string]any{{"path": "scripts/run.sh", "content": "#!/bin/sh\nexit 0\n"}}
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, risky).Status).To(Equal("pending"))

		By("revoking it: run 3's call parks again")
		revoked, err := f.svc.RevokeScope(ctx, f.org, sc.ID, f.admin)
		Expect(err).NotTo(HaveOccurred())
		Expect(revoked.RevokedAt.Valid).To(BeTrue())
		_, err = f.svc.RevokeScope(ctx, f.org, sc.ID, f.owner)
		Expect(err).NotTo(HaveOccurred(), "revoking twice is a no-op")
		Expect(f.auditActions(ctx, "approval_scope.%")).To(Equal([]string{"approval_scope.create", "approval_scope.revoke"}))
		scopes, err = f.svc.ListScopes(ctx, f.org, f.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes).To(BeEmpty())
		f.nextRun(ctx)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-3")).Status).To(Equal("pending"))
	})

	It("ends a scope on its date", func(ctx SpecContext) {
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-1"))
		approveWith(ctx, first, approvals.Scope{Kind: approvals.ScopeAgent, Until: clock.Now().Add(7 * 24 * time.Hour)})

		f.nextRun(ctx)
		clock.Add(6 * 24 * time.Hour)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-2")).Status).To(Equal("approved"))

		f.nextRun(ctx)
		clock.Add(2 * 24 * time.Hour)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-3")).Status).To(Equal("pending"))
	})

	It("keeps a task scope to its task", func(ctx SpecContext) {
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-part-1"))
		approveWith(ctx, first, approvals.Scope{Kind: approvals.ScopeTask})

		By("another call in the same task runs")
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-part-2")).Status).To(Equal("approved"))

		By("another task needs its own approval")
		f.nextRun(ctx)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-part-3")).Status).To(Equal("pending"))
	})

	It("does not let a scope granted on one connection cover the same repository on another host", func(ctx SpecContext) {
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-host-1"))
		approveWith(ctx, first, approvals.Scope{Kind: approvals.ScopeAgent, Until: clock.Now().Add(24 * time.Hour)})
		scopes, err := f.svc.ListScopes(ctx, f.org, f.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(scopes[0].CallBinding)).To(ContainSubstring("connectionId"))

		By("the same connection still covers it")
		f.nextRun(ctx)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-host-2")).Status).To(Equal("approved"))

		By("the connection now points at another host: the scope no longer applies")
		_, err = pool.Exec(ctx, `UPDATE connections SET config = config || '{"baseUrl":"https://ghes.example.test"}'::jsonb WHERE org_id = $1`, f.org)
		Expect(err).NotTo(HaveOccurred())
		f.nextRun(ctx)
		Expect(f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-host-3")).Status).To(Equal("pending"))
	})

	It("does not let a scope in one org or for one agent cover another", func(ctx SpecContext) {
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-1"))
		approveWith(ctx, first, approvals.Scope{Kind: approvals.ScopeAgent, Until: clock.Now().Add(24 * time.Hour)})
		other := newFixture(ctx, "standard")
		scopes, err := other.svc.ListScopes(ctx, other.org, f.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes).To(BeEmpty())
		_, err = other.svc.RevokeScope(ctx, other.org, mustScopeID(ctx, f), other.admin)
		Expect(err).To(MatchError(approvals.ErrScopeNotFound))
	})

	It("refuses to widen a request that was already approved, instead of silently creating no scope", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-replay"))
		_, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", approvals.Scope{})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", approvals.Scope{Kind: approvals.ScopeAgent, Until: time.Now().Add(24 * time.Hour)})
		Expect(err).To(MatchError(approvals.ErrNotPending))
		scopes, err := f.svc.ListScopes(ctx, f.org, f.agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(scopes).To(BeEmpty())
	})

	It("refuses scopes without a usable date and unknown kinds", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-1"))
		for _, sc := range []approvals.Scope{
			{Kind: approvals.ScopeAgent},
			{Kind: approvals.ScopeAgent, Until: time.Now().Add(-time.Hour)},
			{Kind: approvals.ScopeAgent, Until: time.Now().Add(2 * 366 * 24 * time.Hour)},
			{Kind: approvals.ScopeTask, Until: time.Now().Add(60 * 24 * time.Hour)},
			{Kind: "forever"},
		} {
			_, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", sc)
			Expect(err).To(MatchError(approvals.ErrScopeInvalid), "%+v", sc)
		}
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusPending))
	})

	It("does not widen the approval of a request that needs two approvers", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/lcm-week-1"))
		_, err := pool.Exec(ctx, `UPDATE approval_requests SET approvals_required = 2 WHERE org_id = $1 AND id = $2`, f.org, mustID(req.ID))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", approvals.Scope{Kind: approvals.ScopeTask})
		Expect(err).To(MatchError(approvals.ErrScopeNotAllowed))
	})
})

func mustScopeID(ctx context.Context, f *fixture) pgtype.UUID {
	GinkgoHelper()
	scopes, err := f.svc.ListScopes(ctx, f.org, f.agent.ID)
	Expect(err).NotTo(HaveOccurred())
	Expect(scopes).NotTo(BeEmpty())
	return scopes[0].ID
}
