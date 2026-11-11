// SPDX-License-Identifier: Apache-2.0

package sandboxres_test

import (
	"fmt"
	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

var (
	cpuPool = []string{"", "", "0", "10m", "50m", "100m", "250m", "500m", "1", "2", "3", "4", "8"}
	memPool = []string{"", "", "0", "128Mi", "256Mi", "512Mi", "1Gi", "2Gi", "4Gi", "8Gi", "16Gi"}
)

func pick(rng *rand.Rand, pool []string) string { return pool[rng.Intn(len(pool))] }

func randRange(rng *rand.Rand, pool []string) sandboxres.Range {
	r := sandboxres.Range{Min: pick(rng, pool), Max: pick(rng, pool)}
	if r.Min == "0" {
		r.Min = ""
	}
	if r.Max == "0" {
		r.Max = ""
	}
	return r
}

func randRes(rng *rand.Rand) templates.Resources {
	return res(pick(rng, cpuPool), pick(rng, cpuPool), pick(rng, memPool), pick(rng, memPool))
}

func randPolicy(rng *rand.Rand) sandboxres.Policy {
	d := randRes(rng)
	return sandboxres.Policy{
		Default: d,
		Bounds: sandboxres.Bounds{
			CPURequest: randRange(rng, cpuPool), CPULimit: randRange(rng, cpuPool),
			MemoryRequest: randRange(rng, memPool), MemoryLimit: randRange(rng, memPool),
		},
		MemoryRequestEqualsLimit: rng.Intn(2) == 0,
	}
}

func q(s string) resource.Quantity { return resource.MustParse(s) }

func within(v string, r sandboxres.Range) bool {
	if v == "" {
		return true
	}
	x := q(v)
	if r.Min != "" && x.Cmp(q(r.Min)) < 0 {
		return false
	}
	if r.Max != "" && x.Cmp(q(r.Max)) > 0 {
		return false
	}
	return true
}

// violations returns what is wrong with an override the policy accepted, or nil.
func violations(p sandboxres.Policy, tpl, ov templates.Resources) []string {
	var bad []string
	eff, _ := p.Effective(sandboxres.Overlay(tpl, ov))
	for _, f := range []struct {
		name     string
		got, set string
		r        sandboxres.Range
	}{
		{"cpu.request", eff.CPU.Request, ov.CPU.Request, p.Bounds.CPURequest},
		{"cpu.limit", eff.CPU.Limit, ov.CPU.Limit, p.Bounds.CPULimit},
		{"memory.request", eff.Memory.Request, ov.Memory.Request, p.Bounds.MemoryRequest},
		{"memory.limit", eff.Memory.Limit, ov.Memory.Limit, p.Bounds.MemoryLimit},
	} {
		if !within(f.got, f.r) {
			bad = append(bad, fmt.Sprintf("%s effective %q outside %s", f.name, f.got, f.r))
		}
		if f.set == "" || f.got == "" {
			continue
		}
		// With the memory rule on, an explicit request below the limit is raised to it, and
		// Effective reports that; every other explicit value must survive as written.
		if f.name == "memory.request" && p.MemoryRequestEqualsLimit {
			if x, s := q(f.got), q(f.set); x.Cmp(s) < 0 {
				bad = append(bad, fmt.Sprintf("%s lowered from %s to %s", f.name, f.set, f.got))
			}
			continue
		}
		if x, s := q(f.got), q(f.set); x.Cmp(s) != 0 {
			bad = append(bad, fmt.Sprintf("%s set %s but effective %s", f.name, f.set, f.got))
		}
	}
	for _, pair := range [][2]string{{eff.CPU.Request, eff.CPU.Limit}, {eff.Memory.Request, eff.Memory.Limit}} {
		if pair[0] != "" && pair[1] != "" {
			if r, l := q(pair[0]), q(pair[1]); r.Cmp(l) > 0 {
				bad = append(bad, fmt.Sprintf("request %s above limit %s", pair[0], pair[1]))
			}
		}
	}
	return bad
}

var _ = Describe("CheckOverride and Effective (property)", func() {
	It("an accepted override gives an effective size inside every bound, as written", func() {
		rng := rand.New(rand.NewSource(75))
		const cases = 6000
		checked, accepted, badCases := 0, 0, 0
		var first []string
		for i := 0; checked < cases && i < 100*cases; i++ {
			p := sandboxres.DefaultPolicy()
			if i%3 != 0 {
				p = randPolicy(rng)
				if p.Validate() != nil {
					continue
				}
			}
			tpl, ov := randRes(rng), randRes(rng)
			if tpl.Validate() != nil {
				tpl = templates.Resources{}
			}
			checked++
			if p.CheckOverride(tpl, ov) != nil {
				continue
			}
			accepted++
			if v := violations(p, tpl, ov); len(v) > 0 {
				badCases++
				if len(first) < 5 {
					first = append(first, fmt.Sprintf("policy %+v tpl %+v override %+v: %v", p, tpl, ov, v))
				}
			}
		}
		GinkgoWriter.Printf("checked %d, accepted %d, bad %d\n", checked, accepted, badCases)
		Expect(checked).To(BeNumerically(">=", 5000))
		Expect(accepted).To(BeNumerically(">", 500))
		Expect(badCases).To(BeZero(), "bad cases: %d of %d accepted; first: %v", badCases, accepted, first)
	})
})
