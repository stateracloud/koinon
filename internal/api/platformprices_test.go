// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	budgetv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/budget/v1alpha1"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/pricing"
)

// noSource is an adapter whose fetch always fails, so Check sources is exercised without a network.
type noSource struct{}

func (noSource) Name() string { return pricefetch.SourceOpenRouter }
func (noSource) Fetch(context.Context, time.Time) (pricefetch.Snapshot, error) {
	return pricefetch.Snapshot{}, context.DeadlineExceeded
}

var _ = Describe("the platform price RPCs of PriceService", func() {
	var (
		e   *env
		org string
	)
	BeforeEach(func(ctx SpecContext) {
		e = newEnv(ctx, envOptions{})
		org, _ = e.newTeamOrg(ctx)
	})

	// calls runs every platform price RPC with c; the requests are valid, so a refusal is the authz.
	calls := func(ctx context.Context, c *client) map[string]error {
		p := priceClient(c)
		out := map[string]error{}
		_, out["ListPriceProposals"] = p.ListPriceProposals(ctx, &budgetv1alpha1.ListPriceProposalsRequest{})
		_, out["GetPriceProposal"] = p.GetPriceProposal(ctx, &budgetv1alpha1.GetPriceProposalRequest{Id: "00000000-0000-0000-0000-000000000000"})
		_, out["AcceptPriceProposal"] = p.AcceptPriceProposal(ctx, &budgetv1alpha1.AcceptPriceProposalRequest{Id: "00000000-0000-0000-0000-000000000000"})
		_, out["DismissPriceProposal"] = p.DismissPriceProposal(ctx, &budgetv1alpha1.DismissPriceProposalRequest{Id: "00000000-0000-0000-0000-000000000000"})
		_, out["ListPriceSourceFetches"] = p.ListPriceSourceFetches(ctx, &budgetv1alpha1.ListPriceSourceFetchesRequest{})
		_, out["ListPlatformPriceVersions"] = p.ListPlatformPriceVersions(ctx, &budgetv1alpha1.ListPlatformPriceVersionsRequest{})
		_, out["GetPlatformPriceVersion"] = p.GetPlatformPriceVersion(ctx, &budgetv1alpha1.GetPlatformPriceVersionRequest{Version: 1})
		_, out["DiffPlatformPriceVersions"] = p.DiffPlatformPriceVersions(ctx, &budgetv1alpha1.DiffPlatformPriceVersionsRequest{})
		_, out["RollbackPlatformPrices"] = p.RollbackPlatformPrices(ctx, &budgetv1alpha1.RollbackPlatformPricesRequest{Version: 1})
		_, out["GetPriceRefreshSettings"] = p.GetPriceRefreshSettings(ctx, &budgetv1alpha1.GetPriceRefreshSettingsRequest{})
		_, out["CheckPriceSources"] = p.CheckPriceSources(ctx, &budgetv1alpha1.CheckPriceSourcesRequest{})
		_, out["ExplainPlatformPrice"] = p.ExplainPlatformPrice(ctx, &budgetv1alpha1.ExplainPlatformPriceRequest{ProviderType: "openrouter", Model: "x/y"})
		return out
	}

	It("refuses every RPC to an org owner, admin and member who is not a platform admin", func(ctx SpecContext) {
		for _, role := range []store.OrgRole{store.OrgRoleOwner, store.OrgRoleAdmin, store.OrgRoleMember} {
			c := e.userIn(ctx, org, role)
			res := calls(ctx, c)
			Expect(res).To(HaveLen(12))
			for name, err := range res {
				Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "%s as %s", name, role)
				reason, _ := reasonOf(err)
				Expect(reason).To(Equal("PLATFORM_ADMIN_REQUIRED"), "%s as %s", name, role)
			}
		}
	})

	It("lets a platform admin read, and answers unimplemented for writes until the refresher is bound", func(ctx SpecContext) {
		pa := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, pa.userID)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_, _ = e.pool.Exec(ctx, `DELETE FROM platform_admins WHERE user_id = $1`, pa.userID)
		})

		p := priceClient(pa)
		props, err := p.ListPriceProposals(ctx, &budgetv1alpha1.ListPriceProposalsRequest{State: "open"})
		Expect(err).NotTo(HaveOccurred())
		Expect(props.GetProposals()).To(BeEmpty())
		settings, err := p.GetPriceRefreshSettings(ctx, &budgetv1alpha1.GetPriceRefreshSettingsRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(settings.GetMode()).NotTo(BeEmpty())
		_, err = p.GetPlatformPriceVersion(ctx, &budgetv1alpha1.GetPlatformPriceVersionRequest{Version: 999999})
		Expect(codeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = p.RollbackPlatformPrices(ctx, &budgetv1alpha1.RollbackPlatformPricesRequest{Version: 0})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = p.CheckPriceSources(ctx, &budgetv1alpha1.CheckPriceSourcesRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodeUnimplemented))

		By("with the refresher bound, a proposal is accepted as the caller and shows its provenance")
		model := "koinon-test/" + strings.ToLower(pa.userID[:8])
		// A proposal is made for a model that has a price in use, so the refresher's default list prices this one.
		yml := fmt.Sprintf("version: %q\nentries:\n  - id: fake-1\n    provider: openrouter\n    match: {glob: %q}\n    valid_from: 2000-01-01\n"+
			"    prices: {input: \"1\", output: \"2\"}\n    tokenizer: bytes\n    ratio: \"3.0\"\n    max_output: 100000\n"+
			"    source: {url: \"https://example.invalid/test-prices\", accessed: 2000-01-01}\n", "2000-01-01.1", model)
		l, rs, err := pricing.ParseList([]byte(yml))
		Expect(err).NotTo(HaveOccurred())
		svc := prices.New(e.pool, slog.Default(), pricing.Static(l, rs))
		r, err := prices.NewRefresher(svc, prices.RefreshConfig{Adapters: []pricefetch.Adapter{noSource{}}, Metrics: prices.NewMetrics(nil)})
		Expect(err).NotTo(HaveOccurred())
		e.rpc.BindPriceRefresher(r)
		_, err = r.Import(ctx, audit.CLI("test"), pricefetch.File{Format: pricefetch.FileFormat, Snapshots: []pricefetch.Snapshot{{
			Source: pricefetch.SourceOpenRouter, URL: "https://example.invalid/fake", Licence: "fake", FetchedAt: time.Now(),
			SHA256: strings.Repeat("0", 64), Count: 1,
			Offers: []pricefetch.Offer{{Model: model, ProviderType: "openrouter", Spec: pricing.Spec{Prices: pricing.Wire{Input: "1.5", Output: "3"}}, MaxOutput: 4096}},
		}}})
		Expect(err).NotTo(HaveOccurred())

		list, err := p.ListPriceProposals(ctx, &budgetv1alpha1.ListPriceProposalsRequest{State: "open"})
		Expect(err).NotTo(HaveOccurred())
		var id string
		for _, pr := range list.GetProposals() {
			if pr.GetModel() == model {
				id = pr.GetId()
				Expect(pr.GetProposed().GetRates().GetInput()).To(Equal("1.5"))
			}
		}
		Expect(id).NotTo(BeEmpty())
		acc, err := p.AcceptPriceProposal(ctx, &budgetv1alpha1.AcceptPriceProposalRequest{Id: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(acc.GetVersion()).To(BeNumerically(">=", 1))
		got, err := p.GetPriceProposal(ctx, &budgetv1alpha1.GetPriceProposalRequest{Id: id})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetProposal().GetState()).To(Equal("accepted"))
		Expect(got.GetProposal().GetDecidedBy()).To(Equal("user " + pa.userID))
		_, err = p.AcceptPriceProposal(ctx, &budgetv1alpha1.AcceptPriceProposalRequest{Id: id})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "a decided proposal cannot be accepted again")

		ver, err := p.GetPlatformPriceVersion(ctx, &budgetv1alpha1.GetPlatformPriceVersionRequest{Version: acc.GetVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(ver.GetVersion().GetActor()).To(Equal("user " + pa.userID))
		why, err := p.ExplainPlatformPrice(ctx, &budgetv1alpha1.ExplainPlatformPriceRequest{ProviderType: "openrouter", Model: model})
		Expect(err).NotTo(HaveOccurred())
		Expect(why.GetLayer()).To(Equal("platform"))
		Expect(why.GetPriceRef()).To(HavePrefix("platform@"))
		Expect(why.GetProvenance().GetApprover()).To(Equal("user " + pa.userID))

		diff, err := p.DiffPlatformPriceVersions(ctx, &budgetv1alpha1.DiffPlatformPriceVersionsRequest{ToVersion: acc.GetVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(diff.GetChanges()).NotTo(BeEmpty())
		rb, err := p.RollbackPlatformPrices(ctx, &budgetv1alpha1.RollbackPlatformPricesRequest{Version: acc.GetVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rb.GetVersion()).To(BeNumerically(">", acc.GetVersion()))

		chk, err := p.CheckPriceSources(ctx, &budgetv1alpha1.CheckPriceSourcesRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(chk.GetFailed()).To(HaveKey(pricefetch.SourceOpenRouter))
		fetches, err := p.ListPriceSourceFetches(ctx, &budgetv1alpha1.ListPriceSourceFetchesRequest{Limit: 5})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetches.GetFetches()).NotTo(BeEmpty())
	})
})
