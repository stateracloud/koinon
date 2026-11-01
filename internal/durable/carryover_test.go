// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/durable"
)

var _ = Describe("CarryOver (ADR 0085, decision 11)", func() {
	It("forks a workflow pending under another version from step 0, once, and cancels the original", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		entered := make(chan struct{}, 4)
		stuck := true // the old process never finishes its workflow
		runs := make(chan string, 8)
		work := func(c dbos.Context, label string) (string, error) {
			if stuck {
				entered <- struct{}{}
				_, err := dbos.Sleep(c, time.Minute)
				return "", err
			}
			runs <- label
			return "done " + label, nil
		}
		newRuntime := func(version string) *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: version, CarryOverInterval: -1})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
				return nil
			})).To(Succeed())
			return rt
		}

		old := newRuntime("old")
		Expect(old.Launch(ctx)).To(Succeed())
		_, err := dbos.RunWorkflow(old.Context(), work, "x", dbos.WithWorkflowID("wf-1"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())
		_ = old.Shutdown(time.Second)
		stuck = false

		// A runtime of another version does not recover it by itself; Launch carries it over.
		next := newRuntime("new")
		Expect(next.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = next.Shutdown(5 * time.Second) })

		Eventually(runs, 30*time.Second).Should(Receive(Equal("x")))
		fork, err := dbos.RetrieveWorkflow[string](next.Context(), "wf-1~vnew")
		Expect(err).NotTo(HaveOccurred())
		Expect(fork.GetResult()).To(Equal("done x"))

		orig, err := dbos.ListWorkflows(next.Context(), dbos.WithFilterWorkflowIDs("wf-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(orig).To(HaveLen(1))
		Expect(orig[0].Status).To(Equal(dbos.WorkflowStatusCancelled))
		Expect(orig[0].ApplicationVersion).To(Equal("old"))

		// A second sweep finds nothing, and nothing runs twice.
		n, err := next.CarryOver(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		Consistently(runs, 2*time.Second).ShouldNot(Receive())
	}, SpecTimeout(2*time.Minute))

	It("hands a retried fixed-ID request the fork's result, not the cancellation", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		entered := make(chan struct{}, 4)
		stuck := true
		work := func(c dbos.Context, label string) (string, error) {
			if stuck {
				entered <- struct{}{}
				_, err := dbos.Sleep(c, time.Minute)
				return "", err
			}
			return "done " + label, nil
		}
		newRuntime := func(version string) *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: version, CarryOverInterval: -1})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
				return nil
			})).To(Succeed())
			return rt
		}
		old := newRuntime("old")
		Expect(old.Launch(ctx)).To(Succeed())
		_, err := dbos.RunWorkflow(old.Context(), work, "x", dbos.WithWorkflowID("dm:org:msg-1"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())
		_ = old.Shutdown(time.Second)
		stuck = false

		next := newRuntime("new")
		Expect(next.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = next.Shutdown(5 * time.Second) })
		Expect(next.CarriedOverID("dm:org:msg-1")).To(Equal("dm:org:msg-1~vnew"))

		// The client retries under the original ID: DBOS returns the cancelled original.
		h, err := dbos.RunWorkflow(next.Context(), work, "x", dbos.WithWorkflowID("dm:org:msg-1"))
		Expect(err).NotTo(HaveOccurred())
		_, err = h.GetResult()
		Expect(err).To(HaveOccurred())

		out, err := durable.AwaitResult(next, h, "dm:org:msg-1", 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("done x"))
	}, SpecTimeout(2*time.Minute))

	It("follows a chain of carry-overs to the current version", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		entered := make(chan struct{}, 8)
		stuck := true
		work := func(c dbos.Context, label string) (string, error) {
			if stuck {
				entered <- struct{}{}
				_, err := dbos.Sleep(c, time.Minute)
				return "", err
			}
			return "done " + label, nil
		}
		newRuntime := func(version string) *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: version, CarryOverInterval: -1})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
				return nil
			})).To(Succeed())
			return rt
		}
		v1 := newRuntime("1")
		Expect(v1.Launch(ctx)).To(Succeed())
		_, err := dbos.RunWorkflow(v1.Context(), work, "x", dbos.WithWorkflowID("wf"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())
		_ = v1.Shutdown(time.Second)

		// Version 2 carries it over (wf~v2), starts it, and goes away with it still pending.
		v2 := newRuntime("2")
		Expect(v2.Launch(ctx)).To(Succeed())
		Eventually(entered, 30*time.Second).Should(Receive())
		_ = v2.Shutdown(time.Second)
		stuck = false

		v3 := newRuntime("3")
		Expect(v3.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = v3.Shutdown(5 * time.Second) })

		// The client retry under the original ID gets the cancelled original; two forks deep is
		// where the result is.
		h, err := dbos.RunWorkflow(v3.Context(), work, "x", dbos.WithWorkflowID("wf"))
		Expect(err).NotTo(HaveOccurred())
		out, err := durable.AwaitResult(v3, h, "wf", 30*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("done x"))
		_, err = dbos.RetrieveWorkflow[string](v3.Context(), "wf~v2~v3")
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(2*time.Minute))

	It("bounds the whole wait, not each of the two waits", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		entered := make(chan struct{}, 8)
		work := func(c dbos.Context, label string) (string, error) { // never finishes
			entered <- struct{}{}
			_, err := dbos.Sleep(c, time.Minute)
			return "", err
		}
		newRuntime := func(version string) *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: version, CarryOverInterval: -1})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
				return nil
			})).To(Succeed())
			return rt
		}
		v1 := newRuntime("1")
		Expect(v1.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = v1.Shutdown(5 * time.Second) })
		h, err := dbos.RunWorkflow(v1.Context(), work, "x", dbos.WithWorkflowID("wf"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())

		// After 2 s of a 6 s wait a version 2 process carries the workflow over; its fork never
		// finishes, so the wait must end at 6 s in all, not at 8 s.
		start := time.Now()
		launched := make(chan *durable.Runtime, 1)
		go func() {
			defer GinkgoRecover()
			time.Sleep(2 * time.Second)
			v2 := newRuntime("2")
			Expect(v2.Launch(ctx)).To(Succeed())
			launched <- v2
		}()
		_, err = durable.AwaitResult(v1, h, "wf", 6*time.Second)
		Expect(err).To(MatchError(dbos.ErrTimeout))
		Expect(time.Since(start)).To(BeNumerically("<", 7500*time.Millisecond))
		v2 := <-launched
		DeferCleanup(func() { _ = v2.Shutdown(5 * time.Second) })
	}, SpecTimeout(2*time.Minute))

	It("leaves a pending workflow of a newer version alone and carries over a lower integer one", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())

		entered := make(chan struct{}, 4)
		work := func(c dbos.Context, label string) (string, error) {
			entered <- struct{}{}
			_, err := dbos.Sleep(c, time.Minute)
			return "", err
		}
		newRuntime := func(version string) *durable.Runtime {
			rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: version, CarryOverInterval: -1})
			Expect(rt.Register(func(c dbos.Context) error {
				dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
				return nil
			})).To(Succeed())
			return rt
		}
		start := func(version, id string) {
			rt := newRuntime(version)
			Expect(rt.Launch(ctx)).To(Succeed())
			_, err := dbos.RunWorkflow(rt.Context(), work, "x", dbos.WithWorkflowID(id))
			Expect(err).NotTo(HaveOccurred())
			Eventually(entered).Should(Receive())
			_ = rt.Shutdown(time.Second)
		}
		start("1", "wf-lower")
		start("3", "wf-higher")

		mid := newRuntime("2")
		Expect(mid.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = mid.Shutdown(5 * time.Second) })

		status := func(id string) dbos.WorkflowStatusType {
			got, err := dbos.ListWorkflows(mid.Context(), dbos.WithFilterWorkflowIDs(id))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1))
			return got[0].Status
		}
		Expect(status("wf-lower")).To(Equal(dbos.WorkflowStatusCancelled))
		Expect(status("wf-higher")).NotTo(Equal(dbos.WorkflowStatusCancelled))
		forks, err := dbos.ListWorkflows(mid.Context(), dbos.WithFilterWorkflowIDs("wf-higher~v2"))
		Expect(err).NotTo(HaveOccurred())
		Expect(forks).To(BeEmpty())

		n, err := mid.CarryOver(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	}, SpecTimeout(2*time.Minute))

	It("uses durable.WorkflowVersion when no version is configured", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		noop := func(c dbos.Context, _ string) (string, error) { return "ok", nil }
		rt := durable.New(durable.Config{DatabaseURL: dsn, CarryOverInterval: -1})
		Expect(rt.Register(func(c dbos.Context) error {
			dbos.RegisterWorkflow(c, noop, dbos.WithWorkflowName("test.noop"))
			return nil
		})).To(Succeed())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })

		h, err := dbos.RunWorkflow(rt.Context(), noop, "", dbos.WithWorkflowID("v-1"))
		Expect(err).NotTo(HaveOccurred())
		_, err = h.GetResult()
		Expect(err).NotTo(HaveOccurred())
		got, err := dbos.ListWorkflows(rt.Context(), dbos.WithFilterWorkflowIDs("v-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ApplicationVersion).To(Equal(durable.WorkflowVersion))
	}, SpecTimeout(time.Minute))
})
