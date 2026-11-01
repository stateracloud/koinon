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
