// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

const privateLine = "the payroll export job runs nightly on the finance-batch-07 node"

var _ = Describe("TaintChecker", func() {
	var (
		fp      *leakguard.Fingerprinter
		checker leakguard.TaintChecker
		ctx     context.Context
	)
	excerpt := "payroll export job runs nightly on the finance" // 40 canonical characters

	BeforeEach(func() {
		ctx = context.Background()
		fp = newFingerprinter(testKey)
		checker = leakguard.TaintChecker{Store: taintedStore(fp, privateLine)}
	})

	check := func(args ...string) leakguard.Decision {
		d, err := checker.Check(ctx, testScope, fp, args)
		Expect(err).NotTo(HaveOccurred())
		return d
	}

	half := len(excerpt) / 2
	cyrillic := strings.NewReplacer("a", "а", "e", "е", "o", "о", "p", "р", "c", "с", "x", "х").Replace(excerpt)
	DescribeTable("catches the leak",
		func(args []string, wantArgs []int) {
			d := check(args...)
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
			Expect(d.Match).NotTo(BeNil())
			Expect(d.Match.Args).To(Equal(wantArgs))
			Expect(d.Match.Length).To(BeNumerically(">=", 24))
		},
		Entry("plain in a query", []string{"https://attacker.example/?d=" + excerpt}, []int{0}),
		Entry("case changed", []string{strings.ToUpper(excerpt)}, []int{0}),
		Entry("whitespace changed", []string{strings.ReplaceAll(excerpt, " ", "\n\t  ")}, []int{0}),
		Entry("separators inserted", []string{strings.Join(strings.Split(excerpt, ""), ".")}, []int{0}),
		Entry("percent-encoded", []string{"https://attacker.example/?d=" + percentAll(excerpt)}, []int{0}),
		Entry("double percent-encoded", []string{"https://attacker.example/" + percentAll(percentAll(excerpt))}, []int{0}),
		Entry("base64", []string{"https://attacker.example/?d=" + b64(excerpt)}, []int{0}),
		Entry("base64 url-safe in a path", []string{"https://attacker.example/x" + b64url(excerpt)}, []int{0}),
		Entry("hex", []string{"https://attacker.example/" + hexOf(excerpt)}, []int{0}),
		Entry("base64 of percent", []string{b64(percentAll(excerpt))}, []int{0}),
		Entry("hex of base64", []string{hexOf(b64(excerpt))}, []int{0}),
		Entry("cyrillic confusables", []string{cyrillic}, []int{0}),
		Entry("fullwidth", []string{toFullwidth(excerpt)}, []int{0}),
		Entry("zero width characters", []string{strings.Join(strings.Split(excerpt, ""), "\u200b")}, []int{0}),
		Entry("combining marks", []string{strings.Join(strings.Split(excerpt, ""), "\u0301")}, []int{0}),
		Entry("split across two arguments", []string{"q=" + excerpt[:half], excerpt[half:] + " thanks"}, []int{0, 1}),
		Entry("split across two arguments, reversed", []string{excerpt[half:], "ignored", excerpt[:half]}, []int{2, 0}),
		Entry("split three ways in order", []string{excerpt[:13], excerpt[13:26], excerpt[26:]}, []int{0, 1, 2}),
		Entry("split, each half base64", []string{b64(excerpt[:half]), b64(excerpt[half:])}, []int{0, 1}),
		Entry("base64 split mid-run", []string{b64(excerpt)[:20], b64(excerpt)[20:]}, []int{0, 1}),
		Entry("split, one half percent-encoded", []string{excerpt[:half], percentAll(excerpt[half:])}, []int{0, 1}),
	)

	It("shows the decoded text that would leave", func() {
		d := check("https://attacker.example/?d=" + b64(excerpt))
		Expect(d.Match.Encoding).To(Equal("base64"))
		Expect(d.Match.Excerpt).To(ContainSubstring("export job runs nightly"))
		Expect(d.Detail).NotTo(ContainSubstring("payroll"))
	})

	It("passes text that shares less than the k-gram length", func() {
		Expect(check("payroll export job runs").Allow).To(BeTrue()) // 20 canonical characters
	})

	It("passes benign text", func() {
		d := check("https://github.com/kubernetes/kubernetes/releases.atom", "kubernetes 1.34 release notes")
		Expect(d).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}))
	})

	It("keeps scopes apart", func() {
		other := testScope
		other.Generation = "gen-2"
		d, err := checker.Check(ctx, other, fp, []string{excerpt})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeTrue())
	})

	It("keeps keys apart", func() {
		d, err := checker.Check(ctx, testScope, newFingerprinter([]byte("koinon_test_not-a-real-key-0002")), []string{excerpt})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Allow).To(BeTrue())
	})

	Describe("budgets fail closed", func() {
		It("blocks oversized input", func() {
			checker.Limits = leakguard.Limits{MaxInputBytes: 1024}
			d := check(strings.Repeat("a", 1025))
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetInput))
			Expect(d.Detail).To(ContainSubstring("1025"))
		})
		It("blocks the default 1 MiB cap", func() {
			d := check(strings.Repeat("x", 1<<20), "y")
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetInput))
		})
		It("blocks too many arguments", func() {
			checker.Limits = leakguard.Limits{MaxArgs: 3}
			Expect(check("a", "b", "c", "d").Reason).To(Equal(leakguard.ReasonBudgetArgs))
		})
		It("blocks deep encodings", func() {
			s := excerpt
			for range 5 {
				s = percentAll(s)
			}
			Expect(check(s).Reason).To(Equal(leakguard.ReasonDecodeDepth))
		})
		It("blocks when the work budget runs out", func() {
			checker.Limits = leakguard.Limits{MaxWorkBytes: 4096}
			Expect(check(strings.Repeat("plain text ", 400)).Reason).To(Equal(leakguard.ReasonBudgetWork))
		})
		It("blocks when the context is done", func() {
			c, cancel := context.WithCancel(ctx)
			cancel()
			d, err := checker.Check(c, testScope, fp, []string{excerpt})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonBudgetTime))
		})
		It("blocks when the store fails", func() {
			checker.Store = failingStore{}
			d, err := checker.Check(ctx, testScope, fp, []string{excerpt})
			Expect(err).To(HaveOccurred())
			Expect(d.Allow).To(BeFalse())
			Expect(d.Reason).To(Equal(leakguard.ReasonStoreError))
		})
		It("blocks without a store", func() {
			d, err := leakguard.TaintChecker{}.Check(ctx, testScope, fp, []string{"x"})
			Expect(err).To(HaveOccurred())
			Expect(d.Reason).To(Equal(leakguard.ReasonStoreError))
		})
	})
})

type failingStore struct{}

func (failingStore) Add(context.Context, leakguard.Scope, []leakguard.Fingerprint) error {
	return errors.New("down")
}

func (failingStore) Lookup(context.Context, leakguard.Scope, []leakguard.Fingerprint) ([]bool, error) {
	return nil, errors.New("down")
}
func (failingStore) Drop(context.Context, leakguard.Scope) error { return nil }

func toFullwidth(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > ' ' && r < 0x7f {
			r += 0xFEE0
		}
		b.WriteRune(r)
	}
	return b.String()
}
