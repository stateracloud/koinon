// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	connect "connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// blockingChat answers after release is closed, closing entered when its first call starts.
type blockingChat struct {
	release, entered chan struct{}
	calls            atomic.Int32
}

func (b *blockingChat) Complete(ctx context.Context, _ pgtype.UUID, _ string, _ modelformat.Format, _ string) (string, error) {
	if b.calls.Add(1) == 1 {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return modelcheck.Line, nil
}

type staticKey string

func (k staticKey) Key(context.Context, pgtype.UUID) (string, error) { return string(k), nil }

// The provider echo check (rc.6): one request through the gateway, on demand only. The "gateway" is
// the fake model server; CI never calls a paid model.
var _ = Describe("the provider echo check", func() {
	var (
		fake  *fakemodel.Server
		e     *env
		org   string
		owner *client
	)
	BeforeEach(func(ctx SpecContext) {
		var err error
		fake, err = fakemodel.New(
			fakemodel.Script{Model: "fake/faithful", Turns: []fakemodel.Turn{{Text: modelcheck.Line}}},
			fakemodel.Script{Model: "fake/quoting", Turns: []fakemodel.Turn{{Text: "\"" + modelcheck.Line + "\"\n"}}},
			fakemodel.Script{Model: "fake/masking", Turns: []fakemodel.Turn{{
				Text: strings.NewReplacer("Maria Schneider", "[NAME]", "Hauptstrasse 12, 10115 Berlin", "[ADDRESS]").Replace(modelcheck.Line),
			}}},
		)
		Expect(err).NotTo(HaveOccurred())
		srv := httptest.NewServer(fake)
		DeferCleanup(srv.Close)
		e = newEnv(ctx, envOptions{modelChat: modelcheck.GatewayChat{URL: srv.URL, Keys: staticKey("koinon_test_not-a-real-token")}})
		org, owner = e.newTeamOrg(ctx)
	})
	check := func(ctx SpecContext, c *client, provider, model string) (*providerv1alpha1.CheckProviderEchoResponse, error) {
		return c.providers.CheckProviderEcho(ctx, &providerv1alpha1.CheckProviderEchoRequest{OrgId: org, ProviderId: provider, Model: model})
	}

	It("reports a faithful model as unchanged, and shows the time on the provider", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/faithful", true)
		res, err := check(ctx, owner, provider, "fake/faithful")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEchoCheck().GetUnchanged()).To(BeTrue())
		Expect(res.GetEchoCheck().GetSent()).To(Equal(modelcheck.Line))
		Expect(res.GetEchoCheck().GetCheckedAt()).NotTo(BeNil())
		Expect(res.GetEchoCheck().GetDiff()).To(HaveLen(1))

		list, err := owner.providers.ListProviders(ctx, &providerv1alpha1.ListProvidersRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetProviders()).To(HaveLen(1))
		Expect(list.GetProviders()[0].GetEchoCheck().GetModel()).To(Equal("fake/faithful"))

		reqs := fake.Requests()
		Expect(reqs).To(HaveLen(1), "exactly one request")
		Expect(string(reqs[0].Body)).To(ContainSubstring("Echo this line back exactly, character for character, nothing else: " + modelcheck.Line))
		Expect(count(ctx, e, `SELECT count(*) FROM audit_events WHERE org_id = $1 AND action = 'provider.echo_check' AND target_id = $2`, org, provider)).To(Equal(1))
	})

	It("sends the check in the model's own format: Messages for an anthropic model", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/faithful", true)
		_, err := e.pool.Exec(ctx, `UPDATE models SET format = 'anthropic', format_overridden = true WHERE org_id = $1 AND provider_id = $2`, org, provider)
		Expect(err).NotTo(HaveOccurred())
		res, err := check(ctx, owner, provider, "fake/faithful")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEchoCheck().GetUnchanged()).To(BeTrue())
		reqs := fake.Requests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Path).To(Equal("/v1/messages"))
	})

	It("does not mistake a quoted reply for masking", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/quoting", true)
		res, err := check(ctx, owner, provider, "fake/quoting")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEchoCheck().GetUnchanged()).To(BeTrue())
	})

	It("reports masking as changed, with the diff, and replaces the earlier result", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/masking", true)
		_, err := e.pool.Exec(ctx, `INSERT INTO models (org_id, provider_id, name) SELECT org_id, id, 'fake/faithful' FROM providers WHERE id = $1`, provider)
		Expect(err).NotTo(HaveOccurred())
		_, err = e.pool.Exec(ctx, `INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
			VALUES ($1, 'faithful', jsonb_build_object('provider', 'openai', 'match', jsonb_build_object('glob', 'fake/faithful'),
			'validFrom', '2020-01-01', 'prices', jsonb_build_object('input', '1', 'output', '2'),
			'tokenizer', 'bytes', 'ratio', '3.0', 'maxOutput', 10), now() - interval '1 hour', 'test')`, org)
		Expect(err).NotTo(HaveOccurred())

		res, err := check(ctx, owner, provider, "fake/masking")
		Expect(err).NotTo(HaveOccurred())
		ec := res.GetEchoCheck()
		Expect(ec.GetUnchanged()).To(BeFalse())
		Expect(ec.GetReceived()).To(ContainSubstring("[NAME]"))
		var deleted, inserted string
		for _, d := range ec.GetDiff() {
			switch d.GetOp() {
			case providerv1alpha1.EchoDiffSegment_OP_DELETED:
				deleted += d.GetText()
			case providerv1alpha1.EchoDiffSegment_OP_INSERTED:
				inserted += d.GetText()
			}
		}
		Expect(deleted).To(And(ContainSubstring("Maria"), ContainSubstring("Schneider"), ContainSubstring("Hauptstrasse")))
		Expect(inserted).To(And(ContainSubstring("NAME"), ContainSubstring("ADDRESS")))

		res, err = check(ctx, owner, provider, "fake/faithful")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetEchoCheck().GetUnchanged()).To(BeTrue())
		Expect(count(ctx, e, `SELECT count(*) FROM provider_echo_checks WHERE org_id = $1`, org)).To(Equal(1), "the latest result only")
	})

	It("refuses a model the provider does not serve, or one without a price, without a request", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/faithful", false)
		_, err := check(ctx, owner, provider, "fake/faithful")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = check(ctx, owner, provider, "fake/other")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		_, err = check(ctx, owner, provider, "")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(fake.Requests()).To(BeEmpty())
	})

	It("says so when the gateway does not answer, and stores nothing", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/not-scripted", true)
		_, err := check(ctx, owner, provider, "fake/not-scripted") // the fake answers 404 for an unknown model
		Expect(codeOf(err)).To(Equal(connect.CodeUnavailable))
		Expect(count(ctx, e, `SELECT count(*) FROM provider_echo_checks WHERE org_id = $1`, org)).To(BeZero())
	})

	It("runs one check per org at a time: a second while one is in flight is refused, with no second request", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/slow", true)
		release := make(chan struct{})
		entered := make(chan struct{})
		slow := &blockingChat{release: release, entered: entered}
		blocked := newEnvOn(ctx, e, envOptions{modelChat: slow})
		first := make(chan error, 1)
		c := owner.on(blocked)
		go func() {
			defer GinkgoRecover()
			_, err := check(ctx, c, provider, "fake/slow")
			first <- err
		}()
		Eventually(entered).Should(BeClosed())
		_, err := check(ctx, c, provider, "fake/slow")
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("already running"))
		close(release)
		Eventually(first).Should(Receive(BeNil()))
		Expect(slow.calls.Load()).To(Equal(int32(1)))
		_, err = check(ctx, c, provider, "fake/slow")
		Expect(err).NotTo(HaveOccurred(), "the lock is released")
	})

	It("is for admins and owners", func(ctx SpecContext) {
		provider := serveModel(ctx, e, org, "fake/faithful", true)
		member := e.userIn(ctx, org, store.OrgRoleMember)
		_, err := check(ctx, member, provider, "fake/faithful")
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(fake.Requests()).To(BeEmpty())
	})
})
