// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

var _ = Describe("Ingester", func() {
	BeforeEach(needDocker)

	It("attributes each request to the run the agent had at its time", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		first := e.startRun(ctx, e.task(ctx), t0)
		e.finishRun(ctx, first.ID, t0.Add(10*time.Minute))
		second := e.startRun(ctx, e.task(ctx), t0.Add(20*time.Minute))

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
		e := newEnv(ctx, uniq("org"), "", "10.00")
		r := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
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

	It("wakes the reconciler once, when a request reaches the limit (Kubernetes mode revokes the key then)", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05")
		conn, err := db.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, `LISTEN koinon_reconcile`)
		Expect(err).NotTo(HaveOccurred())
		drain := func() []string {
			var got []string
			for {
				wctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				n, err := conn.Conn().WaitForNotification(wctx)
				cancel()
				if err != nil {
					return got
				}
				got = append(got, n.Payload)
			}
		}
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.03", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(drain()).NotTo(ContainElement(e.org.String()), "below the limit")
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.025", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(drain()).To(ContainElement(fmt.Sprint(e.org)), "the request that reaches the limit")
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.01", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(drain()).NotTo(ContainElement(fmt.Sprint(e.org)), "requests past the limit do not wake it again")
	})

	It("wakes the reconciler and cancels the org's runs when a late entry crosses the org cap of an agent whose daily limit is long spent", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05")
		_, err := db.Exec(ctx, `INSERT INTO org_budgets (org_id, monthly_cap, updated_by) VALUES ($1, 1.00, 'test')`, e.org)
		Expect(err).NotTo(HaveOccurred())
		r := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
		// The agent's daily limit ($0.05) is spent first, then the org cap ($1.00) is crossed.
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.06", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		conn, err := db.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, `LISTEN koinon_reconcile`)
		Expect(err).NotTo(HaveOccurred())
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.95", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		Expect(err).NotTo(HaveOccurred(), "the org cap was crossed")
		Expect(n.Payload).To(Equal(fmt.Sprint(e.org)))
		Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled))
		Expect(e.run(ctx, r.ID).OutcomeReason.String).To(Or(HavePrefix("agent daily limit"), HavePrefix("org monthly cap")))
	})

	Describe("the org's system key (the provider echo check)", func() {
		sysEntry := func(e *env, t time.Time, cost string) ledger.Entry {
			x := e.entry(t, cost, 1, 1)
			x.Agent = gateway.AgentRef{Org: e.slug, Name: gateway.ModelCheckAgentName}
			x.KeyID = "k-system"
			return x
		}

		It("records the request with the principal system, no agent and no run, and counts it against the org", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
			res, err := ing.Ingest(ctx, []ledger.Entry{sysEntry(e, time.Now(), "0.03")})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Recorded).To(Equal(1))
			var principal string
			var agentNull, runNull bool
			Expect(db.QueryRow(ctx, `SELECT principal, agent_id IS NULL, run_id IS NULL FROM spend_entries WHERE org_id = $1`, e.org).Scan(&principal, &agentNull, &runNull)).To(Succeed())
			Expect(principal).To(Equal("system"))
			Expect(agentNull && runNull).To(BeTrue())

			agentSpent, err := q.GetAgentSpendSince(ctx, store.GetAgentSpendSinceParams{OrgID: e.org, AgentID: e.agent.ID, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger.FormatUSD(agentSpent)).To(Equal("0.00"), "not the agent's spend")
			orgSpent, err := q.GetOrgSpendSince(ctx, store.GetOrgSpendSinceParams{OrgID: e.org, Since: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger.FormatUSD(orgSpent)).To(Equal("0.03"), "the org's")
		})

		It("stops the org's runs when it reaches the org cap, and wakes the reconciler", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			_, err := db.Exec(ctx, `INSERT INTO org_budgets (org_id, monthly_cap, updated_by) VALUES ($1, 0.05, 'test')`, e.org)
			Expect(err).NotTo(HaveOccurred())
			r := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
			_, err = ing.Ingest(ctx, []ledger.Entry{sysEntry(e, time.Now(), "0.04")})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, r.ID).State).NotTo(Equal(store.RunStateCanceled))
			_, err = ing.Ingest(ctx, []ledger.Entry{sysEntry(e, time.Now(), "0.02")})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, r.ID).State).To(Equal(store.RunStateCanceled))
			Expect(e.run(ctx, r.ID).OutcomeReason.String).To(HavePrefix("org monthly cap"))
		})

		It("does not stop runs for the key's own daily budget, but the next limit pushed is zero", func(ctx SpecContext) {
			e := newEnv(ctx, uniq("org"), "", "10.00")
			r := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
			_, err := ing.Ingest(ctx, []ledger.Entry{sysEntry(e, time.Now(), "0.11")})
			Expect(err).NotTo(HaveOccurred())
			Expect(e.run(ctx, r.ID).State).NotTo(Equal(store.RunStateCanceled))
			l, err := ledger.SystemLimit(ctx, q, e.org, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(l.Amount).To(BeZero())
			Expect(l.Unit).To(Equal(gateway.USD))
		})

		It("leaves an entry of an org that does not exist unattributed", func(ctx SpecContext) {
			x := ledger.Entry{RequestID: uniq("req"), KeyID: "k", Agent: gateway.AgentRef{Org: "no-such-org", Name: gateway.ModelCheckAgentName}, Model: "m", InputTokens: 1, OutputTokens: 1, CostUSD: "0.01", Time: time.Now()}
			res, err := ing.Ingest(ctx, []ledger.Entry{x})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Unattributed).To(Equal(1))
		})
	})

	It("cancels the run with a task event when the agent's spend reaches its limit", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05")
		t := e.task(ctx)
		r := e.startRun(ctx, t, time.Now().Add(-time.Minute))
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
		Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))

		events, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: e.org, TaskID: t.ID, MaxEvents: 100})
		Expect(err).NotTo(HaveOccurred())
		var payload map[string]any
		for _, ev := range events {
			if ev.Kind == runoutcome.KindRunCanceled {
				Expect(json.Unmarshal(ev.Payload, &payload)).To(Succeed())
			}
		}
		Expect(payload).To(HaveKeyWithValue("reason", string(runoutcome.ReasonSpendLimit)))
		Expect(payload).To(HaveKeyWithValue("spend_limit_usd", "0.05"))
		Expect(payload).To(HaveKeyWithValue("limit", "agent_daily"))
		Expect(payload).To(HaveKeyWithValue("failureClass", "budget"))
	})

	// RT1 on rc.5: two tasks of one agent created together, a daily limit, and the agent ended the
	// day above it. The runs were not concurrent (one active run per agent is a unique index); the
	// overshoot is the response in flight when the limit is reached. This holds that bound: the
	// agent's other run is stopped with the budget reason, never starts on the spent budget, and
	// only the in-flight response is recorded past the limit.
	It("bounds an agent's overshoot to the response in flight, with two tasks queued together", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "0.05")
		first := e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))
		second, err := q.CreateRun(ctx, store.CreateRunParams{
			OrgID: e.org, AgentID: e.agent.ID, TaskID: e.task(ctx).ID, MaxWallClockSeconds: 1800,
		})
		Expect(err).NotTo(HaveOccurred())
		claim := func() error {
			_, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: e.org, OwnerID: "agentd-2", LeaseSeconds: 60})
			return err
		}
		Expect(claim()).To(MatchError(pgx.ErrNoRows), "one run per agent: the second task waits")

		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.03", 1, 1), e.entry(time.Now(), "0.025", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		for _, id := range []pgtype.UUID{first.ID, second.ID} {
			run := e.run(ctx, id)
			Expect(run.State).To(Equal(store.RunStateCanceled))
			Expect(run.FailureClass.RunFailureClass).To(Equal(store.RunFailureClassBudget))
			Expect(run.OutcomeReason.String).To(HavePrefix("agent daily limit reached ($0.055 of $0.05"))
		}
		Expect(claim()).To(MatchError(pgx.ErrNoRows), "nothing starts on the spent budget")

		By("the response that was in flight is still recorded, and is the whole overshoot")
		_, err = ing.Ingest(ctx, []ledger.Entry{e.entry(time.Now(), "0.02", 1, 1)})
		Expect(err).NotTo(HaveOccurred())
		l, err := ledger.AgentLimit(ctx, q, e.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeZero())
		var spent string
		Expect(db.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&spent)).To(Succeed())
		Expect(ledger.FormatUSD(spent)).To(Equal("0.075"), "limit $0.05 + the crossing request's $0.005 + the one in flight")
	})

	It("counts entries of unknown orgs and agents as unattributed", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
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

	It("sums cost and tokens per run and per task", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		t := e.task(ctx)
		t0 := time.Now().Add(-time.Hour)
		r1 := e.startRun(ctx, t, t0)
		e.finishRun(ctx, r1.ID, t0.Add(10*time.Minute))
		r2 := e.startRun(ctx, t, t0.Add(20*time.Minute))
		other := e.task(ctx)

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

		tasks, err := q.SumTaskCosts(ctx, store.SumTaskCostsParams{OrgID: e.org, TaskIds: []pgtype.UUID{t.ID, other.ID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(tasks).To(HaveLen(1), "a task without spend has no row")
		Expect(tasks[0].TaskID).To(Equal(t.ID))
		Expect(tasks[0].Requests).To(Equal(int64(3)))
		Expect(ledger.FormatUSD(tasks[0].CostUsd)).To(Equal("1.5003"))
		Expect(tasks[0].Priced).To(BeTrue())
	})
})

var _ = Describe("AgentLimit", func() {
	BeforeEach(needDocker)

	It("nets the template's limit against the current window's spend", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		now := time.Now()
		l, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(money.MustParse("10")))

		_, err = ing.Ingest(ctx, []ledger.Entry{
			e.entry(now.Add(-ledger.Window), "5", 1, 1), // the previous window
			e.entry(now, "2.25", 1, 1),
		})
		Expect(err).NotTo(HaveOccurred())
		net, err := ledger.AgentLimit(ctx, q, e.agent, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(net.Amount).To(Equal(money.MustParse("7.75")))
		Expect(net.Generation).NotTo(Equal(l.Generation))
	})

	It("has no limit for an agent without an active version", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		a := e.agent
		a.ActiveVersionID = pgtype.UUID{}
		l, err := ledger.AgentLimit(ctx, q, a, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(BeNil())
	})
})

var _ = Describe("InsertSpendEntry", func() {
	BeforeEach(needDocker)

	It("records an entry that holds nothing without the settlement fields set", func(ctx SpecContext) {
		e := newEnv(ctx, uniq("org"), "", "10.00")
		_, err := q.InsertSpendEntry(ctx, store.InsertSpendEntryParams{
			Principal: "agent",
			OrgID:     e.org, AgentID: e.agent.ID, RequestID: uniq("req"), KeyID: "k-test", Model: "m",
			CostUsd: "0.5", Priced: true, OccurredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		Expect(err).NotTo(HaveOccurred(), "reserve_usd and response_id default to nothing held")
		var reserve string
		var provisional bool
		Expect(db.QueryRow(ctx, `SELECT reserve_usd::text, provisional FROM spend_entries WHERE org_id = $1`, e.org).Scan(&reserve, &provisional)).To(Succeed())
		Expect(ledger.FormatUSD(reserve)).To(Equal("0.00"))
		Expect(provisional).To(BeFalse())
	})
})

var _ = Describe("An Ingester without prices", func() {
	It("is refused at startup instead of recording every request unpriced", func(ctx SpecContext) {
		in := &ledger.Ingester{}
		Expect(in.Validate()).To(MatchError(ContainSubstring("Prices is required")))
		Expect(agentgateway.Serve(ctx, "127.0.0.1:0", in, nil)).To(MatchError(ContainSubstring("Prices is required")))
		Expect((&ledger.Ingester{Prices: prices.NewResolver(priceList)}).Validate()).To(Succeed())
		Expect((&ledger.Settler{}).Validate()).To(MatchError(ContainSubstring("Settler.Prices is required")))
		_, err := (&ledger.Settler{}).SettleDue(context.Background())
		Expect(err).To(MatchError(ContainSubstring("Settler.Prices is required")), "a pass refuses to run unwired")
		Expect((&ledger.Settler{Prices: prices.NewResolver(priceList)}).Validate()).To(Succeed())
	})
})
