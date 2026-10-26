// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// newDatabase creates an empty database next to the suite's and returns its DSN, so each spec
// starts without DBOS tables.
func newDatabase(ctx context.Context) string {
	name := strings.ReplaceAll(pgtest.Uniq("dur"), "-", "_")
	_, err := pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name))
	Expect(err).NotTo(HaveOccurred())
	cfg := pool.Config().ConnConfig.Copy()
	cfg.Database = name
	return connString(cfg.Host, cfg.Port, cfg.User, cfg.Password, name)
}

func connString(host string, port uint16, user, password, db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", user, password, host, port, db)
}

var _ = Describe("Runtime", func() {
	It("verifies the schema on launch and does not create it", func(ctx SpecContext) {
		dsn := newDatabase(ctx)

		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
		Expect(rt.Launch(ctx)).NotTo(Succeed(), "launch must not migrate on its own")
		Expect(rt.Shutdown(time.Second)).To(Succeed())

		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed(), "migrating twice is harmless")

		rt = durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
		Expect(rt.Launch(ctx)).To(Succeed())
		Expect(rt.Shutdown(5 * time.Second)).To(Succeed())
		Expect(rt.Shutdown(time.Second)).To(Succeed(), "a second shutdown is a no-op")
	})

	It("runs under the component role's DML-only grants", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		cfg, err := pgx.ParseConfig(dsn)
		Expect(err).NotTo(HaveOccurred())

		// Roles are cluster-wide: use per-spec names where possible and drop them afterwards. The
		// grants target the fixed durable.AppRole, so that one is created and dropped here too.
		login := strings.ReplaceAll(pgtest.Uniq("api"), "-", "_")
		_, err = pool.Exec(ctx, fmt.Sprintf("CREATE ROLE %s NOLOGIN", durable.AppRole))
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'koinon_test_not-a-real-token' IN ROLE %s", login, durable.AppRole))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) {
			admin, err := pgx.Connect(ctx, dsn)
			if err == nil {
				_, _ = admin.Exec(ctx, fmt.Sprintf("DROP OWNED BY %s", durable.AppRole))
				_ = admin.Close(ctx)
			}
			_, _ = pool.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", login))
			_, _ = pool.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", durable.AppRole))
		})

		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		admin, err := pgx.Connect(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = admin.Close(ctx) })
		var truncate, insert bool
		Expect(admin.QueryRow(ctx, `SELECT has_table_privilege($1, 'dbos.workflow_status', 'TRUNCATE'), has_table_privilege($1, 'dbos.workflow_status', 'INSERT')`, durable.AppRole).Scan(&truncate, &insert)).To(Succeed())
		Expect(truncate).To(BeFalse(), "the component role gets DML only")
		Expect(insert).To(BeTrue())

		appDSN := connString(cfg.Host, cfg.Port, login, "koinon_test_not-a-real-token", cfg.Database)
		hello := func(_ dbos.Context, name string) (string, error) { return "hello " + name, nil }
		rt := durable.New(durable.Config{DatabaseURL: appDSN, ApplicationVersion: "v1"})
		Expect(rt.Register(func(c dbos.Context) error {
			dbos.RegisterWorkflow(c, hello, dbos.WithWorkflowName("test.hello"))
			return nil
		})).To(Succeed())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		h, err := dbos.RunWorkflow(rt.Context(), hello, "koinon")
		Expect(err).NotTo(HaveOccurred())
		res, err := h.GetResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal("hello koinon"))
	}, SpecTimeout(2*time.Minute))

	It("keeps its tables in the dbos schema", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1"})
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		// The DBOS tables are not in public, where Koinon's own tables are.
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'workflow_status'`).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("runs a scheduled workflow on its schedule", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		var ticks atomic.Int32
		tick := func(_ dbos.Context, in dbos.ScheduledWorkflowInput) (any, error) {
			ticks.Add(1)
			return nil, nil
		}
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1", SchedulerPollingInterval: time.Second})
		Expect(rt.Register(func(c dbos.Context) error {
			dbos.RegisterWorkflow(c, tick, dbos.WithWorkflowName("test.tick"))
			return nil
		})).To(Succeed())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		Expect(rt.Register(func(dbos.Context) error { return nil })).NotTo(Succeed(), "no registration after launch")

		// Cron with seconds: every second.
		Expect(rt.ApplySchedules(dbos.ScheduleSpec{
			ScheduleName: "test-every-second",
			Workflow:     tick,
			Schedule:     "* * * * * *",
		})).To(Succeed())

		Eventually(ticks.Load, 30*time.Second, 200*time.Millisecond).Should(BeNumerically(">=", 3))
	}, SpecTimeout(2*time.Minute))

	It("finishes a durable sleep once, across a restart of the runtime", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		const sleep = 6 * time.Second
		var started, finished atomic.Int32
		entered := make(chan struct{}, 4)
		nap := func(c dbos.Context, label string) (string, error) {
			started.Add(1)
			entered <- struct{}{}
			if _, err := dbos.Sleep(c, sleep); err != nil {
				return "", err
			}
			finished.Add(1)
			return "woke " + label, nil
		}
		newRuntime := func() *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "v1", ExecutorID: "koinon-api-test"})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, nap, dbos.WithWorkflowName("test.nap"))
				return nil
			})).To(Succeed())
			return rt
		}

		first := newRuntime()
		Expect(first.Launch(ctx)).To(Succeed())
		begin := time.Now()
		h, err := dbos.RunWorkflow(first.Context(), nap, "reminder", dbos.WithWorkflowID("nap-1"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())
		Expect(h.GetWorkflowID()).To(Equal("nap-1"))

		// Stop the runtime well into the sleep. The workflow stays pending in Postgres.
		time.Sleep(4 * time.Second)
		_ = first.Shutdown(2 * time.Second)
		Expect(finished.Load()).To(BeZero())

		second := newRuntime()
		Expect(second.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = second.Shutdown(5 * time.Second) })

		h2, err := dbos.RetrieveWorkflow[string](second.Context(), "nap-1")
		Expect(err).NotTo(HaveOccurred())
		res, err := h2.GetResult()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal("woke reminder"))
		// The timer was stored, not restarted: it ended no earlier than the original deadline.
		Expect(time.Since(begin)).To(BeNumerically(">=", sleep-500*time.Millisecond))
		// A restarted timer would end near 4s + 6s.
		Expect(time.Since(begin)).To(BeNumerically("<", sleep+2*time.Second))
		Expect(finished.Load()).To(Equal(int32(1)), "completes exactly once")
		Expect(started.Load()).To(Equal(int32(2)), "the workflow body re-ran once, on recovery")
	}, SpecTimeout(2*time.Minute))
})
