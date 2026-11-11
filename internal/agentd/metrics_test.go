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
	"github.com/stateracloud/koinon/internal/runoutcome"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

var _ = Describe("metrics", func() {
	It("exports every class and reason of failed run event writes from the start, and counts one", func() {
		reg := prometheus.NewRegistry()
		m := agentd.NewMetrics(reg)
		const name = "koinon_agentd_run_event_write_failures_total"
		series := func() map[string]float64 {
			fams, err := reg.Gather()
			Expect(err).NotTo(HaveOccurred())
			out := map[string]float64{}
			for _, f := range fams {
				if f.GetName() != name {
					continue
				}
				for _, mt := range f.GetMetric() {
					l := map[string]string{}
					for _, p := range mt.GetLabel() {
						l[p.GetName()] = p.GetValue()
					}
					Expect(l).To(HaveLen(2), "class and reason only (ADR 0023)")
					out[l["class"]+"/"+l["reason"]] = mt.GetCounter().GetValue()
				}
			}
			return out
		}
		want := map[string]float64{}
		for _, c := range []runoutcome.EventClass{runoutcome.ClassBusiness, runoutcome.ClassDiagnostic} {
			for _, r := range runoutcome.WriteFailures {
				want[c.String()+"/"+string(r)] = 0
			}
		}
		Expect(want).To(HaveLen(10))
		Expect(series()).To(Equal(want))

		agentd.OutcomeMetrics(m).EventWriteFailed(runoutcome.ClassDiagnostic, runoutcome.FailureCanceled)
		want["diagnostic/canceled"] = 1
		Expect(series()).To(Equal(want))
	})

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
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))

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
		e.recordSandbox(ctx, store.SandboxStateDeleted, "")
		r := e.enqueue(ctx, e.task(ctx, "hello"))
		e.pool("agentd-a", withMetrics(m))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateFailed))

		Eventually(func() float64 { return finished(m, "failed") }).Should(Equal(1.0))
		Expect(finished(m, "succeeded")).To(Equal(0.0))
		Expect(testutil.ToFloat64(m.Claimed)).To(Equal(1.0))
	})

	It("counts a takeover by another replica, not as a claim", func(ctx SpecContext) {
		a, b := agentd.NewMetrics(prometheus.NewRegistry()), agentd.NewMetrics(prometheus.NewRegistry())
		e := newEnv(ctx, store.AgentFlavorOpencode)
		r := e.enqueue(ctx, e.task(ctx, markHang))
		killA := e.pool("agentd-a", withMetrics(a), func(c *agentd.Config) { c.KeepLeaseOnShutdown = true })
		Eventually(e.harness.hangs).WithTimeout(racescale.D(10 * time.Second)).Should(Receive())
		killA()
		e.pool("agentd-b", withMetrics(b))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(15 * time.Second)).Should(Equal(store.RunStateSucceeded))

		Expect(testutil.ToFloat64(a.Claimed)).To(Equal(1.0))
		Expect(testutil.ToFloat64(b.Takeovers)).To(Equal(1.0))
		Expect(testutil.ToFloat64(b.Claimed)).To(Equal(0.0))
		Eventually(func() float64 { return finished(b, "succeeded") }).Should(Equal(1.0))
	})
})
