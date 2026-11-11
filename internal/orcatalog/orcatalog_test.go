// SPDX-License-Identifier: Apache-2.0

package orcatalog_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/orcatalog"
	"github.com/stateracloud/koinon/internal/pricefetch"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var t0 = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

func snapshot(at time.Time, metas ...pricefetch.ModelMeta) pricefetch.Snapshot {
	return pricefetch.Snapshot{Source: pricefetch.SourceOpenRouter, FetchedAt: at, Count: len(metas), Meta: metas}
}

func meta(model string, ctxLen, maxOut int) pricefetch.ModelMeta {
	return pricefetch.ModelMeta{
		Model: model, Name: "Fake " + model, Tokenizer: "Claude", SupportedParameters: []string{"tools"},
		InputModalities: []string{"text"}, ContextLength: ctxLen, MaxOutput: maxOut,
	}
}

func installEvents(ctx context.Context, action string) int {
	var n int
	Expect(db.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = $1`, action).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("the per-install OpenRouter catalog", func() {
	BeforeEach(func() { testenv.Require(testenv.Docker) })

	It("tags a row openrouter@<fetched_at>", func() {
		Expect(orcatalog.SourceTag(time.Date(2026, 11, 11, 8, 0, 0, 0, time.FixedZone("x", 3600)))).To(Equal("openrouter@2026-11-11T07:00:00Z"))
	})

	It("stores display fields at once, with the fetch time as the source", func(ctx SpecContext) {
		s := orcatalog.New(db)
		m := uniq()
		_, err := s.Apply(ctx, snapshot(t0, meta(m, 200000, 64000)), nil)
		Expect(err).NotTo(HaveOccurred())

		got, ok, err := s.Effective(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(got.Name).To(Equal("Fake " + m))
		Expect(got.Tokenizer).To(Equal("Claude"))
		Expect(got.SupportedParameters).To(Equal([]string{"tools"}))
		Expect(got.Source).To(Equal("openrouter@2026-11-11T08:00:00Z"))
		Expect(got.FetchedAt).To(BeTemporally("==", t0))
		// The behaviour-changing limits are stored as fetched but are not effective.
		Expect(got.FetchedContextLength).To(Equal(200000))
		Expect(got.ContextLength).To(BeNil(), "models.dev's value applies until a person accepts")
		Expect(got.MaxOutput).To(BeNil())

		_, ok, err = s.Effective(ctx, "fake/never-fetched")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("refreshes weekly: not due inside the week, due after it, and due for a new model in use", func(ctx SpecContext) {
		// A database of its own: the catalog row is a singleton.
		s := orcatalog.New(freshDB(ctx))
		due, err := s.Due(ctx, t0, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(due).To(BeTrue(), "never stored")

		m := uniq()
		_, err = s.Apply(ctx, snapshot(t0, meta(m, 1000, 100)), []string{m})
		Expect(err).NotTo(HaveOccurred())
		for name, c := range map[string]struct {
			at     time.Time
			inUse  []string
			listed []string
			want   bool
		}{
			"a restart an hour later":            {t0.Add(time.Hour), []string{m}, []string{m}, false},
			"just under a week":                  {t0.Add(orcatalog.Interval - time.Minute), []string{m}, []string{m}, false},
			"a week":                             {t0.Add(orcatalog.Interval), []string{m}, []string{m}, true},
			"a listed model in use that is new":  {t0.Add(time.Hour), []string{m, "fake/added-later"}, []string{m, "fake/added-later"}, true},
			"in use but not listed by the fetch": {t0.Add(time.Hour), []string{m, "fake/delisted"}, []string{m}, false},
			"no models in use, inside week":      {t0.Add(time.Hour), nil, []string{m}, false},
		} {
			due, err := s.Due(ctx, c.at, c.inUse, c.listed)
			Expect(err).NotTo(HaveOccurred(), name)
			Expect(due).To(Equal(c.want), name)
		}
	})

	Describe("limit proposals", func() {
		It("proposes the fetched limits of a model in use and changes nothing until accepted", func(ctx SpecContext) {
			s := orcatalog.New(db)
			m := uniq()
			res, err := s.Apply(ctx, snapshot(t0, meta(m, 200000, 64000)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Models).To(Equal(1))
			Expect(res.Proposals).To(Equal(2))

			ps := forModel(ctx, s, m)
			Expect(ps).To(HaveLen(2))
			for _, p := range ps {
				Expect(p.State).To(Equal(orcatalog.StateOpen))
				Expect(p.CurrentValue).To(BeNil(), "nothing accepted yet: models.dev applies")
				Expect(p.Source).To(Equal("openrouter@2026-11-11T08:00:00Z"))
			}

			// The same fetch again does not make a second proposal.
			res, err = s.Apply(ctx, snapshot(t0.Add(time.Hour), meta(m, 200000, 64000)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(BeZero())
			Expect(forModel(ctx, s, m)).To(HaveLen(2))

			got, _, err := s.Effective(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ContextLength).To(BeNil())
		})

		It("makes none for a model nobody uses", func(ctx SpecContext) {
			s := orcatalog.New(db)
			m := uniq()
			res, err := s.Apply(ctx, snapshot(t0, meta(m, 200000, 64000)), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(BeZero())
			Expect(forModel(ctx, s, m)).To(BeEmpty())
		})

		It("proposes nothing for a limit the list does not give", func(ctx SpecContext) {
			s := orcatalog.New(db)
			m := uniq()
			res, err := s.Apply(ctx, snapshot(t0, meta(m, 200000, 0)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(Equal(1))
			Expect(forModel(ctx, s, m)[0].Field).To(Equal(orcatalog.FieldContextLength))
		})

		It("applies an accepted limit, audits it, and proposes against it later", func(ctx SpecContext) {
			s := orcatalog.New(db)
			m := uniq()
			_, err := s.Apply(ctx, snapshot(t0, meta(m, 200000, 64000)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			var ctxProposal orcatalog.Proposal
			for _, p := range forModel(ctx, s, m) {
				if p.Field == orcatalog.FieldContextLength {
					ctxProposal = p
				}
			}
			before := installEvents(ctx, "model_metadata.limit.accepted")
			Expect(s.Accept(ctx, audit.CLI("test"), ctxProposal.ID)).To(Succeed())
			Expect(installEvents(ctx, "model_metadata.limit.accepted")).To(Equal(before + 1))

			got, _, err := s.Effective(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ContextLength).NotTo(BeNil())
			Expect(*got.ContextLength).To(Equal(200000))
			Expect(got.MaxOutput).To(BeNil(), "the other field is still waiting")

			// A decided proposal cannot be decided again.
			err = s.Accept(ctx, audit.CLI("test"), ctxProposal.ID)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&orcatalog.InvalidError{}))

			// A later fetch with the same value proposes nothing; a different one proposes against the accepted value.
			res, err := s.Apply(ctx, snapshot(t0.Add(orcatalog.Interval), meta(m, 200000, 64000)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(BeZero())
			res, err = s.Apply(ctx, snapshot(t0.Add(2*orcatalog.Interval), meta(m, 1000000, 64000)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(Equal(1))
			var next orcatalog.Proposal
			for _, p := range forModel(ctx, s, m) {
				if p.State == orcatalog.StateOpen && p.Field == orcatalog.FieldContextLength {
					next = p
				}
			}
			Expect(next.ProposedValue).To(Equal(1000000))
			Expect(*next.CurrentValue).To(Equal(200000))
			got, _, _ = s.Effective(ctx, m)
			Expect(*got.ContextLength).To(Equal(200000), "the proposal changed nothing")
		})

		It("remembers a dismissed value and supersedes a proposal the fetch moved past", func(ctx SpecContext) {
			s := orcatalog.New(db)
			m := uniq()
			_, err := s.Apply(ctx, snapshot(t0, meta(m, 100000, 0)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			first := forModel(ctx, s, m)[0]
			Expect(s.Dismiss(ctx, audit.CLI("test"), first.ID)).To(Succeed())
			Expect(installEvents(ctx, "model_metadata.limit.dismissed")).To(BeNumerically(">=", 1))

			res, err := s.Apply(ctx, snapshot(t0.Add(time.Hour), meta(m, 100000, 0)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Proposals).To(BeZero(), "the same value does not come back")

			_, err = s.Apply(ctx, snapshot(t0.Add(2*time.Hour), meta(m, 150000, 0)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			_, err = s.Apply(ctx, snapshot(t0.Add(3*time.Hour), meta(m, 160000, 0)), []string{m})
			Expect(err).NotTo(HaveOccurred())
			states := map[int]string{}
			for _, p := range forModel(ctx, s, m) {
				states[p.ProposedValue] = p.State
			}
			Expect(states).To(Equal(map[int]string{100000: "dismissed", 150000: "superseded", 160000: "open"}))
		})

		It("refuses an unknown or malformed proposal id", func(ctx SpecContext) {
			s := orcatalog.New(db)
			Expect(s.Accept(ctx, audit.CLI("test"), "not-a-uuid")).To(BeAssignableToTypeOf(&orcatalog.InvalidError{}))
			Expect(s.Accept(ctx, audit.CLI("test"), "0197f2a0-0000-7000-8000-000000000000")).To(MatchError(orcatalog.ErrNotFound))
		})
	})

	It("stores a model's endpoints without prices, and skips a model with no row", func(ctx SpecContext) {
		s := orcatalog.New(db)
		m := uniq()
		_, err := s.Apply(ctx, snapshot(t0, meta(m, 1000, 100)), nil)
		Expect(err).NotTo(HaveOccurred())
		at := t0.Add(time.Minute)
		Expect(s.StoreEndpoints(ctx, pricefetch.ModelEndpoints{
			Model: m, FetchedAt: at,
			Endpoints: []pricefetch.Endpoint{{Provider: "Fake Up", Quantization: "fp8", ContextLength: 1000}},
		})).To(Succeed())
		Expect(s.StoreEndpoints(ctx, pricefetch.ModelEndpoints{Model: "fake/no-row", FetchedAt: at})).To(Succeed())

		got, _, err := s.Effective(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Endpoints).To(HaveLen(1))
		Expect(got.Endpoints[0].Provider).To(Equal("Fake Up"))
		Expect(got.EndpointsSource).To(Equal("openrouter@2026-11-11T08:01:00Z"))
	})
})

func forModel(ctx context.Context, s *orcatalog.Store, model string) []orcatalog.Proposal {
	all, err := s.ListProposals(ctx, "", 1000)
	Expect(err).NotTo(HaveOccurred())
	var out []orcatalog.Proposal
	for _, p := range all {
		if p.Model == model {
			out = append(out, p)
		}
	}
	return out
}
