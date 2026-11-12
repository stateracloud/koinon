// SPDX-License-Identifier: Apache-2.0

// Package propseed runs property tests from a seeded generator, so a failure replays.
//
// A property runs Cases times; case i draws from rand.New(rand.NewSource(Seed()+i)). The default
// seed is fixed (DefaultSeed), so `make test` is deterministic. A failing case prints the exact
// seed that reproduces it, and KOINON_PROPERTY_SEED=<n> reruns that case first (with
// KOINON_PROPERTY_CASES=1). It uses only the standard library: testing/quick cannot generate
// event orderings with a printed per-case seed (issue 26).
package propseed

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
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

// ForEach runs body once per case with that case's generator. If body fails (a Gomega failure or
// a panic), ForEach reports the seed of the case before the failure propagates.
func ForEach(body func(rng *rand.Rand)) {
	GinkgoHelper()
	for i := 0; i < Cases(); i++ {
		Run(Seed()+int64(i), body)
	}
}

// Run runs body with the generator of one seed, printing the seed if body fails.
func Run(seed int64, body func(rng *rand.Rand)) {
	GinkgoHelper()
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("property failed with seed %d: rerun with %s=%d %s=1", seed, SeedVar, seed, CasesVar)
			AddReportEntry("property seed", msg)
			fmt.Fprintln(GinkgoWriter, msg)
			panic(r)
		}
	}()
	body(rand.New(rand.NewSource(seed))) //nolint:gosec // a reproducible test generator, not security
}
