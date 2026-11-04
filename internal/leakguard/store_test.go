// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

var _ = Describe("Fingerprints and the store", func() {
	ctx := context.Background()

	It("documents k and w", func() {
		p := leakguard.DefaultParams()
		Expect(p.Threshold).To(Equal(24))
		Expect(p.Window).To(Equal(4))
		Expect(p.K()).To(Equal(21))
	})

	It("rejects bad parameters and short keys", func() {
		_, err := leakguard.NewFingerprinter([]byte("short"), leakguard.DefaultParams())
		Expect(err).To(HaveOccurred())
		_, err = leakguard.NewFingerprinter(testKey, leakguard.Params{Threshold: 8, Window: 4})
		Expect(err).To(HaveOccurred())
		_, err = leakguard.NewFingerprinter(testKey, leakguard.Params{Threshold: 24, Window: 20})
		Expect(err).To(HaveOccurred())
		_, err = leakguard.NewFingerprinter(testKey, leakguard.Params{Threshold: 32, Window: 8})
		Expect(err).NotTo(HaveOccurred())
	})

	It("is deterministic per key", func() {
		a := newFingerprinter(testKey).Winnow(privateLine)
		Expect(newFingerprinter(testKey).Winnow(privateLine)).To(Equal(a))
		Expect(newFingerprinter([]byte("koinon_test_not-a-real-key-0002")).Winnow(privateLine)).NotTo(ContainElement(a[0]))
	})

	It("winnows to about 2/(w+1) of the k-grams", func() {
		fp := newFingerprinter(testKey)
		text := randomText(rand.New(rand.NewPCG(1, 2)), 20000)
		n := len(fp.Winnow(text))
		Expect(n).To(BeNumerically("~", 2*(20000-20)/5, 600))
	})

	// The threshold guarantee: every shared canonical substring of 24 or more characters is
	// caught, nothing shorter than k = 21 ever is.
	It("catches every overlap of the threshold and none below k", func() {
		fp := newFingerprinter(testKey)
		rng := rand.New(rand.NewPCG(3, 4))
		private := randomText(rng, 5000)
		st := taintedStore(fp, private)
		checker := leakguard.TaintChecker{Store: st}
		for range 400 {
			n := 24 + rng.IntN(40)
			at := rng.IntN(len(private) - n)
			d, err := checker.Check(ctx, testScope, fp, []string{"00 " + private[at:at+n] + " 00"})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint), "overlap of %d at %d", n, at)
		}
		for range 400 {
			n := 1 + rng.IntN(20)
			at := rng.IntN(len(private) - n)
			d, err := checker.Check(ctx, testScope, fp, []string{"00 " + private[at:at+n] + " 00"})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeTrue(), "overlap of %d at %d", n, at)
		}
	})

	It("honours a configured threshold", func() {
		fp, err := leakguard.NewFingerprinter(testKey, leakguard.Params{Threshold: 40, Window: 4})
		Expect(err).NotTo(HaveOccurred())
		checker := leakguard.TaintChecker{Store: taintedStore(fp, privateLine)}
		d, err := checker.Check(ctx, testScope, fp, []string{"payroll export job runs nightly"}) // 27 canonical
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeTrue())
		d, err = checker.Check(ctx, testScope, fp, []string{privateLine})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeFalse())
	})

	Describe("MemoryStore", func() {
		fps := func(from, to int) []leakguard.Fingerprint {
			var out []leakguard.Fingerprint
			for i := from; i < to; i++ {
				out = append(out, leakguard.Fingerprint(i))
			}
			return out
		}

		It("evicts the oldest fingerprints beyond its size", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 100})
			Expect(st.Add(ctx, testScope, fps(0, 150))).To(Succeed())
			hits, err := st.Lookup(ctx, testScope, []leakguard.Fingerprint{0, 49, 50, 149})
			Expect(err).NotTo(HaveOccurred())
			Expect(hits.Found).To(Equal([]bool{false, false, true, true}))
			Expect(st.Stats()).To(Equal(leakguard.StoreStats{Scopes: 1, Fingerprints: 100, EvictedFingerprints: 50}))
		})

		It("makes a fingerprint new again when it is added again", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 3})
			Expect(st.Add(ctx, testScope, fps(0, 3))).To(Succeed())
			Expect(st.Add(ctx, testScope, []leakguard.Fingerprint{0})).To(Succeed())
			Expect(st.Add(ctx, testScope, []leakguard.Fingerprint{7})).To(Succeed())
			hits, err := st.Lookup(ctx, testScope, []leakguard.Fingerprint{0, 1, 2, 7})
			Expect(err).NotTo(HaveOccurred())
			Expect(hits.Found).To(Equal([]bool{true, false, true, true}))
		})

		It("takes the same fingerprint more than once in one Add and holds it once", func() {
			// The texts of one result winnow to repeated fingerprints (content and raw output). The
			// Postgres store of mcpguard has the same case (guard_test.go), where a repeat failed the insert.
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 100})
			Expect(st.Add(ctx, testScope, []leakguard.Fingerprint{5, 6, 5, 5, 7, 6})).To(Succeed())
			Expect(st.Stats().Fingerprints).To(Equal(3))
		})

		It("stays bounded under repeated adds", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 10})
			for i := range 10000 {
				Expect(st.Add(ctx, testScope, fps(i%20, i%20+5))).To(Succeed())
			}
			Expect(st.Stats().Fingerprints).To(Equal(10))
		})

		It("scopes taint to the home generation", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{})
			Expect(st.Add(ctx, testScope, fps(0, 5))).To(Succeed())
			next := testScope
			next.Generation = "gen-2"
			hits, _ := st.Lookup(ctx, next, fps(0, 5))
			Expect(hits.Found).To(Equal([]bool{false, false, false, false, false}))
			otherAgent := testScope
			otherAgent.Agent = "agent-other"
			hits, _ = st.Lookup(ctx, otherAgent, fps(0, 1))
			Expect(hits.Found).To(Equal([]bool{false}))
			// a later run of the same generation still sees it
			hits, _ = st.Lookup(ctx, testScope, fps(0, 1))
			Expect(hits.Found).To(Equal([]bool{true}))
			Expect(st.Drop(ctx, testScope)).To(Succeed())
			hits, _ = st.Lookup(ctx, testScope, fps(0, 1))
			Expect(hits.Found).To(Equal([]bool{false}))
		})

		It("evicts the least recently used scope", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxScopes: 2})
			a, b, c := testScope, testScope, testScope
			b.Generation, c.Generation = "gen-b", "gen-c"
			Expect(st.Add(ctx, a, fps(0, 2))).To(Succeed())
			Expect(st.Add(ctx, b, fps(0, 2))).To(Succeed())
			_, _ = st.Lookup(ctx, a, fps(0, 1)) // a is now more recent than b
			Expect(st.Add(ctx, c, fps(0, 2))).To(Succeed())
			hb, _ := st.Lookup(ctx, b, fps(0, 1))
			ha, _ := st.Lookup(ctx, a, fps(0, 1))
			Expect(hb.Found).To(Equal([]bool{false}))
			Expect(ha.Found).To(Equal([]bool{true}))
			Expect(st.Stats().EvictedScopes).To(Equal(uint64(1)))
		})

		It("is safe for concurrent feeds, checks and URL budgets", func() {
			fp := newFingerprinter(testKey)
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 500, MaxScopes: 4})
			checker := leakguard.TaintChecker{Store: st}
			p := leakguard.DefaultURLPolicy()
			run := leakguard.NewRunState()
			var wg sync.WaitGroup
			for g := range 8 {
				wg.Go(func() {
					defer GinkgoRecover()
					sc := testScope
					sc.Generation = fmt.Sprintf("gen-%d", g%5)
					for i := range 50 {
						Expect(st.Add(ctx, sc, fp.Winnow(fmt.Sprintf("%s %d %d", privateLine, g, i)))).To(Succeed())
						_, err := checker.Check(ctx, sc, fp, []string{privateLine})
						Expect(err).NotTo(HaveOccurred())
						p.CheckURL(run, fmt.Sprintf("https://h%d.example/p/%d", g, i))
						p.CheckSearch(run, "release notes")
						_ = st.Stats()
					}
				})
			}
			wg.Wait()
		})

		It("reports eviction on Lookup, per scope", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 10, MaxScopes: 2})
			a, b, c := testScope, testScope, testScope
			b.Generation, c.Generation = "gen-b", "gen-c"
			Expect(st.Add(ctx, a, fps(0, 5))).To(Succeed())
			Expect(st.Add(ctx, b, fps(0, 20))).To(Succeed())
			ha, _ := st.Lookup(ctx, a, fps(0, 1))
			hb, _ := st.Lookup(ctx, b, fps(0, 1))
			Expect(ha.Evicted).To(BeFalse())
			Expect(hb.Evicted).To(BeTrue())
			// b is now the least recent: c evicts it whole, and b stays reported as evicted
			_, _ = st.Lookup(ctx, a, fps(0, 1))
			Expect(st.Add(ctx, c, fps(0, 1))).To(Succeed())
			hb, _ = st.Lookup(ctx, b, fps(0, 1))
			Expect(hb.Found).To(Equal([]bool{false}))
			Expect(hb.Evicted).To(BeTrue())
			Expect(st.Add(ctx, b, fps(0, 1))).To(Succeed())
			hb, _ = st.Lookup(ctx, b, fps(0, 1))
			Expect(hb.Evicted).To(BeTrue())
		})

		It("fails an Add past the total cap instead of evicting another scope", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxFingerprints: 100, MaxTotalFingerprints: 150})
			a, b := testScope, testScope
			b.Generation = "gen-b"
			Expect(st.Add(ctx, a, fps(0, 100))).To(Succeed())
			Expect(st.Add(ctx, b, fps(0, 60))).To(MatchError(leakguard.ErrStoreFull))
			hb, _ := st.Lookup(ctx, b, fps(0, 1))
			Expect(hb.Evicted).To(BeTrue()) // the refused add is taint b does not hold
			Expect(st.Add(ctx, b, fps(0, 50))).To(Succeed())
			Expect(st.Add(ctx, a, fps(0, 100))).To(Succeed()) // already held: no growth
			Expect(st.Add(ctx, b, fps(50, 51))).To(MatchError(leakguard.ErrStoreFull))
			hb, _ = st.Lookup(ctx, b, fps(0, 1))
			Expect(hb.Evicted).To(BeTrue())
			ha, _ := st.Lookup(ctx, a, fps(0, 1))
			Expect(ha.Found).To(Equal([]bool{true}))
			Expect(ha.Evicted).To(BeFalse())
			Expect(st.Drop(ctx, a)).To(Succeed())
			Expect(st.Add(ctx, b, fps(50, 100))).To(Succeed())
			Expect(st.Stats().Fingerprints).To(Equal(100))
		})

		It("forgets the oldest evicted scopes past its memory, and counts them", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{MaxScopes: 1})
			scopes := make([]leakguard.Scope, 7)
			for i := range scopes {
				scopes[i] = testScope
				scopes[i].Generation = fmt.Sprintf("gen-%d", i)
				Expect(st.Add(ctx, scopes[i], fps(0, 1))).To(Succeed())
			}
			// six scopes were evicted, four are remembered
			Expect(st.Stats().ForgottenEvictions).To(Equal(uint64(2)))
			h0, _ := st.Lookup(ctx, scopes[0], fps(0, 1))
			h5, _ := st.Lookup(ctx, scopes[5], fps(0, 1))
			fresh := testScope
			fresh.Generation = "gen-new"
			hn, _ := st.Lookup(ctx, fresh, fps(0, 1))
			Expect(h0.Evicted).To(BeFalse())
			Expect(h5.Evicted).To(BeTrue())
			Expect(hn.Evicted).To(BeFalse())
		})

		It("refuses a scope without a generation", func() {
			st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{})
			Expect(st.Add(ctx, leakguard.Scope{Org: "o"}, fps(0, 1))).NotTo(Succeed())
			_, err := st.Lookup(ctx, leakguard.Scope{Org: "o"}, fps(0, 1))
			Expect(err).To(HaveOccurred())
		})
	})
})

// randomText returns n lowercase letters, so its canonical form is itself.
func randomText(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}
