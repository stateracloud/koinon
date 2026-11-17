// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDashboardmetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dashboardmetrics")
}

func write(root, rel, content string) string {
	p := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
	Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
	return p
}

func dashboard(expr string) string {
	return `{"panels":[{"type":"row","panels":[]},{"type":"timeseries","targets":[{"expr":` + `"` + expr + `"` + `}]}]}`
}

var _ = Describe("dashboardmetrics", func() {
	var src string
	BeforeEach(func() {
		src = GinkgoT().TempDir()
		write(src, "internal/x/metrics.go", `package x
var _ = Opts{Name: "koinon_api_thing_total"}
var _ = Opts{Name: "koinon_api_lat_seconds"}`)
		write(src, "internal/x/metrics_test.go", `var _ = Opts{Name: "koinon_only_in_test_total"}`)
	})
	check := func(expr string) (int, string) {
		d := write(GinkgoT().TempDir(), "d.json", dashboard(expr))
		var o, e bytes.Buffer
		code, err := run([]string{src, d}, &o, &e)
		Expect(err).NotTo(HaveOccurred())
		return code, e.String()
	}

	It("accepts a registered metric with fixed labels", func() {
		code, _ := check(`sum by (decision) (increase(koinon_api_thing_total{decision=\"refuse\"}[5m]))`)
		Expect(code).To(Equal(0))
	})
	It("accepts a histogram series of a registered base name", func() {
		code, _ := check(`histogram_quantile(0.9, sum by (le) (rate(koinon_api_lat_seconds_bucket[5m])))`)
		Expect(code).To(Equal(0))
	})
	It("fails on a metric the code does not register", func() {
		code, msg := check(`sum(increase(koinon_api_renamed_total[5m]))`)
		Expect(code).To(Equal(1))
		Expect(msg).To(ContainSubstring("koinon_api_renamed_total"))
	})
	It("fails on a name that only a test registers", func() {
		code, _ := check(`sum(koinon_only_in_test_total)`)
		Expect(code).To(Equal(1))
	})
	It("fails on a grouping label that is an ID", func() {
		code, msg := check(`sum by (org_id) (koinon_api_thing_total)`)
		Expect(code).To(Equal(1))
		Expect(msg).To(ContainSubstring("org_id"))
	})
	It("fails on a matcher label that is an ID", func() {
		code, _ := check(`sum(koinon_api_thing_total{run=\"abc\"})`)
		Expect(code).To(Equal(1))
	})
	It("fails on a dashboard without a query", func() {
		d := write(GinkgoT().TempDir(), "d.json", `{"panels":[]}`)
		var o, e bytes.Buffer
		code, err := run([]string{src, d}, &o, &e)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(1))
	})
	It("reports a usage error", func() {
		_, err := run(nil, &bytes.Buffer{}, &bytes.Buffer{})
		Expect(err).To(HaveOccurred())
	})
})
