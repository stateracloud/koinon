// SPDX-License-Identifier: Apache-2.0

package sandboxres_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

func res(cpuReq, cpuLim, memReq, memLim string) templates.Resources {
	return templates.Resources{
		CPU:    templates.Quantities{Request: cpuReq, Limit: cpuLim},
		Memory: templates.Quantities{Request: memReq, Limit: memLim},
	}
}

var _ = Describe("Policy.Effective", func() {
	DescribeTable("resolves a template against the policy",
		func(mutate func(*sandboxres.Policy), tpl templates.Resources, want templates.Resources, wantClamps ...string) {
			p := sandboxres.DefaultPolicy()
			if mutate != nil {
				mutate(&p)
			}
			Expect(p.Validate()).To(Succeed())
			got, clamps := p.Effective(tpl)
			Expect(got).To(Equal(want))
			var fields []string
			for _, c := range clamps {
				fields = append(fields, c.Field+" "+c.Reason)
			}
			Expect(fields).To(ConsistOf(wantClamps))
		},
		Entry("unset takes the default; the memory request follows the limit, which is not a clamp",
			nil, res("", "", "", ""), res("250m", "2", "2Gi", "2Gi")),
		Entry("the template wins over the default",
			nil, res("250m", "1", "1Gi", "1536Mi"), res("250m", "1", "1536Mi", "1536Mi"), "memory.request raised to the limit"),
		Entry("memory request equal to limit is kept silently",
			nil, res("", "", "1Gi", "1Gi"), res("250m", "2", "1Gi", "1Gi")),
		Entry("memoryRequestEqualsLimit off keeps the request",
			func(p *sandboxres.Policy) { p.MemoryRequestEqualsLimit = false },
			res("", "", "1Gi", "2Gi"), res("250m", "2", "1Gi", "2Gi")),
		Entry("clamps low values up to the minimum",
			func(p *sandboxres.Policy) { p.MemoryRequestEqualsLimit = false },
			res("10m", "", "64Mi", "128Mi"), res("100m", "2", "512Mi", "512Mi"),
			"cpu.request below the minimum", "memory.request below the minimum", "memory.limit below the minimum"),
		Entry("clamps high values down to the maximum",
			func(p *sandboxres.Policy) { p.MemoryRequestEqualsLimit = false },
			res("8", "16", "16Gi", "32Gi"), res("2", "4", "8Gi", "8Gi"),
			"cpu.request above the maximum", "cpu.limit above the maximum", "memory.request above the maximum", "memory.limit above the maximum"),
		Entry("a request above the limit after clamping is lowered to the limit",
			func(p *sandboxres.Policy) { p.MemoryRequestEqualsLimit = false },
			res("2", "500m", "", ""), res("500m", "500m", "1Gi", "2Gi"), "cpu.request above the limit"),
		Entry("a request above the default limit is lowered to it",
			func(p *sandboxres.Policy) { p.Bounds.CPURequest.Max = "" },
			res("3", "", "", ""), res("2", "2", "2Gi", "2Gi"), "cpu.request above the limit"),
		Entry("an empty default leaves the value out",
			func(p *sandboxres.Policy) { p.Default = templates.Resources{}; p.MemoryRequestEqualsLimit = false },
			res("", "", "", ""), res("", "", "", "")),
		Entry("equivalent spellings are not reported as changed",
			func(p *sandboxres.Policy) { p.Default = res("", "", "", "2048Mi") },
			res("250m", "2000m", "", ""), res("250m", "2000m", "2048Mi", "2048Mi")),
	)

	It("reports the value before and after", func() {
		_, clamps := sandboxres.DefaultPolicy().Effective(res("", "", "", "64Gi"))
		Expect(clamps).To(HaveLen(1))
		Expect(clamps[0]).To(Equal(sandboxres.Clamp{Field: "memory.limit", From: "64Gi", To: "8Gi", Reason: "above the maximum"}))
		Expect(clamps[0].String()).To(Equal("memory.limit 64Gi -> 8Gi (above the maximum)"))
	})
})

var _ = Describe("Policy.Validate", func() {
	It("accepts the default policy", func() {
		Expect(sandboxres.DefaultPolicy().Validate()).To(Succeed())
	})

	DescribeTable("refuses",
		func(mutate func(*sandboxres.Policy), want string) {
			p := sandboxres.DefaultPolicy()
			mutate(&p)
			Expect(p.Validate()).To(MatchError(ContainSubstring(want)))
		},
		Entry("a bad bound", func(p *sandboxres.Policy) { p.Bounds.CPULimit.Max = "lots" }, `bounds.cpuLimit.max "lots"`),
		Entry("min above max", func(p *sandboxres.Policy) { p.Bounds.MemoryLimit = sandboxres.Range{Min: "9Gi", Max: "8Gi"} }, "bounds.memoryLimit: min 9Gi is greater than max 8Gi"),
		Entry("a bad default", func(p *sandboxres.Policy) { p.Default.Memory.Limit = "2 gigs" }, "default: memory.limit"),
		Entry("a default request above its limit", func(p *sandboxres.Policy) { p.Default.CPU.Request = "4" }, "default: cpu.request 4 is greater than cpu.limit 2"),
	)
})
