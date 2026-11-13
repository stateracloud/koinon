// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1/budgetv1alpha1connect"
)

var _ = Describe("BudgetService and PriceService", func() {
	var (
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
	})
	budgets, priceSvc := budgetClient, priceClient

	It("lets admins set the cap and an agent's limit, and keeps members out", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)

		_, err := budgets(member).SetMonthlyCap(ctx, &budgetv1alpha1.SetMonthlyCapRequest{OrgId: org, MonthlyCapUsd: "100"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		res, err := budgets(admin).SetMonthlyCap(ctx, &budgetv1alpha1.SetMonthlyCapRequest{OrgId: org, MonthlyCapUsd: "100"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetBudget().GetMonthlyCapUsd()).To(Equal("100.00"))
		_, err = budgets(admin).SetMonthlyCap(ctx, &budgetv1alpha1.SetMonthlyCapRequest{OrgId: org, MonthlyCapUsd: "abc"})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		a := owner.activeAgent(ctx, org)
		got, err := budgets(admin).GetAgentBudget(ctx, &budgetv1alpha1.GetAgentBudgetRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetBudget().GetDailyLimitUsd()).NotTo(BeEmpty(), "a new agent starts with its template's spendUSD")
		set, err := budgets(admin).SetAgentBudget(ctx, &budgetv1alpha1.SetAgentBudgetRequest{OrgId: org, AgentId: a.GetId(), DailyLimitUsd: "3.5"})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetBudget().GetDailyLimitUsd()).To(Equal("3.50"))

		By("the list shows each agent's limit, its template default and today's spend")
		list, err := budgets(admin).ListAgentBudgets(ctx, &budgetv1alpha1.ListAgentBudgetsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetBudgets()).To(HaveLen(1))
		Expect(list.GetBudgets()[0].GetAgentName()).To(Equal(a.GetDisplayName()))
		Expect(list.GetBudgets()[0].GetDailyLimitUsd()).To(Equal("3.50"))
		Expect(list.GetBudgets()[0].GetTemplateDefaultUsd()).NotTo(BeEmpty())
		Expect(list.GetBudgets()[0].GetTemplateDefaultUsd()).NotTo(Equal("3.50"))
		_, err = budgets(member).ListAgentBudgets(ctx, &budgetv1alpha1.ListAgentBudgetsRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("only a platform admin sets the ceiling")
		_, err = budgets(owner).SetPlatformCeiling(ctx, &budgetv1alpha1.SetPlatformCeilingRequest{OrgId: org, PlatformCeilingUsd: "500"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action LIKE 'budget.%'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
	})

	It("lets admins override an agent's run limits, and runs queued afterwards get them (ADR 0081)", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		a := owner.activeAgent(ctx, org)

		got, err := budgets(admin).GetAgentRunLimits(ctx, &budgetv1alpha1.GetAgentRunLimitsRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		l := got.GetLimits()
		Expect(l.GetTemplateDefault().GetMaxModelRequests()).To(Equal(int64(300)), "the template's default")
		Expect(l.GetTemplateDefault().GetMaxSpendUsd()).To(Equal("5.00"))
		Expect(l.GetEffective()).To(Equal(l.GetTemplateDefault()), "no override: the template's values")
		Expect(l.GetOverride().GetMaxModelRequests()).To(BeZero())

		_, err = budgets(member).SetAgentRunLimits(ctx, &budgetv1alpha1.SetAgentRunLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.RunLimitValues{MaxModelRequests: 10}})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		for _, bad := range []*budgetv1alpha1.RunLimitValues{
			{MaxModelRequests: -1}, {MaxModelRequests: 100_001}, {MaxSpendUsd: "abc"}, {MaxSpendUsd: "-1"}, {MaxDurationSeconds: 90_000},
		} {
			_, err = budgets(admin).SetAgentRunLimits(ctx, &budgetv1alpha1.SetAgentRunLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), bad.String())
		}

		set, err := budgets(admin).SetAgentRunLimits(ctx, &budgetv1alpha1.SetAgentRunLimitsRequest{
			OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.RunLimitValues{MaxModelRequests: 40, MaxSpendUsd: "0.25"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLimits().GetEffective().GetMaxModelRequests()).To(Equal(int64(40)))
		Expect(set.GetLimits().GetEffective().GetMaxSpendUsd()).To(Equal("0.25"))
		Expect(set.GetLimits().GetEffective().GetMaxDurationSeconds()).To(Equal(l.GetTemplateDefault().GetMaxDurationSeconds()), "an unset field follows the template")
		Expect(set.GetLimits().GetTemplateDefault()).To(Equal(l.GetTemplateDefault()))

		By("a run queued afterwards carries the override")
		_, r := owner.createTicket(ctx, org, a.GetId(), "limited")
		Expect(r.GetMaxModelRequests()).To(Equal(int64(40)))
		Expect(r.GetMaxSpendUsd()).To(Equal("0.25"))

		By("clearing every field removes the override")
		cleared, err := budgets(admin).SetAgentRunLimits(ctx, &budgetv1alpha1.SetAgentRunLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.RunLimitValues{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(cleared.GetLimits().GetEffective()).To(Equal(l.GetTemplateDefault()))
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'budget.agent_run_limits.set'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
	})

	It("lets admins set the continuation caps of the org and of an agent, audited (issue #99)", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		a := owner.activeAgent(ctx, org)

		got, err := budgets(admin).GetOrgContinuationLimits(ctx, &budgetv1alpha1.GetOrgContinuationLimitsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetLimits().GetOverridden()).To(BeFalse())
		Expect(got.GetLimits().GetTaskSpendUsd()).To(Equal(got.GetLimits().GetPlatformDefaultTaskSpendUsd()))

		_, err = budgets(member).SetOrgContinuationTaskSpend(ctx, &budgetv1alpha1.SetOrgContinuationTaskSpendRequest{OrgId: org, TaskSpendUsd: "7"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		for _, bad := range []string{"abc", "-1", "0"} {
			_, err = budgets(admin).SetOrgContinuationTaskSpend(ctx, &budgetv1alpha1.SetOrgContinuationTaskSpendRequest{OrgId: org, TaskSpendUsd: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), bad)
		}
		set, err := budgets(admin).SetOrgContinuationTaskSpend(ctx, &budgetv1alpha1.SetOrgContinuationTaskSpendRequest{OrgId: org, TaskSpendUsd: "7.50"})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetLimits().GetTaskSpendUsd()).To(Equal("7.50"))
		Expect(set.GetLimits().GetOverridden()).To(BeTrue())

		By("an agent follows the template's count and the org's cap until it is overridden")
		ag, err := budgets(admin).GetAgentContinuationLimits(ctx, &budgetv1alpha1.GetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		l := ag.GetLimits()
		Expect(l.GetEffective()).To(Equal(l.GetTemplateDefault()))
		Expect(l.GetEffective().GetTaskSpendUsd()).To(Equal("7.50"))
		Expect(l.GetOverride().MaxContinuations).To(BeNil())

		_, err = budgets(member).SetAgentContinuationLimits(ctx, &budgetv1alpha1.SetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.ContinuationOverride{TaskSpendUsd: "3"}})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		eleven, zero := int32(11), int32(0)
		for _, bad := range []*budgetv1alpha1.ContinuationOverride{{MaxContinuations: &eleven}, {TaskSpendUsd: "abc"}, {TaskSpendUsd: "-1"}, {TaskSpendUsd: "0"}} {
			_, err = budgets(admin).SetAgentContinuationLimits(ctx, &budgetv1alpha1.SetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: bad})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), bad.String())
		}
		two := int32(2)
		over, err := budgets(admin).SetAgentContinuationLimits(ctx, &budgetv1alpha1.SetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.ContinuationOverride{MaxContinuations: &two, TaskSpendUsd: "3"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(over.GetLimits().GetEffective().GetMaxContinuations()).To(Equal(int32(2)))
		Expect(over.GetLimits().GetEffective().GetTaskSpendUsd()).To(Equal("3.00"))
		Expect(over.GetLimits().GetTemplateDefault()).To(Equal(l.GetTemplateDefault()))

		By("0 switches continuation off for the agent; clearing everything follows the defaults again")
		off, err := budgets(admin).SetAgentContinuationLimits(ctx, &budgetv1alpha1.SetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId(), Override: &budgetv1alpha1.ContinuationOverride{MaxContinuations: &zero}})
		Expect(err).NotTo(HaveOccurred())
		Expect(off.GetLimits().GetEffective().GetMaxContinuations()).To(BeZero())
		Expect(off.GetLimits().GetOverride().MaxContinuations).NotTo(BeNil())
		cleared, err := budgets(admin).SetAgentContinuationLimits(ctx, &budgetv1alpha1.SetAgentContinuationLimitsRequest{OrgId: org, AgentId: a.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(cleared.GetLimits().GetEffective()).To(Equal(l.GetTemplateDefault()))

		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'budget.agent_continuation_limits.set'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(3))
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'budget.org_continuation_task_spend.set'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})

	It("lists every org's cap, ceiling and spend for platform admins only", func(ctx SpecContext) {
		_, err := budgets(owner).ListOrgBudgets(ctx, &budgetv1alpha1.ListOrgBudgetsRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err = budgets(admin).ListOrgBudgets(ctx, &budgetv1alpha1.ListOrgBudgetsRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = e.pool.Exec(ctx, `INSERT INTO org_budgets (org_id, monthly_cap, platform_ceiling, updated_by) VALUES ($1, 10, 50, 'test')`, org)
		Expect(err).NotTo(HaveOccurred())
		pa := e.userIn(ctx, org, store.OrgRoleMember)
		_, err = e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())
		res, err := budgets(pa).ListOrgBudgets(ctx, &budgetv1alpha1.ListOrgBudgetsRequest{})
		Expect(err).NotTo(HaveOccurred())
		var found bool
		for _, o := range res.GetOrgs() {
			if o.GetOrgId() == org {
				found = true
				Expect(o.GetMonthlyCapUsd()).To(Equal("10.00"))
				Expect(o.GetPlatformCeilingUsd()).To(Equal("50.00"))
			}
		}
		Expect(found).To(BeTrue())
	})

	It("shows this month's spend and the estimated part of it", func(ctx SpecContext) {
		var orgID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		a := owner.activeAgent(ctx, org)
		for i, row := range []struct {
			cost      string
			estimated bool
		}{{"3", false}, {"1", true}} {
			_, err := e.pool.Exec(ctx, `INSERT INTO spend_entries (org_id, request_id, agent_id, key_id, model, input_tokens, output_tokens, occurred_at, cost_usd, priced, estimated)
				VALUES ($1, $2, $3, 'k', 'm', 1, 1, now(), $4::numeric, true, $5)`, orgID, "req-"+string(rune('a'+i)), a.GetId(), row.cost, row.estimated)
			Expect(err).NotTo(HaveOccurred())
		}
		got, err := budgets(owner).GetOrgBudget(ctx, &budgetv1alpha1.GetOrgBudgetRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetBudget().GetSpentThisMonthUsd()).To(Equal("4.00"))
		Expect(got.GetBudget().GetEstimatedThisMonthUsd()).To(Equal("1.00"))
	})

	It("lists a user's own notifications and marks them read", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		var orgID, userID pgtype.UUID
		Expect(orgID.Scan(org)).To(Succeed())
		Expect(userID.Scan(admin.userID)).To(Succeed())
		_, err := e.pool.Exec(ctx, `INSERT INTO notifications (org_id, user_id, kind, dedupe_key, payload) VALUES ($1, $2, 'budget.warning', 'k1',
			'{"level":"org_monthly","threshold":80,"limit_usd":"10.00","spent_usd":"8.00","window_start":"2026-10-01T00:00:00Z","agent_id":""}')`, orgID, userID)
		Expect(err).NotTo(HaveOccurred())

		list, err := budgets(admin).ListNotifications(ctx, &budgetv1alpha1.ListNotificationsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetNotifications()).To(HaveLen(1))
		n := list.GetNotifications()[0]
		Expect(n.GetLevel()).To(Equal("org_monthly"))
		Expect(n.GetThreshold()).To(Equal(int32(80)))
		Expect(n.GetReadAt()).To(BeNil())

		other, err := budgets(owner).ListNotifications(ctx, &budgetv1alpha1.ListNotificationsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(other.GetNotifications()).To(BeEmpty(), "each user sees their own")

		_, err = budgets(admin).MarkNotificationRead(ctx, &budgetv1alpha1.MarkNotificationReadRequest{OrgId: org, NotificationId: n.GetId()})
		Expect(err).NotTo(HaveOccurred())
		list, _ = budgets(admin).ListNotifications(ctx, &budgetv1alpha1.ListNotificationsRequest{OrgId: org})
		Expect(list.GetNotifications()[0].GetReadAt()).NotTo(BeNil())
	})

	It("warns when a saved price has a cache_read price but no prompt-cache mode (ADR 0068)", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		uncached, err := priceSvc(admin).SetPrice(ctx, &budgetv1alpha1.SetPriceRequest{
			OrgId: org, Name: "gemini", Provider: "openrouter", Match: &budgetv1alpha1.PriceMatch{Glob: "google/*"},
			Rates: &budgetv1alpha1.Rates{Input: "1", Output: "2", CacheRead: "0.1"}, MaxOutput: 4096,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(uncached.GetWarnings()).To(ConsistOf(ContainSubstring("without caching")), "ADR 0068: no cache mode for this provider format")
		cached, err := priceSvc(admin).SetPrice(ctx, &budgetv1alpha1.SetPriceRequest{
			OrgId: org, Name: "claude", Provider: "openrouter", Match: &budgetv1alpha1.PriceMatch{Glob: "anthropic/*"},
			Rates: &budgetv1alpha1.Rates{Input: "1", Output: "2", CacheRead: "0.1"}, MaxOutput: 4096,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cached.GetWarnings()).To(BeEmpty())
		_, err = priceSvc(admin).RetirePrice(ctx, &budgetv1alpha1.RetirePriceRequest{OrgId: org, Name: "gemini"})
		Expect(err).NotTo(HaveOccurred())
		_, err = priceSvc(admin).RetirePrice(ctx, &budgetv1alpha1.RetirePriceRequest{OrgId: org, Name: "claude"})
		Expect(err).NotTo(HaveOccurred())
	})

	It("versions org prices and shows the default list", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		req := &budgetv1alpha1.SetPriceRequest{
			OrgId: org, Name: "contract", Provider: "openrouter", Match: &budgetv1alpha1.PriceMatch{Glob: "deepseek/*"},
			Rates: &budgetv1alpha1.Rates{Input: "0.5", Output: "1.5"}, MaxOutput: 4096,
			Tiers: []*budgetv1alpha1.PriceTier{{ContextOver: 100000, Rates: &budgetv1alpha1.Rates{Input: "1"}}},
		}
		_, err := priceSvc(member).SetPrice(ctx, req)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		res, err := priceSvc(admin).SetPrice(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetPrice().GetRates().GetInput()).To(Equal("0.5"))
		req.Rates.Input = "1.1234567"
		_, err = priceSvc(admin).SetPrice(ctx, req)
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		list, err := priceSvc(admin).ListPrices(ctx, &budgetv1alpha1.ListPricesRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetPrices()).To(HaveLen(1))
		_, err = priceSvc(admin).RetirePrice(ctx, &budgetv1alpha1.RetirePriceRequest{OrgId: org, Name: "contract"})
		Expect(err).NotTo(HaveOccurred())

		defs, err := priceSvc(admin).GetDefaultPrices(ctx, &budgetv1alpha1.GetDefaultPricesRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(defs.GetEntries()).NotTo(BeEmpty())
		Expect(defs.GetEntries()[0].GetSourceUrl()).To(HavePrefix("https://"))
	})
})

func budgetClient(c *client) budgetv1alpha1connect.BudgetServiceClient {
	return budgetv1alpha1connect.NewBudgetServiceClient(connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL)))
}

func priceClient(c *client) budgetv1alpha1connect.PriceServiceClient {
	return budgetv1alpha1connect.NewPriceServiceClient(connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL)))
}
