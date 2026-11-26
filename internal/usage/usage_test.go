// SPDX-License-Identifier: Apache-2.0

package usage_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/usage"
)

func TestUsage(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Usage Suite")
}

var _ = Describe("Resolve", func() {
	now := time.Date(2026, 11, 26, 15, 4, 5, 0, time.UTC)

	It("defaults to the month to date, through the end of today", func() {
		from, to, err := usage.Resolve(time.Time{}, time.Time{}, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(from).To(Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)))
		Expect(to).To(Equal(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)))
	})

	It("rounds both ends down to the UTC day", func() {
		from, to, err := usage.Resolve(time.Date(2026, 3, 1, 23, 0, 0, 0, time.UTC), time.Date(2026, 3, 5, 1, 0, 0, 0, time.UTC), now)
		Expect(err).NotTo(HaveOccurred())
		Expect(from).To(Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
		Expect(to).To(Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)))
	})

	It("refuses an empty, a reversed and a too long period", func() {
		d := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		for _, p := range [][2]time.Time{{d, d}, {d.AddDate(0, 0, 1), d}, {d, d.AddDate(0, 0, usage.MaxDays+1)}} {
			_, _, err := usage.Resolve(p[0], p[1], now)
			Expect(err).To(MatchError(usage.ErrRange))
		}
		_, _, err := usage.Resolve(d, d.AddDate(0, 0, usage.MaxDays), now)
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Sum", func() {
	It("adds costs exactly, with no float error and no cap", func() {
		var s usage.Sum
		for _, c := range []string{"0.1", "0.2", "0.000000000001", "12345678.5"} {
			Expect(s.Add(usage.Row{Requests: 1, Cost: c})).To(Succeed())
		}
		Expect(s.Cost()).To(Equal("12345678.800000000001"))
		Expect(s.Requests).To(Equal(int64(4)))
		Expect(s.Estimated()).To(Equal("0.00"))
	})

	It("rejects a cost that is not a decimal", func() {
		var s usage.Sum
		Expect(s.Add(usage.Row{Cost: "abc"})).NotTo(Succeed())
	})

	It("takes the cache read share of the input side, and 0 for none", func() {
		var s usage.Sum
		Expect(s.CacheReadShare()).To(BeZero())
		Expect(s.Add(usage.Row{Input: 100, CacheRead: 300, CacheWrite: 100, Output: 9999})).To(Succeed())
		Expect(s.CacheReadShare()).To(BeNumerically("~", 0.6, 1e-12))
	})
})

var _ = Describe("CSV", func() {
	It("quotes a cell a spreadsheet would run as a formula", func() {
		for in, want := range map[string]string{"=1+1": "'=1+1", "+x": "'+x", "-x": "'-x", "@x": "'@x", "\tx": "'\tx", "plain": "plain", "": ""} {
			Expect(usage.Text(in)).To(Equal(want))
		}
	})

	It("writes the header, the rows and the total, with CSV escaping", func() {
		out, err := usage.CSV([]string{"a", "b"}, [][]string{{"x,y", `q"`}}, []string{"total", "1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("a,b\n\"x,y\",\"q\"\"\"\ntotal,1\n"))
	})
})
