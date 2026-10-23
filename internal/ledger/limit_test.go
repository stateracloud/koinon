// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/templates"
)

var _ = Describe("NetLimit", func() {
	now := time.Date(2026, 10, 22, 15, 4, 5, 0, time.UTC)
	midnight := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).Unix()
	ten := templates.USD(1000)

	It("pushes the limit less the window's recorded spend", func() {
		l, err := ledger.NetLimit(ten, "2.5000000000", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Unit).To(Equal(gateway.USD))
		Expect(l.Amount).To(BeNumerically("==", 7.5))
		Expect(l.Window).To(Equal(24 * time.Hour))
		Expect(l.Generation).To(Equal(fmtGen(midnight, "2500000000")))
	})

	It("pushes the full limit with a fresh counter when nothing is recorded", func() {
		l, err := ledger.NetLimit(ten, "0", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeNumerically("==", 10))
		Expect(l.Generation).To(Equal(fmtGen(midnight, "0")))
	})

	It("never pushes a negative amount", func() {
		l, err := ledger.NetLimit(ten, "10.0004", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(BeZero())
	})

	It("starts a new gateway counter whenever the net amount changes", func() {
		a, _ := ledger.NetLimit(ten, "1.000000001", now)
		b, _ := ledger.NetLimit(ten, "1.000000002", now)
		c, _ := ledger.NetLimit(ten, "1.000000002", now.Add(time.Hour))
		d, _ := ledger.NetLimit(ten, "1.000000002", now.Add(24*time.Hour))
		Expect(a.Generation).NotTo(Equal(b.Generation))
		Expect(b).To(Equal(c), "same window, same spend: same budget, so re-pushing is a no-op")
		Expect(d.Generation).NotTo(Equal(c.Generation), "a new window is a new counter")
	})

	It("is exact for amounts a float would round", func() {
		l, err := ledger.NetLimit(templates.USD(30), "0.1", now) // 0.30 - 0.1
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Amount).To(Equal(0.2))
	})

	It("rejects spend that is not a decimal", func() {
		_, err := ledger.NetLimit(ten, "1e3", now)
		Expect(err).To(HaveOccurred())
		_, err = ledger.NetLimit(ten, "-1", now)
		Expect(err).To(HaveOccurred())
	})

	It("renders a valid gateway state", func() {
		l, _ := ledger.NetLimit(ten, "123.4567890123", now)
		var s gateway.State
		s.SetLimit(gateway.AgentRef{Org: "acme", Name: "a"}, &l)
		Expect(s.Validate()).To(Succeed())
	})
})

var _ = Describe("Reached", func() {
	DescribeTable("compares exactly",
		func(limit templates.USD, spent string, want bool) {
			got, err := ledger.Reached(limit, spent)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("below", templates.USD(1), "0.0099999999", false),
		Entry("at", templates.USD(1), "0.0100000000", true),
		Entry("above", templates.USD(1), "0.02", true),
	)
})

var _ = Describe("FormatUSD", func() {
	DescribeTable("trims a numeric",
		func(in, want string) { Expect(ledger.FormatUSD(in)).To(Equal(want)) },
		Entry("zero", "0", "0.00"),
		Entry("trailing zeros", "0.0020000000", "0.002"),
		Entry("whole", "3.0000000000", "3.00"),
		Entry("cents", "12.5", "12.50"),
	)
})

func fmtGen(start int64, nano string) string { return fmt.Sprintf("%d-%s", start, nano) }
