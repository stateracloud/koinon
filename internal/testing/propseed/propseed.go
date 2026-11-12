// SPDX-License-Identifier: Apache-2.0

// Package propseed runs property tests from a seeded generator, so a failure replays.
//
// A property runs Cases times; case i draws from rand.New(rand.NewSource(Seed()+i)). The default
// seed is fixed (DefaultSeed), so `make test` is deterministic. A failing case prints the exact
// seed that reproduces it: KOINON_PROPERTY_SEED=<n> KOINON_PROPERTY_CASES=1 replays exactly that
// case (the base seed replaces the default, so case 0 draws from <n>). It uses only the standard library: testing/quick cannot generate
// event orderings with a printed per-case seed (issue 26).
package propseed

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"

	"github.com/onsi/ginkgo/v2"
)

const (
	// DefaultSeed is the seed of `make test`.
	DefaultSeed int64 = 20261112
	// DefaultCases is how many cases a property runs by default.
	DefaultCases = 25

	// SeedVar overrides the base seed; CasesVar overrides the case count.
	SeedVar  = "KOINON_PROPERTY_SEED"
	CasesVar = "KOINON_PROPERTY_CASES"
)

// Seed is the base seed of this run.
func Seed() int64 { return envInt(SeedVar, DefaultSeed) }

// Cases is the number of cases per property.
func Cases() int { return int(envInt(CasesVar, DefaultCases)) }

func envInt(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// ForEach runs body once per case with that case's generator. The seed of the case that was
// running when the spec failed is printed after the spec ends, whether the failure panicked the
// spec goroutine (Gomega's Expect) or was recorded from another goroutine. A failure recorded from
// another goroutine does not stop the later cases, so assert in the spec goroutine.
func ForEach(body func(rng *rand.Rand)) {
	ginkgo.GinkgoHelper()
	cur := new(int64)
	reportOnFailure(cur)
	for i := 0; i < Cases(); i++ {
		*cur = Seed() + int64(i)
		body(rand.New(rand.NewSource(*cur))) //nolint:gosec // a reproducible test generator, not security
	}
}

// Run runs body with the generator of one seed.
func Run(seed int64, body func(rng *rand.Rand)) {
	ginkgo.GinkgoHelper()
	reportOnFailure(&seed)
	body(rand.New(rand.NewSource(seed))) //nolint:gosec // a reproducible test generator, not security
}

func reportOnFailure(seed *int64) {
	ginkgo.DeferCleanup(func() {
		if ginkgo.CurrentSpecReport().Failed() {
			msg := fmt.Sprintf("property failed with seed %d: rerun with %s=%d %s=1", *seed, SeedVar, *seed, CasesVar)
			ginkgo.AddReportEntry("property seed", msg)
			ginkgo.GinkgoWriter.Println(msg)
		}
	})
}
