// SPDX-License-Identifier: Apache-2.0

package schedule_test

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/triggers"
	"github.com/stateracloud/koinon/internal/triggers/schedule"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

// replica is one koinon-api: a DBOS runtime and a scheduler on a clock the test sets.
type replica struct {
	rt    *durable.Runtime
	sched *schedule.Scheduler
	now   time.Time
	mu    sync.Mutex
	wakes map[pgtype.UUID][]time.Time
}

func startReplica(ctx context.Context, start time.Time) *replica {
	GinkgoHelper()
	r := startUnbound(ctx, start)
	r.sched.Bind(services.New(pool, services.Options{Prices: pricing.Embedded()}))
	return r
}

// startUnbound is startReplica without the task creator, so enqueued fires wait.
func startUnbound(ctx context.Context, start time.Time) *replica {
	GinkgoHelper()
	r := &replica{now: start, wakes: map[pgtype.UUID][]time.Time{}}
	r.rt = durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
	work, err := durable.NewOrgWork(r.rt, durable.OrgQueueConfig{
		Defaults:        durable.OrgLimits{Concurrency: 50, RatePerWindow: 100000},
		PollingInterval: 100 * time.Millisecond,
	})
	Expect(err).NotTo(HaveOccurred())
	r.sched = schedule.New(schedule.Config{
		CatchUp: time.Hour,
		Wake: func(_ context.Context, _ *store.Queries, _, agent pgtype.UUID) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.wakes[agent] = append(r.wakes[agent], r.now)
			return true, nil
		},
	}, pool, work)
	Expect(r.rt.Register(r.sched.Registrar())).To(Succeed())
	Expect(r.rt.Launch(ctx)).To(Succeed())
	return r
}

func (r *replica) stop() { _ = r.rt.Shutdown(5 * time.Second) }

func (r *replica) tick(ctx context.Context, at time.Time) {
	GinkgoHelper()
	r.mu.Lock()
	r.now = at
	r.mu.Unlock()
	Expect(r.sched.Tick(ctx, at)).To(Succeed())
}

func (r *replica) wokenAt(agent pgtype.UUID) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.wakes[agent]...)
}

func newAgent(ctx context.Context, org pgtype.UUID, trs ...templates.Trigger) store.Agent {
	GinkgoHelper()
	q := store.New(pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: pgtest.Uniq("sched"), Harness: harness.OpenCode})
	Expect(err).NotTo(HaveOccurred())
	return activate(ctx, a, trs...)
}

// activate adds a version with trs and makes it the agent's active version.
func activate(ctx context.Context, a store.Agent, trs ...templates.Trigger) store.Agent {
	GinkgoHelper()
	q := store.New(pool)
	raw, err := agentconfig.Config{Instructions: "do the job", Triggers: trs}.Marshal()
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: a.OrgID, AgentID: a.ID, Config: raw})
	Expect(err).NotTo(HaveOccurred())
	a, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: a.OrgID, ID: a.ID, VersionID: v.ID})
	Expect(err).NotTo(HaveOccurred())
	return a
}

func cronTrigger(id, expr string) templates.Trigger {
	return templates.Trigger{
		ID: id, Type: templates.TriggerSchedule, Instruction: "run " + id,
		Filter: templates.TriggerFilter{Cron: expr, Timezone: "UTC"},
	}
}

func tasksOf(ctx context.Context, org pgtype.UUID) int {
	var n int
	Expect(pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE org_id = $1`, org).Scan(&n)).To(Succeed())
	return n
}

// hourAfterNow is a top-of-hour boundary after the real now, so every due time of a test lies after
// the agents' (real) activation time.
func hourAfterNow() time.Time { return time.Now().UTC().Truncate(time.Hour).Add(time.Hour) }

var _ = Describe("schedule triggers", func() {
	var org pgtype.UUID

	BeforeEach(func(ctx SpecContext) {
		org, _, _ = pgtest.NewOrg(ctx, pool)
	})

	It("fires once per due time across replicas and a restart mid-window, as a scheduled task with the instruction", func(ctx SpecContext) {
		agent := newAgent(ctx, org, cronTrigger("hourly", "0 * * * *"))
		due := hourAfterNow()
		start := due.Add(schedule.Jitter(agent.ID, due))

		a, b := startReplica(ctx, due.Add(-10*time.Minute)), startReplica(ctx, due.Add(-10*time.Minute))
		DeferCleanup(a.stop)
		DeferCleanup(b.stop)
		a.tick(ctx, due.Add(-10*time.Minute))
		b.tick(ctx, due.Add(-10*time.Minute))

		// The sandbox is woken the lead time before the run starts, once per replica at most.
		a.tick(ctx, start.Add(-schedule.DefaultLead).Add(time.Second))
		Expect(a.wokenAt(agent.ID)).To(HaveLen(1))
		a.tick(ctx, start.Add(-time.Minute))
		Expect(a.wokenAt(agent.ID)).To(HaveLen(1), "one wake per due time")

		// Replica A stops before the run starts: a restart in the middle of the window.
		a.tick(ctx, start.Add(-time.Second))
		a.stop()
		Consistently(func() int { return tasksOf(ctx, org) }, "500ms").Should(BeZero())

		// Both the surviving replica and the restarted one see the due time.
		a2 := startReplica(ctx, start.Add(2*time.Second))
		DeferCleanup(a2.stop)
		a2.tick(ctx, start.Add(2*time.Second))
		b.tick(ctx, start.Add(2*time.Second))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(Equal(1))

		// Later ticks of every replica, and one more restart, create nothing new for this due time.
		b.tick(ctx, start.Add(time.Minute))
		c := startReplica(ctx, start.Add(5*time.Minute))
		DeferCleanup(c.stop)
		c.tick(ctx, start.Add(5*time.Minute))
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(Equal(1))

		var origin, creator, text, source string
		Expect(pool.QueryRow(ctx, `SELECT r.origin::text, t.created_by_kind::text FROM tasks t JOIN runs r ON r.org_id = t.org_id AND r.task_id = t.id WHERE t.org_id = $1`, org).Scan(&origin, &creator)).To(Succeed())
		Expect(origin).To(Equal("scheduled"))
		Expect(creator).To(Equal("trigger"))
		Expect(pool.QueryRow(ctx, `SELECT payload->>'text' FROM task_events WHERE org_id = $1 AND kind = 'message'`, org).Scan(&text)).To(Succeed())
		Expect(text).To(Equal("run hourly"))
		Expect(pool.QueryRow(ctx, `SELECT source FROM dedupe_records WHERE org_id = $1`, org).Scan(&source)).To(Succeed())
		Expect(source).To(Equal(durable.SourceTrigger))

		// The next due time is a new task.
		next := due.Add(time.Hour)
		nextStart := next.Add(schedule.Jitter(agent.ID, next))
		b.tick(ctx, nextStart.Add(time.Second))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(2))
	}, SpecTimeout(2*time.Minute))

	It("creates a manual fire's task exactly as a scheduled fire's, apart from the marker", func(ctx SpecContext) {
		agent := newAgent(ctx, org, cronTrigger("hourly", "0 * * * *"))
		due := hourAfterNow()
		start := due.Add(schedule.Jitter(agent.ID, due))
		r := startReplica(ctx, due.Add(-time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(-time.Minute))
		r.tick(ctx, start.Add(time.Second))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))

		svc := services.New(pool, services.Options{Prices: pricing.Embedded()})
		manual, err := svc.RunScheduleTrigger(ctx, triggers.RunNow{Org: org, Agent: agent.ID, Trigger: "hourly", Actor: audit.System("koinon-test:spec")})
		Expect(err).NotTo(HaveOccurred())

		shape := func(task pgtype.UUID) map[string]string {
			out := map[string]string{}
			var creator, origin, trigger, text, fire, state string
			Expect(pool.QueryRow(ctx, `SELECT t.created_by_kind::text, coalesce(t.created_by_trigger_id::text, ''), r.origin::text, r.state::text
				FROM tasks t JOIN runs r ON r.org_id = t.org_id AND r.task_id = t.id WHERE t.id = $1`, task).Scan(&creator, &trigger, &origin, &state)).To(Succeed())
			Expect(pool.QueryRow(ctx, `SELECT payload->>'text', coalesce(payload->>'fire', '') FROM task_events WHERE task_id = $1 AND kind = 'message'`, task).Scan(&text, &fire)).To(Succeed())
			out["creator"], out["trigger"], out["origin"], out["text"], out["fire"], out["state"] = creator, trigger, origin, text, fire, state
			return out
		}
		var scheduled pgtype.UUID
		Expect(pool.QueryRow(ctx, `SELECT id FROM tasks WHERE org_id = $1 AND id <> $2`, org, manual.ID).Scan(&scheduled)).To(Succeed())
		sched, man := shape(scheduled), shape(manual.ID)
		Expect(sched["fire"]).To(Equal(triggers.FireScheduled))
		Expect(man["fire"]).To(Equal(triggers.FireManual))
		delete(sched, "fire")
		delete(man, "fire")
		Expect(man).To(Equal(sched), "creator trigger, the same trigger, scheduled origin, queued, the trigger's instruction")
		Expect(man["trigger"]).To(Equal(triggers.UUID(agent.ID, "hourly").String()))
	}, SpecTimeout(2*time.Minute))

	It("starts many agents due in the same minute spread over 30 seconds, not all at once", func(ctx SpecContext) {
		const n = 30
		agents := make([]store.Agent, n)
		for i := range agents {
			agents[i] = newAgent(ctx, org, cronTrigger("minute", "0 * * * *"))
		}
		due := hourAfterNow()
		r := startReplica(ctx, due.Add(-time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(-time.Minute))

		starts := map[int]bool{}
		for _, a := range agents {
			j := schedule.Jitter(a.ID, due)
			Expect(j).To(BeNumerically(">=", 0))
			Expect(j).To(BeNumerically("<", schedule.JitterWindow))
			starts[int(j/time.Second)] = true
		}
		Expect(len(starts)).To(BeNumerically(">=", 10), "30 agents land on at least 10 different seconds")

		fired := func(atSec int) int {
			c := 0
			for _, a := range agents {
				if schedule.Jitter(a.ID, due) <= time.Duration(atSec)*time.Second {
					c++
				}
			}
			return c
		}
		prev := 0
		steps := map[int]bool{}
		for sec := 0; sec <= 31; sec++ {
			r.tick(ctx, due.Add(time.Duration(sec)*time.Second))
			want := fired(sec)
			Eventually(func() int { return tasksOf(ctx, org) }, "20s", "50ms").Should(Equal(want), "after %ds", sec)
			if want > prev {
				steps[sec] = true
			}
			prev = want
		}
		Expect(prev).To(Equal(n))
		Expect(len(steps)).To(BeNumerically(">=", 10), "the starts came in at least 10 separate seconds, not in one burst")
		Expect(fired(0)).To(BeNumerically("<", n))
	}, SpecTimeout(3*time.Minute))

	It("fires at most once on start for the latest missed due time, never a backlog", func(ctx SpecContext) {
		newAgent(ctx, org, cronTrigger("often", "*/10 * * * *"))
		// Three hours later, with nothing running in between: six due times are inside the catch-up
		// window, one fires.
		now := hourAfterNow().Add(3*time.Hour + 25*time.Minute)
		r := startReplica(ctx, now)
		DeferCleanup(r.stop)
		r.tick(ctx, now)
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(Equal(1))
		var title string
		Expect(pool.QueryRow(ctx, `SELECT title FROM tasks WHERE org_id = $1`, org).Scan(&title)).To(Succeed())
		Expect(title).To(ContainSubstring(now.Add(-5*time.Minute).Format("15:04")), "the latest missed due time")
	}, SpecTimeout(time.Minute))

	It("does not fire what was due before the version took effect, and stops a changed or removed schedule", func(ctx SpecContext) {
		base := hourAfterNow().Add(time.Hour)
		r := startReplica(ctx, base)
		DeferCleanup(r.stop)

		// An agent activated now has nothing to catch up for earlier due times.
		agent := newAgent(ctx, org, cronTrigger("old", "*/5 * * * *"))
		r.tick(ctx, time.Now().UTC().Add(-time.Minute))
		Consistently(func() int { return tasksOf(ctx, org) }, "500ms").Should(BeZero())

		// A new version without the trigger replaces the schedule: its due times fire nothing.
		agent = activate(ctx, agent)
		r.tick(ctx, base.Add(2*time.Hour))
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(BeZero())

		// A changed schedule follows the new cron only.
		agent = activate(ctx, agent, cronTrigger("old", "0 * * * *"))
		r.tick(ctx, base.Add(2*time.Hour+30*time.Minute)) // after :00 of that hour
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
		r.tick(ctx, base.Add(2*time.Hour+55*time.Minute)) // :05, :10 ... are not due times any more
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(Equal(1))
		_ = agent
	}, SpecTimeout(time.Minute))

	It("fires a due time even when an unrelated version is activated before the fire runs", func(ctx SpecContext) {
		agent := newAgent(ctx, org, cronTrigger("hourly", "0 * * * *"))
		due := hourAfterNow()
		start := due.Add(schedule.Jitter(agent.ID, due))
		r := startUnbound(ctx, due.Add(-time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(-time.Minute))
		r.tick(ctx, start.Add(time.Second)) // enqueued; the fire waits for the creator

		// A new version: same trigger, other instructions and another trigger.
		raw, err := agentconfig.Config{Instructions: "changed", Triggers: []templates.Trigger{
			cronTrigger("hourly", "0 * * * *"), cronTrigger("other", "30 * * * *"),
		}}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		q := store.New(pool)
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: v.ID})
		Expect(err).NotTo(HaveOccurred())

		r.sched.Bind(services.New(pool, services.Options{Prices: pricing.Embedded()}))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
	}, SpecTimeout(time.Minute))

	It("gives the task the notify target of the trigger in effect at the fire, not at the enqueue", func(ctx SpecContext) {
		withNotify := func(n *templates.NotifyTarget) templates.Trigger {
			tr := cronTrigger("hourly", "0 * * * *")
			tr.Notify = n
			return tr
		}
		agent := newAgent(ctx, org, withNotify(&templates.NotifyTarget{Kind: templates.NotifyRole, Role: store.OrgRoleAdmin}))
		due := hourAfterNow()
		start := due.Add(schedule.Jitter(agent.ID, due))
		r := startUnbound(ctx, due.Add(-time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(-time.Minute))
		r.tick(ctx, start.Add(time.Second)) // enqueued; the fire waits for the creator

		// A new version with the same cron, timezone and instruction, and another notify target.
		raw, err := agentconfig.Config{Instructions: "do the job", Triggers: []templates.Trigger{
			withNotify(&templates.NotifyTarget{Kind: templates.NotifyRole, Role: store.OrgRoleOwner}),
		}}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		q := store.New(pool)
		v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: agent.ID, Config: raw})
		Expect(err).NotTo(HaveOccurred())
		_, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: agent.ID, VersionID: v.ID})
		Expect(err).NotTo(HaveOccurred())

		r.sched.Bind(services.New(pool, services.Options{Prices: pricing.Embedded()}))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
		var kind, role string
		Expect(pool.QueryRow(ctx, `SELECT notify_kind::text, notify_role::text FROM tasks WHERE org_id = $1`, org).Scan(&kind, &role)).To(Succeed())
		Expect(kind).To(Equal("role"))
		Expect(role).To(Equal("owner"))
	}, SpecTimeout(time.Minute))

	It("skips an agent whose config does not parse instead of failing the tick", func(ctx SpecContext) {
		good := newAgent(ctx, org, cronTrigger("hourly", "0 * * * *"))
		bad := newAgent(ctx, org, cronTrigger("hourly", "0 * * * *"))
		_, err := pool.Exec(ctx, `UPDATE agent_versions SET config = '{"triggers": 5}' WHERE org_id = $1 AND agent_id = $2`, org, bad.ID)
		Expect(err).NotTo(HaveOccurred())
		due := hourAfterNow()
		r := startReplica(ctx, due.Add(-time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(-time.Minute))
		r.tick(ctx, due.Add(schedule.JitterWindow+time.Second))
		Eventually(func() int { return tasksOf(ctx, org) }, "20s", "100ms").Should(Equal(1))
		_ = good
	}, SpecTimeout(time.Minute))

	It("fires nothing for an agent that is not active", func(ctx SpecContext) {
		a := newAgent(ctx, org, cronTrigger("daily", "0 * * * *"))
		_, err := store.New(pool).ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: a.ID})
		Expect(err).NotTo(HaveOccurred())
		due := hourAfterNow()
		r := startReplica(ctx, due.Add(time.Minute))
		DeferCleanup(r.stop)
		r.tick(ctx, due.Add(time.Minute))
		Consistently(func() int { return tasksOf(ctx, org) }, "1s", "100ms").Should(BeZero())
	})
})

var _ = Describe("Jitter and the trigger UUID", func() {
	It("are deterministic and keyed by the agent", func() {
		a, b := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
		due := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		Expect(schedule.Jitter(a, due)).To(Equal(schedule.Jitter(a, due)))
		Expect(schedule.Jitter(a, due)).NotTo(Equal(schedule.Jitter(b, due)))
		Expect(triggers.UUID(a, "x")).To(Equal(triggers.UUID(a, "x")))
		Expect(triggers.UUID(a, "x")).NotTo(Equal(triggers.UUID(b, "x")))
		Expect(triggers.UUID(a, "x")).NotTo(Equal(triggers.UUID(a, "y")))
	})
})
