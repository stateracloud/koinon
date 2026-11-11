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

var _ = Describe("Overlay", func() {
	It("lets each override value win and keeps the template's for the rest", func() {
		Expect(sandboxres.Overlay(res("250m", "1", "", "1Gi"), res("", "3", "", ""))).To(Equal(res("250m", "3", "", "1Gi")))
		Expect(sandboxres.Overlay(res("250m", "1", "", "1Gi"), templates.Resources{})).To(Equal(res("250m", "1", "", "1Gi")))
	})
	It("makes the override beat the template, and the template beat the default, through Effective", func() {
		p := sandboxres.DefaultPolicy()
		got, _ := p.Effective(sandboxres.Overlay(res("", "1", "", ""), res("", "3", "", "")))
		Expect(got.CPU).To(Equal(templates.Quantities{Request: "250m", Limit: "3"}))
		got, _ = p.Effective(sandboxres.Overlay(res("", "1", "", ""), templates.Resources{}))
		Expect(got.CPU).To(Equal(templates.Quantities{Request: "250m", Limit: "1"}))
	})
})

var _ = Describe("Policy.CheckOverride", func() {
	p := sandboxres.DefaultPolicy()
	DescribeTable("refuses what the platform would not run, naming the bounds",
		func(tpl, override templates.Resources, want string) {
			err := p.CheckOverride(tpl, override)
			if want == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			var be *sandboxres.BoundsError
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(BeAssignableToTypeOf(be))
		},
		Entry("nothing set clears the override", res("", "", "", ""), res("", "", "", ""), ""),
		Entry("inside the bounds", res("", "", "", ""), res("500m", "3", "2Gi", "4Gi"), ""),
		Entry("the memory limit above the maximum", res("", "", "", ""), res("", "", "", "16Gi"),
			"memory.limit 16Gi is above the maximum: the platform allows 512Mi to 8Gi"),
		Entry("the memory limit below the minimum", res("", "", "", ""), res("", "", "", "128Mi"),
			"memory.limit 128Mi is below the minimum: the platform allows 512Mi to 8Gi"),
		Entry("the cpu limit above the maximum (no minimum)", res("", "", "", ""), res("", "8", "", ""),
			"cpu.limit 8 is above the maximum: the platform allows at most 4"),
		Entry("the cpu request below the minimum", res("", "", "", ""), res("50m", "", "", ""),
			"cpu.request 50m is below the minimum: the platform allows 100m to 2"),
		Entry("a malformed quantity", res("", "", "", ""), res("", "lots", "", ""), `cpu.limit "lots"`),
		Entry("a request above the limit in the override", res("", "", "", ""), res("2", "1", "", ""), "cpu.request 2 is greater than cpu.limit 1"),
		Entry("a request above the limit the template sets", res("", "1", "", ""), res("2", "", "", ""), "cpu.request 2 is greater than cpu.limit 1"),
		Entry("a request above the default limit", res("", "", "", ""), res("", "", "3Gi", ""), "memory.request 3Gi is greater than memory.limit 2Gi"),
	)

	// An unset request is filled from the template or the default and lowered to the limit, as
	// Effective does; only a request the user set can be refused.
	DescribeTable("agrees with Effective on requests the user did not set",
		func(pol sandboxres.Policy, tpl, override templates.Resources, want string, wantMem, wantCPU string) {
			err := pol.CheckOverride(tpl, override)
			if want != "" {
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(err).To(MatchError(SatisfyAny(ContainSubstring("512Mi to 8Gi"), ContainSubstring("at most 4"))))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			eff, _ := pol.Effective(sandboxres.Overlay(tpl, override))
			Expect(eff.Memory.Request).To(Equal(wantMem))
			Expect(eff.CPU.Request).To(Equal(wantCPU))
		},
		Entry("memory limit only, below the default request (request equals limit)",
			p, res("", "", "", ""), res("", "", "", "512Mi"), "", "512Mi", "250m"),
		Entry("memory limit only, below the default request, equal-to-limit off",
			withoutEqual(p), res("", "", "", ""), res("", "", "", "512Mi"), "", "512Mi", "250m"),
		Entry("memory limit only, template request above it",
			p, res("", "", "4Gi", ""), res("", "", "", "512Mi"), "", "512Mi", "250m"),
		Entry("cpu limit only, below the default request",
			p, res("", "", "", ""), res("", "100m", "", ""), "", "2Gi", "100m"),
		Entry("explicit memory request above the explicit limit",
			p, res("", "", "", ""), res("", "", "4Gi", "2Gi"), "memory.request 4Gi is greater than memory.limit 2Gi", "", ""),
		Entry("explicit memory request above the explicit limit, equal-to-limit off",
			withoutEqual(p), res("", "", "", ""), res("", "", "4Gi", "2Gi"), "memory.request 4Gi is greater than memory.limit 2Gi", "", ""),
		Entry("explicit cpu request above the explicit limit",
			p, res("", "", "", ""), res("2", "1", "", ""), "cpu.request 2 is greater than cpu.limit 1", "", ""),
	)
})

func withoutEqual(p sandboxres.Policy) sandboxres.Policy {
	p.MemoryRequestEqualsLimit = false
	return p
}

var _ = Describe("FromConfig", func() {
	It("lays the file over the defaults and validates", func() {
		p, err := sandboxres.FromConfig(func(key string, out any) error {
			Expect(key).To(Equal("sandboxResources"))
			fc := out.(*sandboxres.FileConfig)
			fc.Bounds.Memory.Limit.Max = "16Gi"
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Bounds.MemoryLimit.Max).To(Equal("16Gi"))
		Expect(p.Bounds.MemoryLimit.Min).To(Equal("512Mi"))
	})
})
