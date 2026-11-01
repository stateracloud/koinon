// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
)

var _ = Describe("Executors (ADR 0085, decision 11)", func() {
	table := fmt.Sprintf("%s.%s", durable.Schema, durable.ExecutorsTable)

	// newRuntime registers one workflow that blocks while stuck is set and otherwise reports where it ran.
	type env struct {
		dsn   string
		stuck atomic.Bool
		runs  chan string
		in    chan struct{}
	}
	setup := func(ctx SpecContext) *env {
		e := &env{dsn: newDatabase(ctx), runs: make(chan string, 8), in: make(chan struct{}, 4)}
		e.stuck.Store(true)
		Expect(durable.Migrate(ctx, e.dsn, nil)).To(Succeed())
		return e
	}
	work := func(e *env) func(dbos.Context, string) (string, error) {
		return func(c dbos.Context, label string) (string, error) {
			if e.stuck.Load() {
				e.in <- struct{}{}
				_, err := dbos.Sleep(c, time.Minute)
				return "", err
			}
			e.runs <- dbos.GetExecutorID(c)
			return "done " + label, nil
		}
	}
	newRuntime := func(e *env, id string, cfg durable.Config) *durable.Runtime {
		cfg.DatabaseURL, cfg.ExecutorID, cfg.ApplicationVersion, cfg.CarryOverInterval = e.dsn, id, "1", -1
		rt := durable.New(cfg)
		Expect(rt.Register(func(c dbos.Context) error {
			dbos.RegisterWorkflow(c, work(e), dbos.WithWorkflowName("test.work"))
			return nil
		})).To(Succeed())
		return rt
	}
	It("records the executor's ID in the heartbeat table at Launch and keeps beating", func(ctx SpecContext) {
		e := setup(ctx)
		rt := newRuntime(e, "koinon-api-aaa", durable.Config{HeartbeatInterval: 200 * time.Millisecond, RecoveryInterval: -1})
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		Expect(rt.ExecutorID()).To(Equal("koinon-api-aaa"))

		last := func() time.Time {
			c := connect(ctx, e.dsn)
			defer func() { _ = c.Close(ctx) }()
			var t time.Time
			Expect(c.QueryRow(ctx, "SELECT last_heartbeat FROM "+table+" WHERE executor_id = 'koinon-api-aaa'").Scan(&t)).To(Succeed())
			return t
		}
		first := last()
		Eventually(last, 10*time.Second, 100*time.Millisecond).Should(BeTemporally(">", first))
	})

	It("recovers the pending workflows of an executor whose heartbeat went stale, on a live one", func(ctx SpecContext) {
		e := setup(ctx)
		dead := newRuntime(e, "koinon-api-dead", durable.Config{HeartbeatInterval: -1, RecoveryInterval: -1})
		Expect(dead.Launch(ctx)).To(Succeed())
		_, err := dbos.RunWorkflow(dead.Context(), work(e), "x", dbos.WithWorkflowID("wf-dead"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(e.in).Should(Receive())
		_ = dead.Shutdown(time.Second) // the pod is gone; its name is never used again
		e.stuck.Store(false)

		live := newRuntime(e, "koinon-api-live", durable.Config{
			HeartbeatInterval: 200 * time.Millisecond, ExecutorStaleAfter: 4 * time.Second, RecoveryInterval: 300 * time.Millisecond,
		})
		Expect(live.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = live.Shutdown(5 * time.Second) })

		// Not recovered while the dead executor's last beat is fresh (the window is well inside the
		// 4 s threshold); recovered once it is stale.
		Consistently(e.runs, time.Second).ShouldNot(Receive())
		Eventually(e.runs, 30*time.Second).Should(Receive(Equal("koinon-api-live")))
		h, err := dbos.RetrieveWorkflow[string](live.Context(), "wf-dead")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.GetResult()).To(Equal("done x"))

		c := connect(ctx, e.dsn)
		defer func() { _ = c.Close(ctx) }()
		var n int
		Expect(c.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE executor_id = 'koinon-api-dead'").Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "a recovered executor is forgotten")
		Expect(c.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE executor_id = 'koinon-api-live'").Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1), "the live executor stays")
	})

	It("does not recover an executor that keeps its heartbeat", func(ctx SpecContext) {
		e := setup(ctx)
		beating := newRuntime(e, "koinon-api-busy", durable.Config{HeartbeatInterval: 200 * time.Millisecond, RecoveryInterval: -1})
		Expect(beating.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = beating.Shutdown(5 * time.Second) })
		_, err := dbos.RunWorkflow(beating.Context(), work(e), "x", dbos.WithWorkflowID("wf-busy"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(e.in).Should(Receive())

		other := newRuntime(e, "koinon-api-other", durable.Config{HeartbeatInterval: 200 * time.Millisecond, ExecutorStaleAfter: 2 * time.Second, RecoveryInterval: -1})
		Expect(other.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = other.Shutdown(5 * time.Second) })

		Consistently(func() int {
			n, err := other.RecoverDeadExecutors(ctx)
			Expect(err).NotTo(HaveOccurred())
			return n
		}, 4*time.Second, 500*time.Millisecond).Should(BeZero())
		Consistently(e.in, time.Second).ShouldNot(Receive(), "the workflow was not started a second time")
	})

	It("recovers the pending workflows left under DBOS's default executor ID, which has no heartbeat row", func(ctx SpecContext) {
		e := setup(ctx)
		// An earlier build ran with no executor ID, so DBOS used "local" and nothing heartbeated for it.
		old := newRuntime(e, "local", durable.Config{HeartbeatInterval: -1, RecoveryInterval: -1})
		Expect(old.Launch(ctx)).To(Succeed())
		_, err := dbos.RunWorkflow(old.Context(), work(e), "x", dbos.WithWorkflowID("wf-local"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(e.in).Should(Receive())
		_ = old.Shutdown(time.Second)
		c := connect(ctx, e.dsn)
		defer func() { _ = c.Close(ctx) }()
		_, err = c.Exec(ctx, "DELETE FROM "+table+" WHERE executor_id = 'local'")
		Expect(err).NotTo(HaveOccurred())
		e.stuck.Store(false)

		live := newRuntime(e, "koinon-api-live", durable.Config{
			HeartbeatInterval: 200 * time.Millisecond, ExecutorStaleAfter: 2 * time.Second, RecoveryInterval: 300 * time.Millisecond,
		})
		Expect(live.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = live.Shutdown(5 * time.Second) })

		Eventually(e.runs, 30*time.Second).Should(Receive(Equal("koinon-api-live")))
		h, err := dbos.RetrieveWorkflow[string](live.Context(), "wf-local")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.GetResult()).To(Equal("done x"))
	})

	It("creates the heartbeat table itself when Launch migrates (local and test runs)", func(ctx SpecContext) {
		dsn := newDatabase(ctx) // not durable.Migrate'd: DBOS creates its own tables
		rt := durable.New(durable.Config{DatabaseURL: dsn, Migrate: true, ExecutorID: "koinon-api-solo", ApplicationVersion: "1", CarryOverInterval: -1, RecoveryInterval: -1})
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		c := connect(ctx, dsn)
		defer func() { _ = c.Close(ctx) }()
		var n int
		Expect(c.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE executor_id = 'koinon-api-solo'").Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})
})
