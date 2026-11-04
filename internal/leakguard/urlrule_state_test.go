// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

var _ = Describe("RunState as JSON", func() {
	It("keeps the budgets a run used, without the paths and queries", func() {
		p := leakguard.DefaultURLPolicy()
		st := leakguard.NewRunState()
		Expect(p.CheckURL(st, "https://docs.example.org/guide/install?lang=en").Allow).To(BeTrue())
		Expect(p.CheckSearch(st, "kubernetes release notes").Allow).To(BeTrue())
		b, err := json.Marshal(st)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).NotTo(ContainSubstring("install"))

		back := leakguard.NewRunState()
		Expect(json.Unmarshal(b, back)).To(Succeed())
		again, err := json.Marshal(back)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(MatchJSON(b))

		// the same path and query is not charged twice; the restored state knows it was sent
		small := leakguard.URLPolicy{MaxBytesPerHost: len("/guide/install?lang=en")}
		Expect(small.CheckURL(back, "https://docs.example.org/guide/install?lang=en").Allow).To(BeTrue())
		Expect(small.CheckURL(back, "https://docs.example.org/guide/other").Reason).To(Equal(leakguard.ReasonURLBytesBudget))
	})
})
