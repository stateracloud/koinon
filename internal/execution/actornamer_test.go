// SPDX-License-Identifier: Apache-2.0

package execution_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
)

type namedCompute struct{ execution.Compute }

func (namedCompute) NewActorName(sandbox string) string { return sandbox + "-00000000" }

// wrapper is a wrapper of another package: it has an Unwrap method like the adapter of WithoutEgress.
type wrapper struct{ execution.Backend }

func (w wrapper) Unwrap() execution.Compute { return w.Backend }

var _ = Describe("ActorNamerOf", func() {
	It("finds the capability on the driver, through WithoutEgress and through a wrapper with Unwrap", func() {
		d := namedCompute{}
		n, ok := execution.ActorNamerOf(execution.WithoutEgress(d))
		Expect(ok).To(BeTrue())
		Expect(n.NewActorName("a")).To(Equal("a-00000000"))

		n, ok = execution.ActorNamerOf(wrapper{Backend: execution.WithoutEgress(d)})
		Expect(ok).To(BeTrue())
		Expect(n.NewActorName("b")).To(Equal("b-00000000"))
	})

	It("reports false for a backend without it", func() {
		_, ok := execution.ActorNamerOf(execution.WithoutEgress(struct{ execution.Compute }{}))
		Expect(ok).To(BeFalse())
		_, ok = execution.ActorNamerOf(nil)
		Expect(ok).To(BeFalse())
	})
})
