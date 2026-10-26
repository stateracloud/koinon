// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = Describe("metrics", func() {
	withMetrics := func(m *agentd.Metrics) func(*agentd.Config) {
		return func(c *agentd.Config) { c.Metrics = m }
	}
	finished := func(m *agentd.Metrics, outcome string) float64 {
		return testutil.ToFloat64(m.Finished.WithLabelValues(outcome))
	}

	It("counts a claimed run that succeeds and the wait for its sandbox", func(ctx SpecContext) {
		reg := prometheus.NewRegistry()
		m := agentd.NewMetrics(reg)
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", withMetrics(m))
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		Eventually(func() float64 { return finished(m, "succeeded") }).Should(Equal(1.0))
		Expect(testutil.ToFloat64(m.Claimed)).To(Equal(1.0))
		Expect(testutil.ToFloat64(m.Takeovers)).To(Equal(0.0))
		Expect(finished(m, "failed")).To(Equal(0.0))
		n, err := testutil.GatherAndCount(reg, "koinon_agentd_sandbox_wait_duration_seconds")
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1), "one histogram observed")
		Expect(testutil.CollectAndCount(m.SandboxWait)).To(Equal(1))
	})

	It("counts a failed run", func(ctx SpecContext) {
		m := agentd.NewMetrics(prometheus.NewRegistry())
		e := newEnv(ctx, store.AgentFlavorOpencode)
		e.recordSandbox(ctx, store.SandboxStateStopped, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", withMetrics(m), func(c *agentd.Config) { c.SandboxTimeout = time.Second })
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateFailed))

		Eventually(func() float64 { return finished(m, "failed") }).Should(Equal(1.0))
		Expect(finished(m, "succeeded")).To(Equal(0.0))
		Expect(testutil.ToFloat64(m.Claimed)).To(Equal(1.0))
	})

	It("counts a takeover by another replica, not as a claim", func(ctx SpecContext) {
		a, b := agentd.NewMetrics(prometheus.NewRegistry()), agentd.NewMetrics(prometheus.NewRegistry())
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markHang))
		killA := e.pool("agentd-a", withMetrics(a), func(c *agentd.Config) { c.KeepLeaseOnShutdown = true })
		Eventually(e.harness.hangs).WithTimeout(10 * time.Second).Should(Receive())
		killA()
		e.pool("agentd-b", withMetrics(b))
		Eventually(runState(ctx, r)).WithTimeout(15 * time.Second).Should(Equal(store.RunStateSucceeded))

		Expect(testutil.ToFloat64(a.Claimed)).To(Equal(1.0))
		Expect(testutil.ToFloat64(b.Takeovers)).To(Equal(1.0))
		Expect(testutil.ToFloat64(b.Claimed)).To(Equal(0.0))
		Eventually(func() float64 { return finished(b, "succeeded") }).Should(Equal(1.0))
	})
})
