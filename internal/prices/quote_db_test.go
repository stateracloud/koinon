// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/prices"
)

var _ = Describe("Resolver.Find", func() {
	BeforeEach(func() { needDocker() })

	It("reports a model nobody enabled as having no price", func(ctx SpecContext) {
		rv := prices.NewResolver(defaultList("v1", "fake/known"))
		qt, err := rv.Find(ctx, store.New(db), adminOrg, uniq(), time.Now())
		Expect(err).To(MatchError(prices.ErrUnpriced))
		Expect(qt.Model.Name).To(BeEmpty())
		reason, ok := prices.Unpriceable(err)
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal(prices.ReasonNoPrice))
	})
})
