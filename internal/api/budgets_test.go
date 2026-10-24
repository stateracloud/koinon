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

		By("only a platform admin sets the ceiling")
		_, err = budgets(owner).SetPlatformCeiling(ctx, &budgetv1alpha1.SetPlatformCeilingRequest{OrgId: org, PlatformCeilingUsd: "500"})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action LIKE 'budget.%'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2))
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
