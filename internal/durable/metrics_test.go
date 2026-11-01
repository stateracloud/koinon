// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"context"
	"errors"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/durable"
)

// gauge returns metric name's value for the version label, and whether the series exists.
func gauge(reg *prometheus.Registry, name, version string) (float64, bool) {
	fams, err := reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := m.GetLabel()
			if len(labels) == 1 && labels[0].GetName() == "version" && labels[0].GetValue() == version {
				return m.GetGauge().GetValue(), true
			}
			if version == "" && len(labels) == 0 {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

var _ = Describe("durable metrics (ADR 0085 decision 11, ADR 0023)", func() {
	It("folds every non-integer version into one label value, so the label set stays bounded", func() {
		Expect(durable.VersionLabel("3")).To(Equal("3"))
		Expect(durable.VersionLabel("0.1.0-rc.9")).To(Equal(durable.LegacyVersionLabel))
		Expect(durable.VersionLabel("local")).To(Equal(durable.LegacyVersionLabel))
		Expect(durable.VersionLabel("")).To(Equal(durable.LegacyVersionLabel))
	})

	It("reports pending workflows and executors by version, and a zero for the current version", func() {
		reg := prometheus.NewRegistry()
		reg.MustRegister(durable.NewCollectorFor(func(context.Context) (map[string]float64, map[string]float64, error) {
			return map[string]float64{"1": 4, "legacy": 2}, map[string]float64{"2": 3}, nil
		}, "2", time.Minute))

		v, ok := gauge(reg, "koinon_durable_workflows_pending", "1")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(4.0))
		v, ok = gauge(reg, "koinon_durable_workflows_pending", "legacy")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(2.0))
		v, ok = gauge(reg, "koinon_durable_workflows_pending", "2")
		Expect(ok).To(BeTrue(), "the current version is a series even with nothing pending")
		Expect(v).To(BeZero())
		v, ok = gauge(reg, "koinon_durable_executors", "2")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(3.0))
		v, _ = gauge(reg, "koinon_durable_metrics_read_failing", "")
		Expect(v).To(BeZero())
	})

	It("drops the counts and says so when the database cannot be read, then recovers", func() {
		fail := true
		reg := prometheus.NewRegistry()
		reg.MustRegister(durable.NewCollectorFor(func(context.Context) (map[string]float64, map[string]float64, error) {
			if fail {
				return nil, nil, errors.New("boom")
			}
			return map[string]float64{"1": 1}, map[string]float64{"1": 1}, nil
		}, "1", 0))

		_, ok := gauge(reg, "koinon_durable_workflows_pending", "1")
		Expect(ok).To(BeFalse(), "a failed read must not be reported as zero pending")
		v, _ := gauge(reg, "koinon_durable_metrics_read_failing", "")
		Expect(v).To(Equal(1.0))

		fail = false
		v, ok = gauge(reg, "koinon_durable_workflows_pending", "1")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1.0))
	})

	It("counts the pending workflows and live executors of a launched runtime from the database", func(ctx SpecContext) {
		dsn := newDatabase(ctx)
		Expect(durable.Migrate(ctx, dsn, nil)).To(Succeed())
		entered := make(chan struct{}, 2)
		work := func(c dbos.Context, label string) (string, error) {
			entered <- struct{}{}
			_, err := dbos.Sleep(c, time.Minute)
			return "", err
		}
		rt := durable.New(durable.Config{DatabaseURL: dsn, ApplicationVersion: "1", ExecutorID: "koinon-api-m1", CarryOverInterval: -1, RecoveryInterval: -1})
		Expect(rt.Register(func(c dbos.Context) error {
			dbos.RegisterWorkflow(c, work, dbos.WithWorkflowName("test.work"))
			return nil
		})).To(Succeed())
		Expect(rt.Launch(ctx)).To(Succeed())
		DeferCleanup(func() { _ = rt.Shutdown(5 * time.Second) })
		_, err := dbos.RunWorkflow(rt.Context(), work, "x", dbos.WithWorkflowID("m-1"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(entered).Should(Receive())

		reg := prometheus.NewRegistry()
		reg.MustRegister(rt.Collector())
		v, ok := gauge(reg, "koinon_durable_workflows_pending", "1")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1.0))
		v, ok = gauge(reg, "koinon_durable_executors", "1")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1.0))
	}, SpecTimeout(time.Minute))
})
