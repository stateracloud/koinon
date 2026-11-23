// SPDX-License-Identifier: Apache-2.0

package remoteagents_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/remoteagents"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// fakeCards is a card fetcher: the card of a URL is what the test set, or the error it set.
type fakeCards struct {
	mu    sync.Mutex
	cards map[string]string
	err   error
	last  remoteagents.FetchRequest
	hook  func()
}

// hx makes a valid card hash (64 hex characters) from a short label.
func hx(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func (f *fakeCards) set(url, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards[url] = hash
}

func (f *fakeCards) fetch(_ context.Context, r remoteagents.FetchRequest, _ catalog.URLPolicy) (*a2aout.Card, error) {
	if h := f.hook; h != nil {
		f.hook = nil
		h()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = r
	if f.err != nil {
		return nil, f.err
	}
	h, ok := f.cards[r.URL]
	if !ok {
		h = hx("hash-of-" + r.URL)
	}
	return &a2aout.Card{Hash: h, JSON: `{"name":"` + h + `"}`}, nil
}

var _ = Describe("remote agents", func() {
	var (
		svc   *remoteagents.Service
		fake  *fakeCards
		org   pgtype.UUID
		user  pgtype.UUID
		actor audit.Actor
	)
	const url = "https://agent.example.com"

	BeforeEach(func(ctx SpecContext) {
		fake = &fakeCards{cards: map[string]string{}}
		svc = remoteagents.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"}).WithFetcher(fake.fetch)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
	})
	create := func(ctx SpecContext, name, u string) remoteagents.RemoteAgent {
		GinkgoHelper()
		r, err := svc.Create(ctx, org, actor, user, remoteagents.Input{Name: name, CardURL: u})
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	actions := func(ctx SpecContext, prefix string) int {
		GinkgoHelper()
		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 200})
		Expect(err).NotTo(HaveOccurred())
		n := 0
		for _, r := range rows {
			if r.Action == prefix {
				n++
			}
		}
		return n
	}

	It("creates an agent, pins its card and audits it", func(ctx SpecContext) {
		fake.set(url, hx("h1"))
		r := create(ctx, "billing", url)
		Expect(r.CardHash).To(Equal(hx("h1")))
		Expect(r.Health).To(Equal(remoteagents.HealthUnknown))
		Expect(r.CardChanged()).To(BeFalse())
		got, err := svc.Get(ctx, org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Name).To(Equal("billing"))
		Expect(actions(ctx, "remote_agent.created")).To(Equal(1))
		c, err := svc.GetCard(ctx, org, r.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Hash).To(Equal(hx("h1")))
		Expect(c.JSON).To(ContainSubstring(hx("h1")))
	})

	It("refuses a bad name, a duplicate name and an unreadable card", func(ctx SpecContext) {
		_, err := svc.Create(ctx, org, actor, user, remoteagents.Input{Name: "Bad Name", CardURL: url})
		Expect(err).To(HaveOccurred())
		create(ctx, "dup", url)
		_, err = svc.Create(ctx, org, actor, user, remoteagents.Input{Name: "dup", CardURL: url + "/b"})
		Expect(errors.Is(err, remoteagents.ErrConflict)).To(BeTrue())
		fake.err = a2aout.ErrCard
		_, err = svc.Create(ctx, org, actor, user, remoteagents.Input{Name: "bad", CardURL: url + "/c"})
		var inv *remoteagents.InvalidError
		Expect(errors.As(err, &inv)).To(BeTrue())
		list, err := svc.List(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(1))
	})

	It("seals the credential and keeps it out of the row text", func(ctx SpecContext) {
		r, err := svc.Create(ctx, org, actor, user, remoteagents.Input{
			Name: "secured", CardURL: url, CredentialPrefix: "Bearer ", CredentialValue: "koinon_test_not-a-real-token",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasCredential).To(BeTrue())
		Expect(r.CredentialHeader).To(Equal("Authorization"))
		Expect(fake.last.Secret).To(Equal("koinon_test_not-a-real-token"))
		var raw []byte
		Expect(pool.QueryRow(ctx, `SELECT credential FROM remote_agents WHERE id = $1`, r.ID).Scan(&raw)).To(Succeed())
		Expect(string(raw)).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
	})

	// The gateway scrubs an opened credential only from 8 bytes (ADR 0097), so a shorter one is refused at
	// save, before the card is read with it (#1221). The fake values are short on purpose: 7 and 8 bytes.
	It("refuses a credential of 7 bytes and takes 8, at create and at update", func(ctx SpecContext) {
		const short, long = "fake-07", "fake-008"
		tooShort := func(err error) {
			GinkgoHelper()
			var inv *remoteagents.InvalidError
			Expect(errors.As(err, &inv)).To(BeTrue(), "%v", err)
			Expect(inv.Reason).To(Equal(mcpservers.ReasonCredentialTooShort))
			Expect(inv.Msg).NotTo(ContainSubstring(short), "the message never holds the value")
		}
		_, err := svc.Create(ctx, org, actor, user, remoteagents.Input{Name: "secured", CardURL: url, CredentialValue: short})
		tooShort(err)
		Expect(fake.last.Secret).To(BeEmpty(), "the card was not read with the value")
		list, err := svc.List(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(BeEmpty())

		r, err := svc.Create(ctx, org, actor, user, remoteagents.Input{Name: "secured", CardURL: url, CredentialValue: long})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.HasCredential).To(BeTrue())

		v := short
		_, err = svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{Credential: &remoteagents.CredentialPatch{Header: "Authorization", Value: &v}})
		tooShort(err)
		v = "fake-00008"
		_, err = svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{Credential: &remoteagents.CredentialPatch{Header: "Authorization", Value: &v}})
		Expect(err).NotTo(HaveOccurred())
	})

	It("re-pins on a URL change and keeps the pin on other changes", func(ctx SpecContext) {
		fake.set(url, hx("h1"))
		fake.set(url+"/new", hx("h2"))
		r := create(ctx, "mover", url)
		other := "https://other.example.com"
		_ = other
		up, err := svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{InsecureSkipVerify: nil, CABundle: nil})
		Expect(err).NotTo(HaveOccurred())
		Expect(up.CardHash).To(Equal(hx("h1")))
		nu := url + "/new"
		up, err = svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{CardURL: &nu})
		Expect(err).NotTo(HaveOccurred())
		Expect(up.CardURL).To(Equal(nu))
		Expect(up.CardHash).To(Equal(hx("h2")))
		Expect(actions(ctx, "remote_agent.updated")).To(Equal(2))
	})

	It("deletes an agent with its grants", func(ctx SpecContext) {
		r := create(ctx, "gone", url)
		Expect(svc.Delete(ctx, org, actor, r.ID)).To(Succeed())
		_, err := svc.Get(ctx, org, r.ID)
		Expect(errors.Is(err, remoteagents.ErrNotFound)).To(BeTrue())
		Expect(actions(ctx, "remote_agent.deleted")).To(Equal(1))
	})

	It("isolates orgs", func(ctx SpecContext) {
		r := create(ctx, "mine", url)
		org2, _, _ := pgtest.NewOrg(ctx, pool)
		_, err := svc.Get(ctx, org2, r.ID)
		Expect(errors.Is(err, remoteagents.ErrNotFound)).To(BeTrue())
		list, err := svc.List(ctx, org2)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(BeEmpty())
	})

	Describe("health", func() {
		It("turns unhealthy after the failure threshold, then recovers", func(ctx SpecContext) {
			r := create(ctx, "flaky", url)
			id := pgtype.UUID{}
			Expect(id.Scan(r.ID)).To(Succeed())
			var out remoteagents.RemoteAgent
			var err error
			for i := 1; i < remoteagents.FailureThreshold; i++ {
				out, err = svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Reason: remoteagents.ReasonUnreachable})
				Expect(err).NotTo(HaveOccurred())
				Expect(out.Health).NotTo(Equal(remoteagents.HealthUnhealthy))
			}
			out, err = svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Reason: remoteagents.ReasonUnreachable})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Health).To(Equal(remoteagents.HealthUnhealthy))
			Expect(out.HealthReason).To(Equal(remoteagents.ReasonUnreachable))
			out, err = svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Card: &a2aout.Card{Hash: r.CardHash, JSON: "{}"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Health).To(Equal(remoteagents.HealthHealthy))
			Expect(out.ConsecutiveFailures).To(BeZero())
		})

		It("treats a changed card as a finding, keeps the pin, and holds the finding until accepted", func(ctx SpecContext) {
			fake.set(url, hx("h1"))
			r := create(ctx, "watched", url)
			id := pgtype.UUID{}
			Expect(id.Scan(r.ID)).To(Succeed())
			out, err := svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Card: &a2aout.Card{Hash: hx("h2"), JSON: `{"name":"evil"}`}})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Health).To(Equal(remoteagents.HealthUnhealthy))
			Expect(out.HealthReason).To(Equal(remoteagents.ReasonCardChanged))
			Expect(out.CardHash).To(Equal(hx("h1")))
			Expect(out.ObservedCardHash).To(Equal(hx("h2")))
			Expect(out.CardChanged()).To(BeTrue())

			// The card reverts to the pin: the finding stays.
			out, err = svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Card: &a2aout.Card{Hash: hx("h1"), JSON: "{}"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Health).To(Equal(remoteagents.HealthUnhealthy))
			Expect(out.HealthReason).To(Equal(remoteagents.ReasonCardUnaccept))

			// A wrong hash is refused; the right one moves the pin and clears the finding.
			_, err = svc.AcceptCard(ctx, org, actor, r.ID, hx("h3"))
			Expect(errors.Is(err, remoteagents.ErrCardMoved)).To(BeTrue())
			Expect(errors.Is(err, remoteagents.ErrConflict)).To(BeTrue())
			acc, err := svc.AcceptCard(ctx, org, actor, r.ID, hx("h2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(acc.CardHash).To(Equal(hx("h2")))
			Expect(acc.CardChanged()).To(BeFalse())
			Expect(acc.Health).NotTo(Equal(remoteagents.HealthUnhealthy))
			Expect(actions(ctx, "remote_agent.card.accepted")).To(Equal(1))

			// Nothing left to accept.
			_, err = svc.AcceptCard(ctx, org, actor, r.ID, hx("h2"))
			Expect(err).To(HaveOccurred())
		})

		It("keeps the finding's reason when a later read fails", func(ctx SpecContext) {
			r := create(ctx, "down", url)
			id := pgtype.UUID{}
			Expect(id.Scan(r.ID)).To(Succeed())
			_, err := svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Card: &a2aout.Card{Hash: hx("other"), JSON: "{}"}})
			Expect(err).NotTo(HaveOccurred())
			out, err := svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Reason: remoteagents.ReasonUnreachable})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.HealthReason).To(Equal(remoteagents.ReasonCardChanged))
			Expect(out.ConsecutiveFailures).To(Equal(1))
		})

		It("drops a result that was read for a row an admin changed since", func(ctx SpecContext) {
			r := create(ctx, "stale", url)
			rows, err := svc.ListCheckable(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].ID.String()).To(Equal(r.ID))
			seen := rows[0]
			nu := url + "/new"
			_, err = svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{CardURL: &nu})
			Expect(err).NotTo(HaveOccurred())
			out, err := svc.RecordCheck(ctx, org, rows[0].ID, remoteagents.Outcome{Card: &a2aout.Card{Hash: hx("old-url-card"), JSON: "{}"}, Seen: &seen})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.CardChanged()).To(BeFalse())
			Expect(out.Health).To(Equal(remoteagents.HealthUnknown))
		})

		It("lets a health write land while an update reads the new card, and refuses an admin change", func(ctx SpecContext) {
			r := create(ctx, "racy", url)
			id := pgtype.UUID{}
			Expect(id.Scan(r.ID)).To(Succeed())
			nu := url + "/new"
			// A health write between phase 1 and phase 2 of Update is not a conflict.
			fake.hook = func() {
				_, err := svc.RecordCheck(ctx, org, id, remoteagents.Outcome{Reason: remoteagents.ReasonUnreachable})
				Expect(err).NotTo(HaveOccurred())
			}
			up, err := svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{CardURL: &nu})
			Expect(err).NotTo(HaveOccurred())
			Expect(up.CardURL).To(Equal(nu))
			// An admin change between the phases is refused.
			other := url + "/other"
			fake.hook = func() {
				u := url + "/third"
				_, err := svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{CardURL: &u})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = svc.Update(ctx, org, actor, r.ID, remoteagents.Patch{CardURL: &other})
			Expect(errors.Is(err, remoteagents.ErrChanged)).To(BeTrue())
		})

		It("checks through the fetcher and records the card", func(ctx SpecContext) {
			fake.set(url, hx("h1"))
			r := create(ctx, "polled", url)
			rows, err := svc.ListCheckable(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			orgs, err := svc.ListOrgsToCheck(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(orgs).To(ContainElement(org))

			out, err := svc.Check(ctx, rows[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Health).To(Equal(remoteagents.HealthHealthy))

			fake.set(url, hx("h9"))
			rows, err = svc.ListCheckable(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			out, err = svc.Check(ctx, rows[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(out.ObservedCardHash).To(Equal(hx("h9")))
			Expect(out.CardHash).To(Equal(r.CardHash))

			fake.err = a2aout.ErrCard
			rows, err = svc.ListCheckable(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			out, err = svc.Check(ctx, rows[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(out.ConsecutiveFailures).To(Equal(1))
			Expect(out.HealthReason).NotTo(BeEmpty())
		})
	})

	Describe("agent grants", func() {
		var agent store.Agent
		BeforeEach(func(ctx SpecContext) {
			var err error
			agent, err = store.New(pool).CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "a", Harness: harness.OpenCode, CreatedBy: user})
			Expect(err).NotTo(HaveOccurred())
		})

		It("replaces the grants, refuses unknown and duplicate ids, and audits", func(ctx SpecContext) {
			a, b := create(ctx, "aa", url), create(ctx, "bb", url+"/b")
			got, err := svc.SetAgentGrants(ctx, org, actor, agent.ID, []string{a.ID, b.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ConsistOf(a.ID, b.ID))
			got, err = svc.SetAgentGrants(ctx, org, actor, agent.ID, []string{b.ID})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]string{b.ID}))
			_, err = svc.SetAgentGrants(ctx, org, actor, agent.ID, []string{b.ID, b.ID})
			Expect(err).To(HaveOccurred())
			_, err = svc.SetAgentGrants(ctx, org, actor, agent.ID, []string{"00000000-0000-0000-0000-000000000001"})
			Expect(err).To(HaveOccurred())
			Expect(actions(ctx, "agent.remote_agents.set")).To(Equal(2))
			g, err := svc.AgentGrants(ctx, org, agent.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(g).To(Equal([]string{b.ID}))
		})

		It("refuses a remote agent of another org", func(ctx SpecContext) {
			org2, user2, _ := pgtest.NewOrg(ctx, pool)
			foreign, err := svc.Create(ctx, org2, audit.User(user2), user2, remoteagents.Input{Name: "foreign", CardURL: url})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.SetAgentGrants(ctx, org, actor, agent.ID, []string{foreign.ID})
			Expect(err).To(HaveOccurred())
		})
	})
})
