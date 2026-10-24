// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/scheduler"
)

var _ = Describe("Ingester", func() {
	BeforeEach(needDocker)

	It("attributes each request to the run the agent had at its time", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		first := e.startRun(ctx, e.ticket(ctx), 100_000, t0)
		e.finishRun(ctx, first.ID, t0.Add(10*time.Minute))
		second := e.startRun(ctx, e.ticket(ctx), 100_000, t0.Add(20*time.Minute))

		before := e.entry(t0.Add(-time.Minute), "0.001", 1, 1)                 // no run yet
		during := e.entry(t0.Add(5*time.Minute), "0.002", 10, 20)              // first run
		grace := e.entry(t0.Add(10*time.Minute+30*time.Second), "0.003", 1, 1) // just after it finished
		idle := e.entry(t0.Add(15*time.Minute), "0.004", 1, 1)                 // between runs
		later := e.entry(t0.Add(25*time.Minute), "0.005", 100, 200)            // second run

		res, err := ing.Ingest(ctx, []ledger.Entry{before, during, grace, idle, later})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ledger.Result{Recorded: 5}))

		runOf := map[string]pgtype.UUID{}
		rows, err := db.Query(ctx, `SELECT request_id, run_id FROM spend_entries WHERE org_id = $1`, e.org)
		Expect(err).NotTo(HaveOccurred())
		for rows.Next() {
			var id string
			var run pgtype.UUID
			Expect(rows.Scan(&id, &run)).To(Succeed())
			runOf[id] = run
		}
		Expect(runOf[before.RequestID].Valid).To(BeFalse())
		Expect(runOf[during.RequestID]).To(Equal(first.ID))
		Expect(runOf[grace.RequestID]).To(Equal(first.ID))
		Expect(runOf[idle.RequestID].Valid).To(BeFalse())
		Expect(runOf[later.RequestID]).To(Equal(second.ID))

		// Tokens of the running run feed its token cap; the finished run's are dropped there
		// (the ledger keeps them).
		Expect(e.run(ctx, second.ID).TokensUsed).To(Equal(int64(300)))
	})

	It("records a replayed export once and charges its tokens once", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		r := e.startRun(ctx, e.ticket(ctx), 100_000, time.Now().Add(-time.Minute))
		batch := []ledger.Entry{e.entry(time.Now(), "0.01", 100, 50), e.entry(time.Now(), "0.02", 200, 100)}

		res, err := ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Recorded).To(Equal(2))
		res, err = ing.Ingest(ctx, batch)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ledger.Result{Duplicates: 2}))

		Expect(e.spendEntries(ctx)).To(Equal(2))
		Expect(e.run(ctx, r.ID).TokensUsed).To(Equal(int64(450)))
	})

	It("cancels the run at its token cap", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 1000)
		r := e.startRun(ctx, e.ticket(ctx), 1000, time.Now().Add(-time.Minute))
		_, err := ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.01", 600, 500)})
		Expect(err).NotTo(HaveOccurred())

		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(ContainSubstring("token cap"))
	})

	It("cancels the run with a ticket event when the agent's spend reaches its limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05", 100_000)
		t := e.ticket(ctx)
		r := e.startRun(ctx, t, 100_000, time.Now().Add(-time.Minute))
		// Yesterday's spend is another window.
		yesterday := e.entry(time.Now().Add(-ledger.Window), "1.00", 1, 1)
		_, err := ing.Ingest(ctx, []ledger.Entry{yesterday, e.entry(time.Now(), "0.03", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateRunning))

		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.025", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		run := e.run(ctx, r.ID)
		Expect(run.State).To(Equal(store.RunStateCanceled))
		Expect(run.OutcomeReason.String).To(HavePrefix("agent daily limit reached ($0.055 of $0.05"))

		events, err := q.ListTicketEvents(ctx, store.ListTicketEventsParams{OrgID: e.org, TicketID: t.ID, MaxEvents: 100})
		Expect(err).NotTo(HaveOccurred())
		var payload map[string]any
		for _, ev := range events {
			if ev.Kind == scheduler.KindRunCanceled {
				Expect(json.Unmarshal(ev.Payload, &payload)).To(Succeed())
			}
		}
		Expect(payload).To(HaveKeyWithValue("reason", string(scheduler.ReasonSpendLimit)))
		Expect(payload).To(HaveKeyWithValue("spend_limit_usd", "0.05"))
		Expect(payload).To(HaveKeyWithValue("limit", "agent_daily"))
	})

	It("counts entries of unknown orgs and agents as unattributed", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		unknownAgent := e.entry(time.Now(), "0.01", 1, 1)
		unknownAgent.Agent.Name = "019a0000-0000-7000-8000-00000000ffff"
		unknownOrg := e.entry(time.Now(), "0.01", 1, 1)
		unknownOrg.Agent = gateway.AgentRef{Org: "no-such-org", Name: e.agent.ID.String()}
		res, err := ing.Ingest(ctx, []ledger.Entry{unknownAgent, unknownOrg, e.entry(time.Now(), "0.01", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ledger.Result{Recorded: 1, Unattributed: 2}))
	})
})

var _ = Describe("Totals", func() {
	BeforeEach(needDocker)

	It("sums cost and tokens per run and per ticket", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		t := e.ticket(ctx)
		t0 := time.Now().Add(-time.Hour)
		r1 := e.startRun(ctx, t, 100_000, t0)
		e.finishRun(ctx, r1.ID, t0.Add(10*time.Minute))
		r2 := e.startRun(ctx, t, 100_000, t0.Add(20*time.Minute))
		other := e.ticket(ctx)

		_, err := ing.Ingest(ctx, []ledger.Entry{
			e.entry(t0.Add(time.Minute), "0.0001", 10, 20),
			e.entry(t0.Add(2*time.Minute), "0.0002", 30, 40),
			e.entry(t0.Add(21*time.Minute), "1.5", 100, 200),
		})
		Expect(err).NotTo(HaveOccurred())

		runs, err := q.SumRunCosts(ctx, store.SumRunCostsParams{OrgID: e.org, RunIds: []pgtype.UUID{r1.ID, r2.ID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(runs).To(HaveLen(2))
		byRun := map[pgtype.UUID]store.SumRunCostsRow{}
		for _, r := range runs {
			byRun[r.RunID] = r
		}
		Expect(byRun[r1.ID].Requests).To(Equal(int64(2)))
		Expect(byRun[r1.ID].InputTokens).To(Equal(int64(40)))
		Expect(byRun[r1.ID].OutputTokens).To(Equal(int64(60)))
		Expect(ledger.FormatUSD(byRun[r1.ID].CostUsd)).To(Equal("0.0003"))
		Expect(ledger.FormatUSD(byRun[r2.ID].CostUsd)).To(Equal("1.50"))

		tickets, err := q.SumTicketCosts(ctx, store.SumTicketCostsParams{OrgID: e.org, TicketIds: []pgtype.UUID{t.ID, other.ID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(tickets).To(HaveLen(1), "a ticket without spend has no row")
		Expect(tickets[0].TicketID).To(Equal(t.ID))
		Expect(tickets[0].Requests).To(Equal(int64(3)))
		Expect(ledger.FormatUSD(tickets[0].CostUsd)).To(Equal("1.5003"))
		Expect(tickets[0].Priced).To(BeTrue())
	})
})

var _ = Describe("AgentLimit", func() {
	BeforeEach(needDocker)

	It("nets the template's limit against the current window's spend", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		now := time.Now()
		l, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeNumerically("==", 10))

		_, err = ing.Ingest(ctx, []ledger.Entry{
			e.entry(now.Add(-ledger.Window), "5", 1, 1), // the previous window
			e.entry(now, "2.25", 1, 1),
		})
		Expect(err).NotTo(HaveOccurred())
		net, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(net.Amount).To(BeNumerically("==", 7.75))
		Expect(net.Generation).NotTo(Equal(l.Generation))
	})

	It("has no limit for an agent without an active version", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00", 100_000)
		a := e.agent
		a.ActiveVersionID = pgtype.UUID{}
		l, err := ledger.AgentLimit(ctx, q, a, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(BeNil())
	})
})
