// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/csv"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stateracloud/koinon/internal/db/store"
	usagev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/usage/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/usage/v1alpha1/usagev1alpha1connect"
)

func usageClient(c *client) usagev1alpha1connect.UsageServiceClient {
	return usagev1alpha1connect.NewUsageServiceClient(connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL)))
}

// A fixed month in the past, so the seeded rows do not depend on today.
var (
	usageFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	usageTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

func usageRange() *usagev1alpha1.UsageRange {
	return &usagev1alpha1.UsageRange{From: timestamppb.New(usageFrom), To: timestamppb.New(usageTo)}
}

var _ = Describe("UsageService", func() {
	var (
		e        *env
		org      string
		other    string
		owner    *client
		agent    string
		provider string
	)

	seed := func(ctx context.Context, org, agent, request, principal, model, provider, at string, in, out, cr, cw, reason int, cost string, estimated bool) {
		var agentArg, providerArg any
		if principal == "agent" {
			agentArg = agent
		}
		if provider != "" {
			providerArg = provider
		}
		_, err := e.pool.Exec(ctx, `INSERT INTO spend_entries
			(org_id, request_id, principal, agent_id, key_id, model, provider_id, input_tokens, output_tokens, cache_read_tokens,
			 cache_write_tokens, reasoning_tokens, occurred_at, cost_usd, priced, estimated)
			VALUES ($1, $2, $3, $4, 'k', $5, $6, $7, $8, $9, $10, $11, $12::timestamptz, $13::numeric, true, $14)`,
			org, request, principal, agentArg, model, providerArg, in, out, cr, cw, reason, at, cost, estimated)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, owner = e.newTeamOrg(ctx)
		var otherOwner *client
		other, otherOwner = e.newTeamOrg(ctx)
		a := owner.activeAgent(ctx, org)
		agent = a.GetId()
		oa := otherOwner.activeAgent(ctx, other)

		Expect(e.pool.QueryRow(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, 'acme', 'openai', 'https://example.test/v1') RETURNING id`, org).Scan(&provider)).To(Succeed())

		// Org: two days, two models, a provider on some entries, a system check and an estimated entry.
		seed(ctx, org, agent, "u1", "agent", "model-a", provider, "2026-03-01T01:00:00Z", 1000, 500, 4000, 0, 100, "0.0123456789", false)
		seed(ctx, org, agent, "u2", "agent", "model-a", provider, "2026-03-01T23:59:59Z", 200, 50, 0, 300, 0, "0.0100000001", true)
		seed(ctx, org, agent, "u3", "agent", "model-b", "", "2026-03-01T12:00:00Z", 10, 5, 0, 0, 0, "1.5", false)
		seed(ctx, org, agent, "u4", "agent", "model-a", provider, "2026-03-02T00:00:00Z", 700, 70, 700, 0, 0, "0.25", false)
		seed(ctx, org, "", "u5", "system", "model-a", "", "2026-03-02T10:00:00Z", 5, 1, 0, 0, 0, "0.000000000001", false)
		// Outside the month: not in the rows.
		seed(ctx, org, agent, "u6", "agent", "model-a", "", "2026-02-28T23:59:59Z", 9, 9, 9, 9, 9, "99", false)
		seed(ctx, org, agent, "u7", "agent", "model-a", "", "2026-04-01T00:00:00Z", 9, 9, 9, 9, 9, "99", false)
		// Another org in the same month.
		seed(ctx, other, oa.GetId(), "o1", "agent", "model-a", "", "2026-03-01T01:00:00Z", 1, 1, 1, 1, 1, "777", false)
	})

	It("gives the ledger sums of a seeded month by day, agent, model and provider", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		res, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())

		By("the totals equal the sums of the ledger rows of the org")
		var cost, est string
		var req, in, out, cr, cw, reason int64
		Expect(e.pool.QueryRow(ctx, `SELECT sum(cost_usd)::text, sum(cost_usd) FILTER (WHERE estimated)::text, count(*), sum(input_tokens), sum(output_tokens),
			sum(cache_read_tokens), sum(cache_write_tokens), sum(reasoning_tokens)
			FROM spend_entries WHERE org_id = $1 AND occurred_at >= $2 AND occurred_at < $3`, org, usageFrom, usageTo).
			Scan(&cost, &est, &req, &in, &out, &cr, &cw, &reason)).To(Succeed())
		t := res.GetTotals()
		Expect(t.GetRequests()).To(Equal(req))
		Expect(t.GetRequests()).To(Equal(int64(5)))
		Expect([]int64{t.GetInputTokens(), t.GetOutputTokens(), t.GetCacheReadTokens(), t.GetCacheWriteTokens(), t.GetReasoningTokens()}).
			To(Equal([]int64{in, out, cr, cw, reason}))
		Expect([]int64{in, out, cr, cw, reason}).To(Equal([]int64{1915, 626, 4700, 300, 100}))
		Expect(t.GetCostUsd()).To(Equal("1.772345679001"), "exact, to the twelfth digit: %s in the database", cost)
		Expect(t.GetEstimatedUsd()).To(Equal("0.0100000001"), est)
		Expect(t.GetCacheReadShare()).To(BeNumerically("~", 4700.0/(1915+4700+300), 1e-9))

		By("the rows add up to the totals")
		var sum int64
		for _, r := range res.GetRows() {
			sum += r.GetUsage().GetRequests()
		}
		Expect(sum).To(Equal(req))

		By("a row per day, principal, agent, model and provider")
		Expect(res.GetRows()).To(HaveLen(4))
		first := res.GetRows()[0]
		Expect(first.GetDay()).To(Equal("2026-03-01"))
		Expect(first.GetPrincipal()).To(Equal("agent"))
		Expect(first.GetAgentId()).To(Equal(agent))
		Expect(first.GetAgentName()).NotTo(BeEmpty())
		Expect(first.GetModel()).To(Equal("model-a"))
		Expect(first.GetProviderId()).To(Equal(provider))
		Expect(first.GetProviderName()).To(Equal("acme"))
		Expect(first.GetUsage().GetRequests()).To(Equal(int64(2)))
		Expect(first.GetUsage().GetCostUsd()).To(Equal("0.022345679"))
		Expect(first.GetUsage().GetCacheReadShare()).To(BeNumerically("~", 4000.0/(1200+4000+300), 1e-9))
		last := res.GetRows()[3]
		Expect(last.GetDay()).To(Equal("2026-03-02"))
		Expect(last.GetPrincipal()).To(Equal("system"))
		Expect(last.GetAgentId()).To(BeEmpty())

		By("the filters narrow the rows and the totals")
		for _, tc := range []struct {
			req  *usagev1alpha1.GetOrgUsageRequest
			want int64
		}{
			{&usagev1alpha1.GetOrgUsageRequest{Model: "model-b"}, 1},
			{&usagev1alpha1.GetOrgUsageRequest{ProviderId: provider}, 3},
			{&usagev1alpha1.GetOrgUsageRequest{AgentId: agent}, 4},
		} {
			tc.req.OrgId, tc.req.Range = org, usageRange()
			got, err := usageClient(member).GetOrgUsage(ctx, tc.req)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetTotals().GetRequests()).To(Equal(tc.want), tc.req.String())
		}

		By("a period of one day")
		day, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: &usagev1alpha1.UsageRange{
			From: timestamppb.New(time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)),
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(day.GetTotals().GetRequests()).To(Equal(int64(2)), "the start rounds down to the UTC day")
	})

	It("exports the same numbers as the API", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `UPDATE agents SET display_name = '=HYPERLINK("x")' WHERE org_id = $1 AND id = $2`, org, agent)
		Expect(err).NotTo(HaveOccurred())
		res, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())
		exp, err := usageClient(member).ExportOrgUsage(ctx, &usagev1alpha1.ExportOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())
		Expect(exp.GetFilename()).To(Equal("koinon-usage-org-" + org + "-20260301-20260331.csv"))

		recs, err := csv.NewReader(strings.NewReader(exp.GetCsv())).ReadAll()
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(HaveLen(len(res.GetRows())+2), "header, rows, total")
		col := map[string]int{}
		for i, h := range recs[0] {
			col[h] = i
		}
		check := func(rec []string, u *usagev1alpha1.UsageTotals) {
			n := func(name string) int64 {
				v, err := strconv.ParseInt(rec[col[name]], 10, 64)
				Expect(err).NotTo(HaveOccurred(), name)
				return v
			}
			Expect(n("requests")).To(Equal(u.GetRequests()))
			Expect(n("input_tokens")).To(Equal(u.GetInputTokens()))
			Expect(n("output_tokens")).To(Equal(u.GetOutputTokens()))
			Expect(n("cache_read_tokens")).To(Equal(u.GetCacheReadTokens()))
			Expect(n("cache_write_tokens")).To(Equal(u.GetCacheWriteTokens()))
			Expect(n("reasoning_tokens")).To(Equal(u.GetReasoningTokens()))
			Expect(rec[col["cost_usd"]]).To(Equal(u.GetCostUsd()))
			Expect(rec[col["estimated_usd"]]).To(Equal(u.GetEstimatedUsd()))
			share, err := strconv.ParseFloat(rec[col["cache_read_share"]], 64)
			Expect(err).NotTo(HaveOccurred())
			Expect(share).To(BeNumerically("~", u.GetCacheReadShare(), 0.00005))
		}
		for i, r := range res.GetRows() {
			rec := recs[i+1]
			Expect(rec[col["day"]]).To(Equal(r.GetDay()))
			Expect(rec[col["agent_id"]]).To(Equal(r.GetAgentId()))
			Expect(rec[col["model"]]).To(Equal(r.GetModel()))
			Expect(rec[col["provider_id"]]).To(Equal(r.GetProviderId()))
			check(rec, r.GetUsage())
		}
		total := recs[len(recs)-1]
		Expect(total[col["day"]]).To(Equal("total"))
		check(total, res.GetTotals())

		By("a name that starts like a formula is quoted, so a spreadsheet does not run it")
		Expect(recs[1][col["agent_name"]]).To(Equal(`'=HYPERLINK("x")`))
	})

	It("shows a member only their own org", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		viewer := e.userIn(ctx, org, store.OrgRoleViewer)
		outsider := e.userIn(ctx, other, store.OrgRoleOwner)

		By("a viewer is below the member role")
		_, err := usageClient(viewer).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = usageClient(viewer).ExportOrgUsage(ctx, &usagev1alpha1.ExportOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))

		By("the owner of another org gets NotFound for the usage and the export, with no data")
		for _, c := range []*client{outsider, member} {
			target := org
			if c == member {
				target = other
			}
			res, err := usageClient(c).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: target, Range: usageRange()})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(res.GetRows()).To(BeEmpty())
			exp, err := usageClient(c).ExportOrgUsage(ctx, &usagev1alpha1.ExportOrgUsageRequest{OrgId: target, Range: usageRange()})
			Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(exp.GetCsv()).To(BeEmpty())
		}

		By("a filter by another org's agent finds nothing in this org")
		var otherAgent string
		Expect(e.pool.QueryRow(ctx, `SELECT id FROM agents WHERE org_id = $1 LIMIT 1`, other).Scan(&otherAgent)).To(Succeed())
		got, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: usageRange(), AgentId: otherAgent})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetRows()).To(BeEmpty())
		Expect(got.GetTotals().GetCostUsd()).To(Equal("0.00"))

		By("the org's rows hold none of the other org's cost")
		res, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetTotals().GetCostUsd()).NotTo(Equal("778.772345679001"))

		By("the platform calls refuse an org owner and a member")
		for _, c := range []*client{owner, member} {
			_, err := usageClient(c).GetPlatformUsage(ctx, &usagev1alpha1.GetPlatformUsageRequest{Range: usageRange()})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
			_, err = usageClient(c).ExportPlatformUsage(ctx, &usagev1alpha1.ExportPlatformUsageRequest{Range: usageRange()})
			Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		}
	})

	It("gives platform totals to platform admins only, and the export matches", func(ctx SpecContext) {
		pa := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())

		res, err := usageClient(pa).GetPlatformUsage(ctx, &usagev1alpha1.GetPlatformUsageRequest{Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())
		var cost string
		var req int64
		Expect(e.pool.QueryRow(ctx, `SELECT sum(cost_usd)::text, count(*) FROM spend_entries WHERE occurred_at >= $1 AND occurred_at < $2`, usageFrom, usageTo).Scan(&cost, &req)).To(Succeed())
		Expect(res.GetTotals().GetRequests()).To(Equal(req))
		Expect(res.GetTotals().GetRequests()).To(Equal(int64(6)))
		Expect(res.GetTotals().GetCostUsd()).To(Equal("778.772345679001"), cost)
		perOrg := map[string]int64{}
		for _, r := range res.GetRows() {
			perOrg[r.GetOrgId()] += r.GetUsage().GetRequests()
		}
		Expect(perOrg).To(HaveKeyWithValue(org, int64(5)))
		Expect(perOrg).To(HaveKeyWithValue(other, int64(1)))

		exp, err := usageClient(pa).ExportPlatformUsage(ctx, &usagev1alpha1.ExportPlatformUsageRequest{Range: usageRange()})
		Expect(err).NotTo(HaveOccurred())
		recs, err := csv.NewReader(strings.NewReader(exp.GetCsv())).ReadAll()
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(HaveLen(len(res.GetRows()) + 2))
		total := recs[len(recs)-1]
		Expect(total[0]).To(Equal("total"))
		Expect(total[len(total)-3]).To(Equal(res.GetTotals().GetCostUsd()))
		Expect(total[5]).To(Equal(strconv.FormatInt(res.GetTotals().GetRequests(), 10)))
	})

	It("counts today's runs and failed runs, and rejects a period that is too long", func(ctx SpecContext) {
		member := e.userIn(ctx, org, store.OrgRoleMember)
		pa := e.userIn(ctx, other, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())

		before, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		platformBefore, err := usageClient(pa).GetPlatformUsage(ctx, &usagev1alpha1.GetPlatformUsageRequest{})
		Expect(err).NotTo(HaveOccurred())

		fail := func(run, extra string) {
			_, err := e.pool.Exec(ctx, `UPDATE runs SET state = 'starting', ready_at = NULL, owner_id = 'test', lease_expires_at = now() + interval '1 minute' WHERE org_id = $1 AND id = $2`, org, run)
			Expect(err).NotTo(HaveOccurred())
			_, err = e.pool.Exec(ctx, `UPDATE runs SET state = 'failed', outcome_reason = 'test', finished_at = now(), owner_id = NULL, lease_expires_at = NULL`+extra+` WHERE org_id = $1 AND id = $2`, org, run)
			Expect(err).NotTo(HaveOccurred())
		}
		_, r1 := owner.createTask(ctx, org, agent, "one")
		fail(r1.GetId(), "")
		_, r2 := owner.createTask(ctx, org, agent, "two")
		_, err = e.pool.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'test', finished_at = now(), ready_at = NULL WHERE org_id = $1 AND id = $2`, org, r2.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, r3 := owner.createTask(ctx, org, agent, "old")
		fail(r3.GetId(), ", created_at = now() - interval '3 days'")

		after, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(after.GetRuns().GetRunsToday() - before.GetRuns().GetRunsToday()).To(Equal(int64(2)))
		Expect(after.GetRuns().GetFailedRunsToday() - before.GetRuns().GetFailedRunsToday()).To(Equal(int64(1)))
		Expect(after.GetRuns().GetDayStart().AsTime()).To(BeTemporally("~", time.Now().UTC().Truncate(24*time.Hour), 24*time.Hour))

		platformAfter, err := usageClient(pa).GetPlatformUsage(ctx, &usagev1alpha1.GetPlatformUsageRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(platformAfter.GetRuns().GetRunsToday() - platformBefore.GetRuns().GetRunsToday()).To(Equal(int64(2)))
		Expect(platformAfter.GetRuns().GetFailedRunsToday() - platformBefore.GetRuns().GetFailedRunsToday()).To(Equal(int64(1)))

		By("a period of more than 366 days, or one that ends before it starts")
		for _, rng := range []*usagev1alpha1.UsageRange{
			{From: timestamppb.New(usageFrom), To: timestamppb.New(usageFrom.AddDate(0, 0, 367))},
			{From: timestamppb.New(usageTo), To: timestamppb.New(usageFrom)},
		} {
			_, err := usageClient(member).GetOrgUsage(ctx, &usagev1alpha1.GetOrgUsageRequest{OrgId: org, Range: rng})
			Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		}
	})
})
