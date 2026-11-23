// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/orgsettings"
)

// The optional org rule "the approver must not be the requester" (#1175, ADR 0005 amendment
// 2026-11-23). The fixture's task is created by the owner; the admin is a second person.
var _ = Describe("the rule the approver must not be the requester", func() {
	var (
		f     *fixture
		clock *testClock
	)

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx, "standard")
		clock = &testClock{t: time.Now()}
		f.useClock(clock)
	})

	ruleOn := func(ctx context.Context) {
		GinkgoHelper()
		Expect(orgsettings.NewService(pool).SetApproverNotRequester(ctx, f.org, audit.System("koinon-test:spec"), true)).To(Succeed())
	}

	It("is off by default: the requester can approve their own task's call", func(ctx SpecContext) {
		req := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/rule-off"))
		Expect(req.Status).To(Equal("pending"))
		row, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.owner, req.ArgsSHA256, "", approvals.Scope{})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
	})

	It("when on, refuses the requester and keeps the call pending for a second person", func(ctx SpecContext) {
		ruleOn(ctx)
		req := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/rule-on"))
		Expect(req.Status).To(Equal("pending"))

		_, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.owner, req.ArgsSHA256, "", approvals.Scope{})
		Expect(err).To(MatchError(approvals.ErrApproverIsRequester))
		Expect(f.get(ctx, req.ID).Status).To(Equal(store.ApprovalStatusPending), "the refusal changes nothing")

		row, err := f.svc.ApproveScoped(ctx, f.org, mustID(req.ID), f.admin, req.ArgsSHA256, "", approvals.Scope{})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)
	})

	It("when on, an agent scope does not cover a later task that the scope's creator requested", func(ctx SpecContext) {
		By("the admin approves the owner's call with an agent scope")
		first := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/rule-scope-1"))
		row, err := f.svc.ApproveScoped(ctx, f.org, mustID(first.ID), f.admin, first.ArgsSHA256, "",
			approvals.Scope{Kind: approvals.ScopeAgent, Until: clock.Now().Add(14 * 24 * time.Hour)})
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Status).To(Equal(store.ApprovalStatusExecuted), row.Error.String)

		By("the rule is on; the next task is the admin's own task")
		ruleOn(ctx)
		f.nextRun(ctx)
		_, err = pool.Exec(ctx, `UPDATE tasks SET created_by_user_id = $1 WHERE org_id = $2 AND id = $3`, f.admin, f.org, f.task.ID)
		Expect(err).NotTo(HaveOccurred())
		second := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/rule-scope-2"))
		Expect(second.Status).To(Equal("pending"), "the admin's scope does not approve the admin's own task")

		By("a task of another person is still covered")
		f.nextRun(ctx)
		third := f.call(ctx, githubtools.ToolCreatePullRequest, cleanPR("koinon/rule-scope-3"))
		Expect(third.Status).To(Equal("approved"))
	})
})
